//go:build darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/desktop"
	"golang.org/x/sys/unix"
)

// This is an acceptance test for an already installed, enabled native module.
// It intentionally does not install an app, change extension settings, or use
// macFUSE. See docs/reporeach/fskit-acceptance.md before opting in.
func TestFSKitMountedAcceptance(t *testing.T) {
	if os.Getenv("AFS_RUN_FSKIT_E2E_TESTS") != "1" {
		t.Skip("set AFS_RUN_FSKIT_E2E_TESTS=1 for real macOS 26 FSKit acceptance")
	}
	release, err := unix.Sysctl("kern.osrelease")
	if err != nil {
		t.Fatal(err)
	}
	major, err := strconv.Atoi(strings.Split(release, ".")[0])
	if err != nil || major < 25 {
		t.Skipf("real FSKit acceptance requires macOS 26+ (Darwin 25+); host Darwin %s provides no mounted proof", release)
	}
	fsKitAcceptanceModule(t)

	root, err := os.MkdirTemp("/tmp", "rr-fskit-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	h := &fsKitAcceptanceHarness{root: root, state: filepath.Join(root, "state"), mount: filepath.Join(root, "mount")}
	t.Cleanup(func() { h.cleanup(t) })
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "global-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GH_CONFIG_DIR", filepath.Join(root, "gh-config"))
	// Fail on any GitHub call, including accidental background authentication.
	ghSentinel := filepath.Join(root, "gh-called")
	t.Setenv("AFS_E2E_DESKTOP_GH_SENTINEL", ghSentinel)
	h.gh = filepath.Join(root, "gh")
	fsKitWrite(t, h.gh, []byte("#!/bin/sh\nprintf '%s\\n' called >> \"$AFS_E2E_DESKTOP_GH_SENTINEL\"\nexit 99\n"), 0o700)

	source, bare := filepath.Join(root, "original"), filepath.Join(root, "remote.git")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	fsKitGit(t, source, "init", "--initial-branch=trunk")
	fsKitGit(t, source, "config", "user.name", "FSKit fixture")
	fsKitGit(t, source, "config", "user.email", "fixture@example.invalid")
	text := []byte("initial committed text\n")
	binary := []byte{0, 255, 128, '\n', 0, 254, 1, 2}
	fsKitWrite(t, filepath.Join(source, "tracked.txt"), text, 0o644)
	fsKitWrite(t, filepath.Join(source, "binary.dat"), binary, 0o644)
	fsKitWrite(t, filepath.Join(source, "executable"), []byte("#!/bin/sh\nprintf '%s\\n' fixture-executable\n"), 0o755)
	if err := os.Symlink("tracked.txt", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	fsKitGit(t, source, "add", ".")
	fsKitGit(t, source, "commit", "-m", "initial fixture")
	initialHead := strings.TrimSpace(fsKitGit(t, source, "rev-parse", "HEAD"))
	fsKitGit(t, root, "clone", "--bare", source, bare)
	fsKitGit(t, source, "remote", "add", "origin", bare)
	fsKitWrite(t, filepath.Join(source, "tracked.txt"), []byte("staged original\n"), 0o644)
	fsKitGit(t, source, "add", "tracked.txt")
	fsKitWrite(t, filepath.Join(source, "tracked.txt"), []byte("dirty original\n"), 0o644)
	fsKitWrite(t, filepath.Join(source, "untracked.txt"), []byte("untracked original\n"), 0o644)
	beforeSource := snapshotDesktopAdoptionSource(t, source)

	h.copyServerImage(t)
	h.start(t)
	const owner, repoID = "Acceptance", "Acceptance/project"
	var status desktop.Status
	h.request(t, http.MethodPost, "/v1/repositories/adopt", desktop.AdoptionRequest{RemoteURL: source, Owner: owner, Name: "project"}, &status)
	if status.Account != nil || desktopAdoptedRepository(t, status, repoID).State != "virtual" {
		t.Fatalf("manual adoption was not unauthenticated and lazy: %+v", status)
	}
	h.request(t, http.MethodPost, "/v1/mount", nil, &status)
	if !status.Mounted {
		t.Fatalf("native catalogue did not mount: %+v", status)
	}
	h.identity(t)
	waitDesktopCatalogueNames(t, h.mount, []string{owner})
	waitDesktopCatalogueNames(t, filepath.Join(h.mount, owner), []string{"project"})
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	if desktopAdoptedRepository(t, status, repoID).State != "virtual" {
		t.Fatal("listing catalogue placeholders acquired the Git checkout")
	}
	repo := filepath.Join(h.mount, owner, "project")
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), text)
	fsKitReadEqual(t, filepath.Join(repo, "binary.dat"), binary)
	fsKitReadEqual(t, filepath.Join(repo, "link"), text)
	if target, err := os.Readlink(filepath.Join(repo, "link")); err != nil || target != "tracked.txt" {
		t.Fatalf("native symlink target=%q error=%v", target, err)
	}
	if info, err := os.Stat(filepath.Join(repo, "executable")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("native executable mode: %v, %v", info, err)
	}
	if output := run(t, repo, filepath.Join(repo, "executable")); output != "fixture-executable\n" {
		t.Fatalf("native execution output %q", output)
	}
	waitDesktopCataloguePathMissing(t, filepath.Join(repo, "untracked.txt"))
	if head := strings.TrimSpace(fsKitGit(t, repo, "rev-parse", "HEAD")); head != initialHead {
		t.Fatalf("adoption HEAD=%s want %s", head, initialHead)
	}
	fsKitCleanGit(t, repo)

	// Native Git writes must remain coherent after warm reads and branch checkout.
	committed := []byte("committed through native mount\n")
	fsKitWrite(t, filepath.Join(repo, "tracked.txt"), committed, 0o644)
	fsKitGit(t, repo, "add", "tracked.txt")
	if staged := fsKitGit(t, repo, "status", "--porcelain=v1"); staged != "M  tracked.txt\n" {
		t.Fatalf("native staged state %q", staged)
	}
	fsKitGit(t, repo, "-c", "user.name=FSKit fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "mounted commit")
	fsKitGit(t, repo, "checkout", "-b", "acceptance")
	branchText := []byte("alternate branch bytes\n")
	fsKitWrite(t, filepath.Join(repo, "tracked.txt"), branchText, 0o644)
	fsKitWrite(t, filepath.Join(repo, "branch-only.txt"), []byte("branch-only\n"), 0o644)
	fsKitGit(t, repo, "add", "tracked.txt", "branch-only.txt")
	fsKitGit(t, repo, "-c", "user.name=FSKit fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "alternate tree")
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), branchText)
	fsKitGit(t, repo, "checkout", "trunk")
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), committed)
	waitDesktopCataloguePathMissing(t, filepath.Join(repo, "branch-only.txt"))
	fsKitGit(t, repo, "checkout", "acceptance")
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), branchText)
	fsKitReadEqual(t, filepath.Join(repo, "branch-only.txt"), []byte("branch-only\n"))
	fsKitCleanGit(t, repo)
	fsKitRetainedFile(t, repo)

	// A cwd and open file are retained during a normal detach attempt. No saved
	// visibility, pin, operation or source state may change when it is refused.
	h.server.action(t, repoID, "keep")
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	beforeStatus := status
	beforeState, err := os.ReadFile(filepath.Join(h.state, "catalogue.json"))
	if err != nil {
		t.Fatal(err)
	}
	beforeIdentity := h.identity(t)
	beforeSession := h.session(t)
	held, err := os.Open(filepath.Join(repo, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	sleeper := exec.Command("/bin/sleep", "600")
	sleeper.Dir = repo
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	sleeperDone := make(chan error, 1)
	go func() { sleeperDone <- sleeper.Wait() }()
	sleeperStopped := false
	stopSleeper := func() {
		if sleeperStopped {
			return
		}
		sleeperStopped = true
		_ = sleeper.Process.Signal(syscall.SIGTERM)
		select {
		case <-sleeperDone:
		case <-time.After(5 * time.Second):
			t.Errorf("fixture cwd process PID %d did not stop; preserving native mount", sleeper.Process.Pid)
		}
	}
	t.Cleanup(stopSleeper)
	data, code := h.rawRequest(t, http.MethodPost, "/v1/repositories/visibility", map[string]any{"id": repoID, "enabled": false})
	if code != http.StatusBadRequest {
		t.Fatalf("busy normal detach did not refuse visibility change: %d %s", code, data)
	}
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	afterState, err := os.ReadFile(filepath.Join(h.state, "catalogue.json"))
	if err != nil || !bytes.Equal(beforeState, afterState) || !reflect.DeepEqual(beforeStatus.Repositories, status.Repositories) || !reflect.DeepEqual(beforeStatus.Organizations, status.Organizations) || !reflect.DeepEqual(beforeStatus.Operations, status.Operations) {
		t.Fatalf("busy refusal changed saved catalogue or repository operations: %v", err)
	}
	if !status.Mounted || h.identity(t) != beforeIdentity || h.session(t) != beforeSession {
		t.Fatal("busy refusal replaced or detached the active native session")
	}
	buffer := make([]byte, len(branchText))
	if n, err := held.ReadAt(buffer, 0); n != len(buffer) || err != nil || !bytes.Equal(buffer, branchText) {
		t.Fatalf("busy refusal invalidated retained file: n=%d err=%v bytes=%v", n, err, buffer)
	}
	stopSleeper()
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	// Every membership change must create a fresh connection for the same path
	// resource. Native FSKit 26 cannot publish this via cache invalidation.
	reconnect := func(path string, body any) {
		t.Helper()
		previous := h.session(t)
		h.request(t, http.MethodPost, path, body, nil)
		h.identity(t)
		if h.session(t) == previous {
			t.Fatal("catalogue change reused its old FSKit bridge session")
		}
	}
	reconnect("/v1/repositories/visibility", map[string]any{"id": repoID, "enabled": false})
	waitDesktopCataloguePathMissing(t, repo)
	reconnect("/v1/repositories/visibility", map[string]any{"id": repoID, "enabled": true})
	waitDesktopCataloguePathPresent(t, repo)
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), branchText)
	reconnect("/v1/organizations/settings", map[string]any{"owner": owner, "enabled": false})
	waitDesktopCataloguePathMissing(t, filepath.Join(h.mount, owner))
	reconnect("/v1/repositories/visibility", map[string]any{"id": repoID, "enabled": false})
	reconnect("/v1/organizations/settings", map[string]any{"owner": owner, "enabled": true})
	waitDesktopCataloguePathMissing(t, repo)
	reconnect("/v1/repositories/visibility", map[string]any{"id": repoID, "enabled": true})
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), branchText)

	previous := h.session(t)
	h.request(t, http.MethodPost, "/v1/unmount", nil, &status)
	if status.Mounted || h.resourceAttached(t) {
		t.Fatal("normal unmount did not detach the private resource")
	}
	h.request(t, http.MethodPost, "/v1/mount", nil, &status)
	h.identity(t)
	if h.session(t) == previous {
		t.Fatal("explicit remount reused the old bridge capability")
	}
	previous = h.session(t)
	if !h.stop(t) {
		t.Fatal("normal prepare-quit could not safely stop the private daemon")
	}
	h.start(t)
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	if !status.Mounted || !desktopAdoptedRepository(t, status, repoID).Pinned {
		t.Fatalf("restart lost desired mount or download intent: %+v", status)
	}
	h.identity(t)
	if h.session(t) == previous {
		t.Fatal("service restart reused the old bridge capability")
	}
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), branchText)
	fsKitReadEqual(t, filepath.Join(repo, "binary.dat"), binary)
	fsKitReadEqual(t, filepath.Join(repo, "branch-only.txt"), []byte("branch-only\n"))
	if branch := strings.TrimSpace(fsKitGit(t, repo, "branch", "--show-current")); branch != "acceptance" {
		t.Fatalf("restart branch=%q", branch)
	}
	fsKitCleanGit(t, repo)
	if after := snapshotDesktopAdoptionSource(t, source); !reflect.DeepEqual(beforeSource, after) {
		t.Fatal("mounted operations changed the original checkout bytes, modes, index, refs or configuration")
	}
	if _, err := os.Stat(ghSentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manual native workflow invoked GitHub CLI: %v", err)
	}
}

func fsKitAcceptanceModule(t *testing.T) {
	t.Helper()
	app := os.Getenv("AFS_FSKIT_APP")
	if os.Getenv("AFS_FSKIT_MODULE_CONFIRMED") != "1" || !filepath.IsAbs(app) {
		t.Fatal("prerequisite: AFS_FSKIT_APP must name the installed matching RepoReach.app and AFS_FSKIT_MODULE_CONFIRMED=1 must confirm its matching native module is already enabled; see docs/reporeach/fskit-acceptance.md")
	}
	module := filepath.Join(app, "Contents", "Extensions", "RepoReachFSKit.appex")
	plist := filepath.Join(module, "Contents", "Info.plist")
	read := func(key string) string {
		return strings.TrimSpace(run(t, "", "/usr/libexec/PlistBuddy", "-c", "Print :"+key, plist))
	}
	for key, wanted := range map[string]string{
		"CFBundleIdentifier": "com.enoughtools.reporeach.fskit",
		"EXAppExtensionAttributes:EXExtensionPointIdentifier": "com.apple.fskit.fsmodule",
		"EXAppExtensionAttributes:FSShortName":                "reporeach",
		"EXAppExtensionAttributes:FSSupportsPathURLs":         "true",
	} {
		if got := read(key); got != wanted {
			t.Fatalf("installed native module %s=%q want %q", key, got, wanted)
		}
	}
	executable := read("CFBundleExecutable")
	if executable == "" || filepath.Base(executable) != executable {
		t.Fatal("installed native module has invalid executable name")
	}
	if info, err := os.Stat(filepath.Join(module, "Contents", "MacOS", executable)); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed native module executable: %v, %v", info, err)
	}
	t.Logf("Darwin native acceptance uses already installed module %s version %s (%s); matching source and enablement explicitly confirmed by operator", module, read("CFBundleShortVersionString"), read("CFBundleVersion"))
}

type fsKitAcceptanceHarness struct {
	root, state, mount, gh, image string
	server                        *desktopAdoptionServer
	preserve                      bool
}

func (h *fsKitAcceptanceHarness) copyServerImage(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	h.image = filepath.Join(h.root, "test-engine")
	out, err := os.OpenFile(h.image, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy private daemon image: %v, %v", copyErr, closeErr)
	}
}

func (h *fsKitAcceptanceHarness) start(t *testing.T) {
	t.Helper()
	h.server = &desktopAdoptionServer{socket: filepath.Join(h.root, "control.sock"), mountRoot: h.mount, logPath: filepath.Join(h.root, "server.log"), done: make(chan error, 1)}
	s := h.server
	log, err := os.OpenFile(s.logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// No child test timeout: a failed normal detach must leave its owner alive.
	s.cmd = exec.Command(h.image, "-test.run=^TestDesktopAdoptionServer$", "-test.v", "-test.timeout=0")
	s.cmd.Stdout, s.cmd.Stderr = log, log
	s.cmd.Dir = h.root
	s.cmd.Env = append(os.Environ(), "AFS_E2E_DESKTOP_SERVER=1", "AFS_E2E_DESKTOP_STATE="+h.state, "AFS_E2E_DESKTOP_MOUNT="+h.mount, "AFS_E2E_DESKTOP_SOCKET="+s.socket, "AFS_E2E_DESKTOP_GH="+h.gh)
	if err := s.cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	t.Logf("private native fixture=%s daemon PID=%d socket=%s", h.root, s.cmd.Process.Pid, s.socket)
	go func() { err := s.cmd.Wait(); _ = log.Close(); s.done <- err }()
	waitForCondition(t, 60*time.Second, "private native daemon readiness", func() (bool, string) {
		select {
		case err := <-s.done:
			s.done <- err
			return false, fmt.Sprintf("daemon exited: %v; inspect %s", err, s.logPath)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, code, err := desktop.Request(ctx, s.socket, http.MethodGet, "/v1/status", nil)
		return err == nil && code == http.StatusOK, fmt.Sprintf("status=%d err=%v; log=%s", code, err, s.logPath)
	})
}

func (h *fsKitAcceptanceHarness) rawRequest(t *testing.T, method, path string, body any) ([]byte, int) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	data, code, err := desktop.Request(ctx, h.server.socket, method, path, data)
	if err != nil {
		t.Fatalf("native desktop %s %s: %v; log=%s", method, path, err, h.server.logPath)
	}
	return data, code
}

func (h *fsKitAcceptanceHarness) request(t *testing.T, method, path string, body, response any) {
	t.Helper()
	data, code := h.rawRequest(t, method, path, body)
	if code < 200 || code >= 300 {
		t.Fatalf("native desktop %s %s: %d %s; log=%s", method, path, code, data, h.server.logPath)
	}
	if response != nil {
		if err := json.Unmarshal(data, response); err != nil {
			t.Fatal(err)
		}
	}
}

// Cleanup only asks the owned service to detach normally. A busy/uncertain
// result preserves both the owner and its private resource for investigation.
func (h *fsKitAcceptanceHarness) stop(t *testing.T) (safe bool) {
	t.Helper()
	defer func() {
		if !safe {
			h.preserve = true
		}
	}()
	if h.preserve {
		return false
	}
	s := h.server
	if s == nil || s.stopped || s.cmd == nil || s.cmd.Process == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, code, err := desktop.Request(ctx, s.socket, http.MethodPost, "/v1/prepare-quit", nil)
	if err != nil || code != http.StatusOK {
		t.Errorf("normal prepare-quit failed (%d, %v); preserving fixture %s and daemon PID %d. Inspect %s; close fixture users and retry normal prepare-quit. No forced unmount or process kill was attempted", code, err, h.root, s.cmd.Process.Pid, s.logPath)
		return false
	}
	attached, err := h.attached()
	if err != nil || attached {
		t.Errorf("cannot confirm private resource detached: attached=%v err=%v; preserving %s and daemon PID %d", attached, err, h.root, s.cmd.Process.Pid)
		return false
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Errorf("stop detached private daemon PID %d: %v; preserving %s", s.cmd.Process.Pid, err, h.root)
		return false
	}
	select {
	case err := <-s.done:
		s.stopped = true
		if err != nil {
			t.Errorf("private daemon exit: %v; preserving log %s", err, s.logPath)
			return false
		}
		return true
	case <-time.After(30 * time.Second):
		t.Errorf("detached daemon PID %d did not stop; preserving %s without killing it", s.cmd.Process.Pid, h.root)
		return false
	}
}

func (h *fsKitAcceptanceHarness) cleanup(t *testing.T) {
	t.Helper()
	if !h.stop(t) {
		return
	}
	attached, err := h.attached()
	if err != nil || attached {
		t.Errorf("preserving fixture %s: resource still attached=%v or mount-table error=%v", h.root, attached, err)
		return
	}
	if err := os.RemoveAll(h.root); err != nil {
		t.Errorf("remove detached private fixture %s: %v", h.root, err)
	}
}

type fsKitAcceptanceIdentity struct {
	fsid               [2]int32
	owner              uint32
	root, source, kind string
}

func fsKitAcceptanceMounts() ([]fsKitAcceptanceIdentity, error) {
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	stats := make([]unix.Statfs_t, count+16)
	n, err := unix.Getfsstat(stats, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	if n >= len(stats) {
		return nil, errors.New("mount table changed beyond acceptance buffer; attachment uncertain")
	}
	identities := make([]fsKitAcceptanceIdentity, 0, n)
	for _, stat := range stats[:n] {
		identities = append(identities, fsKitAcceptanceIdentity{fsid: stat.Fsid.Val, owner: stat.Owner, root: unix.ByteSliceToString(stat.Mntonname[:]), source: unix.ByteSliceToString(stat.Mntfromname[:]), kind: unix.ByteSliceToString(stat.Fstypename[:])})
	}
	return identities, nil
}

func (h *fsKitAcceptanceHarness) sourceMatches(source string) bool {
	expected := filepath.Join(h.state, "FSKit")
	if source == expected {
		return true
	}
	u, err := url.Parse(source)
	return err == nil && u.Scheme == "file" && (u.Host == "" || u.Host == "localhost") && u.User == nil && u.Path == expected && u.RawQuery == "" && u.Fragment == "" && u.Opaque == ""
}

func (h *fsKitAcceptanceHarness) attached() (bool, error) {
	mounts, err := fsKitAcceptanceMounts()
	if err != nil {
		return false, err
	}
	for _, identity := range mounts {
		// Preserve the fixture if any volume is attached anywhere below it,
		// including an unexpected child mount. RemoveAll must not traverse one.
		if identity.root == h.root || strings.HasPrefix(identity.root, h.root+string(os.PathSeparator)) || h.sourceMatches(identity.source) {
			return true, nil
		}
	}
	return false, nil
}

func (h *fsKitAcceptanceHarness) resourceAttached(t *testing.T) bool {
	t.Helper()
	attached, err := h.attached()
	if err != nil {
		t.Fatal(err)
	}
	return attached
}

func (h *fsKitAcceptanceHarness) identity(t *testing.T) fsKitAcceptanceIdentity {
	t.Helper()
	mounts, err := fsKitAcceptanceMounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range mounts {
		if identity.root == h.mount && h.sourceMatches(identity.source) && identity.fsid != ([2]int32{}) && identity.kind != "" && identity.owner == uint32(os.Getuid()) {
			return identity
		}
	}
	t.Fatalf("kernel has no owned FSKit mount at %s using private resource %s", h.mount, filepath.Join(h.state, "FSKit"))
	return fsKitAcceptanceIdentity{}
}

func (h *fsKitAcceptanceHarness) session(t *testing.T) [32]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.state, "FSKit", "connection.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Compare session capability changes without ever printing its secret bytes.
	return sha256.Sum256(data)
}

func fsKitWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func fsKitReadEqual(t *testing.T, path string, expected []byte) {
	t.Helper()
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, expected) {
		t.Fatalf("native read %s: bytes=%v want=%v error=%v", path, data, expected, err)
	}
}

func fsKitGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", gitArgsWithSafeDirectory(dir, args...)...)
	command.Dir = dir
	// Fixtures and mounted Git are isolated from inherited repository bindings.
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_CONFIG", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS":
			continue
		}
		if strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			continue
		}
		command.Env = append(command.Env, value)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("fixture native git %v: %v\n%s", args, err, stderr.Bytes())
	}
	return stdout.String()
}

func fsKitCleanGit(t *testing.T, repo string) {
	t.Helper()
	if status := fsKitGit(t, repo, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("native Git tree is dirty: %q", status)
	}
}

func fsKitRetainedFile(t *testing.T, repo string) {
	t.Helper()
	path, renamed := filepath.Join(repo, "retained.tmp"), filepath.Join(repo, "renamed.tmp")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write([]byte("before")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("after!"), 0); err != nil {
		t.Fatal(err)
	}
	fsKitReadEqual(t, renamed, []byte("after!"))
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	waitDesktopCataloguePathMissing(t, renamed)
	if _, err := file.WriteAt([]byte("unlinked"), 0); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, len("unlinked"))
	if n, err := file.ReadAt(data, 0); n != len(data) || err != nil || !bytes.Equal(data, []byte("unlinked")) {
		t.Fatalf("retained unlinked read/write: n=%d err=%v bytes=%v", n, err, data)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	fsKitCleanGit(t, repo)
}
