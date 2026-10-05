//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/cli"
	"github.com/cloudflare/artifact-fs/internal/desktop"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

// This exercises the native app's real socket protocol and filesystem together.
// The desktop server is an external process, just as it is in production: a
// test-side Go file read must never wait on its own runtime's FUSE server.
func TestE2EDesktopAdoptionAndVisibility(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)

	// Unix socket names have a small platform limit. Keep every path private,
	// disposable, and short instead of relying on the test runner's deep TMPDIR.
	root, err := os.MkdirTemp("/tmp", "rr-adopt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "global-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_OPTIONAL_LOCKS", "0")
	t.Setenv("GH_CONFIG_DIR", filepath.Join(root, "gh-config"))

	source := filepath.Join(root, "original")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	run(t, source, "git", "init", "--initial-branch=trunk")
	run(t, source, "git", "config", "user.name", "Disposable fixture")
	run(t, source, "git", "config", "user.email", "fixture@example.invalid")
	binary := []byte{0, 255, 128, '\n', 0, 254, 1, 2}
	committed := []byte("committed source\n")
	for name, content := range map[string][]byte{
		"tracked.txt": committed, "binary.dat": binary, ".gitignore": []byte("ignored.txt\n"),
	} {
		if err := os.WriteFile(filepath.Join(source, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run(t, source, "git", "add", ".")
	run(t, source, "git", "commit", "-m", "committed fixture")
	head := strings.TrimSpace(run(t, source, "git", "rev-parse", "HEAD"))
	writeTestFile(t, source, "tracked.txt", "staged source\n")
	run(t, source, "git", "add", "tracked.txt")
	writeTestFile(t, source, "tracked.txt", "unstaged source\n")
	writeTestFile(t, source, "untracked.txt", "untracked source\n")
	writeTestFile(t, source, "ignored.txt", "ignored source\n")
	sourceStatus := run(t, source, "git", "status", "--porcelain=v1", "--untracked-files=all", "--ignored")
	if !strings.Contains(sourceStatus, "MM tracked.txt") || !strings.Contains(sourceStatus, "?? untracked.txt") || !strings.Contains(sourceStatus, "!! ignored.txt") {
		t.Fatalf("fixture did not include staged, dirty, untracked and ignored work: %q", sourceStatus)
	}
	before := snapshotDesktopAdoptionSource(t, source)

	ghSentinel := filepath.Join(root, "gh-called")
	t.Setenv("AFS_E2E_DESKTOP_GH_SENTINEL", ghSentinel)
	ghPath := filepath.Join(root, "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nprintf '%s\\n' called >> \"$AFS_E2E_DESKTOP_GH_SENTINEL\"\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir, mountRoot := filepath.Join(root, "state"), filepath.Join(root, "mount")
	server := startDesktopAdoptionServer(t, root, stateDir, mountRoot, ghPath)

	const owner, repoID = "Work", "Work/project"
	for _, name := range []string{"project", "neighbor"} {
		var status desktop.Status
		server.request(t, http.MethodPost, "/v1/repositories/adopt", desktop.AdoptionRequest{RemoteURL: source, Owner: owner, Name: name}, &status)
		if status.Account != nil {
			t.Fatal("manual adoption required a GitHub account")
		}
		for _, repo := range status.Repositories {
			if repo.Source != "manual" || repo.State != "virtual" || repo.DefaultBranch != "trunk" {
				t.Fatalf("adoption was not a lazy native-Git source: %+v", repo)
			}
		}
	}
	var status desktop.Status
	server.request(t, http.MethodPost, "/v1/mount", nil, &status)
	if !status.Mounted {
		t.Fatalf("catalogue was not mounted: %+v", status)
	}
	waitDesktopCatalogueNames(t, mountRoot, []string{owner})
	waitDesktopCatalogueNames(t, filepath.Join(mountRoot, owner), []string{"neighbor", "project"})
	// Browsing owner and repository placeholders must not acquire either source.
	server.request(t, http.MethodGet, "/v1/status", nil, &status)
	for _, repo := range status.Repositories {
		if repo.State != "virtual" {
			t.Fatalf("catalogue listing prepared a source: %+v", repo)
		}
	}

	mountPath := filepath.Join(mountRoot, owner, "project")
	if got, err := os.ReadFile(filepath.Join(mountPath, "tracked.txt")); err != nil || !bytes.Equal(got, committed) {
		t.Fatalf("virtual tree exposed uncommitted source work: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(mountPath, "binary.dat")); err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("virtual binary bytes = %v, %v", got, err)
	}
	for _, name := range []string{"untracked.txt", "ignored.txt"} {
		waitDesktopCataloguePathMissing(t, filepath.Join(mountPath, name))
	}
	if got := strings.TrimSpace(gitCmd(t, mountPath, "rev-parse", "HEAD")); got != head {
		t.Fatalf("virtual Git HEAD = %q, want %q", got, head)
	}
	if got := gitCmd(t, mountPath, "status", "--porcelain=v1", "--untracked-files=all"); got != "" {
		t.Fatalf("virtual committed checkout is dirty: %q", got)
	}
	if got := strings.Fields(gitCmd(t, mountPath, "ls-files")); !slices.Equal(got, []string{".gitignore", "binary.dat", "tracked.txt"}) {
		t.Fatalf("virtual Git index = %v", got)
	}

	server.action(t, repoID, "keep")
	server.request(t, http.MethodGet, "/v1/status", nil, &status)
	pinned := desktopAdoptedRepository(t, status, repoID)
	if !pinned.Pinned || pinned.State != "pinned" || pinned.DownloadedBytes != int64(len(committed)+len(binary)+len("ignored.txt\n")) {
		t.Fatalf("Keep Downloaded did not pin the manual committed tree: %+v", pinned)
	}
	// Hiding affects fresh catalogue lookup and leaves already-open files valid.
	open, err := os.Open(filepath.Join(mountPath, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	server.visibility(t, repoID, false)
	waitDesktopCatalogueNames(t, filepath.Join(mountRoot, owner), []string{"neighbor"})
	waitDesktopCataloguePathMissing(t, mountPath)
	got := make([]byte, len(committed))
	if n, err := open.ReadAt(got, 0); n != len(committed) || err != nil && !errors.Is(err, io.EOF) || !bytes.Equal(got, committed) {
		t.Fatalf("hiding invalidated an open committed file: %q, %v", got, err)
	}
	if err := open.Close(); err != nil {
		t.Fatal(err)
	}
	data, code := server.rawRequest(t, http.MethodPost, "/v1/repositories/action", map[string]any{"id": repoID, "action": "keep"})
	if code != http.StatusBadRequest || !bytes.Contains(data, []byte("enable this repository")) {
		t.Fatalf("hidden repository accepted background download: %d %s", code, data)
	}
	server.visibility(t, repoID, true)
	waitDesktopCatalogueNames(t, filepath.Join(mountRoot, owner), []string{"neighbor", "project"})
	waitDesktopCataloguePathPresent(t, mountPath)
	server.request(t, http.MethodGet, "/v1/status", nil, &status)
	restored := desktopAdoptedRepository(t, status, repoID)
	if !restored.Pinned || restored.DownloadedBytes != pinned.DownloadedBytes {
		t.Fatalf("visibility change deleted pin intent or downloaded storage: %+v", restored)
	}

	server.organization(t, owner, false)
	waitDesktopCatalogueNames(t, mountRoot, nil)
	waitDesktopCataloguePathMissing(t, filepath.Join(mountRoot, owner))
	server.visibility(t, repoID, false)
	server.organization(t, owner, true)
	waitDesktopCatalogueNames(t, mountRoot, []string{owner})
	waitDesktopCatalogueNames(t, filepath.Join(mountRoot, owner), []string{"neighbor"})
	waitDesktopCataloguePathMissing(t, mountPath)

	// The group switch must not overwrite an individual switch, including when
	// the daemon reloads its durable catalogue and restores its desired mount.
	server.stop(t)
	server = startDesktopAdoptionServer(t, root, stateDir, mountRoot, ghPath)
	server.request(t, http.MethodGet, "/v1/status", nil, &status)
	if !status.Mounted || !desktopAdoptedRepository(t, status, repoID).Disabled {
		t.Fatalf("restart forgot mount or individual visibility: %+v", status)
	}
	waitDesktopCatalogueNames(t, filepath.Join(mountRoot, owner), []string{"neighbor"})
	waitDesktopCataloguePathMissing(t, mountPath)
	server.visibility(t, repoID, true)
	waitDesktopCatalogueNames(t, filepath.Join(mountRoot, owner), []string{"neighbor", "project"})
	waitDesktopCataloguePathPresent(t, mountPath)
	if got, err := os.ReadFile(filepath.Join(mountPath, "tracked.txt")); err != nil || !bytes.Equal(got, committed) {
		t.Fatalf("restored manual tree = %q, %v", got, err)
	}
	server.request(t, http.MethodPost, "/v1/unmount", nil, &status)
	if status.Mounted {
		t.Fatal("desktop API did not detach the catalogue")
	}
	server.stop(t)
	if got := run(t, source, "git", "status", "--porcelain=v1", "--untracked-files=all", "--ignored"); got != sourceStatus {
		t.Fatalf("source Git work changed: before %q, after %q", sourceStatus, got)
	}
	if after := snapshotDesktopAdoptionSource(t, source); !reflect.DeepEqual(before, after) {
		t.Fatal("adoption, preparation, Keep Downloaded or visibility changed original checkout bytes, modes, index, refs or configuration")
	}
	if _, err := os.Stat(ghSentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native manual adoption invoked GitHub CLI: %v", err)
	}
}

type desktopSourceFile struct {
	Mode    fs.FileMode
	Content []byte
}

func snapshotDesktopAdoptionSource(t *testing.T, source string) map[string]desktopSourceFile {
	t.Helper()
	files := map[string]desktopSourceFile{}
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		file := desktopSourceFile{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			file.Content, err = os.ReadFile(path)
		} else if info.Mode()&os.ModeSymlink != 0 {
			var target string
			target, err = os.Readlink(path)
			file.Content = []byte(target)
		}
		files[relative] = file
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

type desktopAdoptionServer struct {
	socket, mountRoot, logPath string
	cmd                        *exec.Cmd
	done                       chan error
	stopped                    bool
}

func startDesktopAdoptionServer(t *testing.T, root, stateDir, mountRoot, ghPath string) *desktopAdoptionServer {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := &desktopAdoptionServer{socket: filepath.Join(root, "control.sock"), mountRoot: mountRoot, logPath: filepath.Join(root, "server.log"), done: make(chan error, 1)}
	logFile, err := os.OpenFile(server.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	server.cmd = exec.Command(executable, "-test.run=^TestDesktopAdoptionServer$", "-test.v", "-test.timeout=5m")
	server.cmd.Stdout, server.cmd.Stderr = logFile, logFile
	server.cmd.Env = append(os.Environ(), "AFS_E2E_DESKTOP_SERVER=1", "AFS_E2E_DESKTOP_STATE="+stateDir, "AFS_E2E_DESKTOP_MOUNT="+mountRoot, "AFS_E2E_DESKTOP_SOCKET="+server.socket, "AFS_E2E_DESKTOP_GH="+ghPath)
	if err := server.cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	go func() {
		err := server.cmd.Wait()
		_ = logFile.Close()
		server.done <- err
	}()
	t.Cleanup(func() { server.stop(t) })
	waitForCondition(t, 30*time.Second, "private desktop API readiness", func() (bool, string) {
		select {
		case err := <-server.done:
			server.done <- err
			output, _ := os.ReadFile(server.logPath)
			return false, fmt.Sprintf("server exited: %v\n%s", err, output)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, code, err := desktop.Request(ctx, server.socket, http.MethodGet, "/v1/status", nil)
		return err == nil && code == http.StatusOK, fmt.Sprintf("API readiness: %d %v", code, err)
	})
	return server
}

func (s *desktopAdoptionServer) stop(t *testing.T) {
	t.Helper()
	if s.stopped {
		return
	}
	s.stopped = true
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-s.done:
		if err != nil {
			output, _ := os.ReadFile(s.logPath)
			t.Errorf("desktop server exit: %v\n%s", err, output)
		}
	case <-time.After(15 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
		t.Error("desktop server did not stop within 15 seconds")
	}
	deadline := time.Now().Add(10 * time.Second)
	for isMounted(s.mountRoot) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if isMounted(s.mountRoot) {
		t.Errorf("desktop server left its catalogue mounted at %s", s.mountRoot)
		// Preserve the failed assertion while still cleaning our disposable
		// mount if the server crashed or had to be killed during a failed test.
		if err := fusefs.TryUnmount(s.mountRoot); err != nil {
			t.Errorf("cleanup desktop catalogue: %v", err)
		}
	}
}

func (s *desktopAdoptionServer) rawRequest(t *testing.T, method, path string, request any) ([]byte, int) {
	t.Helper()
	var body []byte
	if request != nil {
		var err error
		body, err = json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, code, err := desktop.Request(ctx, s.socket, method, path, body)
	if err != nil {
		t.Fatalf("desktop %s %s: %v", method, path, err)
	}
	return data, code
}

func (s *desktopAdoptionServer) request(t *testing.T, method, path string, request, response any) {
	t.Helper()
	data, code := s.rawRequest(t, method, path, request)
	if code < 200 || code >= 300 {
		t.Fatalf("desktop %s %s: %d %s", method, path, code, data)
	}
	if response != nil {
		if err := json.Unmarshal(data, response); err != nil {
			t.Fatalf("decode desktop %s response: %v", path, err)
		}
	}
}

func (s *desktopAdoptionServer) action(t *testing.T, id, action string) {
	t.Helper()
	var response struct {
		Operation desktop.Operation `json:"operation"`
	}
	s.request(t, http.MethodPost, "/v1/repositories/action", map[string]any{"id": id, "action": action}, &response)
	waitForCondition(t, 30*time.Second, "desktop action completion", func() (bool, string) {
		var status desktop.Status
		s.request(t, http.MethodGet, "/v1/status", nil, &status)
		for _, operation := range status.Operations {
			if operation.ID == response.Operation.ID {
				return operation.Status == "complete", fmt.Sprintf("%s status=%s error=%s", action, operation.Status, operation.Error)
			}
		}
		return false, "operation absent from status"
	})
}

func (s *desktopAdoptionServer) visibility(t *testing.T, id string, enabled bool) {
	t.Helper()
	s.request(t, http.MethodPost, "/v1/repositories/visibility", map[string]any{"id": id, "enabled": enabled}, nil)
}

func (s *desktopAdoptionServer) organization(t *testing.T, owner string, enabled bool) {
	t.Helper()
	s.request(t, http.MethodPost, "/v1/organizations/settings", map[string]any{"owner": owner, "enabled": enabled}, nil)
}

func desktopAdoptedRepository(t *testing.T, status desktop.Status, id string) desktop.Repository {
	t.Helper()
	for _, repo := range status.Repositories {
		if repo.ID == id {
			return repo
		}
	}
	t.Fatalf("adopted repository %q missing from status", id)
	return desktop.Repository{}
}

func waitDesktopCatalogueNames(t *testing.T, path string, wanted []string) {
	t.Helper()
	waitForCondition(t, 10*time.Second, "catalogue names", func() (bool, string) {
		entries, err := os.ReadDir(path)
		if err != nil {
			return false, fmt.Sprintf("ReadDir %s: %v", path, err)
		}
		names := dirEntryNames(entries)
		return slices.Equal(names, wanted), fmt.Sprintf("%s lists %v, want %v", path, names, wanted)
	})
}

func waitDesktopCataloguePathMissing(t *testing.T, path string) {
	t.Helper()
	waitForCondition(t, 10*time.Second, "fresh hidden catalogue lookup", func() (bool, string) {
		_, err := os.Stat(path)
		return errors.Is(err, os.ErrNotExist), fmt.Sprintf("stat %s: %v", path, err)
	})
}

func waitDesktopCataloguePathPresent(t *testing.T, path string) {
	t.Helper()
	waitForCondition(t, 10*time.Second, "fresh restored catalogue lookup", func() (bool, string) {
		info, err := os.Stat(path)
		return err == nil && info.IsDir(), fmt.Sprintf("stat %s: %v", path, err)
	})
}

// Invoked only by startDesktopAdoptionServer. Use the production CLI so native
// Git environment handling, socket ownership and restoration run unchanged.
func TestDesktopAdoptionServer(t *testing.T) {
	if os.Getenv("AFS_E2E_DESKTOP_SERVER") != "1" {
		t.Skip("invoked by the desktop adoption filesystem integration test")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	args := []string{"desktop", "serve", "--state-dir", os.Getenv("AFS_E2E_DESKTOP_STATE"), "--mount-root", os.Getenv("AFS_E2E_DESKTOP_MOUNT"), "--socket", os.Getenv("AFS_E2E_DESKTOP_SOCKET"), "--gh", os.Getenv("AFS_E2E_DESKTOP_GH")}
	if directory := os.Getenv("AFS_E2E_DESKTOP_FSKIT_SOCKET_DIR"); directory != "" {
		args = append(args, "--fskit-socket-dir", directory)
	}
	if code := cli.Run(ctx, args, os.Stdout, os.Stderr); code != 0 {
		t.Fatalf("desktop server returned exit code %d", code)
	}
}
