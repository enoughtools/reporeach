package desktop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

func TestParentCancellationPreservesMountOwnershipUntilClose(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gh := fakeGitHub(t, "exit 4")
	s, err := New(ctx, Options{StateDir: filepath.Join(dir, "state"), MountRoot: filepath.Join(dir, "mount"), GHPath: gh.path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seedDesktopCatalogue(t, s, catalogueRepository("owner", "repo"))
	mounted := newDesktopFakeMount()
	s.dependencyReady = func() bool { return true }
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) { return mounted, nil }
	if err := s.Mount(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := s.ctx.Err(); err != nil {
		t.Fatalf("signal cancellation stopped the live filesystem context: %v", err)
	}
	if !s.Status().Mounted {
		t.Fatal("signal cancellation discarded ownership of an attached kernel mount")
	}
	if _, err := s.catalogMetadata.HasMetadataXattrs(s.ctx); err != nil {
		t.Fatalf("signal cancellation made live catalogue storage unavailable: %v", err)
	}
	if err := s.PrepareQuit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	if mounted.unmountCalls != 1 || s.Status().Mounted {
		t.Fatalf("Close did not detach the retained mount: calls=%d, mounted=%v", mounted.unmountCalls, s.Status().Mounted)
	}
}

func TestGitWorktreeConfigIgnoresInheritedRepositoryBindings(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "private.git")
	original := filepath.Join(dir, "original.git")
	for _, path := range []string{private, original} {
		cmd := exec.Command("git", "init", "--bare", path)
		cmd.Env = nativeAdoptionEnvironment(os.Environ())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("initialize fixture: %v: %s", err, out)
		}
	}
	sentinelConfig := filepath.Join(dir, "source.config")
	sentinel := []byte("[user]\n\tname = Original\n")
	if err := os.WriteFile(sentinelConfig, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	originalConfig, err := os.ReadFile(filepath.Join(original, "config"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", original)
	t.Setenv("GIT_COMMON_DIR", original)
	t.Setenv("GIT_CONFIG", sentinelConfig)
	t.Setenv("GIT_WORK_TREE", dir)
	path := filepath.Join(dir, "virtual")
	if err := configureGitWorktree(context.Background(), private, path); err != nil {
		t.Fatal(err)
	}
	for file, expected := range map[string][]byte{sentinelConfig: sentinel, filepath.Join(original, "config"): originalConfig} {
		actual, err := os.ReadFile(file)
		if err != nil || string(actual) != string(expected) {
			t.Fatalf("inherited source config changed: %v", err)
		}
	}
	cmd := exec.Command("git", "--git-dir", private, "config", "--local", "--get", "core.worktree")
	cmd.Env = nativeAdoptionEnvironment(os.Environ())
	actual, err := cmd.Output()
	if err != nil || string(actual) != path+"\n" {
		t.Fatalf("private worktree config = %q: %v", actual, err)
	}
}
