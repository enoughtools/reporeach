package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
)

type Options struct {
	StateDir       string
	MountRoot      string
	Socket         string
	GHPath         string
	FSKitSocketDir string
	Logger         *slog.Logger
}

// Service owns the catalogue, daemon runtimes, and background operations.
// Closing its control window does not affect its lifetime.
type Service struct {
	ctx                     context.Context
	cancel                  context.CancelFunc
	opts                    Options
	logger                  *slog.Logger
	github                  *GitHub
	engine                  *daemon.Service
	mu                      sync.Mutex
	state                   persistedState
	message                 string
	retainedCheckoutMessage string
	ops                     []Operation
	cancels                 map[string]context.CancelFunc
	locks                   map[string]chan struct{}
	pins                    map[string]string
	closing                 bool
	quitPrepared            bool
	quitReady               chan struct{}
	closed                  bool
	closeMu                 sync.Mutex
	maintenance             bool
	recoveryRequired        bool
	workers                 sync.WaitGroup
	// lifecycle protects mount changes. Repository operations never hold mu
	// while waiting on FUSE or invoking git.
	lifecycle              sync.Mutex
	catalog                *catalogfs.FileSystem
	catalogMetadata        *overlay.Store
	mounted                fusefs.MountedFS
	mountRecoveryAvailable bool
	recoverMountCatalogue  func(context.Context) error
	dependencyReady        func() bool
	mountCatalogue         func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error)
	closeCatalogueMetadata func(*overlay.Store) error
	// FSKit 26 cannot invalidate catalogue changes made outside the mounted
	// filesystem. Publish them only after a normal, successful unmount.
	quiescentCatalogue bool
	// Native virtual repositories live behind ordinary catalogue directories.
	// Local checkouts never need this filesystem to expose their files.
	hybridCatalogue bool
	// Resolve the state ancestor once at startup. Finder reports the canonical
	// mounted path; Status must not traverse a live filesystem to recover it.
	cachedVirtualRoot string
	preview           *PreviewCache
	previewGit        *gitstore.Store
	previewBlobSizes  map[string]map[string]int64
	previewMetaCounts map[string]previewMetadataAccounting
}

func New(ctx context.Context, opts Options) (*Service, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(opts.StateDir) {
		return nil, errors.New("state directory must be absolute")
	}
	if err := validateMountRoot(opts.MountRoot); err != nil {
		return nil, err
	}
	if err := privateDirectory(opts.StateDir, true); err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.GHPath == "" {
		var err error
		opts.GHPath, err = exec.LookPath("gh")
		if err != nil {
			return nil, errors.New("the bundled GitHub CLI is missing")
		}
	}
	if !filepath.IsAbs(opts.GHPath) {
		return nil, errors.New("GitHub CLI path must be absolute")
	}
	state, err := readState(filepath.Join(opts.StateDir, "catalogue.json"), opts.MountRoot)
	if err != nil {
		return nil, err
	}
	if pathsOverlap(state.MountRoot, opts.StateDir) {
		return nil, errors.New("mount folder and state directory must be separate")
	}
	if opts.FSKitSocketDir != "" {
		if !filepath.IsAbs(opts.FSKitSocketDir) || pathsLexicallyOverlap(state.MountRoot, opts.FSKitSocketDir) {
			return nil, errors.New("the File System Extension connection folder must be absolute and outside the repository mount folder")
		}
		if err := privateDirectory(opts.FSKitSocketDir, false); err != nil {
			return nil, errors.New("the private shared File System Extension connection folder is unavailable")
		}
		opts.FSKitSocketDir, err = filepath.EvalSymlinks(opts.FSKitSocketDir)
		if err != nil || pathsOverlap(state.MountRoot, opts.FSKitSocketDir) {
			return nil, errors.New("mount folder and File System Extension connection folder must be separate")
		}
	}
	var cachedVirtualRoot string
	if runtime.GOOS == "darwin" {
		canonicalStateDir, err := filepath.EvalSymlinks(opts.StateDir)
		if err != nil {
			return nil, fmt.Errorf("resolve private virtual catalogue location: %w", err)
		}
		cachedVirtualRoot = filepath.Join(canonicalStateDir, "native-catalogue", "volume")
	}
	// Cancellation requests a normal shutdown once construction is complete.
	// It must not independently tear down a bridge still serving open files.
	serviceCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopStartupCancel := context.AfterFunc(ctx, cancel)
	constructed := false
	defer func() {
		stopStartupCancel()
		if !constructed {
			cancel()
		}
	}()
	engine, err := daemon.New(serviceCtx, filepath.Join(opts.StateDir, "engine"), opts.Logger)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "darwin" {
		if err := engine.SetCatalogViewPolicy(daemon.CatalogViewPersistentWorkingTree); err != nil {
			_ = engine.Close()
			return nil, err
		}
	}
	if err := engine.RecoverStorageTransactions(ctx); err != nil {
		_ = engine.Close()
		return nil, fmt.Errorf("recover repository storage: %w", err)
	}
	engine.SetMountRoot(state.MountRoot)
	s := &Service{
		ctx: serviceCtx, cancel: cancel, opts: opts, logger: opts.Logger,
		github: NewGitHub(opts.GHPath), engine: engine, state: state,
		ops: []Operation{}, cancels: map[string]context.CancelFunc{},
		locks: map[string]chan struct{}{}, pins: map[string]string{},
		quitReady:       make(chan struct{}),
		dependencyReady: platformDependencyReady, quiescentCatalogue: runtime.GOOS == "darwin",
		hybridCatalogue: runtime.GOOS == "darwin", cachedVirtualRoot: cachedVirtualRoot,
	}
	s.mountCatalogue = s.platformMountCatalogue
	s.recoverMountCatalogue = s.platformRecoverMountCatalogue
	s.closeCatalogueMetadata = (*overlay.Store).Close
	if err := s.recoverPreviewContentEviction(ctx); err != nil {
		_ = engine.Close()
		cancel()
		return nil, fmt.Errorf("recover preview content cache: %w", err)
	}
	if err := s.recoverLocalHandoffs(ctx); err != nil {
		_ = engine.Close()
		cancel()
		return nil, fmt.Errorf("recover local checkout handoff: %w", err)
	}
	s.retainedCheckoutMessage, err = s.retainedLocalCheckoutMessage()
	if err != nil {
		_ = engine.Close()
		cancel()
		return nil, fmt.Errorf("inspect retained local checkouts: %w", err)
	}
	if err := s.recoverRootMigration(ctx); err != nil {
		_ = engine.Close()
		cancel()
		return nil, fmt.Errorf("recover mount folder: %w", err)
	}
	for _, repo := range s.state.Repositories {
		if repo.LocalPath != "" {
			if _, err := validateAdoptionLocalSource(repo.LocalPath, opts.StateDir, opts.StateDir); err != nil {
				_ = engine.Close()
				cancel()
				return nil, errors.New("the local checkout overlaps EnoughRepos's private storage")
			}
			continue
		}
		if repo.Source == "manual" {
			remote, err := parseAdoptionRemote(repo.CloneURL)
			if err == nil {
				err = s.adoptionAllowedLocked(remote)
			}
			if err != nil {
				_ = engine.Close()
				cancel()
				return nil, fmt.Errorf("invalid adopted repository location: %w", err)
			}
		}
	}
	// Reconcile UI state with durable engine registration after interrupted work.
	configs, err := engine.ListRepos(ctx)
	if err != nil {
		_ = engine.Close()
		cancel()
		return nil, err
	}
	prepared := map[string]bool{}
	for _, cfg := range configs {
		prepared[cfg.Name] = cfg.PrepareState == "" || cfg.PrepareState == model.PrepareStateReady
	}
	for i := range s.state.Repositories {
		repo := &s.state.Repositories[i]
		if repo.LocalPath != "" {
			repo.State = "local"
		} else if prepared[engineName(repo.ID)] {
			repo.State = "available"
		} else {
			repo.State = "virtual"
		}
		// Pin intent survives a restart, but stays available until verified again.
		repo.Error = ""
	}
	for _, repo := range s.state.Repositories {
		if repo.LocalPath == "" {
			s.notePreviewCachedBytes(repo.ID)
		}
	}
	if err := s.persistLocked(); err != nil {
		_ = engine.Close()
		cancel()
		return nil, err
	}
	if s.hybridCatalogue {
		s.previewGit = gitstore.New(opts.Logger)
		s.preview, err = NewPreviewCache(serviceCtx, opts.StateDir, s.previewGit, s.github)
		if err != nil {
			s.previewGit.Close()
			_ = engine.Close()
			cancel()
			return nil, err
		}
	}
	// Stop forwarding startup cancellation before committing the independent
	// lifetime. A cancellation that already arrived still rejects startup.
	stopStartupCancel()
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return nil, err
	}
	constructed = true
	return s, nil
}

func (s *Service) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	firstClose := !s.closing
	s.closing = true
	s.cancel()
	for _, cancel := range s.cancels {
		cancel()
	}
	s.mu.Unlock()
	if firstClose {
		s.github.Close()
	}
	s.workers.Wait()
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if err := s.detachLocked(); err != nil {
		return err
	}
	if err := s.engine.Close(); err != nil {
		return err
	}
	if s.preview != nil {
		if err := s.preview.Close(); err != nil {
			return err
		}
		s.previewGit.Close()
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func (s *Service) Restore() {
	s.startPreviewSeeding()
	s.mu.Lock()
	desired := s.state.MountDesired
	s.mu.Unlock()
	if desired {
		if err := s.Mount(s.ctx); err != nil {
			s.mu.Lock()
			s.message = safeError(err)
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	if s.closing || s.quitPrepared {
		s.mu.Unlock()
		return
	}
	s.workers.Add(1)
	s.mu.Unlock()
	go s.pinLoop()
}

func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	repos := append([]Repository(nil), s.state.Repositories...)
	if repos == nil {
		repos = []Repository{}
	}
	operations := append([]Operation(nil), s.ops...)
	if operations == nil {
		operations = []Operation{}
	}
	var account *Account
	if s.state.Account != nil {
		copyAccount := *s.state.Account
		account = &copyAccount
	}
	var virtualRoot string
	if s.hybridCatalogue {
		virtualRoot = s.cachedVirtualRoot
	}
	return Status{Version: Version, MountRoot: s.state.MountRoot,
		VirtualRoot: virtualRoot, MountRecoveryAvailable: s.mountRecoveryAvailable,
		Mounted: s.mounted != nil, DependencyReady: s.dependencyReady(),
		Account: account, Repositories: repos, Operations: operations,
		Organizations: s.organizationsLocked(), Message: strings.TrimSpace(s.message + "\n" + s.retainedCheckoutMessage)}
}

func (s *Service) Discover(ctx context.Context) (status Status, retErr error) {
	repos, account, err := s.github.Discover(ctx)
	if err != nil {
		return Status{}, err
	}
	for _, repo := range repos {
		if err := validateRepository(repo); err != nil {
			return Status{}, err
		}
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	if s.closing || s.quitPrepared || s.maintenance || s.recoveryRequired {
		s.mu.Unlock()
		return Status{}, errors.New("wait for the repository service to finish its current change")
	}
	s.mu.Unlock()
	remount, err := s.quiesceCatalogueChange(ctx)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		retErr = errors.Join(retErr, s.restoreCatalogueAfterChange(ctx, remount))
		status = s.Status()
	}()
	s.mu.Lock()
	if s.closing || s.quitPrepared || s.recoveryRequired {
		s.mu.Unlock()
		return Status{}, errors.New("repository service is closing or needs recovery")
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Status{}, err
	}
	previous := s.state
	oldEntries := s.entriesLocked()
	old := map[string]Repository{}
	owners := map[string]string{}
	for _, owner := range s.state.DisabledOrganizations {
		owners[strings.ToLower(owner)] = owner
	}
	for _, repo := range s.state.Repositories {
		old[strings.ToLower(repo.ID)] = repo
		owners[strings.ToLower(repo.Owner)] = repo.Owner
	}
	for i := range repos {
		repo := &repos[i]
		if existing, ok := old[strings.ToLower(repo.ID)]; ok {
			if existing.Source == "manual" {
				// Discovery must not replace a native Git remote or its auth mode.
				*repo = existing
			} else {
				repo.State, repo.Pinned, repo.DownloadedBytes, repo.Error = existing.State, existing.Pinned, existing.DownloadedBytes, existing.Error
				repo.Disabled, repo.Source = existing.Disabled, existing.Source
				repo.LocalPath, repo.LocalKind = existing.LocalPath, existing.LocalKind
				// Keep the stable catalogue identity when GitHub changes casing.
				repo.ID, repo.Owner, repo.Name = existing.ID, existing.Owner, existing.Name
				repo.CloneURL, repo.HTMLURL = existing.CloneURL, existing.HTMLURL
			}
			delete(old, strings.ToLower(repo.ID))
		} else {
			key := strings.ToLower(repo.Owner)
			if owner, exists := owners[key]; exists {
				repo.Owner = owner
				repo.ID = owner + "/" + repo.Name
				repo.HTMLURL = "https://github.com/" + repo.ID
				repo.CloneURL = repo.HTMLURL + ".git"
			} else {
				owners[key] = repo.Owner
			}
			repo.State = "virtual"
		}
	}
	// An access change must not orphan local edits or pinned data. Keep previous
	// entries until the user explicitly frees them, and report the access change.
	for _, repo := range old {
		if repo.Source != "manual" {
			repo.Error = "Repository was not returned by GitHub. Local data has been retained."
		}
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool { return strings.ToLower(repos[i].ID) < strings.ToLower(repos[j].ID) })
	s.state.Repositories, s.state.Account = repos, account
	err = ctx.Err()
	if err == nil {
		err = s.publishHybridCatalogueLocked()
	}
	if err == nil {
		err = s.persistLocked()
		if err == nil {
			err = ctx.Err()
		}
	}
	if err != nil {
		s.state = previous
		err = errors.Join(err, s.persistLocked())
	}
	catalog := s.catalog
	entries := s.entriesLocked()
	if err != nil {
		s.mu.Unlock()
		return Status{}, err
	}
	if catalog != nil {
		if err := catalog.SetEntries(entries); err != nil {
			s.state = previous
			persistErr := s.persistLocked()
			catalogErr := catalog.SetEntries(oldEntries)
			s.mu.Unlock()
			return Status{}, errors.Join(err, persistErr, catalogErr)
		}
	}
	s.mu.Unlock()
	return s.Status(), nil
}

func (s *Service) Mount(ctx context.Context) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if err := s.mountLocked(ctx); err != nil {
		available := s.platformMountRecoveryAvailable()
		s.mu.Lock()
		s.mountRecoveryAvailable = available
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.MountDesired = true
	s.mountRecoveryAvailable = false
	s.message = ""
	return s.persistLocked()
}

func (s *Service) mountLocked(ctx context.Context) error {
	s.mu.Lock()
	if s.closing || s.quitPrepared {
		s.mu.Unlock()
		return errors.New("service is closing")
	}
	if s.recoveryRequired {
		s.mu.Unlock()
		return errors.New("restart EnoughRepos to recover an interrupted storage operation")
	}
	if s.mounted != nil {
		s.mu.Unlock()
		return nil
	}
	root, entries := s.state.MountRoot, s.entriesLocked()
	s.mu.Unlock()
	if err := s.platformPreflightMountRecovery(); err != nil {
		return err
	}
	if !s.dependencyReady() {
		return errors.New(platformDependencyMessage())
	}
	if s.opts.FSKitSocketDir != "" && pathsOverlap(root, s.opts.FSKitSocketDir) {
		return errors.New("mount folder and File System Extension connection folder must be separate")
	}
	if err := s.checkCatalogueDirectory(root); err != nil {
		return err
	}
	if err := s.publishHybridCatalogue(); err != nil {
		return err
	}
	// A previous session may be detached but retain its store after a close
	// failure. Finish that ownership before opening another database handle.
	if err := s.closeCatalogueStoreLocked(); err != nil {
		return err
	}
	metadata, err := openCatalogueMetadata(ctx, s.opts.StateDir)
	if err != nil {
		return fmt.Errorf("open catalogue metadata: %w", err)
	}
	s.mu.Lock()
	s.catalogMetadata = metadata
	s.mu.Unlock()
	fs, err := catalogfs.NewWithPreviewContent(entries, func(ctx context.Context, entry catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		if backend, ok := s.engine.ExistingCatalogRepository(engineName(entry.ID)); ok {
			return backend, nil
		}
		unlock, err := s.lockRepoForActivation(ctx, entry.ID)
		if err != nil {
			return nil, err
		}
		defer unlock()
		return s.ensureRepository(ctx, entry.ID)
	}, metadata, s.cataloguePreview, s.cataloguePreviewContent)
	if err != nil {
		return errors.Join(err, s.closeCatalogueStoreLocked())
	}
	s.mu.Lock()
	s.catalog = fs
	s.mu.Unlock()
	mounted, err := s.mountCatalogue(ctx, root, fs)
	if err != nil && mounted == nil {
		return errors.Join(err, s.closeCatalogueStoreLocked())
	}
	if mounted == nil {
		return errors.Join(errors.New("filesystem mount returned no lifecycle owner"), s.closeCatalogueStoreLocked())
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		cleanupErr := mounted.Unmount()
		drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cleanupErr == nil {
			cleanupErr = mounted.Join(drainCtx)
		}
		if cleanupErr != nil {
			// Keep ownership so Close can retry detachment without closing the
			// stores referenced by a filesystem that is still in use.
			s.mu.Lock()
			s.catalog, s.mounted = fs, mounted
			s.mu.Unlock()
		} else {
			cleanupErr = s.closeCatalogueStoreLocked()
		}
		return errors.Join(errors.New("service is closing"), cleanupErr)
	}
	s.catalog, s.mounted = fs, mounted
	if err != nil {
		// A failed command is not proof that the OS did not mount the volume.
		// Retain ownership and the live stores until detachment is proven.
		s.message = safeError(err)
	}
	// Join observes unexpected unmounts without keeping a second mount alive.
	s.workers.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.workers.Done()
		err := mounted.Join(s.ctx)
		// Serialize database disposal with mount changes. Join errors alone do
		// not establish that an uncertain mount has detached and drained.
		s.lifecycle.Lock()
		defer s.lifecycle.Unlock()
		s.mu.Lock()
		// A canceled observation does not prove the kernel mount detached.
		// Retain ownership until Close can unmount it after parent cancellation.
		if s.mounted == mounted && !s.closing && s.ctx.Err() == nil && err == nil {
			s.mounted = nil
			s.message = "The repository folder was unmounted. Open EnoughRepos to mount it again."
			s.mu.Unlock()
			if closeErr := s.closeCatalogueStoreLocked(); closeErr != nil {
				s.mu.Lock()
				s.message = safeError(closeErr)
				s.mu.Unlock()
			}
			return
		}
		if s.mounted == mounted && !s.closing && s.ctx.Err() == nil && err != nil {
			s.message = safeError(err)
		}
		s.mu.Unlock()
	}()
	return err
}

func (s *Service) Unmount(ctx context.Context) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	closing := s.closing || s.quitPrepared
	s.mu.Unlock()
	if closing {
		return errors.New("service is closing")
	}
	if err := s.detachLocked(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.MountDesired = false
	return s.persistLocked()
}

// PrepareQuit detaches normally while retaining mount intent for the next launch.
// The app must leave its helper running if this reports a busy or uncertain mount.
func (s *Service) PrepareQuit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return errors.New("service is closing")
	}
	if err := s.detachLocked(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// lifecycle remains held through the terminal claim. A control request
	// cannot remount between successful preparation and helper termination.
	s.mu.Lock()
	if !s.quitPrepared {
		s.quitPrepared = true
		close(s.quitReady)
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) detachLocked() error {
	s.mu.Lock()
	mounted := s.mounted
	s.mu.Unlock()
	if mounted == nil {
		return s.closeCatalogueStoreLocked()
	}
	if err := mounted.Unmount(); err != nil {
		return fmt.Errorf("unmount repository folder: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := mounted.Join(ctx); err != nil {
		return fmt.Errorf("drain repository folder: %w", err)
	}
	s.mu.Lock()
	if s.mounted == mounted {
		s.mounted = nil
	}
	s.mu.Unlock()
	return s.closeCatalogueStoreLocked()
}

func (s *Service) Settings(ctx context.Context, root string) error {
	if err := validateMountRoot(root); err != nil {
		return err
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	if s.closing || s.quitPrepared {
		s.mu.Unlock()
		return errors.New("service is closing")
	}
	if len(s.cancels) > 0 || s.maintenance {
		s.mu.Unlock()
		return errors.New("wait for repository operations to finish before changing the folder")
	}
	oldRoot := s.state.MountRoot
	if model.CleanPath(root) == model.CleanPath(oldRoot) {
		s.mu.Unlock()
		return nil
	}
	// Reject nested or enclosing destinations before resolving symlinks or
	// opening a directory. The old root may be served by this very Go process;
	// opening our own FUSE directories can deadlock Go's runtime poller.
	if pathsLexicallyOverlap(root, oldRoot) {
		s.mu.Unlock()
		return errors.New("choose a folder outside the current repository folder")
	}
	if s.opts.FSKitSocketDir != "" && pathsLexicallyOverlap(root, s.opts.FSKitSocketDir) {
		s.mu.Unlock()
		return errors.New("mount folder and File System Extension connection folder must be separate")
	}
	var localRepositories []Repository
	for _, repo := range s.state.Repositories {
		if repo.LocalPath != "" {
			// Ordinary adopted/kept checkouts retain their own physical location
			// when the catalogue moves. The new catalogue links to that location.
			continue
		}
		if repo.Source != "manual" {
			continue
		}
		remote, err := parseAdoptionRemote(repo.CloneURL)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if remote.localPath != "" {
			if pathsLexicallyOverlap(root, remote.localPath) {
				s.mu.Unlock()
				return errors.New("choose a mount folder outside the adopted Git source folders")
			}
			localRepositories = append(localRepositories, repo)
		}
	}
	s.maintenance = true
	desired := s.state.MountDesired
	s.mu.Unlock()
	blocked := false
	defer func() {
		s.mu.Lock()
		s.maintenance = blocked
		s.recoveryRequired = blocked
		s.mu.Unlock()
	}()
	if pathsOverlap(root, oldRoot) {
		return errors.New("choose a folder outside the current repository folder")
	}
	if pathsOverlap(root, s.opts.StateDir) {
		return errors.New("mount folder and state directory must be separate")
	}
	if s.opts.FSKitSocketDir != "" && pathsOverlap(root, s.opts.FSKitSocketDir) {
		return errors.New("mount folder and File System Extension connection folder must be separate")
	}
	for _, repo := range localRepositories {
		if err := validateManualSourceLocation(repo, oldRoot, s.opts.StateDir); err != nil {
			return err
		}
		if err := validateManualSourceLocation(repo, root, s.opts.StateDir); err != nil {
			return err
		}
	}
	if err := s.checkCatalogueDirectory(root); err != nil {
		return err
	}
	if err := s.detachLocked(); err != nil {
		return err
	}
	configs, err := s.engine.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, cfg := range configs {
		if err := s.engine.Unmount(ctx, cfg.Name); err != nil {
			return err
		}
	}
	err = s.migrateRoot(ctx, root)
	if err != nil {
		if errors.Is(err, errRootMigrationRecoveryNeeded) {
			blocked = true
			s.mu.Lock()
			s.message = safeError(err)
			s.mu.Unlock()
			return err
		}
		if desired {
			return errors.Join(err, s.mountLocked(s.ctx))
		}
		return err
	}
	if s.hybridCatalogue {
		if err := s.syncHybridCatalogue(oldRoot, nil); err != nil {
			return err
		}
		if err := s.publishHybridCatalogue(); err != nil {
			return err
		}
	}
	if desired {
		return s.mountLocked(ctx)
	}
	return nil
}

func (s *Service) ensureRepository(ctx context.Context, id string) (*fusefs.ArtifactFuse, error) {
	s.mu.Lock()
	if s.closing || s.quitPrepared || s.recoveryRequired {
		s.mu.Unlock()
		return nil, errors.New("repository service is closing or needs recovery")
	}
	repo, ok := s.repositoryLocked(id)
	enabled := ok && s.repositoryEnabledLocked(repo)
	root := s.state.MountRoot
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("repository is not in the catalogue")
	}
	if !enabled {
		return nil, errors.New("enable this repository and its owner group before opening it virtually")
	}
	if repo.LocalPath != "" {
		return nil, errors.New("this repository is an ordinary local checkout")
	}
	configs, err := s.engine.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	name := engineName(repo.ID)
	var config *model.RepoConfig
	for i := range configs {
		if configs[i].Name == name {
			config = &configs[i]
			break
		}
	}
	if config == nil || config.PrepareError != "" || config.PrepareState == model.PrepareStateFailed || config.PrepareState == model.PrepareStateSyncPreparing {
		if repo.Source == "manual" {
			if err := validateManualSourceLocation(repo, root, s.opts.StateDir); err != nil {
				return nil, err
			}
		}
		var requiredCommit string
		if s.hybridCatalogue && s.preview != nil {
			preview, err := s.preview.Acquire(ctx, repo)
			if err != nil {
				return nil, err
			}
			requiredCommit = preview.Commit
		}
		s.setRepositoryState(id, "preparing", "")
		cfg := model.RepoConfig{
			ID: model.RepoID(name), Name: name, RemoteURL: repo.CloneURL,
			Branch: "refs/heads/" + repo.DefaultBranch, Enabled: true,
			MountRoot: root, MountPath: filepath.Join(root, repo.Owner, repo.Name),
			RefreshInterval: 5 * time.Minute, RemoteRefreshDisabled: true,
			RequiredCommit: requiredCommit,
		}
		if repo.Source != "manual" {
			cfg.CredentialHelper = githubCredentialHelper(s.opts.GHPath)
		}
		if repo.DefaultBranch == "" {
			return nil, errors.New("this repository has no default branch yet")
		}
		if err := s.engine.AddRepo(ctx, cfg); err != nil {
			s.setRepositoryState(id, "error", safeError(err))
			return nil, err
		}
		if config == nil && requiredCommit != "" {
			// Verified source acquisition deliberately produces a detached HEAD.
			// A new desktop working tree instead starts on its requested local
			// branch. Do this before exposing it; existing indexes and branches
			// are never reset when reopening prepared repository storage.
			gitDir := filepath.Join(s.opts.StateDir, "engine", "repos", name, "git")
			branch := "refs/heads/" + repo.DefaultBranch
			if _, err := localCheckoutGit(ctx, gitDir, "update-ref", branch, requiredCommit); err != nil {
				return nil, errors.New("could not initialize the new checkout's local branch")
			}
			if _, err := localCheckoutGit(ctx, gitDir, "symbolic-ref", "HEAD", branch); err != nil {
				return nil, errors.New("could not attach the new checkout to its local branch")
			}
		}
	}
	gitDir := filepath.Join(s.opts.StateDir, "engine", "repos", name, "git")
	if repo.Source != "manual" {
		if err := configureRepositoryAuth(ctx, gitDir, s.opts.GHPath); err != nil {
			s.setRepositoryState(id, "error", safeError(err))
			return nil, err
		}
	}
	fs, err := s.engine.OpenCatalogRepository(ctx, name)
	if err != nil {
		s.setRepositoryState(id, "error", safeError(err))
		return nil, err
	}
	s.mu.Lock()
	if index := s.repositoryIndexLocked(id); index >= 0 {
		if s.state.Repositories[index].State != "pinned" {
			s.state.Repositories[index].State = "available"
		}
		s.state.Repositories[index].Error = ""
		_ = s.persistLocked()
	}
	s.mu.Unlock()
	return fs, nil
}

func (s *Service) Action(id, action string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.quitPrepared {
		return Operation{}, errors.New("service is closing")
	}
	if s.maintenance || s.recoveryRequired {
		return Operation{}, errors.New("wait for the folder change to finish")
	}
	repo, ok := s.repositoryLocked(id)
	if !ok {
		return Operation{}, errors.New("repository is not in the catalogue")
	}
	id = repo.ID
	if action == "cancel" {
		if cancel, ok := s.cancels[id]; ok {
			cancel()
			for i := len(s.ops) - 1; i >= 0; i-- {
				if s.ops[i].RepositoryID == id && s.ops[i].Status == "running" {
					return s.ops[i], nil
				}
			}
		}
		return Operation{}, errors.New("repository has no active operation")
	}
	if action != "keep" && action != "prepare" && action != "refresh" && action != "free" {
		return Operation{}, errors.New("unsupported repository action")
	}
	if action != "free" && !s.repositoryEnabledLocked(repo) {
		return Operation{}, errors.New("enable this repository and its owner group before downloading or refreshing it")
	}
	if _, busy := s.cancels[id]; busy {
		return Operation{}, errors.New("repository already has an active operation")
	}
	op := Operation{ID: fmt.Sprintf("%x", time.Now().UnixNano()), RepositoryID: id, Action: action, Status: "running"}
	opCtx, cancel := context.WithCancel(s.ctx)
	s.cancels[id] = cancel
	s.ops = append(s.ops, op)
	if len(s.ops) > 100 {
		// Retain active operations even when there is a long history.
		for i, previous := range s.ops {
			if previous.Status != "running" {
				s.ops = append(s.ops[:i], s.ops[i+1:]...)
				break
			}
		}
	}
	s.workers.Add(1)
	go s.runAction(opCtx, cancel, op)
	return op, nil
}

func (s *Service) runAction(ctx context.Context, cancel context.CancelFunc, op Operation) {
	defer s.workers.Done()
	defer cancel()
	unlock, err := s.lockRepo(ctx, op.RepositoryID)
	if err == nil {
		defer unlock()
		s.mu.Lock()
		repo, found := s.repositoryLocked(op.RepositoryID)
		s.mu.Unlock()
		if found && s.hybridCatalogue && repo.LocalPath != "" {
			err = s.runLocalAction(ctx, op, repo)
		} else if found && s.hybridCatalogue && repo.State == "virtual" && op.Action == "refresh" {
			err = s.refreshPreview(ctx, repo)
		} else if op.Action == "free" {
			err = s.freeRepository(ctx, op.RepositoryID)
		} else {
			_, err = s.ensureRepository(ctx, op.RepositoryID)
			if err == nil && op.Action == "refresh" {
				s.mu.Lock()
				repo, exists := s.repositoryLocked(op.RepositoryID)
				root := s.state.MountRoot
				s.mu.Unlock()
				if exists && repo.Source == "manual" {
					err = validateManualSourceLocation(repo, root, s.opts.StateDir)
				}
				if err == nil {
					err = s.engine.FetchCatalogUpdates(ctx, engineName(op.RepositoryID))
				}
			}
			if err == nil && op.Action == "keep" {
				err = s.download(ctx, op)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancels, op.RepositoryID)
	for i := range s.ops {
		if s.ops[i].ID == op.ID {
			s.ops[i].Status = "complete"
			if err != nil {
				s.ops[i].Status = "failed"
				s.ops[i].Error = safeError(err)
				if errors.Is(err, context.Canceled) {
					s.ops[i].Status = "canceled"
				}
			}
			break
		}
	}
	if i := s.repositoryIndexLocked(op.RepositoryID); i >= 0 && err != nil {
		s.state.Repositories[i].Error = safeError(err)
		// Cancellation leaves recoverable prepared data available.
		if s.state.Repositories[i].State == "preparing" {
			s.state.Repositories[i].State = "available"
		}
	}
	if persistErr := s.persistLocked(); persistErr != nil {
		s.message = "Could not save repository state: " + safeError(persistErr)
	}
}

func (s *Service) download(ctx context.Context, op Operation) error {
	s.setRepositoryState(op.RepositoryID, "preparing", "")
	result, err := s.engine.DownloadCurrentTree(ctx, engineName(op.RepositoryID), func(progress daemon.DownloadProgress) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i := range s.ops {
			if s.ops[i].ID == op.ID {
				s.ops[i].CompletedBlobs, s.ops[i].TotalBlobs = progress.CompletedBlobs, progress.TotalBlobs
				s.ops[i].DownloadedBytes, s.ops[i].TotalBytes = progress.DownloadedBytes, progress.TotalBytes
				s.ops[i].CurrentPath = progress.CurrentPath
				break
			}
		}
	})
	if err != nil {
		return err
	}
	if s.hybridCatalogue {
		return s.materializeRepository(ctx, op, result.DownloadedBytes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.repositoryIndexLocked(op.RepositoryID); i >= 0 {
		s.state.Repositories[i].State = "pinned"
		s.state.Repositories[i].Pinned = true
		s.state.Repositories[i].DownloadedBytes = result.DownloadedBytes
		s.pins[op.RepositoryID] = result.HeadOID
	}
	return s.persistLocked()
}

func (s *Service) freeRepository(ctx context.Context, id string) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	wasMounted := s.mounted != nil
	repo, exists := s.repositoryLocked(id)
	if !exists {
		s.mu.Unlock()
		return errors.New("repository is not in the catalogue")
	}
	root := s.state.MountRoot
	if repo.Source == "manual" {
		s.mu.Unlock()
		if err := validateManualSourceLocation(repo, root, s.opts.StateDir); err != nil {
			return err
		}
		s.mu.Lock()
	}
	// Persist unpin intent before releasing storage. A crash after the engine
	// commits removal must not cause the pin worker to download it all again.
	i := s.repositoryIndexLocked(id)
	s.state.Repositories[i].Pinned = false
	if err := s.persistLocked(); err != nil {
		s.state.Repositories[i].Pinned = repo.Pinned
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	// No filesystem callback can touch this repository while the safety check
	// and storage removal run. If unmount is busy, refuse rather than force it.
	if err := s.detachLocked(); err != nil {
		s.mu.Lock()
		if i := s.repositoryIndexLocked(id); i >= 0 {
			s.state.Repositories[i].Pinned = repo.Pinned
		}
		_ = s.persistLocked()
		s.mu.Unlock()
		return err
	}
	releasePreview, err := s.pausePreviewContent(ctx, id)
	if releasePreview != nil {
		defer releasePreview()
	}
	var configs []model.RepoConfig
	if err == nil {
		configs, err = s.engine.ListRepos(ctx)
	}
	registered := false
	for _, cfg := range configs {
		if cfg.Name == engineName(id) {
			registered = true
			break
		}
	}
	if err == nil && registered {
		err = s.engine.StopCatalogRepositoryStorage(ctx, engineName(id))
	}
	if err == nil {
		err = s.evictPreviewContent(ctx, id, !registered)
	}
	if err == nil && registered {
		err = s.engine.FreeRepositorySpace(ctx, engineName(id))
	}
	if err == nil {
		s.mu.Lock()
		if i := s.repositoryIndexLocked(id); i >= 0 {
			repo := &s.state.Repositories[i]
			repo.State, repo.Pinned, repo.DownloadedBytes, repo.Error = "virtual", false, 0, ""
			delete(s.pins, id)
			delete(s.previewBlobSizes, id)
			delete(s.previewMetaCounts, id)
		}
		persistErr := s.persistLocked()
		s.mu.Unlock()
		if persistErr != nil {
			err = persistErr
		}
	} else {
		s.mu.Lock()
		if i := s.repositoryIndexLocked(id); i >= 0 {
			s.state.Repositories[i].Pinned = repo.Pinned
		}
		err = errors.Join(err, s.persistLocked())
		s.mu.Unlock()
		recoveryCtx, recoveryCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		recoveryErr := s.engine.RecoverStorageTransactions(recoveryCtx)
		recoveryCancel()
		if recoveryErr != nil {
			s.mu.Lock()
			s.maintenance, s.recoveryRequired = true, true
			s.message = "Restart EnoughRepos to recover retained repository data: " + safeError(recoveryErr)
			s.mu.Unlock()
			return errors.Join(err, recoveryErr)
		}
	}
	if releasePreview != nil {
		releasePreview()
	}
	if wasMounted {
		if mountErr := s.mountLocked(s.ctx); mountErr != nil {
			err = errors.Join(err, mountErr)
		}
	}
	return err
}

func (s *Service) pinLoop() {
	defer s.workers.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			var pinned []Repository
			for _, repo := range s.state.Repositories {
				if repo.Pinned && s.repositoryEnabledLocked(repo) {
					pinned = append(pinned, repo)
				}
			}
			s.mu.Unlock()
			for _, repo := range pinned {
				if repo.LocalPath != "" {
					continue
				}
				st, err := s.engine.Status(s.ctx, engineName(repo.ID))
				s.mu.Lock()
				knownHead := s.pins[repo.ID]
				s.mu.Unlock()
				if err == nil && st.CurrentHEADOID != "" && st.CurrentHEADOID == knownHead {
					continue
				}
				_, _ = s.Action(repo.ID, "keep")
			}
		}
	}
}

func (s *Service) lockRepo(ctx context.Context, id string) (func(), error) {
	s.mu.Lock()
	key := strings.ToLower(id)
	lock := s.locks[key]
	if lock == nil {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		s.locks[key] = lock
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lock:
		return func() { lock <- struct{}{} }, nil
	}
}

func (s *Service) setRepositoryState(id, state, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.repositoryIndexLocked(id); i >= 0 {
		s.state.Repositories[i].State, s.state.Repositories[i].Error = state, message
		if err := s.persistLocked(); err != nil {
			s.message = "Could not save repository state: " + safeError(err)
		}
	}
}

func (s *Service) repositoryIndexLocked(id string) int {
	for i := range s.state.Repositories {
		if strings.EqualFold(s.state.Repositories[i].ID, id) {
			return i
		}
	}
	return -1
}

func (s *Service) repositoryLocked(id string) (Repository, bool) {
	if i := s.repositoryIndexLocked(id); i >= 0 {
		return s.state.Repositories[i], true
	}
	return Repository{}, false
}

func (s *Service) entriesLocked() []catalogfs.Entry {
	entries := make([]catalogfs.Entry, 0, len(s.state.Repositories))
	for _, repo := range s.state.Repositories {
		if !s.repositoryEnabledLocked(repo) || s.hybridCatalogue && repo.LocalPath != "" {
			continue
		}
		entries = append(entries, catalogfs.Entry{ID: repo.ID, Owner: repo.Owner, Name: repo.Name})
	}
	return entries
}

func (s *Service) persistLocked() error {
	return writeState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.state)
}

func engineName(id string) string {
	hash := sha256.Sum256([]byte(strings.ToLower(id)))
	return "repo-" + hex.EncodeToString(hash[:])
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return auth.RedactLogString(err.Error())
}

func pathsOverlap(a, b string) bool {
	physicalA, errA := resolveDirectoryPath(a)
	physicalB, errB := resolveDirectoryPath(b)
	if errA != nil || errB != nil {
		// Failure to prove separation must not expose the private engine state.
		return true
	}
	return pathsLexicallyOverlap(physicalA, physicalB)
}

func pathsLexicallyOverlap(a, b string) bool {
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// resolveDirectoryPath resolves existing ancestors even when the chosen mount
// folder has not been created. This is path resolution, not repository-key
// normalization; repository keys always use model.CleanPath.
func resolveDirectoryPath(path string) (string, error) {
	parent := path
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			rel, err := filepath.Rel(parent, path)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, rel), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", err
		}
		parent = next
	}
}

func safeMountDirectory(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(root, 0o755)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("mount folder must be a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("choose an empty folder so EnoughRepos does not hide existing files")
	}
	return nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func configureRepositoryAuth(ctx context.Context, gitDir, ghPath string) error {
	// This configuration belongs only to EnoughRepos's private clone. Git commands
	// launched by an editor can then use the same official Keychain-backed helper.
	return gitstore.ConfigureCredentialHelper(ctx, model.RepoConfig{
		GitDir: gitDir, RemoteURL: "https://github.com/", CredentialHelper: githubCredentialHelper(ghPath),
	})
}

func githubCredentialHelper(ghPath string) string {
	// Editors and terminal Git commands do not inherit the service environment.
	return "!GH_TELEMETRY=false " + shellQuote(ghPath) + " auth git-credential"
}

func configureGitWorktree(ctx context.Context, gitDir, path string) error {
	return gitConfig(ctx, gitDir, "core.worktree", path)
}

func gitConfig(ctx context.Context, gitDir, key, value string) error {
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "config", "--local", key, value)
	cmd.Env = nativeAdoptionEnvironment(os.Environ())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("configure repository Git integration: %w", err)
	}
	return nil
}
