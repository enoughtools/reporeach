package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

var ErrPreviewUnavailable = errors.New("repository browsing metadata is unavailable")
var ErrPreviewInUse = errors.New("repository browsing metadata is in use")

// PreviewCache owns metadata-only acquisitions. Its Git directories and
// snapshots are separate from writable repositories, indexes and overlays.
// A preview remains bound to one commit for its entire lifetime.
type PreviewCache struct {
	root    string
	git     model.GitStore
	github  *GitHub
	life    context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool
	items   map[string]*RepositoryPreview
	retired []*RepositoryPreview
	runs    map[string]*previewRun
	wg      sync.WaitGroup
}

type previewRun struct {
	done    chan struct{}
	preview *RepositoryPreview
	err     error
}

// RepositoryPreview can answer directory names, types and available sizes
// without opening a writable ArtifactFS runtime or reading blob contents.
type RepositoryPreview struct {
	Commit string
	Ref    string

	cache      *PreviewCache
	repo       Repository
	root       string
	mu         sync.Mutex
	store      model.SnapshotStore
	closeStore func() error
	generation int64
	complete   bool
	trees      map[string]string // authoritative directories already published
	nodes      map[string]model.BaseNode
	retired    bool
}

func NewPreviewCache(ctx context.Context, stateDir string, git model.GitStore, github *GitHub) (*PreviewCache, error) {
	if git == nil || !filepath.IsAbs(stateDir) {
		return nil, errors.New("preview cache requires a Git store and absolute private storage folder")
	}
	root := filepath.Join(stateDir, "previews")
	if err := privateDirectory(root, true); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	return &PreviewCache{root: root, git: git, github: github, life: life, cancel: cancel,
		items: make(map[string]*RepositoryPreview), runs: make(map[string]*previewRun)}, nil
}

func previewKey(repo Repository) string {
	value := repo.ID + "\x00" + repo.CloneURL + "\x00" + repo.DefaultBranch + "\x00" + repo.Source
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Acquire reuses a durable immutable preview. Concurrent callers share one
// acquisition, but canceling a waiter does not cancel another caller's work.
func (c *PreviewCache) Acquire(ctx context.Context, repo Repository) (*RepositoryPreview, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRepository(repo); err != nil {
		return nil, err
	}
	key := previewKey(repo)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrPreviewUnavailable
	}
	if preview := c.items[key]; preview != nil {
		c.mu.Unlock()
		return preview, nil
	}
	run := c.runs[key]
	if run == nil {
		run = &previewRun{done: make(chan struct{})}
		c.runs[key] = run
		c.wg.Add(1)
		go c.acquire(repo, key, run)
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.life.Done():
		return nil, ErrPreviewUnavailable
	case <-run.done:
		return run.preview, run.err
	}
}

func (c *PreviewCache) acquire(repo Repository, key string, run *previewRun) {
	defer c.wg.Done()
	ctx, cancel := context.WithTimeout(c.life, 2*time.Minute)
	defer cancel()
	preview, err := c.acquireMetadata(ctx, repo, key, true)
	c.mu.Lock()
	if c.closed && preview != nil {
		err = errors.Join(ErrPreviewUnavailable, preview.closeStore())
		preview = nil
	}
	if err == nil {
		c.items[key] = preview
	}
	run.preview, run.err = preview, err
	delete(c.runs, key)
	close(run.done)
	c.mu.Unlock()
}

func (c *PreviewCache) acquireMetadata(ctx context.Context, repo Repository, key string, reuseReceipt bool) (*RepositoryPreview, error) {
	root := filepath.Join(c.root, key)
	err := privateDirectory(root, true)
	var preview *RepositoryPreview
	if err == nil && reuseReceipt {
		preview, err = c.load(repo, root)
	}
	if err == nil && preview == nil {
		if repo.Source != "manual" && c.github != nil {
			var roots map[string]githubPreviewRoot
			roots, err = c.github.previewRoots(ctx, []Repository{repo})
			if err == nil {
				preview, err = c.installGitHubRoot(ctx, repo, root, roots[repo.ID])
			}
		} else {
			preview, err = c.acquireNative(ctx, repo, root)
		}
	}
	return preview, err
}

// Refresh replaces a preview transactionally after namespace quiescence.
// Cached offline metadata remains durable throughout network acquisition.
// Cancellation stops and joins its owner before the old namespace can resume.
func (c *PreviewCache) Refresh(ctx context.Context, repo Repository) (*RepositoryPreview, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRepository(repo); err != nil {
		return nil, err
	}
	key := previewKey(repo)
	c.mu.Lock()
	previous := c.items[key]
	c.mu.Unlock()
	if previous == nil {
		if _, err := os.Lstat(filepath.Join(c.root, key, "metadata.json")); errors.Is(err, os.ErrNotExist) {
			return c.Acquire(ctx, repo) // No cached baseline exists to replace.
		} else if err != nil {
			return nil, err
		}
		var err error
		previous, err = c.Acquire(ctx, repo)
		if err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrPreviewUnavailable
	}
	if c.runs[key] != nil || c.items[key] != previous || !previous.mu.TryLock() {
		c.mu.Unlock()
		return nil, ErrPreviewInUse
	}
	previous.mu.Unlock()
	run := &previewRun{done: make(chan struct{})}
	c.runs[key] = run
	c.wg.Add(1)
	workCtx, cancel := context.WithTimeout(c.life, 2*time.Minute)
	stop := context.AfterFunc(ctx, cancel)
	go c.refresh(workCtx, cancel, stop, repo, key, previous, run)
	c.mu.Unlock()
	// The acquisition is bounded and cancellation closes its subprocess
	// descriptors. Joining here prevents a late publication after remount.
	<-run.done
	return run.preview, run.err
}

func (c *PreviewCache) refresh(ctx context.Context, cancel context.CancelFunc, stop func() bool, repo Repository, key string, previous *RepositoryPreview, run *previewRun) {
	defer c.wg.Done()
	defer func() { stop(); cancel() }()
	preview, err := c.acquireMetadata(ctx, repo, key, false)
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	c.mu.Lock()
	previous.mu.Lock()
	if c.closed {
		err = errors.Join(err, ErrPreviewUnavailable)
	}
	if err == nil {
		// An old object's lazy subtree may have finished while acquisition ran.
		// Its receipt cannot win over the validated replacement at publication.
		err = preview.save()
	}
	if err != nil {
		err = errors.Join(err, previous.save())
		if preview != nil {
			err = errors.Join(err, preview.closeStore())
		}
		preview = nil
	} else {
		previous.retired = true
		c.retired = append(c.retired, previous)
		c.items[key] = preview
	}
	run.preview, run.err = preview, err
	delete(c.runs, key)
	close(run.done)
	previous.mu.Unlock()
	c.mu.Unlock()
}

func (c *PreviewCache) acquireNative(ctx context.Context, repo Repository, root string) (*RepositoryPreview, error) {
	cfg := model.RepoConfig{ID: model.RepoID(repo.ID), Name: engineName(repo.ID),
		RemoteURL: repo.CloneURL, Branch: repo.DefaultBranch, HistoryDepth: 1,
		GitDir: filepath.Join(root, "git")}
	if filepath.IsAbs(repo.CloneURL) {
		// Git's path clone optimization ignores --depth and --filter. Explicit
		// file transport preserves shallow acquisition and avoids hardlinks to
		// the user's repository, while retaining native Git authentication.
		cfg.RemoteURL = (&url.URL{Scheme: "file", Path: repo.CloneURL}).String()
	}
	if repo.Source != "manual" && c.github != nil {
		cfg.CredentialHelper = githubCredentialHelper(c.github.path)
	}
	if info, err := os.Lstat(cfg.GitDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
			return nil, ErrPreviewUnavailable
		}
		// Renewal uses a fresh shallow acquisition. A normal fetch into an
		// existing shallow repository can accumulate intervening history.
		// Earlier previews consume only their snapshots, never this Git index.
		acquisition, err := os.MkdirTemp(root, ".acquire-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(acquisition)
		cfg.GitDir = filepath.Join(acquisition, "git")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := c.git.CloneBloblessNonInteractive(ctx, cfg); err != nil {
		return nil, err
	}
	commit, ref, err := c.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if !validPreviewOID(commit) {
		return nil, ErrPreviewUnavailable
	}
	nodes, err := c.git.BuildTreeIndex(ctx, cfg, commit)
	if err != nil {
		return nil, err
	}
	preview, err := c.newPreview(ctx, repo, root, commit, ref)
	if err != nil {
		return nil, err
	}
	preview.complete = true
	for _, node := range nodes {
		if node.Type == "dir" {
			node.SizeState, node.SizeBytes = "known", 0
		}
		preview.nodes[node.Path] = node
	}
	if err := preview.publish(ctx); err != nil {
		_ = preview.closeStore()
		return nil, err
	}
	return preview, nil
}

func (c *PreviewCache) newPreview(ctx context.Context, repo Repository, root, commit, ref string) (*RepositoryPreview, error) {
	if err := safePreviewFile(filepath.Join(root, "snapshot.db"), true); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := safePreviewFile(filepath.Join(root, "snapshot.db")+suffix, false); err != nil {
			return nil, err
		}
	}
	store, err := snapshot.New(ctx, filepath.Join(root, "snapshot.db"))
	if err != nil {
		return nil, err
	}
	preview := &RepositoryPreview{Commit: commit, Ref: ref, cache: c, repo: repo, root: root,
		store: store, closeStore: store.Close, nodes: make(map[string]model.BaseNode), trees: make(map[string]string)}
	preview.nodes["."] = model.BaseNode{RepoID: model.RepoID(repo.ID), Path: ".", Type: "dir", Mode: 0o040755, SizeState: "known"}
	return preview, nil
}

// Directory returns a complete immediate child list for one canonical path.
// A failed or partial acquisition never yields an authoritative empty list.
func (p *RepositoryPreview) Directory(ctx context.Context, path string) (string, []model.BaseNode, error) {
	directoryCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.cache.life, cancel)
	defer func() { stop(); cancel() }()
	ctx = directoryCtx
	path = model.CleanPath(path)
	if path == ".." || strings.HasPrefix(path, "../") {
		return "", nil, os.ErrNotExist
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if p.cache.life.Err() != nil {
		return "", nil, ErrPreviewUnavailable
	}
	if !p.complete {
		if err := p.ensureDirectory(ctx, path); err != nil {
			return "", nil, err
		}
	}
	node, found, err := p.store.LookupNode(ctx, p.generation, path)
	if err != nil {
		return "", nil, err
	}
	if !found {
		return "", nil, os.ErrNotExist
	}
	if node.Type != "dir" {
		return "", nil, fmt.Errorf("preview path is not a directory: %w", os.ErrInvalid)
	}
	entries, err := p.store.ListChildren(p.generation, path)
	return p.Commit, entries, err
}

func (p *RepositoryPreview) publish(ctx context.Context) error {
	nodes := make([]model.BaseNode, 0, len(p.nodes))
	for _, node := range p.nodes {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Path < nodes[j].Path })
	generation, err := p.store.PublishGeneration(ctx, p.Commit, p.Ref, nodes)
	if err != nil {
		return err
	}
	p.generation = generation
	return p.save()
}

// Close cancels owned acquisitions before closing their snapshot handles.
func (c *PreviewCache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.cancel()
	c.mu.Unlock()
	c.wg.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	var result error
	for _, preview := range c.items {
		preview.mu.Lock()
		result = errors.Join(result, preview.closeStore())
		preview.mu.Unlock()
	}
	for _, preview := range c.retired {
		preview.mu.Lock()
		result = errors.Join(result, preview.closeStore())
		preview.mu.Unlock()
	}
	c.items = nil
	c.retired = nil
	return result
}

// Invalidate requests a fresh baseline after the caller has quiesced the
// mounted namespace. Pending acquisitions or Directory calls are refused.
// Previously returned objects keep their immutable commit and snapshot alive;
// they cannot overwrite the next preview's durable receipt.
func (c *PreviewCache) Invalidate(ctx context.Context, repo Repository) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateRepository(repo); err != nil {
		return err
	}
	key := previewKey(repo)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrPreviewUnavailable
	}
	if c.runs[key] != nil {
		return ErrPreviewInUse
	}
	preview := c.items[key]
	if preview != nil {
		if !preview.mu.TryLock() {
			return ErrPreviewInUse
		}
		defer preview.mu.Unlock()
	}
	path := filepath.Join(c.root, key, "metadata.json")
	if err := safePreviewFile(path, false); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if preview != nil {
		preview.retired = true
		c.retired = append(c.retired, preview)
		delete(c.items, key)
	}
	return nil
}

func validPreviewOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, r := range oid {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
