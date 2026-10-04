package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchCatalogUpdatesPreservesCheckoutAndBackgroundPolicy(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	if err := svc.SetRefresh(context.Background(), cfg.Name, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	before := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD"))
	indexBefore, err := os.ReadFile(filepath.Join(cfg.GitDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	origin := strings.TrimPrefix(cfg.RemoteURL, "file://")
	work := filepath.Join(filepath.Dir(origin), "work")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("remote update\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", work, "add", "README.md")
	runCmd(t, "git", "-C", work, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "remote update")
	runCmd(t, "git", "-C", work, "push", "origin", "main")
	if err := svc.FetchCatalogUpdates(context.Background(), cfg.Name); err != nil {
		t.Fatal(err)
	}
	if after := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD")); after != before {
		t.Fatal("explicit fetch changed HEAD")
	}
	indexAfter, err := os.ReadFile(filepath.Join(cfg.GitDir, "index"))
	if err != nil || string(indexBefore) != string(indexAfter) {
		t.Fatal("explicit fetch changed staged index")
	}
	remote := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "origin/main"))
	if remote == before {
		t.Fatal("explicit refresh did not fetch updated remote refs")
	}
	latest, err := svc.registry.GetRepo(context.Background(), cfg.Name)
	if err != nil || !latest.RemoteRefreshDisabled {
		t.Fatal("explicit fetch enabled background synchronization")
	}
}

func TestFreeRepositorySpaceAcceptsManagedCredentialHelper(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	runCmd(t, "git", "--git-dir", cfg.GitDir, "config", "--local", "credential.https://github.com.helper", "!GH_TELEMETRY=false '/Applications/RepoReach.app/Contents/Helpers/gh' auth git-credential")
	if err := svc.FreeRepositorySpace(context.Background(), cfg.Name); err != nil {
		t.Fatalf("managed helper was mistaken for custom local state: %v", err)
	}
}
