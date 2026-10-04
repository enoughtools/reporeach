package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

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
	var snap *snapshot.Store
	var headOID, headRef string
	var gen int64
	err = s.withRepoPrepareLock(ctx, cfg.Name, func() error {
		var err error
		headOID, headRef, err = s.git.ResolveHEAD(ctx, cfg)
		if err != nil {
			return err
		}
		snap, err = snapshot.New(ctx, cfg.MetaDBPath)
		if err != nil {
			return err
		}
		storedOID, storedRef, storedGen, readErr := snap.ReadState(ctx)
		gen = storedGen
		if readErr != nil || gen == 0 || storedOID != headOID || storedRef != headRef {
			gen, _, err = s.publishSnapshot(ctx, cfg, snap, headOID, headRef)
		}
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
	if err := ov.ReconcileChecked(ctx, func(path string) (model.BaseNode, bool, error) {
		return snap.LookupNode(ctx, gen, path)
	}); err != nil {
		_ = ov.Close()
		_ = snap.Close()
		return nil, err
	}
	// A Finder lookup may have a short lifetime. Runtime lifetime belongs to the
	// service and ends through Unmount/Close, rather than that lookup's context.
	runtimeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h := hydrator.New(s.git)
	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	s.refreshCommitTime(ctx, cfg, headOID, resolver, "catalogue commit timestamp unavailable")
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
		refresh: make(chan time.Duration, 1),
		state:   newRuntimeState(cfg.ID, headOID, headRef, gen),
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
