package desktop

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/jacobsa/fuse/fuseops"
)

type catalogueChangeMount struct {
	*desktopFakeMount
	onUnmount   func()
	onJoin      func(context.Context)
	unmountOnce sync.Once
	joinOnce    sync.Once
}

func (m *catalogueChangeMount) Unmount() error {
	m.unmountOnce.Do(func() {
		if m.onUnmount != nil {
			m.onUnmount()
		}
	})
	return m.desktopFakeMount.Unmount()
}

func (m *catalogueChangeMount) Join(ctx context.Context) error {
	// The service's observation worker has no deadline. Run the hook only for
	// the bounded drain, so its scheduling cannot steal the assertion.
	if _, bounded := ctx.Deadline(); bounded {
		m.joinOnce.Do(func() {
			if m.onJoin != nil {
				m.onJoin(ctx)
			}
		})
	}
	return m.desktopFakeMount.Join(ctx)
}

type catalogueChangeHarness struct {
	s          *Service
	catalogues []*catalogfs.FileSystem
	mounts     []*catalogueChangeMount
	onMount    func(context.Context, *catalogfs.FileSystem) error
}

func mountCatalogueChangeService(t *testing.T, s *Service) *catalogueChangeHarness {
	t.Helper()
	h := &catalogueChangeHarness{s: s}
	s.quiescentCatalogue = true
	s.dependencyReady = func() bool { return true }
	s.mountCatalogue = func(ctx context.Context, _ string, fs *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		h.catalogues = append(h.catalogues, fs)
		if h.onMount != nil {
			if err := h.onMount(ctx, fs); err != nil {
				return nil, err
			}
		}
		m := &catalogueChangeMount{desktopFakeMount: newDesktopFakeMount()}
		h.mounts = append(h.mounts, m)
		return m, nil
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h
}

const catalogueChangeGitHub = `
case "$*" in
  'api --hostname github.com user') printf '%s' '{"login":"octocat"}' ;;
  *) printf '%s' '[{"name":"a","owner":{"login":"Team"},"description":"updated","default_branch":"trunk"},{"name":"new","owner":{"login":"Team"},"default_branch":"main"}]' ;;
esac
`

func newCatalogueChangeService(t *testing.T) *Service {
	t.Helper()
	s := newDesktopTestService(t, catalogueChangeGitHub)
	a, b, other := catalogueRepository("Team", "a"), catalogueRepository("Team", "b"), catalogueRepository("other", "repo")
	a.Pinned, a.State, a.DownloadedBytes, a.Error = true, "pinned", 4096, "retained message"
	b.Disabled, b.Pinned = true, true
	other.State, other.DownloadedBytes = "available", 512
	s.mu.Lock()
	s.state.Account = &Account{Login: "before"}
	s.state.Repositories = []Repository{other, a, b}
	s.state.DisabledOrganizations = []string{"Absent"}
	s.pins[a.ID], s.pins[b.ID] = "verified-a", "verified-b"
	s.ops = []Operation{{ID: "completed", RepositoryID: other.ID, Action: "keep", Status: "complete", DownloadedBytes: 512}}
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type catalogueChangeCase struct {
	name  string
	apply func(context.Context, *Service) error
	ids   []string
	check func(*testing.T, *Service)
}

func catalogueChangeCases(t *testing.T) []catalogueChangeCase {
	t.Helper()
	source, _ := adoptionSource(t)
	return []catalogueChangeCase{
		{name: "adoption", apply: func(ctx context.Context, s *Service) error {
			_, err := s.Adopt(ctx, AdoptionRequest{RemoteURL: source, Owner: "TEAM", Name: "new"})
			return err
		}, ids: []string{"Team/a", "Team/new", "other/repo"}, check: func(t *testing.T, s *Service) {
			if repo := visibilityRepository(t, s, "Team/new"); repo.Source != "manual" || repo.DefaultBranch != "trunk" {
				t.Fatalf("adoption did not commit the manual source: %+v", repo)
			}
		}},
		{name: "discovery", apply: func(ctx context.Context, s *Service) error {
			_, err := s.Discover(ctx)
			return err
		}, ids: []string{"Team/a", "Team/new", "other/repo"}, check: func(t *testing.T, s *Service) {
			if status := s.Status(); status.Account == nil || status.Account.Login != "octocat" || visibilityRepository(t, s, "Team/a").Description != "updated" {
				t.Fatalf("discovery did not commit updated account and metadata: %+v", status)
			}
		}},
		{name: "repository visibility", apply: func(ctx context.Context, s *Service) error {
			return s.SetRepositoryEnabled(ctx, "TEAM/A", false)
		}, ids: []string{"other/repo"}, check: func(t *testing.T, s *Service) {
			if repo := visibilityRepository(t, s, "Team/a"); !repo.Disabled || !repo.Pinned || repo.DownloadedBytes != 4096 {
				t.Fatalf("individual visibility changed saved repository data: %+v", repo)
			}
		}},
		{name: "group visibility", apply: func(ctx context.Context, s *Service) error {
			return s.SetOrganizationEnabled(ctx, "TEAM", false)
		}, ids: []string{"other/repo"}, check: func(t *testing.T, s *Service) {
			s.mu.Lock()
			enabled := s.organizationEnabledLocked("Team")
			s.mu.Unlock()
			if enabled || visibilityRepository(t, s, "Team/a").Disabled || !visibilityRepository(t, s, "Team/b").Disabled {
				t.Fatal("group visibility reset individual repository policy")
			}
		}},
	}
}

type catalogueChangeSnapshot struct {
	state            persistedState
	status           Status
	pins             map[string]string
	maintenance      bool
	recoveryRequired bool
	mounted          fusefs.MountedFS
	catalog          *catalogfs.FileSystem
	disk             []byte
}

func snapshotCatalogueChange(t *testing.T, s *Service) catalogueChangeSnapshot {
	t.Helper()
	s.mu.Lock()
	snapshot := catalogueChangeSnapshot{state: s.state, pins: map[string]string{}, maintenance: s.maintenance,
		recoveryRequired: s.recoveryRequired, mounted: s.mounted, catalog: s.catalog}
	snapshot.state.Repositories = append([]Repository(nil), s.state.Repositories...)
	snapshot.state.DisabledOrganizations = append([]string(nil), s.state.DisabledOrganizations...)
	if s.state.Account != nil {
		account := *s.state.Account
		snapshot.state.Account = &account
	}
	for id, head := range s.pins {
		snapshot.pins[id] = head
	}
	s.mu.Unlock()
	snapshot.status = s.Status()
	var err error
	snapshot.disk, err = os.ReadFile(filepath.Join(s.opts.StateDir, "catalogue.json"))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertCatalogueChangeRestored(t *testing.T, s *Service, before catalogueChangeSnapshot, sameMount bool) {
	t.Helper()
	after := snapshotCatalogueChange(t, s)
	if !reflect.DeepEqual(after.state, before.state) || !reflect.DeepEqual(after.pins, before.pins) ||
		!reflect.DeepEqual(after.status.Repositories, before.status.Repositories) || !reflect.DeepEqual(after.status.Operations, before.status.Operations) ||
		after.maintenance != before.maintenance || after.recoveryRequired != before.recoveryRequired || !bytes.Equal(after.disk, before.disk) {
		t.Fatalf("failed catalogue change lost prior state:\nbefore = %+v\nafter = %+v", before, after)
	}
	if !after.status.Mounted || !after.state.MountDesired {
		t.Fatalf("failed change did not restore the desired mount: %+v", after.status)
	}
	if sameMount && (after.mounted != before.mounted || after.catalog != before.catalog || !reflect.DeepEqual(after.status, before.status)) {
		t.Fatal("busy detach changed the active mount or visible status")
	}
}

func assertCatalogueChangeView(t *testing.T, fs *catalogfs.FileSystem, ids []string) {
	t.Helper()
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for owner, names := range map[string][]string{"Team": {"a", "b", "new"}, "other": {"repo"}} {
		hasOwner := false
		for _, name := range names {
			hasOwner = hasOwner || want[owner+"/"+name]
		}
		group := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: owner}
		err := fs.LookUpInode(context.Background(), group)
		if !hasOwner {
			if !errors.Is(err, syscall.ENOENT) {
				t.Fatalf("unexpected catalogue owner %q: %v", owner, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("lookup owner %q: %v", owner, err)
		}
		for _, name := range names {
			err := fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{Parent: group.Entry.Child, Name: name})
			if want[owner+"/"+name] && err != nil {
				t.Fatalf("missing catalogue entry %q: %v", owner+"/"+name, err)
			}
			if !want[owner+"/"+name] && !errors.Is(err, syscall.ENOENT) {
				t.Fatalf("unexpected catalogue entry %q: %v", owner+"/"+name, err)
			}
		}
	}
}

func assertCatalogueChangePersisted(t *testing.T, s *Service) {
	t.Helper()
	loaded, err := readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if !reflect.DeepEqual(loaded, state) || !loaded.MountDesired {
		t.Fatalf("fresh mount did not follow the durable catalogue commit: saved = %+v, memory = %+v", loaded, state)
	}
}

func TestCatalogueChangeBusyDetachPreservesEverything(t *testing.T) {
	for _, test := range catalogueChangeCases(t) {
		t.Run(test.name, func(t *testing.T) {
			s := newCatalogueChangeService(t)
			h := mountCatalogueChangeService(t, s)
			busy := errors.New("fixture: volume is busy")
			h.mounts[0].unmountErr = busy
			t.Cleanup(func() {
				h.mounts[0].mu.Lock()
				h.mounts[0].unmountErr = nil
				h.mounts[0].mu.Unlock()
			})
			before := snapshotCatalogueChange(t, s)
			if err := test.apply(context.Background(), s); !errors.Is(err, busy) {
				t.Fatalf("catalogue change error = %v, want busy detach", err)
			}
			assertCatalogueChangeRestored(t, s, before, true)
			if h.mounts[0].unmountCalls != 1 || len(h.catalogues) != 1 {
				t.Fatal("busy detach started a replacement mount")
			}
			assertCatalogueChangeView(t, h.catalogues[0], []string{"Team/a", "other/repo"})
		})
	}
}

func TestCatalogueChangeCommitsBeforeFreshMount(t *testing.T) {
	for _, test := range catalogueChangeCases(t) {
		t.Run(test.name, func(t *testing.T) {
			s := newCatalogueChangeService(t)
			h := mountCatalogueChangeService(t, s)
			before := snapshotCatalogueChange(t, s)
			h.mounts[0].onUnmount = func() {
				current := snapshotCatalogueChange(t, s)
				if !reflect.DeepEqual(current.state, before.state) || !bytes.Equal(current.disk, before.disk) {
					t.Error("catalogue changed before normal detach succeeded")
				}
			}
			h.onMount = func(_ context.Context, fs *catalogfs.FileSystem) error {
				assertCatalogueChangePersisted(t, s)
				assertCatalogueChangeView(t, fs, test.ids)
				return nil
			}
			if err := test.apply(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if len(h.mounts) != 2 || len(h.catalogues) != 2 || h.catalogues[0] == h.catalogues[1] || h.mounts[0].unmountCalls != 1 || !s.Status().Mounted {
				t.Fatal("successful change did not replace the detached catalogue")
			}
			assertCatalogueChangeView(t, h.catalogues[0], []string{"Team/a", "other/repo"})
			after := snapshotCatalogueChange(t, s)
			if !reflect.DeepEqual(after.pins, before.pins) || !reflect.DeepEqual(after.status.Operations, before.status.Operations) {
				t.Fatal("catalogue commit changed pin verification or operation history")
			}
			test.check(t, s)
		})
	}
}

func TestCatalogueChangeCancellationRestoresMountWithLiveContext(t *testing.T) {
	for _, test := range catalogueChangeCases(t) {
		t.Run(test.name, func(t *testing.T) {
			s := newCatalogueChangeService(t)
			h := mountCatalogueChangeService(t, s)
			before := snapshotCatalogueChange(t, s)
			type requestKey struct{}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), requestKey{}, "request-value"))
			defer cancel()
			h.mounts[0].onUnmount = cancel
			h.onMount = func(ctx context.Context, fs *catalogfs.FileSystem) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				deadline, bounded := ctx.Deadline()
				if ctx.Value(requestKey{}) != "request-value" || !bounded || time.Until(deadline) > 30*time.Second {
					t.Error("rollback mount lost request values or its bounded recovery context")
				}
				assertCatalogueChangeView(t, fs, []string{"Team/a", "other/repo"})
				return nil
			}
			if err := test.apply(ctx, s); !errors.Is(err, context.Canceled) {
				t.Fatalf("catalogue change error = %v, want canceled request", err)
			}
			assertCatalogueChangeRestored(t, s, before, false)
			if len(h.mounts) != 2 || h.catalogues[0] == h.catalogues[1] {
				t.Fatal("canceled request did not restore a fresh mount")
			}
		})
	}
}

func TestCatalogueChangeRemountFailureRetainsCommittedChangeForRetry(t *testing.T) {
	for _, test := range catalogueChangeCases(t) {
		t.Run(test.name, func(t *testing.T) {
			s := newCatalogueChangeService(t)
			h := mountCatalogueChangeService(t, s)
			remountErr := errors.New("fixture: mount failed")
			h.onMount = func(_ context.Context, fs *catalogfs.FileSystem) error {
				assertCatalogueChangePersisted(t, s)
				assertCatalogueChangeView(t, fs, test.ids)
				return remountErr
			}
			if err := test.apply(context.Background(), s); !errors.Is(err, remountErr) {
				t.Fatalf("catalogue change error = %v, want failed remount", err)
			}
			test.check(t, s)
			status := s.Status()
			message := strings.ToLower(status.Message)
			if status.Mounted || !strings.Contains(message, "mount") || (!strings.Contains(message, "retry") && !strings.Contains(message, "again")) {
				t.Fatalf("failed remount did not explain how to retry the saved change: %+v", status)
			}
			assertCatalogueChangePersisted(t, s)
			h.onMount = nil
			if err := s.Mount(context.Background()); err != nil {
				t.Fatalf("retry mount: %v", err)
			}
			if !s.Status().Mounted || len(h.mounts) != 2 || len(h.catalogues) != 3 {
				t.Fatal("saved catalogue could not be mounted on retry")
			}
			assertCatalogueChangeView(t, h.catalogues[2], test.ids)
		})
	}
}

func assertCatalogueChangeCallbacksUnlocked(t *testing.T, s *Service, event string, ids []string) {
	t.Helper()
	acquired := make(chan bool, 1)
	go func() {
		s.mu.Lock()
		maintenance := s.maintenance
		s.mu.Unlock()
		acquired <- maintenance
	}()
	select {
	case maintenance := <-acquired:
		if !maintenance {
			t.Errorf("%s callback ran without the maintenance gate", event)
		}
	case <-time.After(time.Second):
		t.Errorf("%s callback ran while Service.mu was held", event)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, id := range ids {
		unlock, err := s.lockRepo(ctx, id)
		if err != nil {
			t.Errorf("%s callback could not acquire repository %s's lock: %v", event, id, err)
			return
		}
		unlock()
	}
	if _, err := s.Action("Team/a", "catalogue-test-probe"); err == nil || !strings.Contains(err.Error(), "wait") {
		t.Errorf("%s callback could start an explicit action during maintenance: %v", event, err)
	}
}

func TestCatalogueChangeVisibilityDrainReleasesServiceAndRepositoryLocks(t *testing.T) {
	for _, group := range []bool{false, true} {
		name := "repository"
		if group {
			name = "group"
		}
		t.Run(name, func(t *testing.T) {
			s := newCatalogueChangeService(t)
			h := mountCatalogueChangeService(t, s)
			affected := []string{"TEAM/A"}
			if group {
				affected = append(affected, "TEAM/B")
			}
			unmounts, joins, remounts := 0, 0, 0
			h.mounts[0].onUnmount = func() {
				unmounts++
				assertCatalogueChangeCallbacksUnlocked(t, s, "Unmount", affected)
			}
			h.mounts[0].onJoin = func(context.Context) {
				joins++
				assertCatalogueChangeCallbacksUnlocked(t, s, "Join", affected)
			}
			h.onMount = func(context.Context, *catalogfs.FileSystem) error {
				remounts++
				assertCatalogueChangeCallbacksUnlocked(t, s, "remount", affected)
				return nil
			}
			var err error
			if group {
				err = s.SetOrganizationEnabled(context.Background(), "Team", false)
			} else {
				err = s.SetRepositoryEnabled(context.Background(), "Team/a", false)
			}
			if err != nil {
				t.Fatal(err)
			}
			if unmounts != 1 || joins != 1 || remounts != 1 {
				t.Fatalf("catalogue lifecycle hooks were skipped: Unmount = %d, Join = %d, remount = %d", unmounts, joins, remounts)
			}
		})
	}
}

func TestCatalogueChangeRollbackPreservesCompletionDuringDetach(t *testing.T) {
	s := newCatalogueChangeService(t)
	s.mu.Lock()
	s.ops[0].Status = "running"
	s.cancels["other/repo"] = func() {}
	s.mu.Unlock()
	h := mountCatalogueChangeService(t, s)
	block, restore := blockCatalogueChangePersistence(t, s)
	var completed catalogueChangeSnapshot
	h.mounts[0].onUnmount = func() {
		s.mu.Lock()
		index := s.repositoryIndexLocked("other/repo")
		s.state.Repositories[index].State = "pinned"
		s.state.Repositories[index].Pinned = true
		s.state.Repositories[index].DownloadedBytes = 16384
		s.pins["other/repo"] = "completed-head"
		s.ops[0].Status, s.ops[0].DownloadedBytes = "complete", 16384
		delete(s.cancels, "other/repo")
		err := s.persistLocked()
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		completed = snapshotCatalogueChange(t, s)
		// Maintenance is temporary transaction state, not part of the completed
		// operation that must survive the subsequent rollback.
		completed.maintenance = false
		block()
	}
	h.onMount = func(_ context.Context, fs *catalogfs.FileSystem) error {
		restore()
		assertCatalogueChangeView(t, fs, []string{"Team/a", "other/repo"})
		return nil
	}
	if err := s.SetRepositoryEnabled(context.Background(), "Team/a", false); err == nil {
		t.Fatal("catalogue change did not report the persistence failure")
	}
	assertCatalogueChangeRestored(t, s, completed, false)
	if visibilityRepository(t, s, "Team/a").Disabled || len(h.mounts) != 2 {
		t.Fatal("rollback retained the visibility change or lost the desired mount")
	}
}

func blockCatalogueChangePersistence(t *testing.T, s *Service) (func(), func()) {
	t.Helper()
	path := filepath.Join(s.opts.StateDir, "catalogue.json")
	backup := path + ".fixture-backup"
	restore := func() {
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			return
		}
		if err := os.Remove(path); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(backup, path); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(restore)
	block := func() {
		if err := os.Rename(path, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return block, restore
}

func TestCatalogueChangePersistenceFailureRestoresPriorMount(t *testing.T) {
	for _, test := range catalogueChangeCases(t) {
		t.Run(test.name, func(t *testing.T) {
			s := newCatalogueChangeService(t)
			h := mountCatalogueChangeService(t, s)
			before := snapshotCatalogueChange(t, s)
			block, restore := blockCatalogueChangePersistence(t, s)
			h.mounts[0].onUnmount = block
			h.onMount = func(_ context.Context, fs *catalogfs.FileSystem) error {
				s.mu.Lock()
				state := s.state
				s.mu.Unlock()
				if !reflect.DeepEqual(state, before.state) {
					t.Error("persistence failure remounted the uncommitted policy")
				}
				restore()
				assertCatalogueChangeView(t, fs, []string{"Team/a", "other/repo"})
				return nil
			}
			if err := test.apply(context.Background(), s); err == nil {
				t.Fatal("failed catalogue persistence was not reported")
			}
			if len(h.mounts) != 2 {
				t.Fatal("persistence failure did not restore the desired mount")
			}
			assertCatalogueChangeRestored(t, s, before, false)
		})
	}
}

func TestCatalogueChangeRefusesStorageRemovalWaitingForLifecycleBeforeDetach(t *testing.T) {
	s := newCatalogueChangeService(t)
	h := mountCatalogueChangeService(t, s)
	unmounted := make(chan struct{}, 1)
	h.mounts[0].onUnmount = func() { unmounted <- struct{}{} }
	s.lifecycle.Lock()
	locked := true
	defer func() {
		if locked {
			s.lifecycle.Unlock()
		}
	}()
	op, err := s.Action("other/repo", "free")
	if err != nil || op.Status != "running" {
		t.Fatalf("storage removal was not accepted: %+v, %v", op, err)
	}
	// The accepted worker owns its repository reservation and must next wait
	// for lifecycle, which this transaction already holds.
	awaitDesktop(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		lock := s.locks["other/repo"]
		return lock != nil && len(lock) == 0
	})
	before := snapshotCatalogueChange(t, s)
	remount, err := s.quiesceCatalogueChange(context.Background())
	if err == nil || remount || !strings.Contains(err.Error(), "storage removal") {
		t.Fatalf("running storage removal did not block quiescence: remount = %v, error = %v", remount, err)
	}
	select {
	case <-unmounted:
		t.Fatal("quiescence invoked Unmount while storage removal waited for lifecycle")
	default:
	}
	assertCatalogueChangeRestored(t, s, before, true)
	if h.mounts[0].unmountCalls != 0 || len(h.catalogues) != 1 {
		t.Fatal("refused quiescence changed mount ownership")
	}
	if _, err := s.Action("other/repo", "cancel"); err != nil {
		t.Fatal(err)
	}
	s.lifecycle.Unlock()
	locked = false
	awaitDesktop(t, func() bool {
		for _, current := range s.Status().Operations {
			if current.ID == op.ID {
				return current.Status != "running"
			}
		}
		return false
	})
}

func TestCatalogueChangeWithoutMountedSessionKeepsMaintenanceThroughPublication(t *testing.T) {
	s := newCatalogueChangeService(t)
	s.quiescentCatalogue = true
	s.mu.Lock()
	// A real preparation request must be refused by the publication gate. An
	// empty branch makes a guard regression fail locally without remote access.
	s.state.Repositories[s.repositoryIndexLocked("Team/a")].DefaultBranch = ""
	s.state.MountDesired = true
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotCatalogueChange(t, s)
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		t.Error("publication without a mounted session started a mount")
		return nil, errors.New("unexpected mount")
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	remount, err := s.quiesceCatalogueChange(context.Background())
	if err != nil || remount {
		t.Fatalf("quiescing without a mounted session = %v, %v", remount, err)
	}
	s.mu.Lock()
	maintenance := s.maintenance
	s.mu.Unlock()
	if !maintenance {
		t.Fatal("publication released maintenance before finishing the catalogue change")
	}
	if _, err := s.Action("Team/a", "prepare"); err == nil || !strings.Contains(err.Error(), "wait") {
		t.Fatalf("an explicit action bypassed the publication gate: %v", err)
	}
	if err := s.restoreCatalogueAfterChange(context.Background(), remount); err != nil {
		t.Fatal(err)
	}
	after := snapshotCatalogueChange(t, s)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("publication without a session changed mount intent or saved policy:\nbefore = %+v\nafter = %+v", before, after)
	}
}
