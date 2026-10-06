//go:build darwin

package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

type hybridConflictFixture struct {
	service *Service
	before  persistedState
	foreign string
	mounts  []*desktopFakeMount
}

func newHybridConflictFixture(t *testing.T, ghScript string, hidden Repository, hideOwner bool) *hybridConflictFixture {
	t.Helper()
	s := newDesktopTestService(t, ghScript)
	s.hybridCatalogue, s.quiescentCatalogue = true, true
	old := catalogueRepository("base", "kept")
	repositories := []Repository{old}
	if hidden.ID != "" {
		repositories = append(repositories, hidden)
	}
	seedDesktopCatalogue(t, s, repositories...)
	if hideOwner {
		s.mu.Lock()
		s.state.DisabledOrganizations = []string{"work"}
		err := s.persistLocked()
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	fixture := &hybridConflictFixture{service: s}
	s.dependencyReady = func() bool { return true }
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		mount := newDesktopFakeMount()
		fixture.mounts = append(fixture.mounts, mount)
		return mount, nil
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	var err error
	fixture.before, err = readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
	if err != nil {
		t.Fatal(err)
	}
	fixture.foreign = filepath.Join(s.Status().MountRoot, "work", "taken")
	if err := os.MkdirAll(fixture.foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.foreign, "foreign.bin"), []byte{0, 255, 37}, 0o640); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *hybridConflictFixture) assertRestored(t *testing.T, changeErr error) {
	t.Helper()
	if !errors.Is(changeErr, errHybridCatalogueConflict) {
		t.Fatalf("expected foreign checkout conflict, got %v", changeErr)
	}
	s := fixture.service
	after, err := readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
	if err != nil || !reflect.DeepEqual(after, fixture.before) {
		t.Fatalf("failed publication changed durable catalogue: before=%+v after=%+v err=%v", fixture.before, after, err)
	}
	if status := s.Status(); !status.Mounted || !reflect.DeepEqual(status.Repositories, fixture.before.Repositories) {
		t.Fatalf("failed publication did not restore existing catalogue: %+v", status)
	}
	if len(fixture.mounts) != 2 {
		t.Fatalf("previous catalogue was not remounted after conflict: %d mount sessions", len(fixture.mounts))
	}
	select {
	case <-fixture.mounts[0].done:
	default:
		t.Fatal("previous session was not drained before catalogue change")
	}
	info, err := os.Lstat(fixture.foreign)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("foreign checkout was replaced: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(fixture.foreign, "foreign.bin"))
	if err != nil || !reflect.DeepEqual(data, []byte{0, 255, 37}) {
		t.Fatalf("foreign checkout contents changed: %v", err)
	}
	privateRoot, err := s.hybridCatalogueMountRoot()
	if err != nil {
		t.Fatal(err)
	}
	assertHybridLink(t, filepath.Join(s.Status().MountRoot, "base", "kept"), filepath.Join(privateRoot, "base", "kept"))
	s.mu.Lock()
	fs := s.catalog
	s.mu.Unlock()
	owner := visibilityLookup(t, fs, 1, "base")
	visibilityLookup(t, fs, owner, "kept")
	if configs, err := s.engine.ListRepos(context.Background()); err != nil || len(configs) != 0 {
		t.Fatalf("conflicting publication prepared repositories: %+v %v", configs, err)
	}
}

func TestHybridAdoptionConflictRestoresDurableCatalogue(t *testing.T) {
	fixture := newHybridConflictFixture(t, "exit 99", Repository{}, false)
	source, _ := adoptionSource(t)
	before := sourceState(t, source)
	_, err := fixture.service.Adopt(context.Background(), AdoptionRequest{RemoteURL: source, Owner: "work", Name: "taken"})
	fixture.assertRestored(t, err)
	if !reflect.DeepEqual(before, sourceState(t, source)) {
		t.Fatal("conflicting adoption changed original checkout")
	}
}

func TestHybridVisibilityConflictRestoresDurableCatalogue(t *testing.T) {
	for _, owner := range []bool{false, true} {
		name := "repository"
		if owner {
			name = "organization"
		}
		t.Run(name, func(t *testing.T) {
			hidden := catalogueRepository("work", "taken")
			hidden.Disabled = !owner
			fixture := newHybridConflictFixture(t, "exit 99", hidden, owner)
			var err error
			if owner {
				err = fixture.service.SetOrganizationEnabled(context.Background(), "work", true)
			} else {
				err = fixture.service.SetRepositoryEnabled(context.Background(), hidden.ID, true)
			}
			fixture.assertRestored(t, err)
		})
	}
}

func TestHybridDiscoveryConflictRestoresDurableCatalogue(t *testing.T) {
	fixture := newHybridConflictFixture(t, `
case "$*" in
  'api --hostname github.com user') printf '%s' '{"login":"new-account"}' ;;
  'api --hostname github.com --paginate user/repos?per_page=100&visibility=all&affiliation=owner,collaborator,organization_member&sort=full_name&direction=asc') printf '%s' '[{"name":"kept","owner":{"login":"base"},"description":"candidate update","default_branch":"main"},{"name":"taken","owner":{"login":"work"},"default_branch":"main"}]' ;;
  *) exit 1 ;;
esac
`, Repository{}, false)
	_, err := fixture.service.Discover(context.Background())
	fixture.assertRestored(t, err)
}
