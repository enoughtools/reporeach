package desktop

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestStageLocalCheckoutRejectsRealMtimeChangeDuringInventory(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path, _ := adoptionSource(t)
			entry := filepath.Join(path, "tracked.txt")
			if kind == "directory" {
				entry = filepath.Join(path, "private directory name")
				if err := os.Mkdir(entry, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			stamp := time.Unix(1_700_000_000, 123)
			if err := os.Chtimes(entry, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			rootScans := 0
			validator := func(candidate string) error {
				if candidate == path {
					rootScans++
					if rootScans == 2 {
						return os.Chtimes(entry, stamp.Add(time.Second), stamp.Add(time.Second))
					}
				}
				return checkoutValidateNativeMetadata(candidate)
			}
			destination := filepath.Join(t.TempDir(), "kept")
			stage, err := stageLocalCheckout(ctx, model.RepoConfig{GitDir: filepath.Join(path, ".git")}, path, destination, validator)
			if stage != nil || err == nil || !strings.Contains(err.Error(), "working tree changed") || !strings.Contains(err.Error(), "modified time differs") {
				t.Fatalf("real %s timestamp change was not retained and diagnosed: %v", kind, err)
			}
			if strings.Contains(err.Error(), filepath.Base(entry)) || strings.Contains(err.Error(), path) {
				t.Fatalf("source diagnostic exposed a file path: %v", err)
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("timestamp mismatch published a checkout")
			}
		})
	}
}

func TestStageLocalCheckoutRecheckFailureIsDistinctFromSourceChange(t *testing.T) {
	ctx := context.Background()
	path, _ := adoptionSource(t)
	privatePath := filepath.Join(path, "private document name")
	rootScans := 0
	validator := func(candidate string) error {
		if candidate == path {
			rootScans++
			if rootScans == 2 {
				return fmt.Errorf("inventory could not complete: %w", &os.PathError{Op: "lstat", Path: privatePath, Err: syscall.EIO})
			}
		}
		return checkoutValidateNativeMetadata(candidate)
	}
	stage, err := stageLocalCheckout(ctx, model.RepoConfig{GitDir: filepath.Join(path, ".git")}, path, filepath.Join(t.TempDir(), "kept"), validator)
	if stage != nil || !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), "cannot recheck the working tree") || strings.Contains(err.Error(), "working tree changed") {
		t.Fatalf("inventory failure was incorrectly reported as a source edit: %v", err)
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "private document name") {
		t.Fatalf("inventory failure exposed a path: %v", err)
	}
}

func TestStagedLocalCheckoutVerifyReportsHashedMtimeChange(t *testing.T) {
	ctx := context.Background()
	path, _ := adoptionSource(t)
	entry := filepath.Join(path, "tracked.txt")
	stage, err := StageLocalCheckout(ctx, model.RepoConfig{GitDir: filepath.Join(path, ".git")}, path, filepath.Join(t.TempDir(), "kept"))
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	info, err := os.Stat(entry)
	if err != nil {
		t.Fatal(err)
	}
	changed := info.ModTime().Add(time.Second)
	if err := os.Chtimes(entry, changed, changed); err != nil {
		t.Fatal(err)
	}
	err = stage.VerifySource(ctx)
	identity := sha256.Sum256([]byte("tracked.txt"))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("entry %x", identity[:6])) || !strings.Contains(err.Error(), "modified time differs") || strings.Contains(err.Error(), "tracked.txt") {
		t.Fatalf("mtime-only source edit was not safely diagnosed: %v", err)
	}
}

func TestStagedLocalCheckoutVerifyPrivateGitInventoryFailure(t *testing.T) {
	ctx := context.Background()
	path, _ := adoptionSource(t)
	gitDir := filepath.Join(path, ".git")
	stage, err := StageLocalCheckout(ctx, model.RepoConfig{GitDir: gitDir}, path, filepath.Join(t.TempDir(), "kept"))
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	retained := filepath.Join(t.TempDir(), "retained-original-git")
	if err := os.Rename(gitDir, retained); err != nil {
		t.Fatal(err)
	}
	err = stage.VerifyPrivateGit(ctx)
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "cannot recheck Git metadata") || strings.Contains(err.Error(), "Git metadata changed") || strings.Contains(err.Error(), gitDir) {
		t.Fatalf("Git inventory failure was confused with a changed manifest or exposed a path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(retained, "index")); err != nil {
		t.Fatal("failed verification altered retained original metadata")
	}
}

func TestCheckoutExactManifestDifferenceKeepsDirectoryAndLinkMtimeStrict(t *testing.T) {
	for _, mode := range []os.FileMode{os.ModeDir | 0o755, os.ModeSymlink | 0o777} {
		before := checkoutFileRecord{Mode: mode, Modified: time.Unix(1_700_000_000, 123)}
		after := before
		after.Modified = after.Modified.Add(time.Second)
		left := map[string]checkoutFileRecord{"private-path": before}
		right := map[string]checkoutFileRecord{"private-path": after}
		if !checkoutManifestContentEqual(left, right) {
			t.Fatal("copy comparison unexpectedly changed its directory/link timestamp contract")
		}
		diagnostic := checkoutExactManifestDifference(left, right)
		if !strings.Contains(diagnostic, "modified time differs") || strings.Contains(diagnostic, "private-path") {
			t.Fatalf("exact source comparison failed to diagnose timestamp-only drift: %q", diagnostic)
		}
	}
}

func TestCheckoutInventoryFailureRetainsCauseWithoutPath(t *testing.T) {
	privatePath := "/a/private/path with a filename"
	err := checkoutInventoryFailure(fmt.Errorf("wrapped inventory: %w", &os.PathError{Op: "open", Path: privatePath, Err: os.ErrPermission}))
	identity := sha256.Sum256([]byte(privatePath))
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), fmt.Sprintf("entry %x", identity[:6])) || strings.Contains(err.Error(), privatePath) {
		t.Fatalf("unsafe inventory error: %v", err)
	}
	if err := checkoutInventoryFailure(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation cause: %v", err)
	}
}
