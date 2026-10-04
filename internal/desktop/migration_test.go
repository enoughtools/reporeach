package desktop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func addMigrationRepository(t *testing.T, s *Service, repo Repository, withGit bool) model.RepoConfig {
	t.Helper()
	root := s.Status().MountRoot
	name := engineName(repo.ID)
	if err := s.engine.AddRepoWithOptions(context.Background(), model.RepoConfig{
		ID: model.RepoID(name), Name: name, RemoteURL: repo.CloneURL, Branch: "refs/heads/main", Enabled: true,
		MountRoot: root, MountPath: filepath.Join(root, repo.Owner, repo.Name), RemoteRefreshDisabled: true,
	}, daemon.AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range configs {
		if cfg.Name != name {
			continue
		}
		if withGit {
			migrationGit(t, "init", "--bare", cfg.GitDir)
			if err := configureGitWorktree(context.Background(), cfg.GitDir, cfg.MountPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.GitDir, "index"), []byte("staged state must survive"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return cfg
	}
	t.Fatal("repository was not registered")
	return model.RepoConfig{}
}

func migrationGit(t *testing.T, args ...string) {
	t.Helper()
	if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git fixture: %v: %s", err, output)
	}
}

func migrationJournal(t *testing.T, s *Service, newRoot string) rootMigration {
	t.Helper()
	status := s.Status()
	repositories := map[string]Repository{}
	for _, repo := range status.Repositories {
		repositories[engineName(repo.ID)] = repo
	}
	journal := rootMigration{SchemaVersion: 1, OldRoot: status.MountRoot, NewRoot: newRoot, Paths: map[string]rootMigrationPath{}}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range configs {
		repo := repositories[cfg.Name]
		present, err := s.migrationGitDirectory(cfg)
		if err != nil {
			t.Fatal(err)
		}
		entry := rootMigrationPath{RepositoryID: repo.ID, OldPath: cfg.MountPath, NewPath: filepath.Join(newRoot, repo.Owner, repo.Name), GitPresent: present}
		if present {
			entry.OldWorktree, err = readMigrationWorktree(context.Background(), cfg.GitDir)
			if err != nil {
				t.Fatal(err)
			}
		}
		journal.Paths[cfg.Name] = entry
	}
	return journal
}

func assertMigrationLocations(t *testing.T, s *Service, root string, before []model.RepoConfig) {
	t.Helper()
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	repos := map[string]Repository{}
	for _, repo := range s.Status().Repositories {
		repos[engineName(repo.ID)] = repo
	}
	want := append([]model.RepoConfig(nil), before...)
	for i := range want {
		repo := repos[want[i].Name]
		want[i].MountRoot, want[i].MountPath = root, filepath.Join(root, repo.Owner, repo.Name)
		if _, err := os.Stat(want[i].GitDir); errors.Is(err, os.ErrNotExist) {
			continue
		}
		worktree, err := readMigrationWorktree(context.Background(), want[i].GitDir)
		if err != nil || worktree == nil || *worktree != want[i].MountPath {
			t.Fatalf("Git worktree = %v, error = %v, want %s", worktree, err, want[i].MountPath)
		}
		data, err := os.ReadFile(filepath.Join(want[i].GitDir, "index"))
		if err != nil || string(data) != "staged state must survive" {
			t.Fatal("migration rewrote the staged index")
		}
	}
	if !reflect.DeepEqual(configs, want) || s.Status().MountRoot != root {
		t.Fatalf("migration changed wrong fields: configs=%+v, want=%+v, state=%s", configs, want, s.Status().MountRoot)
	}
}

func TestMigrateRootCommitsGitRegistryAndCatalogue(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo, dormant := catalogueRepository("octocat", "repo"), catalogueRepository("org", "dormant")
	seedDesktopCatalogue(t, s, repo, dormant)
	addMigrationRepository(t, s, repo, true)
	addMigrationRepository(t, s, dormant, false)
	before, err := s.engine.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "Chosen folder")
	if err := s.migrateRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	assertMigrationLocations(t, s, root, before)
	saved, err := readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
	if err != nil || saved.MountRoot != root {
		t.Fatalf("catalogue commit failed: %+v, %v", saved, err)
	}
	if _, err := os.Lstat(filepath.Join(s.opts.StateDir, rootMigrationFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed journal was retained: %v", err)
	}
}

func TestMigrateRootGitFailureRollsBackEarlierConfigChanges(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	first, second := catalogueRepository("octocat", "first"), catalogueRepository("org", "second")
	seedDesktopCatalogue(t, s, first, second)
	addMigrationRepository(t, s, first, true)
	addMigrationRepository(t, s, second, true)
	before, err := s.engine.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The registry lists names in order, so this fails after the first config
	// changes. An unchanged locked config must not prevent rolling the first back.
	lock := filepath.Join(before[len(before)-1].GitDir, "config.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = s.migrateRoot(context.Background(), filepath.Join(t.TempDir(), "new"))
	if err == nil || errors.Is(err, errRootMigrationRecoveryNeeded) {
		t.Fatalf("migration failure was not safely rolled back: %v", err)
	}
	assertMigrationLocations(t, s, s.opts.MountRoot, before)
	if _, err := os.Lstat(filepath.Join(s.opts.StateDir, rootMigrationFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled back journal was retained: %v", err)
	}
}

func TestMigrateRootCatalogueFailureRestoresEngineAndRetainsRecoveryJournal(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo := catalogueRepository("octocat", "repo")
	seedDesktopCatalogue(t, s, repo)
	addMigrationRepository(t, s, repo, true)
	before, err := s.engine.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.opts.StateDir, "catalogue.json")
	backup := path + ".original"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	err = s.migrateRoot(context.Background(), filepath.Join(t.TempDir(), "new"))
	if !errors.Is(err, errRootMigrationRecoveryNeeded) {
		t.Fatalf("failed catalogue rollback did not require recovery: %v", err)
	}
	assertMigrationLocations(t, s, s.opts.MountRoot, before)
	if _, err := readRootMigration(filepath.Join(s.opts.StateDir, rootMigrationFile)); err != nil {
		t.Fatalf("recovery journal was lost: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverRootMigration(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertMigrationLocations(t, s, s.opts.MountRoot, before)
}

func TestRecoverRootMigrationUsesCatalogueCommitRecord(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "before catalogue commit"
		if committed {
			name = "after catalogue commit"
		}
		t.Run(name, func(t *testing.T) {
			s := newDesktopTestService(t, "exit 4")
			repo := catalogueRepository("octocat", "repo")
			seedDesktopCatalogue(t, s, repo)
			addMigrationRepository(t, s, repo, true)
			before, err := s.engine.ListRepos(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(t.TempDir(), "new")
			journal := migrationJournal(t, s, root)
			if err := writeRootMigration(filepath.Join(s.opts.StateDir, rootMigrationFile), journal); err != nil {
				t.Fatal(err)
			}
			if err := s.applyRootMigration(context.Background(), journal, true); err != nil {
				t.Fatal(err)
			}
			wantRoot := s.opts.MountRoot
			if committed {
				s.mu.Lock()
				s.state.MountRoot = root
				err = s.persistLocked()
				s.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				wantRoot = root
			}
			opts := s.opts
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			assertMigrationLocations(t, restarted, wantRoot, before)
			if _, err := os.Lstat(filepath.Join(opts.StateDir, rootMigrationFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("startup retained completed recovery: %v", err)
			}
		})
	}
}

func TestRecoverRootMigrationPreservesAbsentOriginalWorktree(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo := catalogueRepository("octocat", "repo")
	seedDesktopCatalogue(t, s, repo)
	cfg := addMigrationRepository(t, s, repo, true)
	if err := writeMigrationWorktree(context.Background(), cfg.GitDir, nil); err != nil {
		t.Fatal(err)
	}
	journal := migrationJournal(t, s, filepath.Join(t.TempDir(), "new"))
	if journal.Paths[cfg.Name].OldWorktree != nil {
		t.Fatal("journal invented an original Git worktree value")
	}
	if err := writeRootMigration(filepath.Join(s.opts.StateDir, rootMigrationFile), journal); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the first Git config write, before registry commit.
	entry := journal.Paths[cfg.Name]
	if err := writeMigrationWorktree(context.Background(), cfg.GitDir, &entry.NewPath); err != nil {
		t.Fatal(err)
	}
	opts := s.opts
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	worktree, err := readMigrationWorktree(context.Background(), cfg.GitDir)
	if err != nil || worktree != nil {
		t.Fatalf("recovery did not restore absent core.worktree: %v, %v", worktree, err)
	}
	configs, err := restarted.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 1 || !reflect.DeepEqual(configs[0], cfg) {
		t.Fatalf("recovery changed original registry fields: %+v, %v", configs, err)
	}
}

func TestRootMigrationRecoveryRejectsUnexpectedOrUnsafeJournal(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*rootMigration, model.RepoConfig)
	}{
		{"schema", func(j *rootMigration, _ model.RepoConfig) { j.SchemaVersion = 2 }},
		{"unexpected root", func(j *rootMigration, _ model.RepoConfig) { j.OldRoot = filepath.Join(t.TempDir(), "unexpected") }},
		{"unregistered path", func(j *rootMigration, _ model.RepoConfig) { j.Paths["unknown"] = rootMigrationPath{} }},
		{"unsafe destination", func(j *rootMigration, cfg model.RepoConfig) {
			e := j.Paths[cfg.Name]
			e.NewPath = "/tmp/unowned"
			j.Paths[cfg.Name] = e
		}},
		{"unsafe original worktree", func(j *rootMigration, cfg model.RepoConfig) {
			e := j.Paths[cfg.Name]
			path := "/tmp/other"
			e.OldWorktree = &path
			j.Paths[cfg.Name] = e
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newDesktopTestService(t, "exit 4")
			repo := catalogueRepository("octocat", "repo")
			seedDesktopCatalogue(t, s, repo)
			cfg := addMigrationRepository(t, s, repo, true)
			journal := migrationJournal(t, s, filepath.Join(t.TempDir(), "new"))
			test.mutate(&journal, cfg)
			path := filepath.Join(s.opts.StateDir, rootMigrationFile)
			if err := writeRootMigration(path, journal); err != nil {
				t.Fatal(err)
			}
			if err := s.recoverRootMigration(context.Background()); err == nil {
				t.Fatal("unsafe migration was recovered")
			}
			assertMigrationLocations(t, s, s.opts.MountRoot, []model.RepoConfig{cfg})
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("unsafe recovery discarded evidence")
			}
		})
	}
}

func TestRootMigrationRefusesSymlinkGitDirectory(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo := catalogueRepository("octocat", "repo")
	seedDesktopCatalogue(t, s, repo)
	cfg := addMigrationRepository(t, s, repo, true)
	outside := filepath.Join(t.TempDir(), "git")
	if err := os.Rename(cfg.GitDir, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, cfg.GitDir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(outside, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.migrateRoot(context.Background(), filepath.Join(t.TempDir(), "new")); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink storage accepted: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(outside, "config"))
	if err != nil || string(after) != string(before) {
		t.Fatal("migration modified Git config outside private state")
	}
}
