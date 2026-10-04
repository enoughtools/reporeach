package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
)

func storageFixture(t *testing.T) (*Service, model.RepoConfig, []byte) {
	t.Helper()
	fixture := t.TempDir()
	bare, work := filepath.Join(fixture, "origin.git"), filepath.Join(fixture, "work")
	runCmd(t, "git", "init", "--bare", "--initial-branch=main", bare)
	runCmd(t, "git", "clone", bare, work)
	payload := []byte{0, 0xff, '\n', 0, 0x80, 'x'}
	for _, name := range []string{"binary.bin", "duplicate.bin"} {
		if err := os.WriteFile(filepath.Join(work, name), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("binary.bin", filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", work, "add", ".")
	runCmd(t, "git", "-C", work, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "fixture")
	runCmd(t, "git", "-C", work, "push", "origin", "main")
	svc, err := New(context.Background(), filepath.Join(fixture, "engine"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	cfg := model.RepoConfig{Name: "test-repo", RemoteURL: "file://" + bare, Branch: "main", Enabled: true}
	if err := svc.AddRepo(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = svc.registry.GetRepo(context.Background(), cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	return svc, cfg, payload
}

func TestDownloadCurrentTreeBinarySafeAndUnique(t *testing.T) {
	svc, cfg, payload := storageFixture(t)
	var events []DownloadProgress
	got, err := svc.DownloadCurrentTree(context.Background(), cfg.Name, func(p DownloadProgress) { events = append(events, p) })
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := int64(len(payload) + len("fixture\n") + len("binary.bin"))
	if !got.Complete || !got.TotalBytesKnown || got.TotalBlobs != 3 || got.CompletedBlobs != 3 || got.DownloadedBytes != wantBytes || got.TotalBytes != wantBytes {
		t.Fatalf("progress = %+v, want three distinct blobs, %d bytes", got, wantBytes)
	}
	if len(events) != 5 || events[0].Complete || !events[len(events)-1].Complete {
		t.Fatalf("unexpected progress sequence: %+v", events)
	}
	oid := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD:binary.bin"))
	data, err := os.ReadFile(filepath.Join(cfg.BlobCacheDir, oid))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("cached binary = %v, err = %v", data, err)
	}
	// A damaged cache must be repaired rather than being counted as downloaded.
	if err := os.WriteFile(filepath.Join(cfg.BlobCacheDir, oid), []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DownloadCurrentTree(context.Background(), cfg.Name, nil); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(cfg.BlobCacheDir, oid))
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatal("corrupt cache was not repaired")
	}
}

func TestDownloadCurrentTreeCancellation(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := svc.DownloadCurrentTree(ctx, cfg.Name, func(p DownloadProgress) {
		if p.CompletedBlobs == 1 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || got.Complete || got.CompletedBlobs != 1 {
		t.Fatalf("download = %+v, err = %v", got, err)
	}
	if got, err := svc.DownloadCurrentTree(context.Background(), cfg.Name, nil); err != nil || !got.Complete {
		t.Fatalf("resumed download = %+v, err = %v", got, err)
	}
}

func TestDownloadCurrentTreeRejectsChangedHEAD(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	commit := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "local"))
	changed := false
	got, err := svc.DownloadCurrentTree(context.Background(), cfg.Name, func(p DownloadProgress) {
		if p.CompletedBlobs == 1 && !changed {
			changed = true
			runCmd(t, "git", "--git-dir", cfg.GitDir, "update-ref", "HEAD", commit)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "changed during download") || got.Complete {
		t.Fatalf("download = %+v, err = %v", got, err)
	}
}

func TestDownloadCurrentTreeRejectsDuplicateCheckoutFilterBlob(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	attributes := filepath.Join(t.TempDir(), "attributes")
	if err := os.WriteFile(attributes, []byte("*.png filter=lfs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oid := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "hash-object", "-w", attributes))
	for _, path := range []string{"a.txt", "z/.gitattributes"} {
		runCmd(t, "git", "--git-dir", cfg.GitDir, "update-index", "--add", "--cacheinfo", "100644,"+oid+","+path)
	}
	tree := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "write-tree"))
	commit := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", tree, "-p", "HEAD", "-m", "LFS fixture"))
	runCmd(t, "git", "--git-dir", cfg.GitDir, "update-ref", "HEAD", commit)
	got, err := svc.DownloadCurrentTree(context.Background(), cfg.Name, nil)
	if err == nil || !strings.Contains(err.Error(), "checkout filters or LFS") || got.Complete {
		t.Fatalf("download = %+v, err = %v", got, err)
	}
}

func TestFreeRepositorySpaceReclaimsAllOwnedData(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	if _, err := svc.DownloadCurrentTree(context.Background(), cfg.Name, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.FreeRepositorySpace(context.Background(), cfg.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.registry.GetRepo(context.Background(), cfg.Name); err == nil {
		t.Fatal("engine registration still present")
	}
	for _, path := range []string{filepath.Dir(cfg.GitDir), cfg.OverlayDir, cfg.BlobCacheDir, cfg.MetaDBPath} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned data remains at %s: %v", path, err)
		}
	}
}

func TestFreeRepositorySpaceRefusesLocalWork(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *Service, model.RepoConfig)
		want   string
	}{
		{"delete tombstone", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			ov, err := overlay.New(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer ov.Close()
			if err := ov.Remove(context.Background(), "README.md"); err != nil {
				t.Fatal(err)
			}
		}, "local files or deletions"},
		{"ignored file", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			if err := os.MkdirAll(filepath.Join(cfg.OverlayDir, "upper"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.OverlayDir, "upper", "secret.env"), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "untracked or ignored"},
		{"staged change", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			oid := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD:binary.bin"))
			runCmd(t, "git", "--git-dir", cfg.GitDir, "update-index", "--cacheinfo", "100644,"+oid+",README.md")
		}, "staged changes"},
		{"unpushed commit", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			commit := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "local"))
			runCmd(t, "git", "--git-dir", cfg.GitDir, "update-ref", "HEAD", commit)
		}, "unpushed or recovered history"},
		{"reflog-only commit", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			head := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD"))
			commit := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "local"))
			runCmd(t, "git", "--git-dir", cfg.GitDir, "update-ref", "HEAD", commit)
			runCmd(t, "git", "--git-dir", cfg.GitDir, "update-ref", "HEAD", head)
		}, "unpushed or recovered history"},
		{"stash", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			head := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD"))
			runCmd(t, "git", "--git-dir", cfg.GitDir, "update-ref", "refs/stash", head)
		}, "stash"},
		{"untracked storage file", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			if err := os.WriteFile(filepath.Join(filepath.Dir(cfg.GitDir), "notes.txt"), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "untracked local files"},
		{"custom hook", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			if err := os.WriteFile(filepath.Join(cfg.GitDir, "hooks", "pre-commit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "custom Git hooks"},
		{"custom config", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			runCmd(t, "git", "--git-dir", cfg.GitDir, "config", "--local", "user.email", "local-setting@example.com")
		}, "custom local Git configuration"},
		{"custom info metadata", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			if err := os.WriteFile(filepath.Join(cfg.GitDir, "info", "notes.txt"), []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "unrecognized local Git metadata"},
		{"unreachable commit", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			runCmd(t, "git", "--git-dir", cfg.GitDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "unattached local commit")
		}, "unreachable Git objects"},
		{"unreachable blob", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			path := filepath.Join(t.TempDir(), "unreachable")
			if err := os.WriteFile(path, []byte("unstaged local content"), 0o600); err != nil {
				t.Fatal(err)
			}
			runCmd(t, "git", "--git-dir", cfg.GitDir, "hash-object", "-w", path)
		}, "unreachable Git objects"},
		{"missing remote", func(t *testing.T, _ *Service, cfg model.RepoConfig) {
			if err := os.RemoveAll(strings.TrimPrefix(cfg.RemoteURL, "file://")); err != nil {
				t.Fatal(err)
			}
		}, "cannot verify remote backup"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc, cfg, _ := storageFixture(t)
			test.mutate(t, svc, cfg)
			if err := svc.FreeRepositorySpace(context.Background(), cfg.Name); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if _, err := os.Stat(cfg.GitDir); err != nil {
				t.Fatal("Git data was removed after refusal")
			}
			latest, err := svc.registry.GetRepo(context.Background(), cfg.Name)
			if err != nil || !latest.Enabled {
				t.Fatalf("registration after refusal = %+v, err = %v", latest, err)
			}
		})
	}
}

func TestFreeRepositorySpaceRefusesExternalStorage(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "keep"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.GitDir = external
	if err := svc.registry.AddRepo(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := svc.FreeRepositorySpace(context.Background(), cfg.Name); err == nil || !strings.Contains(err.Error(), "engine-owned") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(external, "keep")); err != nil {
		t.Fatal("external data was removed")
	}
}
