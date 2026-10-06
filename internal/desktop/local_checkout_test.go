package desktop

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestInspectLocalCheckoutPreservesExistingNativeState(t *testing.T) {
	path, _ := adoptionSource(t)
	path, _ = filepath.EvalSymlinks(path)
	writeCheckoutFixture(t, filepath.Join(path, "tracked.txt"), []byte("staged\n"), 0600)
	adoptionGit(t, path, "add", "tracked.txt")
	writeCheckoutFixture(t, filepath.Join(path, "tracked.txt"), []byte("unstaged\n"), 0600)
	writeCheckoutFixture(t, filepath.Join(path, "untracked.txt"), []byte("untracked\n"), 0600)
	before, err := checkoutTreeManifest(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "poison-index"))
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "poison-git"))
	checkout, err := InspectLocalCheckout(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if checkout.Path != path || checkout.Branch != "trunk" || checkout.GitDir != filepath.Join(path, ".git") || checkout.CommonDir != checkout.GitDir || !validAdoptionOID(checkout.HeadOID) {
		t.Fatalf("unexpected checkout: %+v", checkout)
	}
	after, err := checkoutTreeManifest(context.Background(), path, false)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("native checkout changed during inspection: %v", err)
	}
}

func TestInspectLocalCheckoutSupportsDetachedAndLinkedWorktrees(t *testing.T) {
	path, _ := adoptionSource(t)
	path, _ = filepath.EvalSymlinks(path)
	adoptionGit(t, path, "checkout", "--detach")
	checkout, err := InspectLocalCheckout(context.Background(), path)
	if err != nil || checkout.Branch != "" {
		t.Fatalf("detached HEAD: %+v, %v", checkout, err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	adoptionGit(t, path, "worktree", "add", "--detach", linked)
	linked, _ = filepath.EvalSymlinks(linked)
	checkout, err = InspectLocalCheckout(context.Background(), linked)
	if err != nil || checkout.Path != linked || checkout.GitDir == checkout.CommonDir {
		t.Fatalf("linked checkout: %+v, %v", checkout, err)
	}
	if _, err := InspectLocalCheckout(context.Background(), filepath.Join(path, ".git")); err == nil {
		t.Fatal("Git metadata was accepted as a working tree")
	}
}

func TestStageLocalCheckoutPreservesGitAndMergedWorkingState(t *testing.T) {
	path, _ := adoptionSource(t)
	writeCheckoutFixture(t, filepath.Join(path, "tracked.txt"), []byte("staged\n"), 0600)
	adoptionGit(t, path, "add", "tracked.txt")
	unstaged := []byte{0, 255, 128, 'u', 0, 254}
	writeCheckoutFixture(t, filepath.Join(path, "tracked.txt"), unstaged, 0600)
	if err := os.Remove(filepath.Join(path, "binary.dat")); err != nil {
		t.Fatal(err)
	}
	writeCheckoutFixture(t, filepath.Join(path, "untracked.sh"), []byte("#!/bin/sh\nexit 0\n"), 0751)
	writeCheckoutFixture(t, filepath.Join(path, "ignored.dat"), []byte{0, 255, 0}, 0600)
	if err := os.Symlink("../external-target", filepath.Join(path, "untracked-link")); err != nil {
		t.Fatal(err)
	}
	attrs := map[string][]byte{"user.reporeach.fixture": {0, 255, 127, 0}}
	if err := checkoutWriteXattrs(filepath.Join(path, "tracked.txt"), attrs); err != nil {
		t.Fatal(err)
	}
	adoptionGit(t, path, "config", "core.fsmonitor", filepath.Join(path, ".git", "hooks", "artifact-fs-fsmonitor"))
	before, err := checkoutTreeManifest(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "kept")
	parent, _ := filepath.EvalSymlinks(filepath.Dir(destination))
	destination = filepath.Join(parent, "kept")
	stage, err := StageLocalCheckout(context.Background(), model.RepoConfig{GitDir: filepath.Join(path, ".git")}, path, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging published the checkout prematurely")
	}
	if err := stage.VerifySource(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stage.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stage.VerifyPublished(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"HEAD", "index"} {
		original, _ := os.ReadFile(filepath.Join(path, ".git", filename))
		kept, _ := os.ReadFile(filepath.Join(destination, ".git", filename))
		if !bytes.Equal(original, kept) {
			t.Fatalf("%s changed", filename)
		}
	}
	if content, err := os.ReadFile(filepath.Join(destination, "tracked.txt")); err != nil || !bytes.Equal(content, unstaged) {
		t.Fatalf("binary unstaged file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "binary.dat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted working file was restored")
	}
	monitor := strings.TrimSpace(string(adoptionGit(t, destination, "config", "--local", "--get", "core.fsmonitor")))
	if monitor != "false" {
		t.Fatalf("virtual fsmonitor retained: %q", monitor)
	}
	checkout, err := InspectLocalCheckout(context.Background(), destination)
	if err != nil || checkout.Path != destination || checkout.Branch != "trunk" {
		t.Fatalf("ordinary checkout: %+v %v", checkout, err)
	}
	after, err := checkoutTreeManifest(context.Background(), path, false)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("source changed during materialization: %v", err)
	}
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatal("closing a published stage removed the local checkout")
	}
}

func TestStageLocalCheckoutNeverReplacesExistingDestination(t *testing.T) {
	path, _ := adoptionSource(t)
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "kept")
			if kind == "directory" {
				if err := os.Mkdir(destination, 0700); err != nil {
					t.Fatal(err)
				}
				writeCheckoutFixture(t, filepath.Join(destination, "sentinel"), []byte("preserve"), 0600)
			} else if err := os.Symlink(path, destination); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Lstat(destination)
			stage, err := StageLocalCheckout(context.Background(), model.RepoConfig{GitDir: filepath.Join(path, ".git")}, path, destination)
			if err != nil {
				t.Fatal(err)
			}
			defer stage.Close()
			if err := stage.Publish(context.Background()); err == nil {
				t.Fatal("existing destination was replaced")
			}
			after, _ := os.Lstat(destination)
			if !os.SameFile(before, after) {
				t.Fatal("existing destination changed")
			}
		})
	}
}

func TestStageLocalCheckoutDetectsSourceAndPublishedEdits(t *testing.T) {
	path, _ := adoptionSource(t)
	destination := filepath.Join(t.TempDir(), "kept")
	stage, err := StageLocalCheckout(context.Background(), model.RepoConfig{GitDir: filepath.Join(path, ".git")}, path, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	writeCheckoutFixture(t, filepath.Join(path, "tracked.txt"), []byte("external editor\n"), 0600)
	if err := stage.VerifySource(context.Background()); err == nil {
		t.Fatal("external source edit was not detected")
	}
	if err := stage.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeCheckoutFixture(t, filepath.Join(destination, "tracked.txt"), []byte("external kept edit\n"), 0600)
	if err := stage.VerifyPublished(context.Background()); err == nil {
		t.Fatal("external published edit was not detected")
	}
}

func TestStageLocalCheckoutRefusesExternalGitMetadataAndLocks(t *testing.T) {
	for _, relative := range []string{"commondir", "objects/info/alternates", "config.worktree", "index.lock"} {
		t.Run(relative, func(t *testing.T) {
			path, _ := adoptionSource(t)
			writeCheckoutFixture(t, filepath.Join(path, ".git", relative), []byte("outside\n"), 0600)
			if _, err := StageLocalCheckout(context.Background(), model.RepoConfig{GitDir: filepath.Join(path, ".git")}, path, filepath.Join(t.TempDir(), "kept")); err == nil {
				t.Fatal("external/shared/busy Git metadata was accepted")
			}
		})
	}
	path, _ := adoptionSource(t)
	adoptionGit(t, path, "config", "include.path", filepath.Join(t.TempDir(), "outside-config"))
	if _, err := StageLocalCheckout(context.Background(), model.RepoConfig{GitDir: filepath.Join(path, ".git")}, path, filepath.Join(t.TempDir(), "kept")); err == nil {
		t.Fatal("external configuration was accepted")
	}
}

func TestVerifyLocalCheckoutSafeToFreeRejectsEveryUnpublishedWorkingFile(t *testing.T) {
	for _, kind := range []string{"staged", "unstaged", "untracked", "ignored"} {
		t.Run(kind, func(t *testing.T) {
			path, _ := adoptionSource(t)
			filename := "tracked.txt"
			if kind == "untracked" {
				filename = "untracked.txt"
			} else if kind == "ignored" {
				filename = "ignored.dat"
			}
			writeCheckoutFixture(t, filepath.Join(path, filename), []byte("local work\n"), 0600)
			if kind == "staged" {
				adoptionGit(t, path, "add", filename)
			}
			before, err := checkoutTreeManifest(context.Background(), path, false)
			if err != nil {
				t.Fatal(err)
			}
			err = VerifyLocalCheckoutSafeToFree(context.Background(), path, "https://example.invalid/org/repo.git")
			if err == nil || !strings.Contains(err.Error(), "working") && !strings.Contains(err.Error(), "untracked") {
				t.Fatalf("local work was not rejected: %v", err)
			}
			after, err := checkoutTreeManifest(context.Background(), path, false)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("free preflight changed source: %v", err)
			}
		})
	}
}

func TestVerifyLocalCheckoutSafeToFreeChecksFreshRemoteWithoutMutatingSource(t *testing.T) {
	for _, kind := range []string{"published", "unpushed", "unreachable", "empty-folder", "extended-metadata", "offline", "self-remote"} {
		t.Run(kind, func(t *testing.T) {
			path, _ := adoptionSource(t)
			remote := filepath.Join(t.TempDir(), "remote.git")
			adoptionGit(t, path, "clone", "--bare", path, remote)
			adoptionGit(t, path, "remote", "add", "origin", remote)
			adoptionGit(t, path, "config", "core.worktree", path)
			if kind == "unpushed" {
				writeCheckoutFixture(t, filepath.Join(path, "tracked.txt"), []byte("committed local work\n"), 0600)
				adoptionGit(t, path, "add", "tracked.txt")
				adoptionGit(t, path, "commit", "-m", "unpublished")
			} else if kind == "unreachable" {
				writeCheckoutFixture(t, filepath.Join(path, "tracked.txt"), []byte("staged then reset\n"), 0600)
				adoptionGit(t, path, "add", "tracked.txt")
				adoptionGit(t, path, "reset", "--hard")
			} else if kind == "empty-folder" {
				if err := os.Mkdir(filepath.Join(path, "local-empty-folder"), 0700); err != nil {
					t.Fatal(err)
				}
			} else if kind == "extended-metadata" {
				if err := checkoutWriteXattrs(filepath.Join(path, "tracked.txt"), map[string][]byte{"user.reporeach.fixture": []byte("local metadata")}); err != nil {
					t.Fatal(err)
				}
			} else if kind == "offline" {
				remote = filepath.Join(t.TempDir(), "missing.git")
			} else if kind == "self-remote" {
				remote = path
			}
			// User-specific local settings cannot be reconstructed from Git's
			// remote and are correctly refused by the existing discard policy.
			adoptionGit(t, path, "config", "--unset", "user.name")
			adoptionGit(t, path, "config", "--unset", "user.email")
			if err := os.Remove(filepath.Join(path, ".git", "COMMIT_EDITMSG")); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			before, err := checkoutTreeManifest(context.Background(), path, false)
			if err != nil {
				t.Fatal(err)
			}
			err = VerifyLocalCheckoutSafeToFree(context.Background(), path, remote)
			if kind == "published" && err != nil {
				t.Fatalf("published clean checkout refused: %v", err)
			}
			if kind != "published" && err == nil {
				t.Fatalf("%s checkout was allowed to free", kind)
			}
			after, manifestErr := checkoutTreeManifest(context.Background(), path, false)
			if manifestErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("remote verification changed native source: %v", manifestErr)
			}
		})
	}
}

func TestVerifyLocalCheckoutSafeToFreeRejectsIndexFlagsThatHideDirtyBytes(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			path, _ := adoptionSource(t)
			adoptionGit(t, path, "update-index", flag, "tracked.txt")
			writeCheckoutFixture(t, filepath.Join(path, "tracked.txt"), []byte{0, 255, 128, 'l', 0, 254}, 0600)
			status, err := localCheckoutGit(context.Background(), path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching")
			if err != nil || status != "" {
				t.Fatalf("fixture does not reproduce dirty bytes hidden from status: %q, %v", status, err)
			}
			before, err := checkoutTreeManifest(context.Background(), path, false)
			if err != nil {
				t.Fatal(err)
			}
			err = VerifyLocalCheckoutSafeToFree(context.Background(), path, "https://example.invalid/org/repo.git")
			if err == nil || !strings.Contains(err.Error(), "index flags") {
				t.Fatalf("hidden dirty bytes were not refused before remote verification: %v", err)
			}
			after, err := checkoutTreeManifest(context.Background(), path, false)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("flag refusal changed native working bytes or index: %v", err)
			}
		})
	}
}

func writeCheckoutFixture(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}
