package desktop

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type previewContentGit struct {
	model.GitStore
	prepares        atomic.Int64
	acquisitions    atomic.Int64
	blobs           atomic.Int64
	started         chan struct{}
	release         chan struct{}
	canceled        chan struct{}
	cancelJoined    chan struct{}
	metadataStarted chan struct{}
	metadataRelease chan struct{}
	metadataCalls   atomic.Int64
	prepareStarted  chan struct{}
	prepareRelease  chan struct{}
}

func (g *previewContentGit) CloneBloblessNonInteractive(ctx context.Context, cfg model.RepoConfig) error {
	if g.metadataCalls.Add(1) == 1 && g.metadataStarted != nil {
		close(g.metadataStarted)
	}
	if g.metadataRelease != nil {
		select {
		case <-g.metadataRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.GitStore.CloneBloblessNonInteractive(ctx, cfg)
}

func (g *previewContentGit) PrepareSource(ctx context.Context, cfg model.RepoConfig, requirement model.SourceRequirement) (model.PreparedSource, error) {
	if g.prepares.Add(1) == 1 && g.prepareStarted != nil {
		close(g.prepareStarted)
	}
	if g.prepareRelease != nil {
		select {
		case <-g.prepareRelease:
		case <-ctx.Done():
			return model.PreparedSource{}, ctx.Err()
		}
	}
	prepared, err := g.GitStore.PrepareSource(ctx, cfg, requirement)
	if prepared.Acquired {
		g.acquisitions.Add(1)
	}
	return prepared, err
}

func (g *previewContentGit) BlobToCache(ctx context.Context, cfg model.RepoConfig, oid, path string) (int64, error) {
	if g.blobs.Add(1) == 1 && g.started != nil {
		close(g.started)
	}
	if g.release != nil {
		select {
		case <-g.release:
		case <-ctx.Done():
			if g.canceled != nil {
				close(g.canceled)
			}
			if g.cancelJoined != nil {
				<-g.cancelJoined
			}
			return 0, ctx.Err()
		}
	}
	return g.GitStore.BlobToCache(ctx, cfg, oid, path)
}

func previewContentCache(t *testing.T, state string, store model.GitStore) *PreviewCache {
	t.Helper()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	cache, err := NewPreviewCache(context.Background(), state, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Error(err)
		}
	})
	return cache
}

func previewContentFixture(t *testing.T) (Repository, string, []byte) {
	t.Helper()
	source, binary := adoptionSource(t)
	if err := os.Symlink("binary.dat", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	adoptionGit(t, source, "add", ".")
	adoptionGit(t, source, "commit", "-m", "second revision")
	adoptionGit(t, source, "config", "uploadpack.allowFilter", "true")
	adoptionGit(t, source, "config", "uploadpack.allowAnySHA1InWant", "true")
	return Repository{ID: "local/project", Owner: "local", Name: "project", DefaultBranch: "trunk", CloneURL: source, Source: "manual"}, source, binary
}

func readPreviewContent(t *testing.T, preview *RepositoryPreview, path string) []byte {
	t.Helper()
	file, err := preview.OpenContent(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func previewMissingBlob(t *testing.T, directory, oid string) bool {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", directory, "cat-file", "--batch-check")
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	cmd.Stdin = strings.NewReader(oid + "\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(output), " missing")
}

func TestPreviewContentBinaryShallowOnlyRequestedBlobAndSourceUntouched(t *testing.T) {
	repo, source, binary := previewContentFixture(t)
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("staged user work"), 0o600); err != nil {
		t.Fatal(err)
	}
	adoptionGit(t, source, "add", "tracked.txt")
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("unstaged user work"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := sourceState(t, source)
	state := t.TempDir()
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	cache := previewContentCache(t, state, store)
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	_, entries, err := preview.Directory(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if got := readPreviewContent(t, preview, "binary.dat"); !bytes.Equal(got, binary) {
		t.Fatalf("binary content changed: %v", got)
	}
	gitDir := preview.contentConfig().GitDir
	if got := strings.TrimSpace(string(adoptionGit(t, state, "--git-dir", gitDir, "rev-list", "--count", "HEAD"))); got != "1" {
		t.Fatalf("content acquisition fetched history: %s", got)
	}
	for _, entry := range entries {
		if entry.Type == "dir" {
			continue
		}
		missing := previewMissingBlob(t, gitDir, entry.ObjectOID)
		if missing != (entry.Path != "binary.dat") {
			t.Fatalf("blob availability for %q = missing %v", entry.Path, missing)
		}
	}
	if !reflect.DeepEqual(before, sourceState(t, source)) {
		t.Fatal("content acquisition modified source index, HEAD, config, or local work")
	}
	if got := readPreviewContent(t, preview, "link"); !bytes.Equal(got, []byte("binary.dat")) {
		t.Fatalf("symlink blob changed: %v", got)
	}
	if _, err := os.Lstat(filepath.Join(state, "engine", "config", "repos.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview content activated writable registry")
	}
}

func TestPreviewContentConcurrentReadDeduplicatesAndCanceledWaiterIsIndependent(t *testing.T) {
	repo, _, binary := previewContentFixture(t)
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	counted := &previewContentGit{GitStore: store, started: make(chan struct{}), release: make(chan struct{})}
	cache := previewContentCache(t, t.TempDir(), counted)
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	owner := make(chan error, 1)
	go func() {
		f, err := preview.OpenContent(context.Background(), "binary.dat")
		if f != nil {
			_ = f.Close()
		}
		owner <- err
	}()
	select {
	case <-counted.started:
	case <-time.After(5 * time.Second):
		t.Fatal("content fetch did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := preview.OpenContent(ctx, "binary.dat"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter = %v", err)
	}
	const readers = 12
	var wg sync.WaitGroup
	results := make(chan error, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := preview.OpenContent(context.Background(), "binary.dat")
			if err == nil {
				var content []byte
				content, err = io.ReadAll(f)
				_ = f.Close()
				if err == nil && !bytes.Equal(content, binary) {
					err = errors.New("binary content changed")
				}
			}
			results <- err
		}()
	}
	close(counted.release)
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if counted.prepares.Load() != 1 || counted.blobs.Load() != 1 {
		t.Fatalf("duplicate acquisition: sources=%d blobs=%d", counted.prepares.Load(), counted.blobs.Load())
	}
}

func TestPreviewContentRejectsInvalidPathsAndTamperedCache(t *testing.T) {
	repo, _, binary := previewContentFixture(t)
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	counted := &previewContentGit{GitStore: store}
	cache := previewContentCache(t, t.TempDir(), counted)
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside", "a/../../outside", "bad\x00path", ".", "absent"} {
		if _, err := preview.OpenContent(context.Background(), path); err == nil {
			t.Fatalf("invalid path %q accepted", path)
		}
	}
	if counted.prepares.Load() != 0 || counted.blobs.Load() != 0 {
		t.Fatal("invalid path acquired content")
	}
	if got := readPreviewContent(t, preview, "binary.dat"); !bytes.Equal(got, binary) {
		t.Fatal("unexpected content")
	}
	node := preview.nodes["binary.dat"]
	path := filepath.Join(preview.contentBlobDirectory(), node.ObjectOID)
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, len(binary)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := preview.OpenContent(context.Background(), "binary.dat"); !errors.Is(err, ErrPreviewUnavailable) {
		t.Fatalf("same-size corrupt blob = %v", err)
	}
	if counted.blobs.Load() != 1 {
		t.Fatal("tampered cache was silently overwritten")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(target, binary, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if f, err := preview.OpenContent(context.Background(), "binary.dat"); err == nil {
		f.Close()
		t.Fatal("symlink cache accepted")
	}
}

func TestPreviewContentCloseCancelsAndJoinsOwner(t *testing.T) {
	repo, _, _ := previewContentFixture(t)
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	counted := &previewContentGit{GitStore: store, started: make(chan struct{}), release: make(chan struct{})}
	cache := previewContentCache(t, t.TempDir(), counted)
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := preview.OpenContent(context.Background(), "binary.dat")
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case <-counted.started:
	case <-time.After(5 * time.Second):
		t.Fatal("content fetch did not start")
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pending reader survived close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not join content acquisition")
	}
	if _, err := preview.OpenContent(context.Background(), "binary.dat"); err == nil {
		t.Fatal("closed preview acquired content")
	}
}

func TestPreviewContentCloseJoinsAdmittedDirectorySetup(t *testing.T) {
	repo, _, _ := previewContentFixture(t)
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	cache := previewContentCache(t, t.TempDir(), store)
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	cache.contentSetupMu.Lock()
	locked := true
	defer func() {
		if locked {
			cache.contentSetupMu.Unlock()
		}
	}()
	readDone := make(chan error, 1)
	go func() {
		f, err := preview.OpenContent(context.Background(), "binary.dat")
		if f != nil {
			f.Close()
		}
		readDone <- err
	}()
	wait := func(predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !predicate() {
			if time.Now().After(deadline) {
				t.Fatal("lifecycle transition did not occur")
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait(func() bool { cache.mu.Lock(); defer cache.mu.Unlock(); return cache.contentCalls[repo.ID] == 1 })
	closeDone := make(chan error, 1)
	go func() { closeDone <- cache.Close() }()
	wait(func() bool { cache.mu.Lock(); defer cache.mu.Unlock(); return cache.closed })
	select {
	case <-closeDone:
		t.Fatal("Close returned while an admitted content setup was still running")
	default:
	}
	cache.contentSetupMu.Unlock()
	locked = false
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-readDone; err == nil {
		t.Fatal("shutdown content setup succeeded")
	}
}

func TestPreviewContentPauseCancelsOwnersGatesCachedReadsAndResumes(t *testing.T) {
	repo, _, binary := previewContentFixture(t)
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	counted := &previewContentGit{GitStore: store, started: make(chan struct{}), release: make(chan struct{})}
	cache := previewContentCache(t, t.TempDir(), counted)
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := preview.OpenContent(context.Background(), "binary.dat")
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case <-counted.started:
	case <-time.After(5 * time.Second):
		t.Fatal("content fetch did not start")
	}
	release, gitDirs, err := cache.PauseContent(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := <-done; err == nil {
		t.Fatal("paused owner succeeded")
	}
	if !reflect.DeepEqual(gitDirs, []string{preview.contentConfig().GitDir}) {
		t.Fatalf("paused source directories = %v", gitDirs)
	}
	for _, path := range gitDirs {
		store.CloseRepository(path)
	}
	if _, err := preview.OpenContent(context.Background(), "binary.dat"); !errors.Is(err, ErrPreviewInUse) {
		t.Fatalf("paused read = %v", err)
	}
	if _, _, err := cache.PauseContent(context.Background(), repo.ID); !errors.Is(err, ErrPreviewInUse) {
		t.Fatalf("double pause = %v", err)
	}
	release()
	release()
	close(counted.release)
	if got := readPreviewContent(t, preview, "binary.dat"); !bytes.Equal(got, binary) {
		t.Fatal("resumed blob changed")
	}
	if counted.prepares.Load() != 2 || counted.acquisitions.Load() != 1 {
		t.Fatalf("resume did not revalidate existing source: prepares=%d acquired=%d", counted.prepares.Load(), counted.acquisitions.Load())
	}
	release, _, err = cache.PauseContent(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preview.OpenContent(context.Background(), "binary.dat"); !errors.Is(err, ErrPreviewInUse) {
		t.Fatalf("paused cached read = %v", err)
	}
	release()
}

func TestPreviewContentCanceledPauseRetainsSourceTracking(t *testing.T) {
	repo, _, _ := previewContentFixture(t)
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	counted := &previewContentGit{GitStore: store, started: make(chan struct{}), release: make(chan struct{}),
		canceled: make(chan struct{}), cancelJoined: make(chan struct{})}
	cache := previewContentCache(t, t.TempDir(), counted)
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		f, err := preview.OpenContent(context.Background(), "binary.dat")
		if f != nil {
			f.Close()
		}
		readDone <- err
	}()
	select {
	case <-counted.started:
	case <-time.After(5 * time.Second):
		t.Fatal("content owner did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	pauseDone := make(chan error, 1)
	go func() {
		release, _, err := cache.PauseContent(ctx, repo.ID)
		if release != nil {
			release()
		}
		pauseDone <- err
	}()
	select {
	case <-counted.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not cancel owner")
	}
	cancel()
	close(counted.cancelJoined)
	if err := <-pauseDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pause = %v", err)
	}
	if err := <-readDone; err == nil {
		t.Fatal("canceled read succeeded")
	}
	release, dirs, err := cache.PauseContent(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !reflect.DeepEqual(dirs, []string{preview.contentConfig().GitDir}) {
		t.Fatalf("aborted pause lost pooled-source tracking: %v", dirs)
	}
	for _, dir := range dirs {
		store.CloseRepository(dir)
	}
}

func TestPreviewContentSharedAcrossRetiredPreviewRevisions(t *testing.T) {
	repo, source, binary := previewContentFixture(t)
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	counted := &previewContentGit{GitStore: store, started: make(chan struct{}), release: make(chan struct{})}
	cache := previewContentCache(t, t.TempDir(), counted)
	previous, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	read := func(preview *RepositoryPreview) {
		f, err := preview.OpenContent(context.Background(), "binary.dat")
		if err == nil {
			content, readErr := io.ReadAll(f)
			f.Close()
			err = readErr
			if err == nil && !bytes.Equal(content, binary) {
				err = errors.New("revision changed binary")
			}
		}
		done <- err
	}
	go read(previous)
	select {
	case <-counted.started:
	case <-time.After(5 * time.Second):
		t.Fatal("old revision content owner did not start")
	}
	if err := os.WriteFile(filepath.Join(source, "new-file"), []byte("new revision"), 0o600); err != nil {
		t.Fatal(err)
	}
	adoptionGit(t, source, "add", "new-file")
	adoptionGit(t, source, "commit", "-m", "third revision")
	current, err := cache.Refresh(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if current.Commit == previous.Commit || !previous.retired {
		t.Fatal("fixture did not retire immutable old preview")
	}
	go read(current)
	close(counted.release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if counted.prepares.Load() != 1 || counted.blobs.Load() != 1 {
		t.Fatalf("identical OID across revisions was duplicated: sources=%d blobs=%d", counted.prepares.Load(), counted.blobs.Load())
	}
}

func TestPreviewContentPauseRefusesMetadataOwnerAndGatesNewAcquisition(t *testing.T) {
	repo, _, _ := previewContentFixture(t)
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	counted := &previewContentGit{GitStore: store, metadataStarted: make(chan struct{}), metadataRelease: make(chan struct{})}
	cache := previewContentCache(t, t.TempDir(), counted)
	acquired := make(chan *RepositoryPreview, 1)
	errorsDone := make(chan error, 1)
	go func() { p, err := cache.Acquire(context.Background(), repo); acquired <- p; errorsDone <- err }()
	select {
	case <-counted.metadataStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata acquisition did not start")
	}
	if _, _, err := cache.PauseContent(context.Background(), repo.ID); !errors.Is(err, ErrPreviewInUse) {
		t.Fatalf("pause with active metadata owner = %v", err)
	}
	close(counted.metadataRelease)
	preview := <-acquired
	if err := <-errorsDone; err != nil {
		t.Fatal(err)
	}
	release, _, err := cache.PauseContent(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got, err := cache.Acquire(context.Background(), repo); err != nil || got != preview {
		t.Fatalf("paused cached metadata = %p, %v", got, err)
	}
	if _, _, err := preview.Directory(context.Background(), "."); err != nil {
		t.Fatalf("paused cached listing = %v", err)
	}
	if _, err := cache.Refresh(context.Background(), repo); !errors.Is(err, ErrPreviewInUse) {
		t.Fatalf("paused metadata refresh = %v", err)
	}
	if err := cache.Invalidate(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Acquire(context.Background(), repo); !errors.Is(err, ErrPreviewInUse) {
		t.Fatalf("paused new metadata acquisition = %v", err)
	}
	if counted.metadataCalls.Load() != 1 {
		t.Fatal("paused cache started another metadata owner")
	}
	release()
	if _, err := cache.Acquire(context.Background(), repo); err != nil {
		t.Fatalf("resumed metadata acquisition = %v", err)
	}
}

func TestPreviewContentExclusivePublicationRetainsExistingWinner(t *testing.T) {
	repo, _, binary := previewContentFixture(t)
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	cache := previewContentCache(t, t.TempDir(), store)
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := preview.contentDirectories(); err != nil {
		t.Fatal(err)
	}
	node := preview.nodes["binary.dat"]
	path := filepath.Join(preview.contentBlobDirectory(), node.ObjectOID)
	staging := filepath.Join(preview.contentBlobDirectory(), ".publication-test")
	for _, target := range []string{path, staging} {
		if err := os.WriteFile(target, binary, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	winner, err := handoffReadIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishPreviewContentBlob(context.Background(), cache.stateDir, repo.ID, staging, path, node); err != nil {
		t.Fatal(err)
	}
	current, err := handoffReadIdentity(path)
	if err != nil || current != winner {
		t.Fatal("exclusive publication replaced existing valid winner")
	}
	if _, err := os.Lstat(staging); err != nil {
		t.Fatal("existing winner consumed the unpublished stage")
	}
	if err := os.Remove(staging); err != nil {
		t.Fatal(err)
	}
	if err := verifyPreviewBlobGeneration(context.Background(), cache.stateDir, repo.ID, preview.contentBlobDirectory()); err == nil {
		t.Fatal("existing foreign winner was adopted by producer receipt")
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, len(binary)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, binary, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(staging)
	if err := publishPreviewContentBlob(context.Background(), cache.stateDir, repo.ID, staging, path, node); !errors.Is(err, ErrPreviewUnavailable) {
		t.Fatalf("unverified existing winner = %v", err)
	}
	current, err = handoffReadIdentity(path)
	if err != nil || current != winner {
		t.Fatal("exclusive publication replaced corrupt existing winner")
	}
}

func TestPreviewContentCachedReadCreatesNoSourceParents(t *testing.T) {
	repo, _, binary := previewContentFixture(t)
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	counted := &previewContentGit{GitStore: store}
	cache := previewContentCache(t, t.TempDir(), counted)
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := preview.contentDirectories(); err != nil {
		t.Fatal(err)
	}
	node := preview.nodes["binary.dat"]
	if err := os.WriteFile(filepath.Join(preview.contentBlobDirectory(), node.ObjectOID), binary, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readPreviewContent(t, preview, "binary.dat"); !bytes.Equal(got, binary) {
		t.Fatal("cached binary changed")
	}
	entries, err := os.ReadDir(preview.contentGitRoot())
	if err != nil || len(entries) != 0 {
		t.Fatalf("cached read created unused source parents: %v, %v", entries, err)
	}
	if counted.prepares.Load() != 0 || counted.blobs.Load() != 0 {
		t.Fatal("cached read acquired another source")
	}
}

func TestPreviewContentCanceledPreparationRemovesOnlyOwnedEmptyParents(t *testing.T) {
	repo, _, _ := previewContentFixture(t)
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	counted := &previewContentGit{GitStore: store, prepareStarted: make(chan struct{}), prepareRelease: make(chan struct{})}
	cache := previewContentCache(t, t.TempDir(), counted)
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := preview.OpenContent(context.Background(), "binary.dat")
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case <-counted.prepareStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("source preparation did not start")
	}
	release, _, err := cache.PauseContent(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := <-done; err == nil {
		t.Fatal("canceled source preparation succeeded")
	}
	entries, err := os.ReadDir(preview.contentGitRoot())
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled source preparation retained empty owned parents: %v, %v", entries, err)
	}
}

// A real smart-HTTP source counts requests while streaming binary CGI output.
// Requests and backend children share a bounded context; no external service or
// user credential/configuration is involved.
func previewContentHTTP(t *testing.T, source string) (string, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "source.git")
	adoptionGit(t, root, "clone", "--bare", source, bare)
	adoptionGit(t, root, "--git-dir", bare, "config", "uploadpack.allowFilter", "true")
	adoptionGit(t, root, "--git-dir", bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	requests, online := &atomic.Int64{}, &atomic.Bool{}
	online.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !online.Load() {
			http.Error(w, "offline fixture", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "http-backend")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_PROJECT_ROOT="+root, "GIT_HTTP_EXPORT_ALL=1", "REQUEST_METHOD="+r.Method,
			"QUERY_STRING="+r.URL.RawQuery, "PATH_INFO="+r.URL.Path, "CONTENT_TYPE="+r.Header.Get("Content-Type"),
			"CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10), "HTTP_GIT_PROTOCOL="+r.Header.Get("Git-Protocol"), "GATEWAY_INTERFACE=CGI/1.1")
		cmd.Stdin = http.MaxBytesReader(w, r.Body, 1<<20)
		cmd.WaitDelay = time.Second
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			http.Error(w, "backend pipe", 500)
			return
		}
		if err := cmd.Start(); err != nil {
			stdout.Close()
			http.Error(w, "backend start", 500)
			return
		}
		defer func() { stdout.Close(); _ = cmd.Wait() }()
		reader := bufio.NewReader(stdout)
		headers, err := textproto.NewReader(reader).ReadMIMEHeader()
		if err != nil {
			cancel()
			http.Error(w, "backend headers", 502)
			return
		}
		status := http.StatusOK
		if value := headers.Get("Status"); value != "" {
			status, _ = strconv.Atoi(strings.Fields(value)[0])
			headers.Del("Status")
		}
		for key, values := range headers {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(status)
		_, _ = io.Copy(w, io.LimitReader(reader, 8<<20))
	}))
	t.Cleanup(server.Close)
	return server.URL + "/source.git", requests, online
}

func TestPreviewContentHTTPDurableOfflineAndVerifiedSourceReuse(t *testing.T) {
	repo, source, binary := previewContentFixture(t)
	remote, requests, online := previewContentHTTP(t, source)
	repo.CloneURL = remote
	repo.HTMLURL = strings.TrimSuffix(remote, ".git")
	state := t.TempDir()
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	cache := previewContentCache(t, state, store)
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := readPreviewContent(t, preview, "binary.dat"); !bytes.Equal(got, binary) {
		t.Fatal("HTTP binary changed")
	}
	_, entries, err := preview.Directory(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Type == "dir" {
			continue
		}
		if missing := previewMissingBlob(t, preview.contentConfig().GitDir, entry.ObjectOID); missing != (entry.Path != "binary.dat") {
			t.Fatalf("HTTP content fetched unrelated blob %q: missing=%v", entry.Path, missing)
		}
	}
	if requests.Load() == 0 {
		t.Fatal("fixture did not exercise HTTP transport")
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	counted := &previewContentGit{GitStore: store}
	reopened := previewContentCache(t, state, counted)
	preview, err = reopened.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	before := requests.Load()
	online.Store(false)
	if got := readPreviewContent(t, preview, "binary.dat"); !bytes.Equal(got, binary) {
		t.Fatal("durable offline binary changed")
	}
	if requests.Load() != before {
		t.Fatal("cached blob contacted offline source")
	}
	online.Store(true)
	if got := readPreviewContent(t, preview, "link"); !bytes.Equal(got, []byte("binary.dat")) {
		t.Fatal("new HTTP symlink changed")
	}
	if counted.prepares.Load() != 1 || counted.acquisitions.Load() != 0 {
		t.Fatalf("verified shallow source was recloned: preparations=%d acquisitions=%d", counted.prepares.Load(), counted.acquisitions.Load())
	}
}
