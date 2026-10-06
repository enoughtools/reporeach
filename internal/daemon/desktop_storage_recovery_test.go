package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestRecoverStorageTransactionsRollsBackInterruptedEviction(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	if _, err := svc.DownloadCurrentTree(context.Background(), cfg.Name, nil); err != nil {
		t.Fatal(err)
	}
	disabled := cfg
	disabled.Enabled, disabled.ConfigVersion = false, "interrupted-eviction"
	transaction, err := svc.newStorageTransaction(cfg, disabled)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.registry.AddRepo(context.Background(), disabled); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 2} {
		if err := os.Rename(repositoryStoragePaths(cfg)[i], filepath.Join(transaction.dir, fmt.Sprintf("%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.RecoverStorageTransactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	latest, err := svc.registry.GetRepo(context.Background(), cfg.Name)
	if err != nil || !latest.Enabled || latest.ConfigVersion == disabled.ConfigVersion {
		t.Fatalf("recovered config = %+v, err = %v", latest, err)
	}
	for _, path := range []string{cfg.GitDir, cfg.BlobCacheDir} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("data not restored at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(transaction.dir); !os.IsNotExist(err) {
		t.Fatal("completed journal remains")
	}
}

func TestRecoverStorageTransactionsFinishesCommittedEviction(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	disabled := cfg
	disabled.Enabled, disabled.ConfigVersion = false, "committed-eviction"
	transaction, err := svc.newStorageTransaction(cfg, disabled)
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range repositoryStoragePaths(cfg) {
		if transaction.PreserveOverlay && i == storageOverlayPathIndex {
			continue
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if err := os.Rename(path, filepath.Join(transaction.dir, fmt.Sprintf("%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.registry.RemoveRepo(context.Background(), cfg.Name); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverStorageTransactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transaction.dir); !os.IsNotExist(err) {
		t.Fatal("committed eviction was not cleaned up")
	}
	if _, err := os.Stat(cfg.GitDir); !os.IsNotExist(err) {
		t.Fatal("committed eviction unexpectedly restored the repository")
	}
	if _, err := os.Stat(cfg.OverlayDBPath); err != nil {
		t.Fatalf("committed eviction removed retained metadata: %v", err)
	}
}

func TestRecoverStorageTransactionsPreservesCollision(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	disabled := cfg
	disabled.Enabled, disabled.ConfigVersion = false, "interrupted-eviction"
	transaction, err := svc.newStorageTransaction(cfg, disabled)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.registry.AddRepo(context.Background(), disabled); err != nil {
		t.Fatal(err)
	}
	target := filepath.Dir(cfg.GitDir)
	if err := os.Rename(target, filepath.Join(transaction.dir, "0")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "new-file"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverStorageTransactions(context.Background()); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("recovery error = %v", err)
	}
	for _, path := range []string{filepath.Join(target, "new-file"), filepath.Join(transaction.dir, "0", "git", "HEAD")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("collision data lost at %s: %v", path, err)
		}
	}
}

func writeStorageFixtureManifest(t *testing.T, transaction *storageTransaction) {
	t.Helper()
	data, err := json.Marshal(transaction)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transaction.dir, "transaction.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func moveStorageFixturePaths(t *testing.T, transaction *storageTransaction) {
	t.Helper()
	for i, path := range repositoryStoragePaths(transaction.Original) {
		if transaction.PreserveOverlay && i == storageOverlayPathIndex {
			continue
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, filepath.Join(transaction.dir, fmt.Sprintf("%d", i))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecoverStorageTransactionsPreservesMetadata(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			ctx := context.Background()
			svc, cfg, _ := storageFixture(t)
			value := []byte{0, 0xff, '\n', 0x80, 0}
			id := setStorageFixtureMetadata(t, cfg, value)
			disabled := cfg
			disabled.Enabled, disabled.ConfigVersion = false, "metadata-eviction"
			transaction, err := svc.newStorageTransaction(cfg, disabled)
			if err != nil {
				t.Fatal(err)
			}
			if !transaction.PreserveOverlay || transaction.Version != 2 || transaction.OverlayIdentity == nil {
				t.Fatalf("metadata preservation was not journalled: %+v", transaction)
			}
			if err := svc.registry.AddRepo(ctx, disabled); err != nil {
				t.Fatal(err)
			}
			moveStorageFixturePaths(t, transaction)
			if committed {
				if err := svc.registry.RemoveRepo(ctx, cfg.Name); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := svc.RecoverStorageTransactions(ctx); err != nil {
					t.Fatal(err)
				}
				assertStorageFixtureMetadata(t, cfg, id, value)
			}
			if _, err := os.Stat(transaction.dir); !os.IsNotExist(err) {
				t.Fatal("completed journal remains")
			}
			if committed {
				if err := svc.AddRepo(ctx, cfg); err != nil {
					t.Fatal(err)
				}
			}
			assertStorageFixtureMetadata(t, cfg, id, value)
		})
	}
}

func TestRecoverStorageTransactionsLegacyOverlayMoves(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			ctx := context.Background()
			svc, cfg, _ := storageFixture(t)
			value := []byte{0, 0xfe, 0x80}
			id := setStorageFixtureMetadata(t, cfg, value)
			disabled := cfg
			disabled.Enabled, disabled.ConfigVersion = false, "legacy-eviction"
			transaction, err := svc.newStorageTransaction(cfg, disabled)
			if err != nil {
				t.Fatal(err)
			}
			transaction.Version, transaction.PreserveOverlay, transaction.OverlayIdentity = 1, false, nil
			writeStorageFixtureManifest(t, transaction)
			if err := svc.registry.AddRepo(ctx, disabled); err != nil {
				t.Fatal(err)
			}
			moveStorageFixturePaths(t, transaction)
			if _, err := os.Stat(filepath.Join(transaction.dir, "1", "meta.sqlite")); err != nil {
				t.Fatalf("legacy overlay not moved: %v", err)
			}
			if committed {
				if err := svc.registry.RemoveRepo(ctx, cfg.Name); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.RecoverStorageTransactions(ctx); err != nil {
				t.Fatal(err)
			}
			if committed {
				if _, err := os.Stat(cfg.OverlayDir); !os.IsNotExist(err) {
					t.Fatal("legacy committed eviction unexpectedly preserved overlay")
				}
			} else {
				assertStorageFixtureMetadata(t, cfg, id, value)
			}
			if _, err := os.Stat(transaction.dir); !os.IsNotExist(err) {
				t.Fatal("legacy completed journal remains")
			}
		})
	}
}

func TestRecoverStorageTransactionsRejectsMovedRetainedOverlay(t *testing.T) {
	ctx := context.Background()
	svc, cfg, _ := storageFixture(t)
	value := []byte{0, 0xfe, 0xff}
	id := setStorageFixtureMetadata(t, cfg, value)
	disabled := cfg
	disabled.ConfigVersion = "invalid-overlay-move"
	transaction, err := svc.newStorageTransaction(cfg, disabled)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cfg.OverlayDir, filepath.Join(transaction.dir, "1")); err != nil {
		t.Fatal(err)
	}
	if err := svc.registry.RemoveRepo(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverStorageTransactions(ctx); err == nil || !strings.Contains(err.Error(), "moved a retained overlay") {
		t.Fatalf("recovery accepted invalid committed journal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(transaction.dir, "1", "meta.sqlite")); err != nil {
		t.Fatalf("invalid journal destroyed retained metadata: %v", err)
	}
	if err := os.Rename(filepath.Join(transaction.dir, "1"), cfg.OverlayDir); err != nil {
		t.Fatal(err)
	}
	assertStorageFixtureMetadata(t, cfg, id, value)
}

func TestRecoverStorageTransactionsRejectsChangedRetainedStorage(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *Service, model.RepoConfig, *storageTransaction)
		want   string
	}{
		{"missing directory", func(t *testing.T, _ *Service, cfg model.RepoConfig, _ *storageTransaction) {
			if err := os.Rename(cfg.OverlayDir, cfg.OverlayDir+".saved"); err != nil {
				t.Fatal(err)
			}
		}, "verify retained overlay"},
		{"replacement directory", func(t *testing.T, _ *Service, cfg model.RepoConfig, _ *storageTransaction) {
			if err := os.Rename(cfg.OverlayDir, cfg.OverlayDir+".saved"); err != nil {
				t.Fatal(err)
			}
			setStorageFixtureMetadata(t, cfg, []byte("replacement"))
		}, "directory was replaced"},
		{"directory symbolic link", func(t *testing.T, _ *Service, cfg model.RepoConfig, _ *storageTransaction) {
			if err := os.Rename(cfg.OverlayDir, cfg.OverlayDir+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(cfg.OverlayDir+".saved", cfg.OverlayDir); err != nil {
				t.Fatal(err)
			}
		}, "not a real directory"},
		{"missing database", func(t *testing.T, _ *Service, cfg model.RepoConfig, _ *storageTransaction) {
			if err := os.Rename(cfg.OverlayDBPath, cfg.OverlayDBPath+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(cfg.OverlayDBPath+".saved", filepath.Join(t.TempDir(), "meta.saved")); err != nil {
				t.Fatal(err)
			}
		}, "metadata is missing"},
		{"database symbolic link", func(t *testing.T, _ *Service, cfg model.RepoConfig, _ *storageTransaction) {
			retained := filepath.Join(t.TempDir(), "meta.saved")
			if err := os.Rename(cfg.OverlayDBPath, retained); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(retained, cfg.OverlayDBPath); err != nil {
				t.Fatal(err)
			}
		}, "invalid metadata file"},
		{"database replaced by directory", func(t *testing.T, _ *Service, cfg model.RepoConfig, _ *storageTransaction) {
			if err := os.Rename(cfg.OverlayDBPath, filepath.Join(t.TempDir(), "meta.saved")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(cfg.OverlayDBPath, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "invalid metadata file"},
		{"same version changed paths", func(t *testing.T, svc *Service, cfg model.RepoConfig, transaction *storageTransaction) {
			cfg.ConfigVersion, cfg.Enabled = transaction.DisabledVersion, false
			cfg.OverlayDir, cfg.OverlayDBPath = t.TempDir(), filepath.Join(t.TempDir(), "different.sqlite")
			if err := svc.registry.AddRepo(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
		}, "changed repository storage"},
		{"changed manifest paths", func(t *testing.T, _ *Service, _ model.RepoConfig, transaction *storageTransaction) {
			transaction.Original.OverlayDBPath = filepath.Join(t.TempDir(), "different.sqlite")
			writeStorageFixtureManifest(t, transaction)
		}, "unexpected paths"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			svc, cfg, _ := storageFixture(t)
			setStorageFixtureMetadata(t, cfg, []byte("retained"))
			disabled := cfg
			disabled.Enabled, disabled.ConfigVersion = false, "changed-storage"
			transaction, err := svc.newStorageTransaction(cfg, disabled)
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.registry.AddRepo(ctx, disabled); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Dir(cfg.GitDir), filepath.Join(transaction.dir, "0")); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, svc, cfg, transaction)
			if err := svc.RecoverStorageTransactions(ctx); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("recovery error = %v, want %q", err, test.want)
			}
			if _, err := os.Stat(filepath.Join(transaction.dir, "0", "git", "HEAD")); err != nil {
				t.Fatalf("recovery changed journal before validating metadata: %v", err)
			}
		})
	}
}

func TestRecoverStorageTransactionsPreservesRefreshChanges(t *testing.T) {
	ctx := context.Background()
	svc, cfg, _ := storageFixture(t)
	disabled := cfg
	disabled.Enabled, disabled.ConfigVersion = false, "refresh-during-eviction"
	transaction, err := svc.newStorageTransaction(cfg, disabled)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.registry.AddRepo(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetRefresh(ctx, cfg.Name, 13*time.Minute, true); err != nil {
		t.Fatal(err)
	}
	moveStorageFixturePaths(t, transaction)
	if err := svc.RecoverStorageTransactions(ctx); err != nil {
		t.Fatal(err)
	}
	latest, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil || !latest.Enabled || latest.RefreshInterval != 13*time.Minute || !latest.RemoteRefreshDisabled {
		t.Fatalf("recovery discarded refresh settings: %+v, err = %v", latest, err)
	}
}

func TestRestoreMovedStoragePathsPreservesMetadataAndCollisions(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(fmt.Sprintf("collision=%t", collision), func(t *testing.T) {
			ctx := context.Background()
			svc, cfg, _ := storageFixture(t)
			if _, err := svc.DownloadCurrentTree(ctx, cfg.Name, nil); err != nil {
				t.Fatal(err)
			}
			value := []byte{0, 0xff, 0x80, 0}
			id := setStorageFixtureMetadata(t, cfg, value)
			disabled := cfg
			disabled.Enabled, disabled.ConfigVersion = false, "live-rollback"
			transaction, err := svc.newStorageTransaction(cfg, disabled)
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.registry.AddRepo(ctx, disabled); err != nil {
				t.Fatal(err)
			}
			paths := repositoryStoragePaths(cfg)
			var moved []movedStoragePath
			for _, index := range []int{0, 2} {
				target := filepath.Join(transaction.dir, fmt.Sprintf("%d", index))
				if err := os.Rename(paths[index], target); err != nil {
					t.Fatal(err)
				}
				moved = append(moved, movedStoragePath{paths[index], target})
			}
			var collisionIdentity storageDirectoryIdentity
			if collision {
				// Even an empty replacement directory has its own identity and
				// must not be silently replaced by the rollback rename.
				if err := os.Mkdir(paths[0], 0o700); err != nil {
					t.Fatal(err)
				}
				collisionIdentity, err = readStorageDirectoryIdentity(paths[0])
				if err != nil {
					t.Fatal(err)
				}
			}
			err = restoreMovedStoragePaths(*transaction, moved)
			if collision {
				if err == nil || !strings.Contains(err.Error(), "rollback collision") {
					t.Fatalf("rollback accepted replacement directory: %v", err)
				}
				if identity, err := readStorageDirectoryIdentity(paths[0]); err != nil || identity != collisionIdentity {
					t.Fatalf("rollback replaced collision: %+v, %v", identity, err)
				}
				if _, err := os.Stat(filepath.Join(transaction.dir, "0", "git", "HEAD")); err != nil {
					t.Fatalf("rollback lost journalled Git data: %v", err)
				}
				if err := os.Remove(paths[0]); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			assertStorageFixtureMetadata(t, cfg, id, value)
			if _, err := os.Stat(cfg.BlobCacheDir); err != nil {
				t.Fatalf("rollback did not restore unaffected cache: %v", err)
			}
			if err := svc.RecoverStorageTransactions(ctx); err != nil {
				t.Fatalf("restart could not finish rollback: %v", err)
			}
			assertStorageFixtureMetadata(t, cfg, id, value)
		})
	}
}

func TestRecoverStorageTransactionsCleansCompletedRollbackBeforeNewRegistration(t *testing.T) {
	ctx := context.Background()
	svc, cfg, _ := storageFixture(t)
	value := []byte{0, 0xfe, 0xff}
	id := setStorageFixtureMetadata(t, cfg, value)
	disabled := cfg
	disabled.Enabled, disabled.ConfigVersion = false, "completed-rollback"
	transaction, err := svc.newStorageTransaction(cfg, disabled)
	if err != nil {
		t.Fatal(err)
	}
	// A completed rollback can leave only its manifest after cleanup fails.
	// A later registration may legitimately select other storage paths.
	latest := cfg
	latest.ConfigVersion = "new-registration"
	latest.GitDir = filepath.Join(t.TempDir(), "git")
	if err := svc.registry.AddRepo(ctx, latest); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverStorageTransactions(ctx); err != nil {
		t.Fatalf("completed journal blocked newer registration: %v", err)
	}
	got, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil || got.GitDir != latest.GitDir || got.ConfigVersion != latest.ConfigVersion {
		t.Fatalf("completed journal changed newer registration: %+v, %v", got, err)
	}
	if _, err := os.Stat(transaction.dir); !os.IsNotExist(err) {
		t.Fatal("completed rollback journal remains")
	}
	assertStorageFixtureMetadata(t, cfg, id, value)
}

func TestRecoverStorageTransactionsAcceptsMetadataDatabaseReplacement(t *testing.T) {
	ctx := context.Background()
	svc, cfg, _ := storageFixture(t)
	value := []byte{0, 0xff, 0x80}
	id := setStorageFixtureMetadata(t, cfg, value)
	disabled := cfg
	disabled.Enabled, disabled.ConfigVersion = false, "checkpointed-overlay"
	transaction, err := svc.newStorageTransaction(cfg, disabled)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.registry.AddRepo(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	// Metadata files may change identity during maintenance. Only the retained
	// directory, whose lifecycle belongs to the engine, has a stable identity.
	data, err := os.ReadFile(cfg.OverlayDBPath)
	if err != nil {
		t.Fatal(err)
	}
	replacement := cfg.OverlayDBPath + ".replacement"
	if err := os.WriteFile(replacement, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, cfg.OverlayDBPath); err != nil {
		t.Fatal(err)
	}
	moveStorageFixturePaths(t, transaction)
	if err := svc.RecoverStorageTransactions(ctx); err != nil {
		t.Fatal(err)
	}
	assertStorageFixtureMetadata(t, cfg, id, value)
}

func TestRecoverStorageTransactionsRejectsUnknownManifest(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*storageTransaction) []byte
	}{
		{"future version", func(transaction *storageTransaction) []byte {
			transaction.Version = 3
			data, _ := json.Marshal(transaction)
			return data
		}},
		{"legacy preserve flag", func(transaction *storageTransaction) []byte {
			transaction.Version = 1
			data, _ := json.Marshal(transaction)
			return data
		}},
		{"missing preserve flag", func(transaction *storageTransaction) []byte {
			transaction.PreserveOverlay = false
			data, _ := json.Marshal(transaction)
			return data
		}},
		{"missing identity", func(transaction *storageTransaction) []byte {
			transaction.OverlayIdentity = nil
			data, _ := json.Marshal(transaction)
			return data
		}},
		{"unknown field", func(transaction *storageTransaction) []byte {
			data, _ := json.Marshal(transaction)
			return bytes.Replace(data, []byte(`"version":2`), []byte(`"version":2,"futureBehavior":true`), 1)
		}},
		{"trailing document", func(transaction *storageTransaction) []byte {
			data, _ := json.Marshal(transaction)
			return append(data, []byte(` {"version":1}`)...)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			svc, cfg, _ := storageFixture(t)
			value := []byte{0, 0xff, 0x80}
			id := setStorageFixtureMetadata(t, cfg, value)
			disabled := cfg
			disabled.ConfigVersion = "unknown-manifest"
			transaction, err := svc.newStorageTransaction(cfg, disabled)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(transaction.dir, "transaction.json"), test.edit(transaction), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := svc.registry.RemoveRepo(ctx, cfg.Name); err != nil {
				t.Fatal(err)
			}
			if err := svc.RecoverStorageTransactions(ctx); err == nil {
				t.Fatal("invalid committed journal was cleaned up")
			}
			if _, err := os.Stat(transaction.dir); err != nil {
				t.Fatalf("invalid journal was not retained: %v", err)
			}
			assertStorageFixtureMetadata(t, cfg, id, value)
		})
	}
}
