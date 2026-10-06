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

// A preview cache and the writable engine use different Git object stores.
// Keeping already verified bytes must work offline even if the engine clone
// itself still lacks every selected blob.
func TestDownloadCurrentTreeUsesCachedBlobsOfflineWithoutFetching(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	origin := strings.TrimPrefix(cfg.RemoteURL, "file://")
	work := filepath.Join(filepath.Dir(origin), "work")
	if err := os.WriteFile(filepath.Join(work, ".gitattributes"), []byte("*.bin -text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", work, "add", ".gitattributes")
	runCmd(t, "git", "-C", work, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "attributes")
	runCmd(t, "git", "-C", work, "push", "origin", "main")
	if err := svc.git.FetchRefNonInteractive(context.Background(), cfg, "main"); err != nil {
		t.Fatal(err)
	}
	if err := svc.git.PrepareFetchedBranch(context.Background(), cfg, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DownloadCurrentTree(context.Background(), cfg.Name, nil); err != nil {
		t.Fatal(err)
	}
	replaceStorageFixtureWithBloblessClone(t, svc, cfg, origin)
	if err := os.Rename(origin, origin+".offline"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.DownloadCurrentTree(context.Background(), cfg.Name, nil)
	if err != nil || !got.Complete || got.CompletedBlobs != 4 {
		t.Fatalf("offline cached download = %+v, %v", got, err)
	}
}

func TestDownloadCurrentTreeBulkFetchFailureDoesNotCompleteProgress(t *testing.T) {
	svc, cfg, _ := storageFixture(t)
	origin := strings.TrimPrefix(cfg.RemoteURL, "file://")
	replaceStorageFixtureWithBloblessClone(t, svc, cfg, origin)
	if err := os.Rename(origin, origin+".offline"); err != nil {
		t.Fatal(err)
	}
	var events []DownloadProgress
	got, err := svc.DownloadCurrentTree(context.Background(), cfg.Name, func(p DownloadProgress) { events = append(events, p) })
	if err == nil || got.Complete || got.CompletedBlobs != 0 || len(events) != 1 || events[0].Complete {
		t.Fatalf("failed bulk download = %+v, events=%+v, err=%v", got, events, err)
	}
	if err := os.Rename(origin+".offline", origin); err != nil {
		t.Fatal(err)
	}
	got, err = svc.DownloadCurrentTree(context.Background(), cfg.Name, nil)
	if err != nil || !got.Complete || got.CompletedBlobs != 3 {
		t.Fatalf("resumed bulk download = %+v, %v", got, err)
	}
}

func replaceStorageFixtureWithBloblessClone(t *testing.T, svc *Service, cfg model.RepoConfig, origin string) {
	t.Helper()
	svc.git.CloseRepository(cfg.GitDir)
	if err := os.RemoveAll(cfg.GitDir); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "--git-dir", origin, "config", "uploadpack.allowFilter", "true")
	runCmd(t, "git", "--git-dir", origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	if err := svc.git.CloneBlobless(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	head, _, err := svc.git.ResolveHEAD(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := svc.git.BuildTreeIndex(context.Background(), cfg, head)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if node.Type == "file" && node.SizeState == "known" {
			t.Fatal("fixture clone eagerly acquired a selected blob")
		}
	}
}

func TestFreeRepositorySpaceReclaimsRecoverableData(t *testing.T) {
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
	for _, path := range []string{filepath.Dir(cfg.GitDir), cfg.BlobCacheDir, cfg.MetaDBPath} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned data remains at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(cfg.OverlayDBPath); err != nil {
		t.Fatalf("local metadata database was not retained: %v", err)
	}
	if err := verifyEmptyOverlayUpper(cfg.OverlayDir); err != nil {
		t.Fatal(err)
	}
}

func setStorageFixtureMetadata(t *testing.T, cfg model.RepoConfig, value []byte) model.MetadataObjectID {
	t.Helper()
	ctx := context.Background()
	store, err := overlay.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	object, err := store.BindMetadata(ctx, "README.md", "file")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataXattr(ctx, object.ID, "com.apple.provenance", value, model.XattrAlwaysSet); err != nil {
		t.Fatal(err)
	}
	if entries, err := store.ListByPrefix(ctx, "."); err != nil || len(entries) != 0 {
		t.Fatalf("metadata dirtied file overlay: entries = %+v, err = %v", entries, err)
	}
	return object.ID
}

func assertStorageFixtureMetadata(t *testing.T, cfg model.RepoConfig, id model.MetadataObjectID, value []byte) {
	t.Helper()
	ctx := context.Background()
	store, err := overlay.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	object, err := store.BindMetadata(ctx, "README.md", "file")
	if err != nil || object.ID != id {
		t.Fatalf("retained metadata identity = %+v, err = %v, want %s", object, err, id)
	}
	got, found, err := store.GetMetadataXattr(ctx, id, "com.apple.provenance")
	if err != nil || !found || !bytes.Equal(got, value) {
		t.Fatalf("retained attribute = %v, found = %v, err = %v, want %v", got, found, err, value)
	}
	if entries, err := store.ListByPrefix(ctx, "."); err != nil || len(entries) != 0 {
		t.Fatalf("metadata became file overlay: entries = %+v, err = %v", entries, err)
	}
}

func TestFreeRepositorySpacePreservesMetadataAcrossReacquisition(t *testing.T) {
	ctx := context.Background()
	svc, cfg, _ := storageFixture(t)
	value := []byte{0, 0xff, 0x80, 1, 2, 3, 4, 5, 6, 7, 0}
	id := setStorageFixtureMetadata(t, cfg, value)
	before, err := readStorageDirectoryIdentity(cfg.OverlayDir)
	if err != nil {
		t.Fatal(err)
	}
	for cycle := range 2 {
		if _, err := svc.DownloadCurrentTree(ctx, cfg.Name, nil); err != nil {
			t.Fatal(err)
		}
		if err := svc.FreeRepositorySpace(ctx, cfg.Name); err != nil {
			t.Fatalf("release %d: %v", cycle, err)
		}
		assertStorageFixtureMetadata(t, cfg, id, value)
		if after, err := readStorageDirectoryIdentity(cfg.OverlayDir); err != nil || after != before {
			t.Fatalf("release changed retained directory: %+v, err = %v, want %+v", after, err, before)
		}
		if err := svc.AddRepo(ctx, cfg); err != nil {
			t.Fatalf("reacquire %d: %v", cycle, err)
		}
		assertStorageFixtureMetadata(t, cfg, id, value)
	}
	if _, err := os.Stat(filepath.Join(strings.TrimPrefix(cfg.RemoteURL, "file://"), "HEAD")); err != nil {
		t.Fatalf("release affected original source: %v", err)
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
