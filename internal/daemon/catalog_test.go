package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
)

func TestOpenCatalogRepositoryPreservesLocalBranchAndStagedIndex(t *testing.T) {
	ctx := context.Background()
	svc, cfg, binary := newCatalogTestRepository(t)

	// A user's current branch may differ from the registered default branch.
	// Advance its HEAD locally, then stage a separate file without a FUSE mount.
	localOID := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir,
		"-c", "user.name=test", "-c", "user.email=test@example.com",
		"commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "local-only commit"))
	runCmd(t, "git", "--git-dir", cfg.GitDir, "update-ref", "refs/heads/local-work", localOID)
	runCmd(t, "git", "--git-dir", cfg.GitDir, "symbolic-ref", "HEAD", "refs/heads/local-work")
	staged := filepath.Join(t.TempDir(), "staged.bin")
	if err := os.WriteFile(staged, []byte{0, 0xff, 1, 0xfe, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	stagedOID := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "hash-object", "-w", staged))
	runCmd(t, "git", "--git-dir", cfg.GitDir, "update-index", "--add", "--cacheinfo", "100644,"+stagedOID+",staged.bin")
	indexBefore := readCatalogFile(t, filepath.Join(cfg.GitDir, "index"))
	gitConfigBefore := readCatalogFile(t, filepath.Join(cfg.GitDir, "config"))

	fs, err := svc.OpenCatalogRepository(ctx, cfg.Name)
	if err != nil {
		t.Fatalf("OpenCatalogRepository: %v", err)
	}
	if fs == nil {
		t.Fatal("catalogue filesystem is nil")
	}
	if got := readCatalogFile(t, filepath.Join(cfg.GitDir, "index")); !bytes.Equal(got, indexBefore) {
		t.Fatal("opening catalogue rewrote the staged index")
	}
	if got := readCatalogFile(t, filepath.Join(cfg.GitDir, "config")); !bytes.Equal(got, gitConfigBefore) {
		t.Fatal("opening catalogue rewrote Git configuration")
	}
	headOID, headRef, err := svc.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if headOID != localOID || headRef != "local-work" {
		t.Fatalf("HEAD = (%q, %q), want (%q, local-work)", headOID, headRef, localOID)
	}
	status, err := svc.Status(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if status.CurrentHEADOID != localOID || status.CurrentHEADRef != headRef {
		t.Fatalf("catalogue status did not use local HEAD: %+v", status)
	}
	if got := readCatalogFuseFile(t, fs, "binary.bin", 0, len(binary)+32); !bytes.Equal(got, binary) {
		t.Fatalf("hydrated binary changed: got %x, want %x", got, binary)
	}
	if got := readCatalogFuseFile(t, fs, "binary.bin", 3, 6); !bytes.Equal(got, binary[3:9]) {
		t.Fatalf("binary range changed: got %x, want %x", got, binary[3:9])
	}
}

func TestOpenCatalogRepositoryRuntimeOutlivesRequestAndStops(t *testing.T) {
	ctx := context.Background()
	svc, cfg, binary := newCatalogTestRepository(t)
	requestCtx, cancelRequest := context.WithCancel(ctx)
	fs, err := svc.OpenCatalogRepository(requestCtx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	rt := svc.running[cfg.ID]
	runtimeCount := len(svc.running)
	svc.mu.Unlock()
	if runtimeCount != 1 || rt == nil {
		t.Fatalf("runtime count = %d, runtime = %v", runtimeCount, rt)
	}
	if rt.mfs != nil || rt.joinDone != nil {
		t.Fatal("opening catalogue created a separate per-repository FUSE mount")
	}
	cancelRequest()
	if err := rt.ctx.Err(); err != nil {
		t.Fatalf("runtime canceled with Finder request: %v", err)
	}
	if got := readCatalogFuseFile(t, fs, "binary.bin", 0, len(binary)); !bytes.Equal(got, binary) {
		t.Fatalf("read after request cancellation = %x, want %x", got, binary)
	}
	if _, err := svc.OpenCatalogRepository(ctx, cfg.Name); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	svc.mu.Lock()
	secondRuntime := svc.running[cfg.ID]
	runtimeCount = len(svc.running)
	svc.mu.Unlock()
	if runtimeCount != 1 || secondRuntime != rt {
		t.Fatal("reopening catalogue created a duplicate runtime")
	}
	if err := svc.Unmount(ctx, cfg.Name); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if !errors.Is(rt.ctx.Err(), context.Canceled) {
		t.Fatalf("unmounted runtime context = %v, want canceled", rt.ctx.Err())
	}
	svc.mu.Lock()
	runtimeCount = len(svc.running)
	svc.mu.Unlock()
	if runtimeCount != 0 {
		t.Fatalf("runtime count after Unmount = %d", runtimeCount)
	}
	if _, err := svc.OpenCatalogRepository(ctx, cfg.Name); err != nil {
		t.Fatalf("open after Unmount: %v", err)
	}
	svc.mu.Lock()
	reopened := svc.running[cfg.ID]
	svc.mu.Unlock()
	if reopened == rt || reopened == nil {
		t.Fatal("open after Unmount did not create a new runtime")
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !errors.Is(reopened.ctx.Err(), context.Canceled) {
		t.Fatalf("closed runtime context = %v, want canceled", reopened.ctx.Err())
	}
}

func TestOpenCatalogRepositoryRejectsUnpreparedRepository(t *testing.T) {
	ctx := context.Background()
	svc, cfg, _ := newCatalogTestRepository(t)
	for _, state := range []string{model.PrepareStatePreparing, model.PrepareStateSyncPreparing, model.PrepareStateFailed} {
		t.Run(state, func(t *testing.T) {
			cfg.PrepareState = state
			if err := svc.registry.AddRepo(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.OpenCatalogRepository(ctx, cfg.Name); err == nil {
				t.Fatalf("opened repository in %q state", state)
			}
			svc.mu.Lock()
			count := len(svc.running)
			svc.mu.Unlock()
			if count != 0 {
				t.Fatalf("unprepared repository started %d runtimes", count)
			}
		})
	}
}

func TestUpdateCatalogMountRootRequiresStoppedRuntimesAndPreservesRepository(t *testing.T) {
	ctx := context.Background()
	svc, cfg, _ := newCatalogTestRepository(t)
	indexBefore := readCatalogFile(t, filepath.Join(cfg.GitDir, "index"))
	gitConfigBefore := readCatalogFile(t, filepath.Join(cfg.GitDir, "config"))
	root := filepath.Join(t.TempDir(), "GitHub")
	paths := map[string]string{cfg.Name: filepath.Join(root, "example", cfg.Name)}
	if _, err := svc.OpenCatalogRepository(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateCatalogMountRoot(ctx, root, paths); err == nil {
		t.Fatal("changed catalogue root with a live runtime")
	}
	if got, err := svc.registry.GetRepo(ctx, cfg.Name); err != nil || !reflect.DeepEqual(got, cfg) {
		t.Fatalf("rejected root change modified configuration: got %+v, err %v", got, err)
	}
	if err := svc.Unmount(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateCatalogMountRoot(ctx, root, paths); err != nil {
		t.Fatalf("UpdateCatalogMountRoot after Unmount: %v", err)
	}
	got, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	want := cfg
	want.MountRoot = root
	want.MountPath = paths[cfg.Name]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("root update changed unrelated repository configuration: got %+v, want %+v", got, want)
	}
	if svc.mountRoot != root {
		t.Fatalf("service mount root = %q, want %q", svc.mountRoot, root)
	}
	if got := readCatalogFile(t, filepath.Join(cfg.GitDir, "index")); !bytes.Equal(got, indexBefore) {
		t.Fatal("root update rewrote the index")
	}
	if got := readCatalogFile(t, filepath.Join(cfg.GitDir, "config")); !bytes.Equal(got, gitConfigBefore) {
		t.Fatal("root update rewrote Git configuration")
	}
}

func TestUpdateCatalogMountRootRejectsPreparingRepository(t *testing.T) {
	ctx := context.Background()
	svc, cfg, _ := newCatalogTestRepository(t)
	svc.mu.Lock()
	svc.preparing[cfg.ID] = 1
	svc.mu.Unlock()
	defer func() {
		svc.mu.Lock()
		delete(svc.preparing, cfg.ID)
		svc.mu.Unlock()
	}()
	root := filepath.Join(t.TempDir(), "GitHub")
	if err := svc.UpdateCatalogMountRoot(ctx, root, map[string]string{cfg.Name: filepath.Join(root, cfg.Name)}); err == nil {
		t.Fatal("changed catalogue root while preparation was in progress")
	}
	if got, err := svc.registry.GetRepo(ctx, cfg.Name); err != nil || !reflect.DeepEqual(got, cfg) {
		t.Fatalf("rejected root change modified configuration: got %+v, err %v", got, err)
	}
}

func TestUpdateCatalogMountRootRejectsIncompleteCatalogueWithoutPartialUpdate(t *testing.T) {
	ctx := context.Background()
	svc, cfg, _ := newCatalogTestRepository(t)
	second := cfg
	second.ID = "second"
	second.Name = "second"
	second.MountPath = filepath.Join(cfg.MountRoot, second.Name)
	if err := svc.registry.AddRepo(ctx, second); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "GitHub")
	if err := svc.UpdateCatalogMountRoot(ctx, root, map[string]string{cfg.Name: filepath.Join(root, cfg.Name)}); err == nil {
		t.Fatal("changed root with an incomplete catalogue")
	}
	for _, want := range []model.RepoConfig{cfg, second} {
		got, err := svc.registry.GetRepo(ctx, want.Name)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("incomplete catalogue partially updated %s: got %+v, err %v", want.Name, got, err)
		}
	}
}

func newCatalogTestRepository(t *testing.T) (*Service, model.RepoConfig, []byte) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	work := filepath.Join(tmp, "work")
	bare := filepath.Join(tmp, "origin.git")
	binary := []byte{0x00, 0x01, 0x7f, 0x80, 0xff, 0x00, '\n', '\r', 0x1a, 0xfe, 0xfd}
	runCmd(t, "git", "init", "--initial-branch", "main", work)
	if err := os.WriteFile(filepath.Join(work, "binary.bin"), binary, 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", work, "add", "binary.bin")
	runCmd(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "binary fixture")
	runCmd(t, "git", "clone", "--bare", work, bare)
	runCmd(t, "git", "--git-dir", bare, "config", "uploadpack.allowFilter", "true")
	svc, err := New(ctx, filepath.Join(tmp, "state"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if err := svc.AddRepo(ctx, model.RepoConfig{
		Name: "repo", ID: "repo", RemoteURL: "file://" + bare,
		Branch: "refs/heads/main", RefreshInterval: time.Hour,
		RemoteRefreshDisabled: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	return svc, cfg, binary
}

func readCatalogFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readCatalogFuseFile(t *testing.T, fs *fusefs.ArtifactFuse, name string, offset int64, size int) []byte {
	t.Helper()
	ctx := context.Background()
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: name}
	if err := fs.LookUpInode(ctx, lookup); err != nil {
		t.Fatalf("LookUpInode(%s): %v", name, err)
	}
	open := &fuseops.OpenFileOp{Inode: lookup.Entry.Child}
	if err := fs.OpenFile(ctx, open); err != nil {
		t.Fatalf("OpenFile(%s): %v", name, err)
	}
	defer func() {
		if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: open.Handle}); err != nil {
			t.Errorf("ReleaseFileHandle(%s): %v", name, err)
		}
	}()
	read := &fuseops.ReadFileOp{Inode: lookup.Entry.Child, Handle: open.Handle, Offset: offset, Size: int64(size)}
	if err := fs.ReadFile(ctx, read); err != nil {
		t.Fatalf("ReadFile(%s): %v", name, err)
	}
	data := bytes.Join(read.Data, nil)
	if read.BytesRead != len(data) {
		t.Fatalf("ReadFile(%s) reported %d bytes, returned %d", name, read.BytesRead, len(data))
	}
	return data
}
