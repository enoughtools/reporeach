package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/jacobsa/fuse/fuseops"
)

func visibilityEntries(s *Service) []catalogfs.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entriesLocked()
}

func visibilityRepository(t *testing.T, s *Service, id string) Repository {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	repo, ok := s.repositoryLocked(id)
	if !ok {
		t.Fatalf("repository %q disappeared from saved state", id)
	}
	return repo
}

func visibilityMount(t *testing.T, s *Service) *catalogfs.FileSystem {
	t.Helper()
	s.dependencyReady = func() bool { return true }
	var fs *catalogfs.FileSystem
	s.mountCatalogue = func(_ context.Context, _ string, catalogue *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		fs = catalogue
		return newDesktopFakeMount(), nil
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	return fs
}

func visibilityLookup(t *testing.T, fs *catalogfs.FileSystem, parent fuseops.InodeID, name string) fuseops.InodeID {
	t.Helper()
	op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
	if err := fs.LookUpInode(context.Background(), op); err != nil {
		t.Fatalf("lookup %q: %v", name, err)
	}
	return op.Entry.Child
}

func TestVisibilityRepositoryAndOwnerPoliciesAreIndependent(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	a, b, other := catalogueRepository("Team", "a"), catalogueRepository("Team", "b"), catalogueRepository("other", "a")
	a.Source, a.State, a.Pinned, a.DownloadedBytes, a.Error = "github", "pinned", true, 8192, "retained message"
	seedDesktopCatalogue(t, s, a, b, other)
	dataPath := filepath.Join(s.opts.StateDir, "retained-edit")
	data := []byte{'e', 'd', 'i', 't', 0, 0xff}
	if err := os.WriteFile(dataPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.pins[a.ID] = "verified-head"
	s.mu.Unlock()
	if err := s.SetRepositoryEnabled(context.Background(), "TEAM/A", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOrganizationEnabled(context.Background(), "tEaM", false); err != nil {
		t.Fatal(err)
	}
	if got := visibilityEntries(s); !reflect.DeepEqual(got, []catalogfs.Entry{{ID: other.ID, Owner: other.Owner, Name: other.Name}}) {
		t.Fatalf("hidden owner entries remained visible: %+v", got)
	}
	if err := s.SetOrganizationEnabled(context.Background(), "TEAM", true); err != nil {
		t.Fatal(err)
	}
	want := []catalogfs.Entry{{ID: b.ID, Owner: b.Owner, Name: b.Name}, {ID: other.ID, Owner: other.Owner, Name: other.Name}}
	if got := visibilityEntries(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("enabling owner reset individual policy: %+v", got)
	}
	hidden := visibilityRepository(t, s, a.ID)
	wantRepo := a
	wantRepo.Disabled = true
	if hidden != wantRepo {
		t.Fatalf("hiding changed repository data or pin intent: %+v", hidden)
	}
	if err := s.SetRepositoryEnabled(context.Background(), a.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := visibilityRepository(t, s, a.ID); got != a {
		t.Fatalf("re-enabling lost repository state: %+v", got)
	}
	if got, err := os.ReadFile(dataPath); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("visibility changed retained binary edit: %v", err)
	}
	s.mu.Lock()
	head := s.pins[a.ID]
	s.mu.Unlock()
	if head != "verified-head" {
		t.Fatal("visibility discarded the verified pin head")
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("visibility acquired repository storage: %+v, %v", configs, err)
	}
}

func TestVisibilityMountedCataloguePublishesFreshLookups(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	a, b := catalogueRepository("Team", "a"), catalogueRepository("Team", "b")
	seedDesktopCatalogue(t, s, a, b)
	fs := visibilityMount(t, s)
	owner := visibilityLookup(t, fs, fuseops.RootInodeID, "Team")
	oldRepo := visibilityLookup(t, fs, owner, "a")
	if err := s.SetRepositoryEnabled(context.Background(), a.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{Parent: owner, Name: "a"}); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("new lookup of hidden repository = %v, want ENOENT", err)
	}
	if err := fs.GetInodeAttributes(context.Background(), &fuseops.GetInodeAttributesOp{Inode: oldRepo}); !errors.Is(err, syscall.ESTALE) {
		t.Fatalf("retired placeholder attributes = %v, want ESTALE", err)
	}
	visibilityLookup(t, fs, owner, "b")
	if err := s.SetOrganizationEnabled(context.Background(), "team", false); err != nil {
		t.Fatal(err)
	}
	if err := fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "Team"}); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("new lookup of hidden owner = %v, want ENOENT", err)
	}
	if err := s.SetOrganizationEnabled(context.Background(), "TEAM", true); err != nil {
		t.Fatal(err)
	}
	newOwner := visibilityLookup(t, fs, fuseops.RootInodeID, "Team")
	if newOwner == owner {
		t.Fatal("retired owner inode was reused")
	}
	visibilityLookup(t, fs, newOwner, "b")
	if err := s.SetRepositoryEnabled(context.Background(), a.ID, true); err != nil {
		t.Fatal(err)
	}
	if newRepo := visibilityLookup(t, fs, newOwner, "a"); newRepo == oldRepo {
		t.Fatal("retired repository inode was reused")
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("placeholder visibility activated Git storage: %+v, %v", configs, err)
	}
}

func TestVisibilityPoliciesPersistAcrossRestartAndAbsentOwners(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo := catalogueRepository("Team", "repo")
	repo.Pinned = true
	seedDesktopCatalogue(t, s, repo)
	if err := s.SetRepositoryEnabled(context.Background(), repo.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOrganizationEnabled(context.Background(), "Absent-Group", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOrganizationEnabled(context.Background(), "team", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(context.Background(), s.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	wantGroups := []Organization{{Name: "Absent-Group", Enabled: false}, {Name: "Team", Enabled: false}}
	if status := restarted.Status(); !reflect.DeepEqual(status.Organizations, wantGroups) || !status.Repositories[0].Disabled || !status.Repositories[0].Pinned {
		t.Fatalf("restart lost independent visibility or pin intent: %+v", status)
	}
	// The absent owner's saved switch applies if its repositories return later.
	returning := catalogueRepository("ABSENT-GROUP", "returned")
	restarted.mu.Lock()
	restarted.state.Repositories = append(restarted.state.Repositories, returning)
	restarted.mu.Unlock()
	if entries := visibilityEntries(restarted); len(entries) != 0 {
		t.Fatalf("a returning disabled owner became visible: %+v", entries)
	}
	if err := restarted.SetOrganizationEnabled(context.Background(), "absent-group", true); err != nil {
		t.Fatal(err)
	}
	if entries := visibilityEntries(restarted); len(entries) != 1 || entries[0].ID != returning.ID {
		t.Fatalf("absent owner's saved switch could not be recovered: %+v", entries)
	}
	// Status snapshots must not let UI callers alter saved switches.
	status := restarted.Status()
	status.Organizations[0].Enabled = false
	if reflect.DeepEqual(status.Organizations, restarted.Status().Organizations) {
		t.Fatal("status exposed mutable visibility state")
	}
}

func TestVisibilityRejectsBusyRepositoryWithoutLockOrderDeadlock(t *testing.T) {
	for _, busy := range []string{"lazy opening", "explicit action"} {
		t.Run(busy, func(t *testing.T) {
			s := newDesktopTestService(t, "exit 4")
			repo := catalogueRepository("Team", "repo")
			seedDesktopCatalogue(t, s, repo)
			unlock, err := s.lockRepo(context.Background(), "TEAM/REPO")
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			if busy == "explicit action" {
				if _, err := s.Action(repo.ID, "prepare"); err != nil {
					t.Fatal(err)
				}
				defer func() { _, _ = s.Action(repo.ID, "cancel") }()
			}
			for _, change := range []func() error{
				func() error { return s.SetRepositoryEnabled(context.Background(), "team/repo", false) },
				func() error { return s.SetOrganizationEnabled(context.Background(), "team", false) },
			} {
				done := make(chan error, 1)
				go func() { done <- change() }()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("visibility changed while the affected repository was busy")
					}
				case <-time.After(time.Second):
					t.Fatal("visibility waited for a repository lock while holding lifecycle")
				}
				if got := visibilityRepository(t, s, repo.ID); got.Disabled {
					t.Fatal("failed visibility update changed saved policy")
				}
				if entries := visibilityEntries(s); len(entries) != 1 {
					t.Fatal("failed visibility update hid the owner")
				}
				// Storage removal takes lifecycle while owning this repository's
				// lock. That order must remain possible after a rejected toggle.
				lifecycleDone := make(chan struct{})
				go func() {
					s.lifecycle.Lock()
					s.lifecycle.Unlock()
					close(lifecycleDone)
				}()
				select {
				case <-lifecycleDone:
				case <-time.After(time.Second):
					t.Fatal("rejected visibility update retained lifecycle")
				}
			}
		})
	}
}

func TestVisibilityDisabledRepositoriesRefuseAcquisitionButAllowFreeCancel(t *testing.T) {
	for _, scope := range []string{"repository", "owner"} {
		t.Run(scope, func(t *testing.T) {
			s := newDesktopTestService(t, "exit 4")
			repo := catalogueRepository("Team", "repo")
			// An empty branch makes a guard regression fail locally, before any
			// clone or remote access, while still exposing incorrect preparation.
			repo.DefaultBranch = ""
			seedDesktopCatalogue(t, s, repo)
			var err error
			if scope == "repository" {
				err = s.SetRepositoryEnabled(context.Background(), repo.ID, false)
			} else {
				err = s.SetOrganizationEnabled(context.Background(), "team", false)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := s.Status()
			unlock, err := s.lockRepo(context.Background(), repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			if _, err := s.ensureRepository(context.Background(), "TEAM/REPO"); err == nil || !strings.Contains(err.Error(), "enable") {
				t.Fatalf("disabled preparation error = %v", err)
			}
			for _, action := range []string{"prepare", "refresh", "keep"} {
				if _, err := s.Action(repo.ID, action); err == nil {
					t.Fatalf("disabled repository accepted %s", action)
				}
			}
			if after := s.Status(); !reflect.DeepEqual(after, before) {
				t.Fatalf("refused acquisition changed catalogue or operations: %+v", after)
			}
			configs, err := s.engine.ListRepos(context.Background())
			if err != nil || len(configs) != 0 {
				t.Fatalf("disabled repository acquired Git storage: %+v, %v", configs, err)
			}
			op, err := s.Action("team/repo", "free")
			if err != nil || op.RepositoryID != repo.ID {
				t.Fatalf("disabled free = %+v, %v", op, err)
			}
			if canceled, err := s.Action("TEAM/REPO", "cancel"); err != nil || canceled.ID != op.ID {
				t.Fatalf("disabled cancel = %+v, %v", canceled, err)
			}
			awaitDesktop(t, func() bool {
				status := s.Status()
				return len(status.Operations) == 1 && status.Operations[0].Status == "canceled"
			})
		})
	}
}

func TestVisibilityPinnedWorkerSkipsHiddenRepositories(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newDesktopTestService(t, "exit 4")
		individual := catalogueRepository("Team", "individual")
		individual.Disabled, individual.Pinned, individual.DefaultBranch = true, true, ""
		group := catalogueRepository("Hidden", "group")
		group.Pinned, group.DefaultBranch = true, ""
		seedDesktopCatalogue(t, s, individual, group)
		if err := s.SetOrganizationEnabled(context.Background(), group.Owner, false); err != nil {
			t.Fatal(err)
		}
		s.workers.Add(1)
		go s.pinLoop()
		// The test clock advances through two real worker ticks. Empty branches
		// prevent network access if this visibility guard ever regresses.
		time.Sleep(21 * time.Second)
		synctest.Wait()
		if status := s.Status(); len(status.Operations) != 0 || !status.Repositories[0].Pinned || !status.Repositories[1].Pinned {
			t.Fatalf("pin worker acted on hidden repositories or cleared intent: %+v", status)
		}
		configs, err := s.engine.ListRepos(context.Background())
		if err != nil || len(configs) != 0 {
			t.Fatalf("hidden pin worker acquired repository storage: %+v, %v", configs, err)
		}
	})
}

func TestVisibilityHTTPRequiresExplicitBooleanAndValidIdentity(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo := catalogueRepository("Team", "repo")
	seedDesktopCatalogue(t, s, repo)
	for _, endpoint := range []struct{ path, key, identity string }{
		{"/v1/organizations/settings", "owner", "Team"},
		{"/v1/repositories/visibility", "id", repo.ID},
	} {
		for name, body := range map[string]string{
			"empty": "", "array": "[]", "null object": "null", "malformed": "{",
			"absent boolean":  `{"` + endpoint.key + `":"` + endpoint.identity + `"}`,
			"null boolean":    `{"` + endpoint.key + `":"` + endpoint.identity + `","enabled":null}`,
			"string boolean":  `{"` + endpoint.key + `":"` + endpoint.identity + `","enabled":"false"}`,
			"number boolean":  `{"` + endpoint.key + `":"` + endpoint.identity + `","enabled":0}`,
			"absent identity": `{"enabled":false}`,
			"unknown field":   `{"` + endpoint.key + `":"` + endpoint.identity + `","enabled":false,"extra":1}`,
			"trailing body":   `{"` + endpoint.key + `":"` + endpoint.identity + `","enabled":false} {}`,
			"unsafe identity": `{"` + endpoint.key + `":"../escape","enabled":false}`,
		} {
			t.Run(endpoint.key+"/"+name, func(t *testing.T) {
				before := s.Status()
				response := httptest.NewRecorder()
				s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(body)))
				if response.Code != http.StatusBadRequest || !json.Valid(response.Body.Bytes()) {
					t.Fatalf("invalid visibility request = %d, %s", response.Code, response.Body.String())
				}
				if after := s.Status(); !reflect.DeepEqual(after, before) {
					t.Fatal("invalid visibility request mutated policy")
				}
			})
		}
		for _, enabled := range []string{"false", "true"} {
			body := `{"` + endpoint.key + `":"` + endpoint.identity + `","enabled":` + enabled + `}`
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(body)))
			if response.Code != http.StatusOK {
				t.Fatalf("explicit boolean request = %d, %s", response.Code, response.Body.String())
			}
			if visible := len(visibilityEntries(s)) == 1; visible != (enabled == "true") {
				t.Fatal("HTTP visibility did not publish the requested policy")
			}
		}
	}
}

func TestVisibilityPersistenceFailureRestoresDetachedPolicies(t *testing.T) {
	for _, scope := range []string{"repository", "owner removal", "owner addition"} {
		t.Run(scope, func(t *testing.T) {
			s := newDesktopTestService(t, "exit 4")
			repo := catalogueRepository("Team", "repo")
			repo.Pinned, repo.Source = true, "github"
			seedDesktopCatalogue(t, s, repo)
			if err := s.SetOrganizationEnabled(context.Background(), "Team", false); err != nil {
				t.Fatal(err)
			}
			if err := s.SetOrganizationEnabled(context.Background(), "Other", false); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			oldRepos, oldOwners := s.state.Repositories, s.state.DisabledOrganizations
			reposCopy, ownersCopy := append([]Repository(nil), oldRepos...), append([]string(nil), oldOwners...)
			s.mu.Unlock()
			before := s.Status()
			path := filepath.Join(s.opts.StateDir, "catalogue.json")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			switch scope {
			case "repository":
				err = s.SetRepositoryEnabled(context.Background(), repo.ID, false)
			case "owner removal":
				err = s.SetOrganizationEnabled(context.Background(), "team", true)
			case "owner addition":
				err = s.SetOrganizationEnabled(context.Background(), "Third", false)
			}
			if err == nil {
				t.Fatal("visibility ignored a failed durable save")
			}
			if after := s.Status(); !reflect.DeepEqual(after, before) {
				t.Fatalf("failed save changed policy: %+v", after)
			}
			if !reflect.DeepEqual(oldRepos, reposCopy) || !reflect.DeepEqual(oldOwners, ownersCopy) {
				t.Fatal("rollback snapshots shared modified slice storage")
			}
			// A failed save must release all repository reservations for retry.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := s.SetOrganizationEnabled(context.Background(), "team", true); err != nil {
				t.Fatalf("visibility could not recover after failed save: %v", err)
			}
		})
	}
}

func TestVisibilityPublicationFailureRestoresSavedPolicyAndCatalogue(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	visible := catalogueRepository("Team", "visible")
	hidden := catalogueRepository("TEAM", "hidden")
	hidden.Disabled, hidden.Pinned, hidden.DownloadedBytes = true, true, 8192
	// Deliberately seed an incompatible owner alias to exercise publication
	// failure. Loading rejects this malformed state, but an attempted policy
	// change must still restore its durable input if publication fails.
	seedDesktopCatalogue(t, s, visible, hidden)
	fs := visibilityMount(t, s)
	before := s.Status()
	statePath := filepath.Join(s.opts.StateDir, "catalogue.json")
	beforeDisk, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	owner := visibilityLookup(t, fs, fuseops.RootInodeID, visible.Owner)
	if err := s.SetRepositoryEnabled(context.Background(), hidden.ID, true); err == nil {
		t.Fatal("owner spelling collision was published")
	}
	if status := s.Status(); !reflect.DeepEqual(status, before) {
		t.Fatalf("failed catalogue publication changed saved repository state: %+v", status)
	}
	visibilityLookup(t, fs, owner, visible.Name)
	if err := fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: hidden.Owner}); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("failed publication exposed conflicting owner: %v", err)
	}
	afterDisk, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(beforeDisk, afterDisk) {
		t.Fatalf("failed publication changed durable hidden policy: %v", err)
	}
	if _, err := readState(statePath, s.opts.MountRoot); err == nil {
		t.Fatal("malformed owner collision fixture was accepted on reload")
	}
	// The failed publication cannot retain a repository reservation.
	if err := s.SetOrganizationEnabled(context.Background(), "team", false); err != nil {
		t.Fatalf("failed publication retained repository locks: %v", err)
	}
}

func TestVisibilityStateMigratesLegacyAndRejectsInvalidPolicies(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mount")
	repo := catalogueRepository("Team", "repo")
	legacy := persistedState{SchemaVersion: 1, MountRoot: root, Repositories: []Repository{repo}}
	path := filepath.Join(t.TempDir(), "catalogue.json")
	if err := writeState(path, legacy); err != nil {
		t.Fatal(err)
	}
	loaded, err := readState(path, root)
	if err != nil || loaded.SchemaVersion != 3 || len(loaded.DisabledOrganizations) != 0 || loaded.Repositories[0].Disabled || loaded.Repositories[0].Source != "" || loaded.Repositories[0] != repo {
		t.Fatalf("legacy migration changed repository defaults: %+v, %v", loaded, err)
	}
	for name, mutate := range map[string]func(*persistedState){
		"unknown schema":  func(s *persistedState) { s.SchemaVersion = 4 },
		"unknown source":  func(s *persistedState) { s.Repositories[0].Source = "unsupported" },
		"relative owner":  func(s *persistedState) { s.DisabledOrganizations = []string{".."} },
		"owner traversal": func(s *persistedState) { s.DisabledOrganizations = []string{"../Team"} },
		"owner separator": func(s *persistedState) { s.DisabledOrganizations = []string{"Team/other"} },
		"duplicate case":  func(s *persistedState) { s.DisabledOrganizations = []string{"Team", "TEAM"} },
	} {
		t.Run(name, func(t *testing.T) {
			state := persistedState{SchemaVersion: 2, MountRoot: root, Repositories: []Repository{repo}}
			mutate(&state)
			if err := writeState(path, state); err != nil {
				t.Fatal(err)
			}
			if _, err := readState(path, root); err == nil {
				t.Fatal("invalid saved policy was accepted")
			}
		})
	}
}

func TestVisibilityDiscoveryPreservesManualPoliciesAndStableOwnerCasing(t *testing.T) {
	responses := filepath.Join(t.TempDir(), "repositories.json")
	t.Setenv("FAKE_VISIBILITY_REPOS", responses)
	s := newDesktopTestService(t, `
case "$*" in
  'api --hostname github.com user') printf '%s' '{"login":"octocat"}' ;;
  *) cat "$FAKE_VISIBILITY_REPOS" ;;
esac
`)
	old := catalogueRepository("Team", "old")
	old.Disabled, old.Pinned, old.Source, old.State, old.DownloadedBytes = true, true, "github", "pinned", 4096
	manual := catalogueRepository("Team", "manual")
	manual.Source, manual.CloneURL, manual.HTMLURL = "manual", "https://git.example/Team/manual.git", "https://git.example/Team/manual"
	manual.Disabled, manual.Pinned, manual.State, manual.DownloadedBytes = true, true, "pinned", 2048
	absent := manual
	absent.ID, absent.Owner, absent.Name = "Elsewhere/retained", "Elsewhere", "retained"
	seedDesktopCatalogue(t, s, old, manual, absent)
	fs := visibilityMount(t, s)
	if err := os.WriteFile(responses, []byte(`[{"name":"OLD","owner":{"login":"TEAM"},"description":"updated","default_branch":"trunk"},{"name":"manual","owner":{"login":"TEAM"},"default_branch":"other"},{"name":"new","owner":{"login":"TEAM"},"default_branch":"main"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := s.Discover(context.Background())
	if err != nil {
		t.Fatalf("discovery with a newly accessible case-alias owner failed: %v", err)
	}
	if len(status.Repositories) != 4 {
		t.Fatalf("discovery discarded saved manual repositories: %+v", status.Repositories)
	}
	got := visibilityRepository(t, s, old.ID)
	if got.ID != old.ID || got.Owner != old.Owner || got.Name != old.Name || got.CloneURL != old.CloneURL || got.HTMLURL != old.HTMLURL || !got.Disabled || !got.Pinned || got.Source != "github" || got.State != "pinned" || got.DownloadedBytes != old.DownloadedBytes || got.Description != "updated" {
		t.Fatalf("discovery replaced stable identity or local policy: %+v", got)
	}
	if got := visibilityRepository(t, s, manual.ID); got != manual {
		t.Fatalf("GitHub discovery replaced manual remote/auth policy: %+v", got)
	}
	if got := visibilityRepository(t, s, absent.ID); got != absent {
		t.Fatalf("discovery altered absent manual source: %+v", got)
	}
	owner := visibilityLookup(t, fs, fuseops.RootInodeID, "Team")
	visibilityLookup(t, fs, owner, "new")
	newRepo := visibilityRepository(t, s, "team/new")
	if newRepo.Owner != "Team" || newRepo.ID != "Team/new" || validateRepository(newRepo) != nil {
		t.Fatalf("new repository did not reuse canonical group spelling: %+v", newRepo)
	}
	loaded, err := readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
	if err != nil || !reflect.DeepEqual(loaded.Repositories, status.Repositories) {
		t.Fatalf("stable identities and policies were not persisted: %v", err)
	}
}
