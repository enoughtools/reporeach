package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/registry"
)

type storageTransaction struct {
	Version         int                       `json:"version"`
	Original        model.RepoConfig          `json:"original"`
	DisabledVersion string                    `json:"disabledVersion"`
	PreserveOverlay bool                      `json:"preserveOverlay,omitempty"`
	OverlayIdentity *storageDirectoryIdentity `json:"overlayIdentity,omitempty"`
	dir             string
}

// The overlay keeps metadata that Git cannot restore. Its directory identity
// remains stable across normal SQLite updates and WAL checkpoints.
type storageDirectoryIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

const storageOverlayPathIndex = 1

type movedStoragePath struct{ original, temporary string }

func repositoryStoragePaths(cfg model.RepoConfig) []string {
	return []string{filepath.Dir(cfg.GitDir), cfg.OverlayDir, cfg.BlobCacheDir,
		cfg.MetaDBPath, cfg.MetaDBPath + "-wal", cfg.MetaDBPath + "-shm"}
}

func (s *Service) newStorageTransaction(original, disabled model.RepoConfig) (*storageTransaction, error) {
	// A never-mounted repository may not have an overlay yet. Establish its
	// canonical database before journalling the directory we will preserve.
	store, err := overlay.New(context.Background(), original)
	if err != nil {
		return nil, err
	}
	if err := store.Close(); err != nil {
		return nil, err
	}
	if err := errors.Join(syncStorageDirectory(original.OverlayDir), syncStorageDirectory(filepath.Dir(original.OverlayDir))); err != nil {
		return nil, err
	}
	identity, err := readStorageDirectoryIdentity(original.OverlayDir)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(s.root, ".free-space-"+string(original.ID)+"-")
	if err != nil {
		return nil, err
	}
	transaction := &storageTransaction{Version: 2, Original: original, DisabledVersion: disabled.ConfigVersion,
		PreserveOverlay: true, OverlayIdentity: &identity, dir: dir}
	data, err := json.Marshal(transaction)
	if err != nil {
		_ = os.Remove(dir)
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "transaction.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.Remove(dir)
		return nil, err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := syncStorageDirectory(dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := syncStorageDirectory(s.root); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return transaction, nil
}

func readStorageDirectoryIdentity(path string) (storageDirectoryIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return storageDirectoryIdentity{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return storageDirectoryIdentity{}, errors.New("retained overlay is not a real directory; repository data was retained")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return storageDirectoryIdentity{}, errors.New("cannot identify retained overlay directory; repository data was retained")
	}
	return storageDirectoryIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func sameRepositoryStorage(a, b model.RepoConfig) bool {
	return a.ID == b.ID && a.Name == b.Name && a.PreparedGitDir == b.PreparedGitDir &&
		a.GitDir == b.GitDir && a.OverlayDir == b.OverlayDir && a.OverlayDBPath == b.OverlayDBPath &&
		a.BlobCacheDir == b.BlobCacheDir && a.MetaDBPath == b.MetaDBPath
}

// Restore in reverse move order without replacing data that appeared after the
// original move. Any failed restoration leaves its journalled copy recoverable.
func restoreMovedStoragePaths(transaction storageTransaction, moved []movedStoragePath) (retErr error) {
	if transaction.PreserveOverlay {
		if err := validateRetainedOverlay(transaction); err != nil {
			return err
		}
		for _, path := range moved {
			if path.original == transaction.Original.OverlayDir {
				return errors.New("storage rollback would replace a retained overlay; retained data needs recovery")
			}
		}
	}
	for i := len(moved) - 1; i >= 0; i-- {
		if _, err := os.Lstat(moved[i].original); err == nil {
			retErr = errors.Join(retErr, fmt.Errorf("storage rollback collision at %s; retained data remains at %s", moved[i].original, moved[i].temporary))
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, err)
			continue
		}
		if err := os.Rename(moved[i].temporary, moved[i].original); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("restore data retained at %s: %w", moved[i].temporary, err))
			continue
		}
		if err := errors.Join(syncStorageDirectory(filepath.Dir(moved[i].original)), syncStorageDirectory(transaction.dir)); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("persist restored data at %s: %w", moved[i].original, err))
		}
	}
	return retErr
}

func syncRetainedOverlay(transaction storageTransaction) error {
	if !transaction.PreserveOverlay {
		return nil
	}
	if err := validateRetainedOverlay(transaction); err != nil {
		return err
	}
	return errors.Join(syncStorageDirectory(transaction.Original.OverlayDir), syncStorageDirectory(filepath.Dir(transaction.Original.OverlayDir)))
}

func syncStorageDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func removeStorageTransaction(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return syncStorageDirectory(filepath.Dir(path))
}

// RecoverStorageTransactions must run at startup before mounts or catalogue
// activation. It rolls back interrupted eviction, or finishes deleting an
// eviction whose engine-registration removal committed. Unknown/colliding data
// is retained and reported rather than overwritten.
func (s *Service) RecoverStorageTransactions(ctx context.Context) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".free-space-") {
			continue
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected storage transaction entry; repository data was retained")
		}
		dir := filepath.Join(s.root, entry.Name())
		transaction, err := readStorageTransaction(dir)
		if err != nil {
			// A kill before the manifest was written can leave an empty directory;
			// nothing was disabled or moved yet, so removing an empty dir is safe.
			if errors.Is(err, os.ErrNotExist) {
				if err := os.Remove(dir); err == nil {
					continue
				}
			}
			return fmt.Errorf("read retained storage transaction %s: %w", dir, err)
		}
		if err := s.validateStorageTransaction(transaction); err != nil {
			return err
		}
		cfg := transaction.Original
		if err := s.withRepoPrepareLock(ctx, cfg.Name, func() error {
			return s.withRepoConfigLock(ctx, cfg.Name, func() error {
				latest, err := s.registry.GetRepo(ctx, cfg.Name)
				if errors.Is(err, registry.ErrRepoNotFound) {
					if err := syncRetainedOverlay(transaction); err != nil {
						return err
					}
					return removeStorageTransaction(dir) // Registration removal committed.
				}
				if err != nil {
					return err
				}
				if err := syncRetainedOverlay(transaction); err != nil {
					return err
				}
				if latest.ConfigVersion != transaction.DisabledVersion && latest.ConfigVersion != cfg.ConfigVersion {
					pending := false
					for i := range repositoryStoragePaths(cfg) {
						if _, err := os.Lstat(filepath.Join(dir, fmt.Sprintf("%d", i))); err == nil {
							pending = true
						} else if !errors.Is(err, os.ErrNotExist) {
							return err
						}
					}
					if !pending {
						return removeStorageTransaction(dir) // Rollback committed before journal cleanup.
					}
					return errors.New("storage transaction conflicts with a newer repository configuration; retained data needs recovery")
				}
				if !sameRepositoryStorage(latest, cfg) {
					return errors.New("storage transaction conflicts with changed repository storage; retained data needs recovery")
				}
				for i, destination := range repositoryStoragePaths(cfg) {
					if transaction.PreserveOverlay && i == storageOverlayPathIndex {
						continue
					}
					if transaction.PreserveOverlay {
						if err := validateRetainedOverlay(transaction); err != nil {
							return err
						}
					}
					source := filepath.Join(dir, fmt.Sprintf("%d", i))
					if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
						continue
					} else if err != nil {
						return err
					}
					if _, err := os.Lstat(destination); err == nil {
						return fmt.Errorf("storage recovery collision at %s; retained data remains at %s", destination, source)
					} else if !errors.Is(err, os.ErrNotExist) {
						return err
					}
					if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
						return err
					}
					if err := os.Rename(source, destination); err != nil {
						return err
					}
					if err := errors.Join(syncStorageDirectory(filepath.Dir(destination)), syncStorageDirectory(dir)); err != nil {
						return err
					}
				}
				// A previous recovery attempt may have restored a path and then
				// failed its directory sync. Flush all surviving parents before
				// making the registration usable or removing the journal.
				for _, path := range repositoryStoragePaths(cfg) {
					parent := filepath.Dir(path)
					if _, err := os.Stat(parent); errors.Is(err, os.ErrNotExist) {
						continue
					} else if err != nil {
						return err
					}
					if err := syncStorageDirectory(parent); err != nil {
						return err
					}
				}
				if err := syncStorageDirectory(dir); err != nil {
					return err
				}
				if err := syncRetainedOverlay(transaction); err != nil {
					return err
				}
				if latest.ConfigVersion == transaction.DisabledVersion {
					latest.Enabled = cfg.Enabled
					latest.ConfigVersion = rand.Text()
					if err := s.registry.AddRepo(ctx, latest); err != nil {
						return err
					}
				}
				return removeStorageTransaction(dir)
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

func readStorageTransaction(dir string) (storageTransaction, error) {
	const maxManifestBytes = 64 << 10
	path := filepath.Join(dir, "transaction.json")
	info, err := os.Lstat(path)
	if err != nil {
		return storageTransaction{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxManifestBytes {
		return storageTransaction{}, errors.New("unexpected storage transaction manifest; retained data needs recovery")
	}
	file, err := os.Open(path)
	if err != nil {
		return storageTransaction{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return storageTransaction{}, err
	}
	if !os.SameFile(info, opened) {
		return storageTransaction{}, errors.New("storage transaction manifest changed; retained data needs recovery")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return storageTransaction{}, err
	}
	if len(data) > maxManifestBytes {
		return storageTransaction{}, errors.New("storage transaction manifest is too large; retained data needs recovery")
	}
	var transaction storageTransaction
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&transaction); err != nil {
		return storageTransaction{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return storageTransaction{}, errors.New("storage transaction manifest has trailing data; retained data needs recovery")
	}
	transaction.dir = dir
	return transaction, nil
}

func (s *Service) validateStorageTransaction(transaction storageTransaction) error {
	cfg := transaction.Original
	if transaction.DisabledVersion == "" ||
		(transaction.Version != 1 && transaction.Version != 2) ||
		(transaction.Version == 1 && (transaction.PreserveOverlay || transaction.OverlayIdentity != nil)) ||
		(transaction.Version == 2 && (!transaction.PreserveOverlay || transaction.OverlayIdentity == nil)) {
		return errors.New("unrecognized storage transaction; retained data needs recovery")
	}
	if err := model.ValidateRepoName(cfg.Name); err != nil {
		return err
	}
	if err := model.ValidateRepoName(string(cfg.ID)); err != nil {
		return err
	}
	if !strings.HasPrefix(filepath.Base(transaction.dir), ".free-space-"+string(cfg.ID)+"-") {
		return errors.New("storage transaction has an unexpected identity; retained data needs recovery")
	}
	expected := model.RepoConfig{ID: cfg.ID, Name: cfg.Name}
	s.fillPaths(&expected)
	if cfg.PreparedGitDir || cfg.GitDir != expected.GitDir || cfg.OverlayDir != expected.OverlayDir || cfg.BlobCacheDir != expected.BlobCacheDir || cfg.MetaDBPath != expected.MetaDBPath || cfg.OverlayDBPath != expected.OverlayDBPath {
		return errors.New("storage transaction contains unexpected paths; retained data needs recovery")
	}
	// Recovery must not follow a replaced storage parent outside the state root.
	for _, path := range repositoryStoragePaths(cfg) {
		for parent := filepath.Dir(path); model.CleanPath(parent) != model.CleanPath(s.root); parent = filepath.Dir(parent) {
			if parent == filepath.Dir(parent) {
				return errors.New("storage transaction escapes its state root")
			}
			info, err := os.Lstat(parent)
			if err == nil && info.Mode()&os.ModeSymlink != 0 {
				return errors.New("storage transaction has a symbolic link parent; retained data needs recovery")
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	entries, err := os.ReadDir(transaction.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("storage transaction contains a symbolic link; retained data needs recovery")
		}
		if transaction.PreserveOverlay && entry.Name() == "1" {
			return errors.New("storage transaction moved a retained overlay; retained data needs recovery")
		}
		if entry.Name() != "transaction.json" && entry.Name() != "0" && entry.Name() != "1" && entry.Name() != "2" && entry.Name() != "3" && entry.Name() != "4" && entry.Name() != "5" {
			return errors.New("storage transaction contains unknown files; retained data needs recovery")
		}
	}
	if transaction.PreserveOverlay {
		return validateRetainedOverlay(transaction)
	}
	return nil
}

func validateRetainedOverlay(transaction storageTransaction) error {
	cfg := transaction.Original
	identity, err := readStorageDirectoryIdentity(cfg.OverlayDir)
	if err != nil {
		return fmt.Errorf("verify retained overlay: %w", err)
	}
	if transaction.OverlayIdentity == nil || identity != *transaction.OverlayIdentity {
		return errors.New("retained overlay directory was replaced; retained data needs recovery")
	}
	entries, err := os.ReadDir(cfg.OverlayDir)
	if err != nil {
		return err
	}
	databaseFound, upperFound := false, false
	for _, entry := range entries {
		path := filepath.Join(cfg.OverlayDir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch entry.Name() {
		case "upper":
			upperFound = true
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("retained overlay has an invalid upper directory; retained data needs recovery")
			}
		case "meta.sqlite", "meta.sqlite-wal", "meta.sqlite-shm":
			databaseFound = databaseFound || entry.Name() == "meta.sqlite"
			if !info.Mode().IsRegular() {
				return errors.New("retained overlay has an invalid metadata file; retained data needs recovery")
			}
		default:
			return errors.New("retained overlay has unrecognized files; retained data needs recovery")
		}
	}
	if !databaseFound || !upperFound {
		return errors.New("retained overlay metadata is missing; retained data needs recovery")
	}
	return nil
}
