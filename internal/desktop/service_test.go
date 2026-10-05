package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

func newDesktopTestService(t *testing.T, ghScript string) *Service {
	t.Helper()
	directory := t.TempDir()
	gh := fakeGitHub(t, ghScript)
	s, err := New(context.Background(), Options{
		StateDir: filepath.Join(directory, "state"), MountRoot: filepath.Join(directory, "repositories"), GHPath: gh.path,
	})
	if err != nil {
		t.Fatal(err)
	}
	// These shared fixtures exercise FUSE's live catalogue publication. Tests
	// for native FSKit's quiescent catalogue transactions opt in explicitly.
	s.quiescentCatalogue = false
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close desktop service: %v", err)
		}
	})
	return s
}

func seedDesktopCatalogue(t *testing.T, s *Service, repositories ...Repository) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Repositories = append([]Repository(nil), repositories...)
	if err := s.persistLocked(); err != nil {
		t.Fatal(err)
	}
}

func TestServiceNewValidatesPrivateDirectoriesAndPaths(t *testing.T) {
	base := t.TempDir()
	gh := fakeGitHub(t, "exit 4")
	valid := Options{StateDir: filepath.Join(base, "state"), MountRoot: filepath.Join(base, "mount"), GHPath: gh.path}
	tests := map[string]func(*Options){
		"relative state":       func(o *Options) { o.StateDir = "state" },
		"relative mount":       func(o *Options) { o.MountRoot = "mount" },
		"filesystem root":      func(o *Options) { o.MountRoot = "/" },
		"relative GitHub CLI":  func(o *Options) { o.GHPath = "gh" },
		"state below mount":    func(o *Options) { o.StateDir = filepath.Join(o.MountRoot, "state") },
		"mount below state":    func(o *Options) { o.MountRoot = filepath.Join(o.StateDir, "mount") },
		"same state and mount": func(o *Options) { o.MountRoot = o.StateDir },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			opts := valid
			mutate(&opts)
			s, err := New(context.Background(), opts)
			if err == nil {
				_ = s.Close()
				t.Fatal("invalid desktop paths were accepted")
			}
		})
	}
	s := newDesktopTestService(t, "exit 4")
	for path, mode := range map[string]os.FileMode{s.opts.StateDir: 0o700, filepath.Join(s.opts.StateDir, "catalogue.json"): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s permissions = %o, want %o", filepath.Base(path), info.Mode().Perm(), mode)
		}
	}
}

func TestServiceNewRejectsSymlinkStateDirectory(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	gh := fakeGitHub(t, "exit 4")
	s, err := New(context.Background(), Options{StateDir: link, MountRoot: filepath.Join(directory, "mount"), GHPath: gh.path})
	if err == nil {
		_ = s.Close()
		t.Fatal("symlink state directory was accepted")
	}
}

func TestServiceStatusReturnsDetachedSnapshots(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	status := s.Status()
	if status.Version != Version || status.MountRoot != s.opts.MountRoot || status.Mounted || status.Repositories == nil || status.Operations == nil {
		t.Fatalf("unexpected empty status: %+v", status)
	}
	s.mu.Lock()
	s.state.Account = &Account{Login: "octocat"}
	s.state.Repositories = []Repository{catalogueRepository("octocat", "repo")}
	s.ops = []Operation{{ID: "one", RepositoryID: "octocat/repo", Status: "complete"}}
	s.mu.Unlock()
	status = s.Status()
	status.Account.Login = "changed"
	status.Repositories[0].Name = "changed"
	status.Operations[0].Status = "changed"
	current := s.Status()
	if current.Account.Login != "octocat" || current.Repositories[0].Name != "repo" || current.Operations[0].Status != "complete" {
		t.Fatalf("status exposed mutable state: %+v", current)
	}
}

func TestServiceDiscoveryRetainsLocalStateAndDistinctOwners(t *testing.T) {
	responses := filepath.Join(t.TempDir(), "repositories.json")
	t.Setenv("FAKE_DESKTOP_REPOS", responses)
	s := newDesktopTestService(t, `
case "$*" in
  'api --hostname github.com user') printf '%s' '{"login":"octocat"}' ;;
  'api --hostname github.com --paginate user/repos?per_page=100&visibility=all&affiliation=owner,collaborator,organization_member&sort=full_name&direction=asc') cat "$FAKE_DESKTOP_REPOS" ;;
  *) exit 1 ;;
esac
`)
	pinned := catalogueRepository("octocat", "shared")
	pinned.State, pinned.Pinned, pinned.DownloadedBytes = "pinned", true, 4096
	missing := catalogueRepository("private-org", "retained")
	missing.State, missing.Private = "available", true
	seedDesktopCatalogue(t, s, pinned, missing)
	if err := os.WriteFile(responses, []byte(`[{"name":"shared","owner":{"login":"octocat"},"description":"updated","default_branch":"trunk"},{"name":"shared","owner":{"login":"another-org"},"default_branch":"main"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := s.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Account == nil || status.Account.Login != "octocat" || len(status.Repositories) != 3 {
		t.Fatalf("unexpected discovery result: %+v", status)
	}
	byID := map[string]Repository{}
	for _, repo := range status.Repositories {
		byID[repo.ID] = repo
	}
	if repo := byID[pinned.ID]; !repo.Pinned || repo.State != "pinned" || repo.DownloadedBytes != 4096 || repo.Description != "updated" || repo.DefaultBranch != "trunk" {
		t.Fatalf("refresh did not retain local pin state: %+v", repo)
	}
	if repo := byID["another-org/shared"]; repo.State != "virtual" || repo.Pinned {
		t.Fatalf("different owner's repository inherited local state: %+v", repo)
	}
	if repo := byID[missing.ID]; repo.State != "available" || !repo.Private || !strings.Contains(repo.Error, "retained") {
		t.Fatalf("access change discarded local repository: %+v", repo)
	}
	if engineName(pinned.ID) == engineName("another-org/shared") || engineName(pinned.ID) != engineName("OCTOCAT/SHARED") {
		t.Fatal("engine identities do not distinguish owners or normalize case")
	}
	loaded, err := readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
	if err != nil || !reflect.DeepEqual(loaded.Repositories, status.Repositories) {
		t.Fatalf("discovery was not persisted: %v", err)
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("metadata discovery prepared clones: %v, %+v", err, configs)
	}
}

func TestServiceDiscoveryAuthFailureDoesNotMutateCatalogue(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo := catalogueRepository("private-org", "repo")
	repo.Pinned, repo.Private = true, true
	seedDesktopCatalogue(t, s, repo)
	before := s.Status()
	path := filepath.Join(s.opts.StateDir, "catalogue.json")
	beforeDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Discover(context.Background()); !errors.Is(err, errGitHubSignIn) {
		t.Fatalf("error = %v, want sign-in required", err)
	}
	if after := s.Status(); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed discovery changed local status: %+v", after)
	}
	afterDisk, err := os.ReadFile(path)
	if err != nil || string(beforeDisk) != string(afterDisk) {
		t.Fatal("failed discovery rewrote the saved catalogue")
	}
}

func TestServiceDiscoveryRollsBackOnPersistenceFailure(t *testing.T) {
	s := newDesktopTestService(t, `
case "$*" in
  'api --hostname github.com user') printf '%s' '{"login":"octocat"}' ;;
  *) printf '%s' '[{"name":"new","owner":{"login":"octocat"},"default_branch":"main"}]' ;;
esac
`)
	seedDesktopCatalogue(t, s, catalogueRepository("octocat", "old"))
	before := s.Status()
	path := filepath.Join(s.opts.StateDir, "catalogue.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Discover(context.Background()); err == nil {
		t.Fatal("discovery did not report failed persistence")
	}
	if after := s.Status(); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed persistence changed catalogue: %+v", after)
	}
}

func TestServiceActionValidationAndCancellation(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo := catalogueRepository("octocat", "repo")
	repo.State = "available"
	seedDesktopCatalogue(t, s, repo)
	for _, request := range [][2]string{{"missing/repo", "keep"}, {repo.ID, "unsupported"}, {repo.ID, "cancel"}} {
		if _, err := s.Action(request[0], request[1]); err == nil {
			t.Fatalf("invalid action %q for %q was accepted", request[1], request[0])
		}
	}
	// Hold the repository lock so cancellation is exercised before any clone or
	// network work can begin. The worker must leave the existing data available.
	unlock, err := s.lockRepo(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	op, err := s.Action(repo.ID, "keep")
	if err != nil || op.Status != "running" {
		t.Fatalf("operation = %+v, error = %v", op, err)
	}
	if _, err := s.Action(repo.ID, "refresh"); err == nil {
		t.Fatal("concurrent action for the same repository was accepted")
	}
	if err := s.Settings(context.Background(), filepath.Join(t.TempDir(), "different")); err == nil {
		t.Fatal("mount path changed during a repository operation")
	}
	canceled, err := s.Action(repo.ID, "cancel")
	if err != nil || canceled.ID != op.ID {
		t.Fatalf("cancel operation = %+v, error = %v", canceled, err)
	}
	awaitDesktop(t, func() bool {
		status := s.Status()
		return len(status.Operations) == 1 && status.Operations[0].Status == "canceled"
	})
	status := s.Status()
	if status.Repositories[0].Pinned || status.Repositories[0].State != "available" {
		t.Fatalf("canceled keep operation claimed a completed download: %+v", status.Repositories[0])
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("canceled action prepared a clone: %v, %+v", err, configs)
	}
}

func TestServiceCloseCancelsOperationsWithoutDeadlock(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo := catalogueRepository("octocat", "repo")
	seedDesktopCatalogue(t, s, repo)
	unlock, err := s.lockRepo(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := s.Action(repo.ID, "prepare"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close waited on a repository lock instead of canceling the worker")
	}
	if _, err := s.Action(repo.ID, "keep"); err == nil {
		t.Fatal("closed service accepted another action")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
}

type desktopFakeMount struct {
	done         chan struct{}
	once         sync.Once
	unmountErr   error
	mu           sync.Mutex
	unmountCalls int
}

func newDesktopFakeMount() *desktopFakeMount { return &desktopFakeMount{done: make(chan struct{})} }

func (m *desktopFakeMount) Join(ctx context.Context) error {
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *desktopFakeMount) Unmount() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unmountCalls++
	if m.unmountErr != nil {
		return m.unmountErr
	}
	m.once.Do(func() { close(m.done) })
	return nil
}

func TestServiceMountSettingsAndUnmountPersistIntent(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	seedDesktopCatalogue(t, s, catalogueRepository("octocat", "repo"))
	s.dependencyReady = func() bool { return true }
	var roots []string
	var mounts []*desktopFakeMount
	s.mountCatalogue = func(_ context.Context, root string, _ *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		roots = append(roots, root)
		mount := newDesktopFakeMount()
		mounts = append(mounts, mount)
		return mount, nil
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.Status().Mounted || len(mounts) != 1 {
		t.Fatal("repeat Mount started another filesystem")
	}
	newRoot := filepath.Join(t.TempDir(), "Chosen folder")
	if err := s.Settings(context.Background(), newRoot); err != nil {
		t.Fatal(err)
	}
	if status := s.Status(); !status.Mounted || status.MountRoot != newRoot || len(mounts) != 2 || roots[1] != newRoot {
		t.Fatalf("settings did not remount at chosen path: %+v", status)
	}
	loaded, err := readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
	if err != nil || loaded.MountRoot != newRoot || !loaded.MountDesired {
		t.Fatalf("mount intent not persisted: %+v, %v", loaded, err)
	}
	if err := s.Unmount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Status().Mounted {
		t.Fatal("Unmount retained a mounted status")
	}
	loaded, err = readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
	if err != nil || loaded.MountDesired {
		t.Fatalf("unmount intent not persisted: %+v, %v", loaded, err)
	}
}

func TestServiceMountRefusesMissingDependencyAndOccupiedFolders(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return false }
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		t.Fatal("mount attempted despite failed prerequisites")
		return nil, nil
	}
	if err := s.Mount(context.Background()); err == nil || err.Error() != platformDependencyMessage() {
		t.Fatalf("error = %v, want platform filesystem guidance", err)
	}
	s.dependencyReady = func() bool { return true }
	if err := os.MkdirAll(s.opts.MountRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.opts.MountRoot, "existing.txt")
	if err := os.WriteFile(file, []byte("retain me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Mount(context.Background()); err == nil {
		t.Fatal("mount covered an existing user's file")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "retain me" {
		t.Fatal("failed mount altered the user's file")
	}
	if s.Status().Mounted {
		t.Fatal("failed mount reported a mounted status")
	}
}

func TestServiceMountUnexpectedDisconnectCanBeRecovered(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return true }
	var mounts []*desktopFakeMount
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		mount := newDesktopFakeMount()
		mounts = append(mounts, mount)
		return mount, nil
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mounts[0].Unmount(); err != nil {
		t.Fatal(err)
	}
	awaitDesktop(t, func() bool { return !s.Status().Mounted })
	if s.Status().Message == "" {
		t.Fatal("unexpected unmount did not report recovery guidance")
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := s.Status(); !status.Mounted || status.Message != "" || len(mounts) != 2 {
		t.Fatalf("remount did not recover: %+v", status)
	}
}

func TestServiceSettingsRejectsUnsafeMountFolders(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	before := s.Status().MountRoot
	parent := t.TempDir()
	occupied := filepath.Join(parent, "occupied")
	if err := os.Mkdir(occupied, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"relative", "/", s.opts.StateDir, filepath.Join(s.opts.StateDir, "nested"), filepath.Dir(s.opts.StateDir), occupied, link} {
		if err := s.Settings(context.Background(), root); err == nil {
			t.Fatalf("unsafe settings folder %q was accepted", root)
		}
		if s.Status().MountRoot != before {
			t.Fatal("failed settings changed the current mount folder")
		}
	}
}

func TestServiceRestartPreservesPinIntentWithoutClaimingOffline(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo := catalogueRepository("octocat", "repo")
	repo.State, repo.Pinned, repo.DownloadedBytes = "pinned", true, 2048
	seedDesktopCatalogue(t, s, repo)
	opts := s.opts
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	status := restarted.Status()
	if len(status.Repositories) != 1 || !status.Repositories[0].Pinned || status.Repositories[0].State == "pinned" {
		t.Fatalf("restart lost pin intent or claimed an unverified offline copy: %+v", status.Repositories)
	}
}

func awaitDesktop(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for desktop service")
}
