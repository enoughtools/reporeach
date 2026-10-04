package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
