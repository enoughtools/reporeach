//go:build !windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// TestE2ECatalogue validates the actual single mount, rather than invoking FUSE
// methods directly. It uses local bare remotes and never changes global Git
// configuration. Run with AFS_RUN_E2E_TESTS=1 and a usable FUSE device/driver.
func TestE2ECatalogue(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("set AFS_RUN_E2E_TESTS=1 to run catalogue FUSE integration")
	}
	skipIfNoFUSE(t)
	remote := createLocalTestRepo(t)
	stateRoot, mountRoot := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := daemon.New(ctx, stateRoot, logger)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetMountRoot(mountRoot)
	entries := []catalogfs.Entry{{ID: "alice/project", Owner: "alice", Name: "project"}, {ID: "team/project", Owner: "team", Name: "project"}}
	var activations atomic.Int64
	var mounted fusefs.MountedFS
	stopMount := func() {
		t.Helper()
		if mounted == nil {
			return
		}
		if err := mounted.Unmount(); err != nil {
			t.Fatalf("unmount catalogue: %v", err)
		}
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer joinCancel()
		if err := mounted.Join(joinCtx); err != nil {
			t.Fatalf("join catalogue mount: %v", err)
		}
		mounted = nil
	}
	t.Cleanup(func() {
		stopMount()
		if err := svc.Close(); err != nil {
			t.Errorf("close catalogue runtimes: %v", err)
		}
	})
	mount := func() {
		t.Helper()
		fs, err := catalogfs.New(entries, func(request context.Context, entry catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
			activations.Add(1)
			name := entry.Owner + "-project"
			configs, err := svc.ListRepos(request)
			if err != nil {
				return nil, err
			}
			registered := false
			for _, cfg := range configs {
				if cfg.Name == name {
					registered = true
				}
			}
			if !registered {
				cfg := model.RepoConfig{ID: model.RepoID(name), Name: name, RemoteURL: remote, Branch: "main", MountRoot: mountRoot, MountPath: filepath.Join(mountRoot, entry.Owner, entry.Name), RemoteRefreshDisabled: true, RefreshInterval: time.Hour, Enabled: true}
				if err := svc.AddRepo(request, cfg); err != nil {
					return nil, err
				}
			}
			return svc.OpenCatalogRepository(request, name)
		})
		if err != nil {
			t.Fatal(err)
		}
		mounted, err = catalogfs.Mount(ctx, mountRoot, fs)
		if err != nil {
			t.Fatalf("mount catalogue: %v", err)
		}
	}
	mount()
	if names := lsDir(t, mountRoot); len(names) != 2 || names[0] != "alice" || names[1] != "team" {
		t.Fatalf("owner catalogue: %v", names)
	}
	alicePath, teamPath := filepath.Join(mountRoot, "alice", "project"), filepath.Join(mountRoot, "team", "project")
	for _, owner := range []string{"alice", "team"} {
		if names := lsDir(t, filepath.Join(mountRoot, owner)); len(names) != 1 || names[0] != "project" {
			t.Fatalf("repo placeholders: %v", names)
		}
		if info, err := os.Stat(filepath.Join(mountRoot, owner, "project")); err != nil || !info.IsDir() {
			t.Fatalf("placeholder stat: %v", err)
		}
	}
	if activations.Load() != 0 {
		t.Fatalf("root browsing prepared %d repositories", activations.Load())
	}
	if configs, err := svc.ListRepos(ctx); err != nil || len(configs) != 0 {
		t.Fatalf("root browsing registered clones: %v %v", configs, err)
	}
	if names := lsDir(t, alicePath); len(names) < 5 {
		t.Fatalf("prepared repository contents: %v", names)
	}
	if activations.Load() != 1 {
		t.Fatalf("entering one repository activated %d", activations.Load())
	}
	if status, err := svc.Status(ctx, "alice-project"); err != nil || status.HydratedBlobCount != 0 {
		t.Fatalf("listing prepared repository hydrated contents: %+v %v", status, err)
	}
	primeMountedFilePolling(t, alicePath)
	if status, err := svc.Status(ctx, "alice-project"); err != nil || status.HydratedBlobCount != 0 {
		t.Fatalf("synthetic Git pointer hydrated repository contents: %+v %v", status, err)
	}
	gitFile, err := os.ReadFile(filepath.Join(alicePath, ".git"))
	if err != nil || !bytes.HasPrefix(gitFile, []byte("gitdir: ")) {
		t.Fatalf("Git directory pointer: %q %v", gitFile, err)
	}
	if got := readFileStr(t, filepath.Join(alicePath, "README.md")); !strings.Contains(got, "Test Repo") {
		t.Fatalf("on-demand contents: %q", got)
	}

	git := func(dir string, args ...string) string {
		t.Helper()
		options := []string{"-c", "safe.directory=" + dir, "-c", "core.fsmonitor=false"}
		return run(t, dir, "git", append(options, args...)...)
	}
	if status := strings.TrimSpace(git(alicePath, "status", "--porcelain=v1")); status != "" {
		t.Fatalf("initial Git status: %q", status)
	}
	_ = git(alicePath, "checkout", "-b", "catalogue-work")
	binaryContents := []byte{0, 0xff, 1, 2, 3, 'r', 'e', 'p', 'o', '\n'}
	localPath := filepath.Join(alicePath, "local.bin")
	if err := os.WriteFile(localPath, binaryContents, 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(localPath); err != nil || !bytes.Equal(data, binaryContents) {
		t.Fatalf("binary write/read: %x %v", data, err)
	}
	if status := git(alicePath, "status", "--porcelain=v1"); !strings.Contains(status, "?? local.bin") {
		t.Fatalf("Git did not see local work: %q", status)
	}
	_ = git(alicePath, "add", "local.bin")
	_ = git(alicePath, "-c", "user.name=Catalogue Test", "-c", "user.email=catalogue@example.invalid", "commit", "-m", "Keep local binary work")
	commit := strings.TrimSpace(git(alicePath, "rev-parse", "HEAD"))
	waitForCondition(t, 10*time.Second, "catalogue commit snapshot reconciliation", func() (bool, string) {
		status := strings.TrimSpace(git(alicePath, "status", "--porcelain=v1"))
		return status == "", status
	})
	if got := readFileStr(t, filepath.Join(teamPath, "README.md")); !strings.Contains(got, "Test Repo") {
		t.Fatalf("second repository content: %q", got)
	}
	if activations.Load() != 2 {
		t.Fatalf("same-name repository activation: %d", activations.Load())
	}
	if _, err := os.Stat(filepath.Join(teamPath, "local.bin")); !os.IsNotExist(err) {
		t.Fatalf("local edit leaked between repositories: %v", err)
	}
	if status := strings.TrimSpace(git(teamPath, "status", "--porcelain=v1")); status != "" {
		t.Fatalf("independent Git index: %q", status)
	}
	if err := os.Rename(localPath, filepath.Join(teamPath, "moved.bin")); err == nil {
		t.Fatal("cross-repository rename succeeded")
	}

	_ = git(alicePath, "checkout", "main")
	waitForCondition(t, 10*time.Second, "branch switch removes local branch file", func() (bool, string) { _, err := os.Stat(localPath); return os.IsNotExist(err), fmt.Sprint(err) })
	_ = git(alicePath, "checkout", "catalogue-work")
	waitForCondition(t, 10*time.Second, "branch switch restores local branch file", func() (bool, string) {
		data, err := os.ReadFile(localPath)
		return err == nil && bytes.Equal(data, binaryContents), fmt.Sprint(err)
	})
	stopMount()
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	svc, err = daemon.New(ctx, stateRoot, logger)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetMountRoot(mountRoot)
	mount()
	primeMountedFilePolling(t, alicePath)
	if got := strings.TrimSpace(git(alicePath, "rev-parse", "HEAD")); got != commit {
		t.Fatalf("restart discarded local commit: %s != %s", got, commit)
	}
	if branch := strings.TrimSpace(git(alicePath, "branch", "--show-current")); branch != "catalogue-work" {
		t.Fatalf("restart changed local branch: %s", branch)
	}
	if data, err := os.ReadFile(localPath); err != nil || !bytes.Equal(data, binaryContents) {
		t.Fatalf("restart lost binary content: %x %v", data, err)
	}
	if status := strings.TrimSpace(git(alicePath, "status", "--porcelain=v1")); status != "" {
		t.Fatalf("restart changed staged/index state: %q", status)
	}
}
