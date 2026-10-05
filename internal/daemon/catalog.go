package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/registry"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

// CatalogViewPolicy determines whether changes to Git HEAD replace the
// catalogue's workingtree baseline. The persistent policy supports transports
// that cannot invalidate out-of-band kernel caches, such as macOS 26 FSKit.
type CatalogViewPolicy uint8

const (
	CatalogViewLive CatalogViewPolicy = iota
	// CatalogViewPersistentWorkingTree preserves the initial snapshot plus
	// in-band overlay writes across mounts and service restarts. Git HEAD and
	// index changes do not themselves modify workingtree contents.
	CatalogViewPersistentWorkingTree
)

// SetCatalogViewPolicy must be called before any catalogue activation or
// preparation. Services default to live views. A persistent service must use
// shared catalogue mounts rather than Start or Remount.
func (s *Service) SetCatalogViewPolicy(policy CatalogViewPolicy) error {
	if policy != CatalogViewLive && policy != CatalogViewPersistentWorkingTree {
		return errors.New("unsupported catalogue view policy")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.catalogPolicyLocked || len(s.running) != 0 || len(s.preparing) != 0 {
		return errors.New("set the catalogue view policy before opening repositories")
	}
	s.catalogViewPolicy = policy
	return nil
}

func (s *Service) rejectPersistentRuntimePreparation(ctx context.Context, cfg model.RepoConfig) error {
	s.mu.Lock()
	persistent := s.catalogViewPolicy == CatalogViewPersistentWorkingTree
	active := s.running[cfg.ID] != nil && s.running[cfg.ID].catalogViewPolicy == CatalogViewPersistentWorkingTree
	s.mu.Unlock()
	if active {
		return errors.New("preserve the existing persistent workingtree; preparation cannot replace its baseline")
	}
	if !persistent {
		return nil
	}
	existing, err := s.registry.GetRepo(ctx, cfg.Name)
	if errors.Is(err, registry.ErrRepoNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	s.fillPaths(&existing)
	prepared := existing.PrepareState == model.PrepareStateReady || (existing.PrepareState == "" && existing.PrepareError == "")
	if err := s.validatePersistentCatalogStorage(existing); err != nil {
		return err
	}
	info, err := os.Lstat(existing.MetaDBPath)
	if errors.Is(err, os.ErrNotExist) && !prepared {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("prepared repository is missing its persistent workingtree baseline")
	}
	snap, err := snapshot.New(ctx, existing.MetaDBPath)
	if err != nil {
		return err
	}
	_, _, generation, readErr := snap.ReadState(ctx)
	if err := errors.Join(readErr, snap.Close()); err != nil {
		return err
	}
	if generation != 0 || prepared {
		return errors.New("preserve the existing persistent workingtree; preparation cannot replace its baseline")
	}
	return nil
}

// Persistent baselines may update an application-owned ref. Reject aliases
// below the trusted state root before opening any persistent repository stores.
// The state root itself can be a normal system alias such as macOS /var.
func (s *Service) validatePersistentCatalogStorage(cfg model.RepoConfig) error {
	if cfg.PreparedGitDir {
		return errors.New("persistent workingtree requires application-owned repository storage")
	}
	expected := model.RepoConfig{ID: cfg.ID, Name: cfg.Name, MountRoot: s.root}
	s.fillPaths(&expected)
	if cfg.GitDir != expected.GitDir || cfg.MetaDBPath != expected.MetaDBPath || cfg.OverlayDir != expected.OverlayDir || cfg.OverlayDBPath != expected.OverlayDBPath || cfg.BlobCacheDir != expected.BlobCacheDir {
		return errors.New("persistent workingtree requires application-owned repository storage")
	}
	paths := []struct {
		path      string
		directory bool
	}{
		{cfg.GitDir, true}, {cfg.OverlayDir, true}, {cfg.BlobCacheDir, true},
		{filepath.Join(cfg.OverlayDir, "upper"), true},
		{cfg.MetaDBPath, false}, {cfg.MetaDBPath + "-wal", false}, {cfg.MetaDBPath + "-shm", false},
		{cfg.OverlayDBPath, false}, {cfg.OverlayDBPath + "-wal", false}, {cfg.OverlayDBPath + "-shm", false},
	}
	for _, storage := range paths {
		path := storage.path
		for parent := filepath.Dir(path); model.CleanPath(parent) != model.CleanPath(s.root); parent = filepath.Dir(parent) {
			if parent == filepath.Dir(parent) {
				return errors.New("persistent workingtree storage escapes the state root")
			}
			info, err := os.Lstat(parent)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("persistent workingtree storage contains a symbolic link or invalid directory")
			}
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (storage.directory && !info.IsDir()) || (!storage.directory && !info.Mode().IsRegular()) {
			return errors.New("persistent workingtree storage contains a symbolic link or invalid storage file")
		}
	}
	return nil
}

// OpenCatalogRepository opens a registered repository for a shared catalogue
// mount. Unlike Remount, it does not mount a second filesystem. The caller must
// serialize opening the same repository and detach the shared mount before
// calling Unmount, FreeRepositorySpace, or Close on this service. Start must not
// run on a service used for a shared catalogue.
//
// Existing HEAD and index state are preserved. Opening a prepared checkout does
// not fetch, change branches, or reset the index.
func (s *Service) OpenCatalogRepository(ctx context.Context, name string) (*fusefs.ArtifactFuse, error) {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, errors.New("service is closing")
	}
	policy := s.catalogViewPolicy
	s.catalogPolicyLocked = true
	if rt := s.running[cfg.ID]; rt != nil {
		if rt.stopping || rt.engine == nil {
			s.mu.Unlock()
			return nil, errors.New("repository is stopping")
		}
		fs := fusefs.NewArtifactFuse(rt.cfg, rt.resolver, rt.engine)
		s.mu.Unlock()
		return fs, nil
	}
	s.mu.Unlock()
	if cfg.PrepareState != "" && cfg.PrepareState != model.PrepareStateReady {
		return nil, fmt.Errorf("repository is not prepared: %s", cfg.PrepareState)
	}
	s.fillPaths(&cfg)
	if policy == CatalogViewPersistentWorkingTree {
		if err := s.validatePersistentCatalogStorage(cfg); err != nil {
			return nil, err
		}
	}
	var snap *snapshot.Store
	var headOID, headRef, baselineOID string
	var gen int64
	err = s.withRepoPrepareLock(ctx, cfg.Name, func() error {
		var err error
		if policy == CatalogViewPersistentWorkingTree {
			info, err := os.Lstat(cfg.MetaDBPath)
			if err != nil || !info.Mode().IsRegular() {
				return errors.New("prepared repository is missing its persistent workingtree baseline")
			}
		}
		snap, err = snapshot.New(ctx, cfg.MetaDBPath)
		if err != nil {
			return err
		}
		storedOID, storedRef, storedGen, readErr := snap.ReadState(ctx)
		gen = storedGen
		if policy == CatalogViewPersistentWorkingTree && readErr != nil {
			return readErr
		}
		if policy == CatalogViewPersistentWorkingTree {
			if gen <= 0 || storedOID == "" {
				return errors.New("prepared repository has an invalid persistent workingtree baseline")
			}
			if root, exists, err := snap.LookupNode(ctx, gen, "."); err != nil || !exists || root.Type != "dir" {
				return errors.New("prepared repository has an incomplete persistent workingtree baseline")
			}
			if err := s.git.PinWorkingTreeBaseline(ctx, cfg, storedOID); err != nil {
				return err
			}
		}
		headOID, headRef, err = s.git.ResolveHEAD(ctx, cfg)
		if policy == CatalogViewPersistentWorkingTree && gen != 0 {
			baselineOID = storedOID
			// An unborn or otherwise unavailable HEAD does not erase an
			// existing workingtree. Status reports its Git error separately.
			if err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		if err != nil {
			return err
		}
		if readErr != nil || gen == 0 || storedOID != headOID || storedRef != headRef {
			gen, _, err = s.publishSnapshot(ctx, cfg, snap, headOID, headRef)
		}
		baselineOID = headOID
		return err
	})
	if err != nil {
		if snap != nil {
			_ = snap.Close()
		}
		return nil, err
	}
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		_ = snap.Close()
		return nil, err
	}
	if policy != CatalogViewPersistentWorkingTree {
		if err := ov.ReconcileChecked(ctx, func(path string) (model.BaseNode, bool, error) {
			return snap.LookupNode(ctx, gen, path)
		}); err != nil {
			_ = ov.Close()
			_ = snap.Close()
			return nil, err
		}
	}
	// A Finder lookup may have a short lifetime. Runtime lifetime belongs to the
	// service and ends through Unmount/Close, rather than that lookup's context.
	runtimeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h := hydrator.New(s.git)
	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	s.refreshCommitTime(ctx, cfg, baselineOID, resolver, "catalogue commit timestamp unavailable")
	sizes := newSizeUpdateBatcher(snap, s.logger, cfg.Name)
	sizes.Start(runtimeCtx)
	h.SetOnHydrated(func(_ model.RepoID, oid string, size int64) {
		sizes.Add(resolver.Generation(), oid, size)
	})
	h.Start(s.hydrationWorkers(), cfg)
	engine := &fusefs.Engine{Resolver: resolver, Repo: cfg, Overlay: ov, Hydrator: h}
	rt := &repoRuntime{
		cfg: cfg, ctx: runtimeCtx, cancel: cancel, snapshot: snap, overlay: ov,
		hydrator: h, sizes: sizes, resolver: resolver, engine: engine,
		refresh:           make(chan time.Duration, 1),
		state:             newRuntimeState(cfg.ID, headOID, headRef, gen),
		catalogViewPolicy: policy,
	}
	s.startRuntime(rt)
	s.startRepoBackground(rt)
	return fusefs.NewArtifactFuse(cfg, resolver, engine), nil
}

// FetchCatalogUpdates performs one explicit remote fetch without enabling the
// background refresh loop. It preserves HEAD, branches, the index and overlay.
func (s *Service) FetchCatalogUpdates(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	if cfg.PrepareState != "" && cfg.PrepareState != model.PrepareStateReady {
		return fusefs.ErrRepoNotReady
	}
	if err := s.git.Fetch(ctx, cfg); err != nil {
		return err
	}
	state, err := s.fetchState(ctx, cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if rt := s.running[cfg.ID]; rt != nil {
		markFetchSuccess(&rt.state, time.Now(), state, true)
	}
	s.mu.Unlock()
	return nil
}

// UpdateCatalogMountRoot changes only engine-owned worktree locations. The
// catalogue must be unmounted and its runtimes closed first. No index or branch
// state is rewritten.
func (s *Service) UpdateCatalogMountRoot(ctx context.Context, root string, paths map[string]string) error {
	s.mu.Lock()
	busy := len(s.running) != 0 || len(s.preparing) != 0
	s.mu.Unlock()
	if busy {
		return errors.New("close catalogue runtimes before changing the mount root")
	}
	if err := s.registry.SetCatalogMountPaths(ctx, root, paths); err != nil {
		return err
	}
	s.SetMountRoot(root)
	return nil
}
