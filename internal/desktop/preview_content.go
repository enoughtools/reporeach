package desktop

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

type previewContentRun struct {
	done   chan struct{}
	err    error
	repoID string
	cancel context.CancelFunc
}

type previewContentSource struct {
	mu       sync.Mutex
	cfg      model.RepoConfig
	verified bool
	repoID   string
}

// OpenContent reads one immutable preview blob without activating a writable
// repository. The caller owns the returned descriptor, positioned at offset 0.
// Opening a filesystem read handle does not need to call this method: defer it
// until the first actual read so Finder's metadata probes stay metadata-only.
func (p *RepositoryPreview) OpenContent(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	contentCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.cache.life, cancel)
	defer func() { stop(); cancel() }()
	ctx = contentCtx
	path = model.CleanPath(path)
	if path == "." || path == ".." || strings.HasPrefix(path, "../") || strings.IndexByte(path, 0) >= 0 {
		return nil, os.ErrNotExist
	}
	// Resolve only this parent, never enumerate the repository tree. For a path
	// from an existing directory listing this is a local snapshot lookup.
	if _, _, err := p.Directory(ctx, model.CleanPath(filepath.Dir(path))); err != nil {
		return nil, err
	}
	p.mu.Lock()
	node, found := p.nodes[path]
	p.mu.Unlock()
	validType := node.Type == "file" && (node.Mode == 0o100644 || node.Mode == 0o100755) || node.Type == "symlink" && node.Mode == 0o120000
	if !found {
		return nil, os.ErrNotExist
	}
	if !validType || node.Path != path || node.RepoID != model.RepoID(p.repo.ID) || !validPreviewOID(node.ObjectOID) || node.SizeBytes < 0 ||
		(node.SizeState != "known" && node.SizeState != "unknown") || node.SizeState == "unknown" && node.SizeBytes != 0 {
		return nil, ErrPreviewUnavailable
	}
	if !validPreviewOID(p.Commit) {
		return nil, ErrPreviewUnavailable
	}
	c := p.cache
	c.mu.Lock()
	if c.closed || c.contentPaused[p.repo.ID] {
		c.mu.Unlock()
		return nil, ErrPreviewInUse
	}
	if c.contentCalls == nil {
		c.contentCalls = make(map[string]int)
		c.contentIdle = make(map[string]chan struct{})
	}
	if c.contentCalls[p.repo.ID] == 0 {
		c.contentIdle[p.repo.ID] = make(chan struct{})
	}
	c.contentCalls[p.repo.ID]++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.contentCalls[p.repo.ID]--
		if c.contentCalls[p.repo.ID] == 0 {
			close(c.contentIdle[p.repo.ID])
		}
		c.mu.Unlock()
	}()
	if err := p.contentDirectories(); err != nil {
		return nil, err
	}
	cachePath := filepath.Join(p.contentBlobDirectory(), node.ObjectOID)
	// A verified durable blob can be read with the network unavailable, even
	// after the selected branch has advanced or this preview has retired.
	file, err := openVerifiedPreviewBlob(ctx, cachePath, node)
	if err == nil {
		return file, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	c.mu.Lock()
	if c.closed || c.contentPaused[p.repo.ID] {
		c.mu.Unlock()
		return nil, ErrPreviewInUse
	}
	// The previous owner may have published between the first cache lookup
	// and acquiring this lock. Recheck existence before creating a new run.
	if _, err := os.Lstat(cachePath); err == nil {
		c.mu.Unlock()
		return openVerifiedPreviewBlob(ctx, cachePath, node)
	} else if !errors.Is(err, os.ErrNotExist) {
		c.mu.Unlock()
		return nil, err
	}
	if c.contentRuns == nil {
		c.contentRuns = make(map[string]*previewContentRun)
		c.contentSources = make(map[string]*previewContentSource)
		c.contentSlots = make(chan struct{}, 4)
	}
	sourceKey := previewKey(p.repo) + "/" + p.Commit
	// Blob bytes are shared with the writable engine and other immutable
	// revisions of this repository. Their object IDs are their identity.
	runKey := engineName(p.repo.ID) + "/" + node.ObjectOID
	run := c.contentRuns[runKey]
	if run == nil {
		workCtx, cancel := context.WithTimeout(c.life, 2*time.Minute)
		run = &previewContentRun{done: make(chan struct{}), repoID: p.repo.ID, cancel: cancel}
		c.contentRuns[runKey] = run
		source := c.contentSources[sourceKey]
		if source == nil {
			source = &previewContentSource{cfg: p.contentConfig(), repoID: p.repo.ID}
			c.contentSources[sourceKey] = source
		}
		c.wg.Add(1)
		go p.acquireContent(workCtx, source, runKey, run, cachePath, node)
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.life.Done():
		return nil, ErrPreviewUnavailable
	case <-run.done:
		if run.err != nil {
			return nil, run.err
		}
		return openVerifiedPreviewBlob(ctx, cachePath, node)
	}
}

func (p *RepositoryPreview) contentDirectories() error {
	p.cache.contentSetupMu.Lock()
	defer p.cache.contentSetupMu.Unlock()
	// Inspect each private ancestor before creating descendants. Do not follow
	// a replacement symlink or adopt another owner's storage.
	for _, path := range []string{p.cache.root, p.root} {
		if err := privateDirectory(path, false); err != nil {
			return err
		}
	}
	engine := filepath.Join(p.cache.stateDir, "engine")
	for _, path := range []string{engine, filepath.Join(engine, "repos"), filepath.Join(engine, "repos", engineName(p.repo.ID)),
		filepath.Join(engine, "cache"), filepath.Join(engine, "cache", "blobs"), p.contentBlobDirectory()} {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			if err := privateDirectory(path, true); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
				return ErrPreviewUnavailable
			}
		}
	}
	if err := ensurePreviewContentOwnership(p.cache.stateDir, p.repo.ID, p.contentGitRoot()); err != nil {
		return err
	}
	return nil
}

func (p *RepositoryPreview) contentSourceDirectories(ctx context.Context) (func(), error) {
	var created []struct {
		path     string
		identity localHandoffIdentity
	}
	cleanup := func() {
		for i := len(created) - 1; i >= 0; i-- {
			_ = handoffRemoveOwnedEmptyDirectory(created[i].path, created[i].identity)
		}
	}
	for _, path := range []string{filepath.Join(p.contentGitRoot(), previewKey(p.repo)), p.contentDirectory()} {
		if err := ctx.Err(); err != nil {
			cleanup()
			return nil, err
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				cleanup()
				return nil, err
			} else if err == nil {
				identity, err := handoffReadIdentity(path)
				if err != nil {
					cleanup()
					return nil, err
				}
				created = append(created, struct {
					path     string
					identity localHandoffIdentity
				}{path, identity})
			}
		} else if err != nil {
			cleanup()
			return nil, err
		}
		if err := privateDirectory(path, false); err != nil {
			cleanup()
			return nil, err
		}
	}
	return cleanup, nil
}

func (p *RepositoryPreview) contentDirectory() string {
	return filepath.Join(p.contentGitRoot(), previewKey(p.repo), p.Commit)
}

func (p *RepositoryPreview) contentGitRoot() string {
	return filepath.Join(p.cache.stateDir, "engine", "repos", engineName(p.repo.ID), "preview-git")
}

func (p *RepositoryPreview) contentBlobDirectory() string {
	return filepath.Join(p.cache.stateDir, "engine", "cache", "blobs", engineName(p.repo.ID))
}

func (p *RepositoryPreview) contentConfig() model.RepoConfig {
	cfg := model.RepoConfig{ID: model.RepoID(p.repo.ID), Name: engineName(p.repo.ID), RemoteURL: p.repo.CloneURL,
		Branch: p.repo.DefaultBranch, HistoryDepth: 1, GitDir: filepath.Join(p.contentDirectory(), "git")}
	if filepath.IsAbs(cfg.RemoteURL) {
		cfg.RemoteURL = (&url.URL{Scheme: "file", Path: cfg.RemoteURL}).String()
	}
	if p.repo.Source != "manual" && p.cache.github != nil {
		cfg.CredentialHelper = githubCredentialHelper(p.cache.github.path)
	}
	return cfg
}

func (p *RepositoryPreview) acquireContent(ctx context.Context, source *previewContentSource, key string, run *previewContentRun, path string, node model.BaseNode) {
	c := p.cache
	defer c.wg.Done()
	defer run.cancel()
	var err error
	select {
	case c.contentSlots <- struct{}{}:
		defer func() { <-c.contentSlots }()
		err = p.fetchContent(ctx, source, path, node)
	case <-ctx.Done():
		err = ctx.Err()
	}
	c.mu.Lock()
	run.err = err
	delete(c.contentRuns, key)
	close(run.done)
	c.mu.Unlock()
}

func (p *RepositoryPreview) fetchContent(ctx context.Context, source *previewContentSource, path string, node model.BaseNode) error {
	source.mu.Lock()
	defer source.mu.Unlock()
	cleanup, err := p.contentSourceDirectories(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := beginPreviewGitGeneration(ctx, p.cache.stateDir, p.repo.ID, source.cfg.GitDir); err != nil {
		return err
	}
	if !source.verified {
		ref := "refs/heads/" + p.repo.DefaultBranch
		cfg := source.cfg
		if info, err := os.Lstat(cfg.GitDir); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 {
				return ErrPreviewUnavailable
			}
			commit, _, resolveErr := p.cache.git.ResolveHEAD(ctx, cfg)
			if resolveErr == nil && commit == p.Commit {
				cfg.AcquiredRef, cfg.AcquiredCommit = ref, p.Commit
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		prepared, err := p.cache.git.PrepareSource(ctx, cfg, model.SourceRequirement{Ref: ref, RequiredCommit: p.Commit, Depth: 1})
		if err != nil {
			return err
		}
		if !prepared.Verified || prepared.Commit != p.Commit || prepared.Ref != ref {
			return ErrPreviewUnavailable
		}
		if err := finishPreviewGitGeneration(ctx, p.cache.stateDir, p.repo.ID, cfg.GitDir); err != nil {
			return err
		}
		cfg.AcquiredRef, cfg.AcquiredCommit = ref, p.Commit
		source.cfg, source.verified = cfg, true
		if err := beginPreviewGitGeneration(ctx, p.cache.stateDir, p.repo.ID, cfg.GitDir); err != nil {
			return err
		}
	}
	cfg := source.cfg
	// Each owner writes a unique staging name. Verify both its exact object hash
	// and known metadata size before publishing the final content-addressed name.
	temporary, err := os.CreateTemp(filepath.Dir(path), ".content-*")
	if err != nil {
		return err
	}
	staging := temporary.Name()
	defer os.Remove(staging)
	if err := temporary.Close(); err != nil {
		return err
	}
	if _, err := p.cache.git.BlobToCache(ctx, cfg, node.ObjectOID, staging); err != nil {
		return err
	}
	if err := finishPreviewGitGeneration(ctx, p.cache.stateDir, p.repo.ID, cfg.GitDir); err != nil {
		return err
	}
	file, err := openVerifiedPreviewBlob(ctx, staging, node)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return publishPreviewContentBlob(ctx, p.cache.stateDir, p.repo.ID, staging, path, node)
}

func publishPreviewContentBlob(ctx context.Context, stateDir, repoID, staging, path string, node model.BaseNode) error {
	identity, err := handoffReadIdentity(staging)
	if err != nil {
		return err
	}
	if err := handoffRenameOwnedObject(staging, path, identity); errors.Is(err, os.ErrExist) {
		// A writable hydrator may have published the same immutable object
		// while the preview read was in flight. Exclusive publication retains
		// that winner; accept only verified bytes and never claim its ownership.
		existing, err := openVerifiedPreviewBlob(ctx, path, node)
		if err != nil {
			return err
		}
		return existing.Close()
	} else if err != nil {
		return err
	}
	return recordPreviewBlobGeneration(ctx, stateDir, repoID, path, node)
}

// PauseContent gates a repository before its private content storage is moved
// or reclaimed. It cancels and joins owned acquisitions, then returns their
// exact Git directories so the service can close its concrete Git-store pools.
// The returned release function is idempotent. Existing callers own any open
// blob descriptors; the caller must also quiesce its mounted namespace.
func (c *PreviewCache) PauseContent(ctx context.Context, repoID string) (release func(), gitDirs []string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	if c.closed || c.contentPaused[repoID] {
		c.mu.Unlock()
		return nil, nil, ErrPreviewInUse
	}
	for _, run := range c.runs {
		if run.repoID == repoID {
			c.mu.Unlock()
			return nil, nil, ErrPreviewInUse
		}
	}
	if c.contentPaused == nil {
		c.contentPaused = make(map[string]bool)
	}
	c.contentPaused[repoID] = true
	var once sync.Once
	release = func() { once.Do(func() { c.mu.Lock(); delete(c.contentPaused, repoID); c.mu.Unlock() }) }
	var runs []*previewContentRun
	for _, run := range c.contentRuns {
		if run.repoID == repoID {
			runs = append(runs, run)
			run.cancel()
		}
	}
	idle := c.contentIdle[repoID]
	c.mu.Unlock()
	for _, run := range runs {
		<-run.done
	}
	if idle != nil {
		<-idle
	}
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		// Keep source tracking on an aborted pause. The caller cannot reclaim
		// storage until a later successful pause returns all pooled handles.
		c.mu.Unlock()
		release()
		return nil, nil, err
	}
	for key, source := range c.contentSources {
		if source.repoID == repoID {
			gitDirs = append(gitDirs, source.cfg.GitDir)
			delete(c.contentSources, key)
		}
	}
	c.mu.Unlock()
	sort.Strings(gitDirs)
	return release, gitDirs, nil
}

func openVerifiedPreviewBlob(ctx context.Context, path string, node model.BaseNode) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) { _ = file.Close(); return nil, err }
	info, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 || stat.Nlink != 1 ||
		node.SizeState == "known" && info.Size() != node.SizeBytes {
		return fail(ErrPreviewUnavailable)
	}
	var digest hash.Hash = sha1.New()
	if len(node.ObjectOID) == 64 {
		digest = sha256.New()
	}
	_, _ = fmt.Fprintf(digest, "blob %d\x00", info.Size())
	if _, err := io.Copy(digest, &previewContentReader{ctx: ctx, reader: file}); err != nil {
		return fail(err)
	}
	if hex.EncodeToString(digest.Sum(nil)) != node.ObjectOID {
		return fail(ErrPreviewUnavailable)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return file, nil
}

type previewContentReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *previewContentReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
