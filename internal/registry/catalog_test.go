package registry

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestSetCatalogMountPathsIsAtomicAndPreservesOtherFields(t *testing.T) {
	ctx := context.Background()
	store, err := New(ctx, filepath.Join(t.TempDir(), "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldRoot, root := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	for _, name := range []string{"first", "second"} {
		if err := store.AddRepo(ctx, model.RepoConfig{
			ID: model.RepoID(name), Name: name, MountRoot: oldRoot, MountPath: filepath.Join(oldRoot, name),
			Branch: "local-branch", PrepareState: model.PrepareStateFailed, PrepareError: "retained",
			RefreshInterval: 13 * time.Minute, ConfigVersion: "v-" + name,
			RemoteRefreshDisabled: true, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, paths := range map[string]map[string]string{
		"missing":   {"first": filepath.Join(root, "first")},
		"outside":   {"first": filepath.Join(root, "first"), "second": filepath.Join(oldRoot, "second")},
		"duplicate": {"first": filepath.Join(root, "same"), "second": filepath.Join(root, "same")},
		"root":      {"first": filepath.Join(root, "first"), "second": root},
		"relative":  {"first": filepath.Join(root, "first"), "second": "second"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.SetCatalogMountPaths(ctx, root, paths); err == nil {
				t.Fatal("invalid catalogue paths were accepted")
			}
			after, err := store.ListRepos(ctx)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("invalid migration changed repositories: %+v, %v", after, err)
			}
		})
	}
	paths := map[string]string{
		"first": filepath.Join(root, "owner", "first"), "second": filepath.Join(root, "owner", "second"),
		"dormant": filepath.Join(root, "owner", "dormant"),
	}
	if err := store.SetCatalogMountPaths(ctx, root, paths); err != nil {
		t.Fatal(err)
	}
	after, err := store.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range before {
		before[i].MountRoot, before[i].MountPath = root, paths[before[i].Name]
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("mount migration changed unrelated fields: got %+v, want %+v", after, before)
	}
}

func TestSetCatalogMountPathsRollsBackDatabaseFailure(t *testing.T) {
	ctx := context.Background()
	store, err := New(ctx, filepath.Join(t.TempDir(), "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldRoot, root := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	for _, name := range []string{"first", "second"} {
		if err := store.AddRepo(ctx, model.RepoConfig{ID: model.RepoID(name), Name: name, MountRoot: oldRoot, MountPath: filepath.Join(oldRoot, name)}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_second_mount BEFORE UPDATE OF mount_path ON repos WHEN NEW.name='second' BEGIN SELECT RAISE(ABORT, 'injected disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCatalogMountPaths(ctx, root, map[string]string{"first": filepath.Join(root, "first"), "second": filepath.Join(root, "second")}); err == nil {
		t.Fatal("database failure was ignored")
	}
	after, err := store.ListRepos(ctx)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("database failure left partial changes: %+v, %v", after, err)
	}
}
