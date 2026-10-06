package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/registry"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

// DownloadProgress counts unique blobs required by the committed checkout and,
// when enabled, its persistent workingtree baseline. TotalBytes is a lower
// bound while TotalBytesKnown is false. Complete is set only after all blobs
// are present and HEAD has been rechecked.
type DownloadProgress struct {
	TotalBlobs      int64  `json:"totalBlobs"`
	CompletedBlobs  int64  `json:"completedBlobs"`
	TotalBytes      int64  `json:"totalBytes"`
	DownloadedBytes int64  `json:"downloadedBytes"`
	TotalBytesKnown bool   `json:"totalBytesKnown"`
	CurrentPath     string `json:"currentPath,omitempty"`
	HeadOID         string `json:"headOID,omitempty"`
	Complete        bool   `json:"complete"`
}

// DownloadCurrentTree hydrates the current committed tree and the persistent
// workingtree baseline when enabled. It does not reset the index, modify the
// overlay, or download unrelated history. The callback is synchronous; callers
// can cancel ctx in response to progress. Desktop callers serialize this
// operation with repository removal.
func (s *Service) DownloadCurrentTree(ctx context.Context, name string, progress func(DownloadProgress)) (result DownloadProgress, err error) {
	if err := model.ValidateRepoName(name); err != nil {
		return result, err
	}
	err = s.withRepoPrepareLock(ctx, name, func() error {
		cfg, err := s.registry.GetRepo(ctx, name)
		if err != nil {
			return err
		}
		head, _, err := s.git.ResolveHEAD(ctx, cfg)
		if err != nil {
			return err
		}
		nodes, err := s.git.BuildTreeIndex(ctx, cfg, head)
		if err != nil {
			return err
		}
		s.mu.Lock()
		persistent := s.catalogViewPolicy == CatalogViewPersistentWorkingTree
		s.mu.Unlock()
		if persistent {
			// HEAD can move without changing the workingtree (soft/mixed reset,
			// ref updates). Keep the frozen baseline available offline as well
			// as the current committed tree; overlay bytes are already local.
			snap, err := snapshot.New(ctx, cfg.MetaDBPath)
			if err != nil {
				return err
			}
			baseline, _, generation, readErr := snap.ReadState(ctx)
			if err := errors.Join(readErr, snap.Close()); err != nil {
				return err
			}
			if generation != 0 && baseline != head {
				baselineNodes, err := s.git.BuildTreeIndex(ctx, cfg, baseline)
				if err != nil {
					return fmt.Errorf("read persistent workingtree baseline: %w", err)
				}
				nodes = append(nodes, baselineNodes...)
			}
		}
		unique := make([]model.BaseNode, 0, len(nodes))
		seen := make(map[string]bool, len(nodes))
		unknownSizes := int64(0)
		result.HeadOID = head
		for _, node := range nodes {
			if node.Mode&0o170000 == 0o160000 {
				return errors.New("keeping submodules offline is not supported yet")
			}
			if (node.Type != "file" && node.Type != "symlink") || node.ObjectOID == "" {
				continue
			}
			if seen[node.ObjectOID] {
				continue
			}
			if !isObjectOIDName(node.ObjectOID) {
				return errors.New("repository tree has an invalid blob identifier")
			}
			seen[node.ObjectOID] = true
			unique = append(unique, node)
			if node.SizeState == "known" {
				result.TotalBytes += node.SizeBytes
			} else {
				unknownSizes++
			}
		}
		result.TotalBlobs = int64(len(unique))
		result.TotalBytesKnown = unknownSizes == 0
		if progress != nil {
			progress(result)
		}
		// Preview reads and earlier interrupted downloads share this cache.
		// Keep valid cached objects offline rather than fetching another copy
		// merely because the engine's separate promisor clone lacks the blob.
		cachedSizes := make(map[string]int64, len(unique))
		missing := make([]string, 0, len(unique))
		missingAttributes := make([]string, 0)
		for _, node := range unique {
			if err := ctx.Err(); err != nil {
				return err
			}
			cachePath := filepath.Join(cfg.BlobCacheDir, node.ObjectOID)
			if info, statErr := os.Lstat(cachePath); statErr == nil {
				if !info.Mode().IsRegular() {
					return errors.New("blob cache contains an unexpected nonregular file")
				}
				valid, err := s.git.VerifyBlob(ctx, cfg, node.ObjectOID, cachePath)
				if err != nil {
					return err
				}
				if valid {
					cachedSizes[node.ObjectOID] = info.Size()
					continue
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
			missing = append(missing, node.ObjectOID)
		}
		// Attribute paths must be inspected even when their OID was already
		// deduplicated under a different filename (for example a.txt).
		attributesSeen := make(map[string]bool)
		for _, node := range nodes {
			if (node.Type == "file" || node.Type == "symlink") && node.ObjectOID != "" && filepath.Base(node.Path) == ".gitattributes" && !attributesSeen[node.ObjectOID] {
				attributesSeen[node.ObjectOID] = true
				if _, cached := cachedSizes[node.ObjectOID]; !cached {
					missingAttributes = append(missingAttributes, node.ObjectOID)
				}
			}
		}
		if err := s.git.PrefetchBlobs(ctx, cfg, missingAttributes); err != nil {
			return err
		}
		for oid := range attributesSeen {
			var attributes []byte
			if _, cached := cachedSizes[oid]; cached {
				attributes, err = readCachedCheckoutAttributes(filepath.Join(cfg.BlobCacheDir, oid))
			} else {
				attributes, err = s.git.ReadBlob(ctx, cfg, oid, 1<<20)
			}
			if err != nil {
				return fmt.Errorf("cannot verify checkout attributes: %w", err)
			}
			if bytes.Contains(attributes, []byte("filter=")) {
				return errors.New("keeping repositories with Git checkout filters or LFS offline is not supported yet")
			}
		}
		// cat-file lazily fetching each missing OID is a connection per file.
		// Acquire selected current-tree blobs as packs before local extraction.
		if err := s.git.PrefetchBlobs(ctx, cfg, missing); err != nil {
			return err
		}
		for _, node := range unique {
			if err := ctx.Err(); err != nil {
				return err
			}
			result.CurrentPath = node.Path
			size, valid := cachedSizes[node.ObjectOID]
			if !valid {
				cachePath := filepath.Join(cfg.BlobCacheDir, node.ObjectOID)
				size, err = s.git.BlobToCache(ctx, cfg, node.ObjectOID, cachePath)
				if err != nil {
					return err
				}
			}
			if node.SizeState != "known" {
				result.TotalBytes += size
				unknownSizes--
			}
			result.TotalBytesKnown = unknownSizes == 0
			result.CompletedBlobs++
			result.DownloadedBytes += size
			if progress != nil {
				progress(result)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		latest, _, err := s.git.ResolveHEAD(ctx, cfg)
		if err != nil {
			return err
		}
		if latest != head {
			return errors.New("repository changed during download; keep it downloaded again to include the new checkout")
		}
		result.CurrentPath = ""
		result.Complete = true
		if progress != nil {
			progress(result)
		}
		return nil
	})
	return result, err
}

// Bound cached attribute reads just as ReadBlob bounds uncached ones. The
// verified cache contains bytes, not a UTF-8 string or Git-filtered worktree.
func readCachedCheckoutAttributes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("blob cache contains an unexpected nonregular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, model.ErrBlobTooLarge
	}
	return data, nil
}

// FreeRepositorySpace removes only engine-owned, remotely recoverable data.
// The clean overlay database remains in place because local file metadata is
// not stored in Git and must survive reacquiring the repository.
// The desktop catalogue must detach and gate access to this repository before
// calling, and retain its discovery entry independently of the engine registry.
// A rejected operation leaves all data in place, but its runtime is stopped.
func (s *Service) FreeRepositorySpace(ctx context.Context, name string) (retErr error) {
	if err := model.ValidateRepoName(name); err != nil {
		return err
	}
	var original, disabled model.RepoConfig
	var transaction *storageTransaction
	if err := s.withRepoConfigLock(ctx, name, func() error {
		var err error
		original, err = s.registry.GetRepo(ctx, name)
		if err != nil {
			return err
		}
		if err := s.validateOwnedRepoStorage(original); err != nil {
			return err
		}
		disabled = original
		disabled.Enabled = false
		disabled.ConfigVersion = rand.Text()
		transaction, err = s.newStorageTransaction(original, disabled)
		if err != nil {
			return err
		}
		if err := s.registry.AddRepo(ctx, disabled); err != nil {
			_ = os.RemoveAll(transaction.dir)
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	removed := false
	rollbackIncomplete := false
	defer func() {
		if removed || rollbackIncomplete {
			return
		}
		// A failure or cancellation must not strand the durable registration in
		// a disabled state. A new version prevents stale preparation work.
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := s.withRepoConfigLock(restoreCtx, name, func() error {
			latest, err := s.registry.GetRepo(restoreCtx, name)
			if err != nil {
				return err
			}
			if latest.ConfigVersion != disabled.ConfigVersion {
				return registry.ErrRepoChanged
			}
			if !sameRepositoryStorage(latest, original) {
				return errors.New("repository storage changed; retained data needs recovery")
			}
			if err := syncRetainedOverlay(*transaction); err != nil {
				return err
			}
			latest.Enabled = original.Enabled
			latest.ConfigVersion = rand.Text()
			return s.registry.AddRepo(restoreCtx, latest)
		})
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("restore repository registration: %w", err))
		} else if err := removeStorageTransaction(transaction.dir); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove completed storage transaction: %w", err))
		}
	}()
	if err := s.unmount(original.ID); err != nil {
		return err
	}
	s.git.CloseRepository(original.GitDir)
	return s.withRepoPrepareLock(ctx, name, func() error {
		return s.withRepoConfigLock(ctx, name, func() error {
			latest, err := s.registry.GetRepo(ctx, name)
			if err != nil {
				return err
			}
			if latest.ConfigVersion != disabled.ConfigVersion {
				return registry.ErrRepoChanged
			}
			if !sameRepositoryStorage(latest, original) {
				return errors.New("repository storage changed; retained data needs recovery")
			}
			if err := s.validateOwnedRepoStorage(latest); err != nil {
				return err
			}
			ov, err := overlay.New(ctx, latest)
			if err != nil {
				return err
			}
			// DirtyCount excludes delete tombstones, which are still local work.
			entries, readErr := ov.ListByPrefix(ctx, ".")
			closeErr := ov.Close()
			if readErr != nil || closeErr != nil {
				return errors.Join(readErr, closeErr)
			}
			if len(entries) != 0 {
				s.mu.Lock()
				persistent := s.catalogViewPolicy == CatalogViewPersistentWorkingTree
				s.mu.Unlock()
				if persistent {
					return errors.New("persistent workingtree edits are retained; safe overlay compaction is required before freeing space")
				}
				return errors.New("repository has local files or deletions; commit and push them before freeing space")
			}
			s.mu.Lock()
			persistent := s.catalogViewPolicy == CatalogViewPersistentWorkingTree
			s.mu.Unlock()
			if persistent {
				snap, err := snapshot.New(ctx, latest.MetaDBPath)
				if err != nil {
					return err
				}
				baseline, _, generation, readErr := snap.ReadState(ctx)
				if err := errors.Join(readErr, snap.Close()); err != nil {
					return err
				}
				head, _, err := s.git.ResolveHEAD(ctx, latest)
				if err != nil {
					return err
				}
				if generation <= 0 || baseline != head {
					return errors.New("persistent workingtree baseline differs from Git HEAD; preserve it until a safe workingtree migration can be performed")
				}
			}
			if err := verifyEmptyOverlayUpper(latest.OverlayDir); err != nil {
				return err
			}
			if err := s.git.VerifySafeToDiscard(ctx, latest); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := syncRetainedOverlay(*transaction); err != nil {
				return err
			}
			paths := repositoryStoragePaths(latest)
			tombstone := transaction.dir
			var moved []movedStoragePath
			rollback := func(cause error) error {
				err := restoreMovedStoragePaths(*transaction, moved)
				rollbackIncomplete = err != nil
				return errors.Join(cause, err)
			}
			for i, path := range paths {
				if transaction.PreserveOverlay && i == storageOverlayPathIndex {
					continue
				}
				if err := validateRetainedOverlay(*transaction); err != nil {
					return rollback(err)
				}
				if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return rollback(err)
				}
				target := filepath.Join(tombstone, fmt.Sprintf("%d", i))
				if err := os.Rename(path, target); err != nil {
					return rollback(err)
				}
				moved = append(moved, movedStoragePath{path, target})
				if err := errors.Join(syncStorageDirectory(filepath.Dir(path)), syncStorageDirectory(tombstone)); err != nil {
					return rollback(err)
				}
			}
			if err := validateRetainedOverlay(*transaction); err != nil {
				return rollback(err)
			}
			if err := s.registry.RemoveRepo(ctx, name); err != nil {
				return rollback(err)
			}
			removed = true
			// The commit point is registry removal. A cleanup failure retains
			// recoverable data in the tombstone rather than restoring half a repo.
			if err := validateRetainedOverlay(*transaction); err != nil {
				return fmt.Errorf("repository released; retained metadata needs recovery: %w", err)
			}
			if err := removeStorageTransaction(tombstone); err != nil {
				return fmt.Errorf("repository released; cleanup remains at %s: %w", tombstone, err)
			}
			return nil
		})
	})
}

func (s *Service) validateOwnedRepoStorage(cfg model.RepoConfig) error {
	if err := model.ValidateRepoName(string(cfg.ID)); err != nil {
		return errors.New("repository has an unsafe storage identifier")
	}
	expected := model.RepoConfig{ID: cfg.ID, Name: cfg.Name}
	s.fillPaths(&expected)
	if cfg.PreparedGitDir || cfg.GitDir != expected.GitDir || cfg.OverlayDir != expected.OverlayDir ||
		cfg.BlobCacheDir != expected.BlobCacheDir || cfg.MetaDBPath != expected.MetaDBPath || cfg.OverlayDBPath != expected.OverlayDBPath {
		return errors.New("freeing space is allowed only for engine-owned repository storage")
	}
	paths := []string{filepath.Dir(cfg.GitDir), cfg.OverlayDir, cfg.BlobCacheDir, cfg.MetaDBPath, cfg.MetaDBPath + "-wal", cfg.MetaDBPath + "-shm"}
	for _, path := range paths {
		// Reject symlinked parents beneath the state root, not merely the final
		// component, to prevent deletion outside engine-owned storage.
		for parent := filepath.Dir(path); model.CleanPath(parent) != model.CleanPath(s.root); parent = filepath.Dir(parent) {
			if parent == filepath.Dir(parent) {
				return errors.New("repository storage escapes the state root")
			}
			info, err := os.Lstat(parent)
			if err == nil && info.Mode()&os.ModeSymlink != 0 {
				return errors.New("repository storage contains a symbolic link")
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return errors.New("repository storage contains a symbolic link")
			}
			return nil
		}); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(filepath.Dir(cfg.GitDir))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "git" {
			return errors.New("repository storage contains untracked local files")
		}
	}
	cache, err := os.ReadDir(cfg.BlobCacheDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range cache {
		if entry.IsDir() || (!isObjectOIDName(entry.Name()) && !strings.HasPrefix(entry.Name(), ".artifact-fs-blob-")) {
			return errors.New("blob cache contains unrecognized local files")
		}
	}
	storage, err := os.ReadDir(cfg.OverlayDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range storage {
		if entry.Name() != "upper" && entry.Name() != "meta.sqlite" && entry.Name() != "meta.sqlite-wal" && entry.Name() != "meta.sqlite-shm" {
			return errors.New("overlay storage contains unrecognized local files")
		}
	}
	return nil
}

func verifyEmptyOverlayUpper(overlayDir string) error {
	return filepath.WalkDir(filepath.Join(overlayDir, "upper"), func(_ string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return errors.New("repository contains untracked or ignored local files; preserve them before freeing space")
		}
		return nil
	})
}
