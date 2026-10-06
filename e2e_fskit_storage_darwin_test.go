//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/desktop"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

// This is a separate disposable fixture, intended to run after the primary
// mounted sequence passes. Preparation acquires a clone; Keep must acquire its
// still-missing blobs. There are no mounted repository lookups before that proof.
func TestFSKitMountedColdStorageAcceptance(t *testing.T) {
	if os.Getenv("AFS_RUN_FSKIT_E2E_TESTS") != "1" {
		t.Skip("set AFS_RUN_FSKIT_E2E_TESTS=1 for real mounted FSKit storage acceptance")
	}
	release, err := unix.Sysctl("kern.osrelease")
	if err != nil {
		t.Fatal(err)
	}
	major, err := strconv.Atoi(strings.Split(release, ".")[0])
	if err != nil || major < 25 {
		t.Skipf("mounted storage acceptance requires macOS 26+; Darwin %s supplies no mounted proof", release)
	}
	prerequisites := fsKitAcceptanceModule(t)
	root, err := os.MkdirTemp("/tmp", "rr-cold-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	h := &fsKitAcceptanceHarness{root: root, state: filepath.Join(root, "state"), mount: filepath.Join(root, "mount"), prerequisites: prerequisites}
	t.Cleanup(func() { h.cleanup(t) })
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "global-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_OPTIONAL_LOCKS", "0")
	t.Setenv("GH_CONFIG_DIR", filepath.Join(root, "gh-config"))
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost,::1")
	t.Setenv("no_proxy", "127.0.0.1,localhost,::1")
	ghSentinel := filepath.Join(root, "gh-called")
	t.Setenv("AFS_E2E_DESKTOP_GH_SENTINEL", ghSentinel)
	h.gh = filepath.Join(root, "gh")
	fsKitWrite(t, h.gh, []byte("#!/bin/sh\nprintf '%s\\n' called >> \"$AFS_E2E_DESKTOP_GH_SENTINEL\"\nexit 99\n"), 0o700)

	// HTTP preserves a real filtered transport. Desktop adoption normalizes
	// file:// to local paths, whose clone optimization may copy every blob.
	source, bare := filepath.Join(root, "original"), filepath.Join(root, "remote.git")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	fsKitStorageGit(t, source, nil, "init", "--initial-branch=trunk")
	fsKitStorageGit(t, source, nil, "config", "user.name", "Cold fixture")
	fsKitStorageGit(t, source, nil, "config", "user.email", "fixture@example.invalid")
	text, binary := []byte("cold committed text\n"), []byte{0, 255, 128, '\n', 0, 254, 1, 2}
	files := map[string][]byte{"tracked.txt": text, "binary.dat": binary, "duplicate.dat": binary}
	for name, value := range files {
		fsKitWrite(t, filepath.Join(source, name), value, 0o644)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	fsKitStorageGit(t, source, nil, "add", ".")
	fsKitStorageGit(t, source, nil, "commit", "-m", "cold fixture")
	head := strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-parse", "HEAD")))
	unique := make(map[string][]byte)
	for name, value := range files {
		oid := strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-parse", "HEAD:"+name)))
		unique[oid] = value
	}
	linkOID := strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-parse", "HEAD:link")))
	unique[linkOID] = []byte("tracked.txt")
	if len(unique) != 3 {
		t.Fatalf("fixture needs three unique blobs, got %d", len(unique))
	}
	fsKitStorageGit(t, root, nil, "clone", "--bare", source, bare)
	fsKitStorageGit(t, bare, nil, "config", "uploadpack.allowFilter", "true")
	fsKitWrite(t, filepath.Join(source, "tracked.txt"), []byte("staged original\n"), 0o644)
	fsKitStorageGit(t, source, nil, "add", "tracked.txt")
	fsKitWrite(t, filepath.Join(source, "tracked.txt"), []byte("dirty original\n"), 0o644)
	fsKitWrite(t, filepath.Join(source, "untracked.txt"), []byte("untracked original\n"), 0o644)
	beforeSource, beforeBare := snapshotDesktopAdoptionSource(t, source), snapshotDesktopAdoptionSource(t, bare)
	transport := fsKitStorageHTTP(t, h)

	h.copyServerImage(t)
	h.start(t)
	const owner, id = "ColdAcceptance", "ColdAcceptance/project"
	repo := filepath.Join(h.mount, owner, "project")
	var status desktop.Status
	h.request(t, http.MethodPost, "/v1/repositories/adopt", desktop.AdoptionRequest{RemoteURL: transport.URL + "/remote.git", Owner: owner, Name: "project"}, &status)
	if status.Account != nil || desktopAdoptedRepository(t, status, id).State != "virtual" {
		t.Fatalf("cold adoption required an account or acquired a tree: %+v", status)
	}
	h.request(t, http.MethodPost, "/v1/mount", nil, &status)
	h.identity(t)
	fsKitStorageAction(t, h, id, "prepare")
	paths, err := fsKitStorageRegistry(h.state, repo)
	if err != nil {
		t.Fatal(err)
	}
	index := fsKitStorageRead(t, filepath.Join(paths.git, "index"))
	if got := strings.TrimSpace(string(fsKitStorageGit(t, root, nil, "--git-dir", paths.git, "rev-parse", "HEAD"))); got != head {
		t.Fatalf("prepared HEAD=%s want %s", got, head)
	}
	entries, err := os.ReadDir(paths.cache)
	if err != nil && !errors.Is(err, os.ErrNotExist) || len(entries) != 0 {
		t.Fatalf("pre-Keep blob cache is not empty: count=%d error=%v", len(entries), err)
	}
	oids := fsKitStorageOIDs(unique)
	input := []byte(strings.Join(oids, "\n") + "\n")
	output := fsKitStorageGitNoFetch(t, root, input, "--git-dir", paths.git, "cat-file", "--batch-check")
	if err := fsKitStorageRequireMissing(output, oids); err != nil {
		t.Fatalf("cold prerequisite failed: %v", err)
	}
	t.Logf("cold proof: %d missing Git blobs and empty cache; no mounted repository reads before Keep", len(oids))

	operation := fsKitStorageAction(t, h, id, "keep")
	wantBytes := int64(0)
	for oid, value := range unique {
		wantBytes += int64(len(value))
		fsKitReadEqual(t, filepath.Join(paths.cache, oid), value)
	}
	if operation.TotalBlobs != int64(len(unique)) || operation.CompletedBlobs != operation.TotalBlobs || operation.TotalBytes != wantBytes || operation.DownloadedBytes != wantBytes {
		t.Fatalf("Keep did not count exact unique committed bytes: %+v; want blobs=%d bytes=%d", operation, len(unique), wantBytes)
	}
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	pinned := desktopAdoptedRepository(t, status, id)
	if !pinned.Pinned || pinned.State != "pinned" || pinned.DownloadedBytes != wantBytes || !bytes.Equal(index, fsKitStorageRead(t, filepath.Join(paths.git, "index"))) {
		t.Fatalf("Keep did not preserve index and persist exact pin accounting: %+v", pinned)
	}
	fsKitStorageDurablePolicy(t, h, id, true, wantBytes, "pinned")

	// Keep the exact listener/URL allocated, but refuse all transport requests.
	// Restart and cache reads must generate no requests at all, even failed ones.
	transport.online.Store(false)
	beforeRequests := transport.requests.Load()
	previous := h.session(t)
	if !h.stop(t) {
		t.Fatal("could not normally detach before offline restart")
	}
	h.start(t)
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	if !status.Mounted || !desktopAdoptedRepository(t, status, id).Pinned || h.session(t) == previous {
		t.Fatalf("offline restart lost mounted pin intent or reused session: %+v", status)
	}
	h.identity(t)
	fsKitStorageTree(t, repo, files, head)
	if transport.requests.Load() != beforeRequests {
		t.Fatalf("offline restart/reads contacted source: before=%d after=%d denied=%d", beforeRequests, transport.requests.Load(), transport.denied.Load())
	}
	if !bytes.Equal(index, fsKitStorageRead(t, filepath.Join(paths.git, "index"))) {
		t.Fatal("offline restart/read changed the private index")
	}

	// Native metadata alone must not prevent safe eviction, and must not turn
	// into tracked/untracked AppleDouble files. Never add local Git config here.
	xattrs := []fsKitXattrExpectation{fsKitProbeNativeXattrs(t, fsKitXattrTarget{path: filepath.Join(repo, "tracked.txt")}, "cold committed metadata")}
	fsKitAssertNativeXattrs(t, "before clean Free", xattrs)
	fsKitCleanGit(t, repo)
	beforeOverlay, err := os.Stat(paths.overlay)
	if err != nil {
		t.Fatal(err)
	}
	transport.online.Store(true)
	previous = h.session(t)
	fsKitStorageAction(t, h, id, "free")
	h.identity(t)
	if h.session(t) == previous {
		t.Fatal("clean Free did not reconnect the native catalogue")
	}
	// These inspections are private-state/API only. A mounted lookup would
	// legitimately reacquire the virtual entry and invalidate eviction evidence.
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	freed := desktopAdoptedRepository(t, status, id)
	if !status.Mounted || freed.State != "virtual" || freed.Pinned || freed.DownloadedBytes != 0 {
		t.Fatalf("clean Free lost catalogue or retained pin/accounting: %+v", status)
	}
	fsKitStorageDurablePolicy(t, h, id, false, 0, "virtual")
	if _, err := fsKitStorageRegistry(h.state, repo); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("clean Free did not remove engine registration: %v", err)
	}
	for _, path := range []string{paths.git, paths.cache, paths.snapshot} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recoverable private storage remains after clean Free: %s error=%v", path, err)
		}
	}
	afterOverlay, err := os.Stat(paths.overlay)
	if err != nil || !os.SameFile(beforeOverlay, afterOverlay) {
		t.Fatalf("clean Free replaced retained metadata directory: %v", err)
	}
	if info, err := os.Stat(paths.overlayDB); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("clean Free removed retained metadata database: %v", err)
	}
	beforeReacquire := transport.requests.Load()
	fsKitStorageAction(t, h, id, "prepare")
	if transport.requests.Load() <= beforeReacquire {
		t.Fatal("reacquisition did not read the source after managed storage was removed")
	}
	fsKitStorageTree(t, repo, files, head)
	fsKitAssertNativeXattrs(t, "after reacquisition", xattrs)
	fsKitCleanGit(t, repo)
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	if restored := desktopAdoptedRepository(t, status, id); restored.Pinned || restored.DownloadedBytes != 0 {
		t.Fatalf("prepare unexpectedly restored discarded pin intent: %+v", restored)
	}
	fsKitStorageDurablePolicy(t, h, id, false, 0, "available")
	if !reflect.DeepEqual(beforeSource, snapshotDesktopAdoptionSource(t, source)) || !reflect.DeepEqual(beforeBare, snapshotDesktopAdoptionSource(t, bare)) {
		t.Fatal("cold Keep, serving, Free or reacquisition changed original source/index or bare source bytes/modes/refs/config")
	}
	if _, err := os.Stat(ghSentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cold native Git workflow invoked GitHub CLI: %v", err)
	}
	t.Logf("storage acceptance completed: uniqueBlobs=%d downloadedBytes=%d offlineRequests=0 cleanFree=true nativeMetadataRetained=true", len(unique), wantBytes)
}

// Only upload-pack is exported; paths cannot reach the fixture's private state.
// Tests inject a passive HTTP handler to exercise these controls without any
// listener, Git backend, signed helper, account, app or mount.
type fsKitStorageTransport struct {
	URL      string
	online   atomic.Bool
	requests atomic.Int64
	denied   atomic.Int64
	backend  http.Handler
}

func (s *fsKitStorageTransport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	if !s.online.Load() {
		s.denied.Add(1)
		http.Error(w, "disposable source is offline", http.StatusServiceUnavailable)
		return
	}
	query := r.URL.Query()
	service := query["service"]
	allowed := r.Method == http.MethodGet && r.URL.Path == "/remote.git/info/refs" && len(query) == 1 && len(service) == 1 && service[0] == "git-upload-pack" ||
		r.Method == http.MethodPost && r.URL.Path == "/remote.git/git-upload-pack" && len(query) == 0
	if !allowed {
		http.Error(w, "fixture exports upload-pack only", http.StatusForbidden)
		return
	}
	s.backend.ServeHTTP(w, r)
}

func fsKitStorageHTTP(t *testing.T, h *fsKitAcceptanceHarness) *fsKitStorageTransport {
	t.Helper()
	root := h.root
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(root, "source-http.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	backend := &fsKitStorageBackend{git: git, root: root, logger: log.New(logFile, "", 0), stderr: logFile}
	transport := &fsKitStorageTransport{backend: backend}
	transport.online.Store(true)
	server := httptest.NewUnstartedServer(transport)
	server.Config.ReadTimeout, server.Config.WriteTimeout, server.Config.IdleTimeout = 30*time.Second, 30*time.Second, 5*time.Second
	server.Start() // httptest binds a private ephemeral 127.0.0.1 listener.
	transport.URL = server.URL
	t.Cleanup(func() {
		// Reach the identity-checked detach/preserve decision before waiting for
		// source handlers. The earlier h.cleanup runs after this bounded drain.
		_ = h.stop(t)
		server.Close()
		_ = logFile.Close()
	})
	endpoint, err := url.Parse(server.URL)
	if err != nil || net.ParseIP(endpoint.Hostname()) == nil || !net.ParseIP(endpoint.Hostname()).IsLoopback() {
		t.Fatal("disposable Git transport did not bind a loopback address")
	}
	return transport
}

// The standard CGI handler does not cancel/reap children with request context.
// Keep this fixture's read-only backend bounded so source cleanup cannot prevent
// the filesystem owner from reaching its ordinary detach/preserve decision.
type fsKitStorageBackend struct {
	git, root string
	logger    *log.Logger
	stderr    io.Writer
}

func (b *fsKitStorageBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, b.git, "http-backend")
	command.Dir, command.Stderr = b.root, b.stderr
	command.Stdin = http.MaxBytesReader(w, r.Body, 1<<20)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_PROJECT_ROOT=" + b.root, "GIT_HTTP_EXPORT_ALL=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(b.root, "global-config"), "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0",
		"REQUEST_METHOD=" + r.Method, "QUERY_STRING=" + r.URL.RawQuery, "PATH_INFO=" + r.URL.Path,
		"CONTENT_TYPE=" + r.Header.Get("Content-Type"), "CONTENT_LENGTH=" + strconv.FormatInt(r.ContentLength, 10),
		"HTTP_GIT_PROTOCOL=" + r.Header.Get("Git-Protocol"), "SERVER_PROTOCOL=HTTP/1.1", "GATEWAY_INTERFACE=CGI/1.1",
	}
	if gitExecPath := os.Getenv("GIT_EXEC_PATH"); gitExecPath != "" {
		command.Env = append(command.Env, "GIT_EXEC_PATH="+gitExecPath)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := unix.Kill(-command.Process.Pid, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 2 * time.Second
	stdout, err := command.StdoutPipe()
	if err != nil {
		http.Error(w, "disposable Git backend unavailable", http.StatusInternalServerError)
		return
	}
	if err := command.Start(); err != nil {
		_ = stdout.Close()
		http.Error(w, "disposable Git backend could not start", http.StatusInternalServerError)
		return
	}
	defer func() {
		_ = stdout.Close()
		if err := command.Wait(); err != nil {
			b.logger.Printf("disposable Git backend exit: %s", auth.RedactLogString(err.Error()))
		}
	}()
	reader := bufio.NewReader(stdout)
	headers, status, err := fsKitStorageCGIHeaders(reader)
	if err != nil {
		cancel()
		http.Error(w, "disposable Git backend returned invalid headers", http.StatusBadGateway)
		return
	}
	for name, values := range headers {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(status)
	// Only headers are parsed as text; Git pack bytes stream directly.
	written, copyErr := io.Copy(w, io.LimitReader(reader, (16<<20)+1))
	if copyErr != nil || written > 16<<20 {
		cancel()
		b.logger.Printf("disposable Git body stream failed or exceeded bound")
	}
}

func fsKitStorageCGIHeaders(reader *bufio.Reader) (http.Header, int, error) {
	var raw bytes.Buffer
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil || raw.Len()+len(line) > 8192 {
			return nil, 0, errors.New("invalid or oversized Git CGI headers")
		}
		raw.Write(line)
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			break
		}
	}
	headers, err := textproto.NewReader(bufio.NewReader(&raw)).ReadMIMEHeader()
	if err != nil {
		return nil, 0, err
	}
	status := http.StatusOK
	if value := headers.Get("Status"); value != "" {
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return nil, 0, errors.New("empty Git CGI status")
		}
		status, err = strconv.Atoi(fields[0])
		if err != nil || status < 100 || status > 599 {
			return nil, 0, errors.New("invalid Git CGI status")
		}
	}
	headers.Del("Status")
	return http.Header(headers), status, nil
}

func fsKitStorageAction(t *testing.T, h *fsKitAcceptanceHarness, id, action string) desktop.Operation {
	t.Helper()
	var accepted struct {
		Operation desktop.Operation `json:"operation"`
	}
	h.request(t, http.MethodPost, "/v1/repositories/action", map[string]any{"id": id, "action": action}, &accepted)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var status desktop.Status
		h.request(t, http.MethodGet, "/v1/status", nil, &status)
		for _, operation := range status.Operations {
			if operation.ID != accepted.Operation.ID {
				continue
			}
			switch operation.Status {
			case "complete":
				t.Logf("storage action=%s operation=%s complete blobs=%d/%d bytes=%d/%d", action, operation.ID, operation.CompletedBlobs, operation.TotalBlobs, operation.DownloadedBytes, operation.TotalBytes)
				return operation
			case "failed", "canceled":
				t.Fatalf("storage action=%s operation=%s status=%s error=%s", action, operation.ID, operation.Status, operation.Error)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("storage action %s operation %s did not complete; inspect %s", action, accepted.Operation.ID, h.server.logPath)
	return desktop.Operation{}
}

type fsKitStoragePaths struct {
	git, cache, snapshot, overlay, overlayDB string
}

func fsKitStorageRegistry(state, mountPath string) (fsKitStoragePaths, error) {
	path := filepath.Join(state, "engine", "config", "repos.sqlite")
	if _, err := os.Stat(path); err != nil {
		return fsKitStoragePaths{}, err
	}
	// Opening through registry.New would run writable migrations. A live WAL
	// reader must use mode=ro, not immutable=1 or a main-database-only copy.
	query := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "busy_timeout(5000)"}}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String())
	if err != nil {
		return fsKitStoragePaths{}, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var paths fsKitStoragePaths
	err = db.QueryRowContext(ctx, "SELECT git_dir,blob_cache_dir,meta_db_path,overlay_dir,overlay_db_path FROM repos WHERE mount_path=?", mountPath).
		Scan(&paths.git, &paths.cache, &paths.snapshot, &paths.overlay, &paths.overlayDB)
	return paths, err
}

func fsKitStorageDurablePolicy(t *testing.T, h *fsKitAcceptanceHarness, id string, pinned bool, size int64, state string) {
	t.Helper()
	var catalogue struct {
		MountDesired bool                 `json:"mountDesired"`
		Repositories []desktop.Repository `json:"repositories"`
	}
	if err := json.Unmarshal(fsKitStorageRead(t, filepath.Join(h.state, "catalogue.json")), &catalogue); err != nil {
		t.Fatal(err)
	}
	if !catalogue.MountDesired {
		t.Fatal("storage operation lost durable desired mount")
	}
	for _, repository := range catalogue.Repositories {
		if repository.ID == id {
			if repository.Pinned != pinned || repository.DownloadedBytes != size || repository.State != state {
				t.Fatalf("durable repository policy %+v; want pinned=%v bytes=%d state=%s", repository, pinned, size, state)
			}
			return
		}
	}
	t.Fatalf("storage operation removed durable catalogue entry %s", id)
}

func fsKitStorageGit(t *testing.T, dir string, input []byte, args ...string) []byte {
	t.Helper()
	return fsKitStorageGitOutput(t, dir, input, nil, args...)
}

func fsKitStorageGitNoFetch(t *testing.T, dir string, input []byte, args ...string) []byte {
	t.Helper()
	return fsKitStorageGitOutput(t, dir, input, []string{"GIT_NO_LAZY_FETCH=1"}, args...)
}

func fsKitStorageGitOutput(t *testing.T, dir string, input []byte, extra []string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", gitArgsWithSafeDirectory(dir, args...)...)
	command.Dir, command.Stdin = dir, bytes.NewReader(input)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "GIT_") && name != "GIT_CONFIG_GLOBAL" && name != "GIT_CONFIG_NOSYSTEM" && name != "GIT_OPTIONAL_LOCKS" && name != "GIT_EXEC_PATH" {
			continue
		}
		command.Env = append(command.Env, value)
	}
	command.Env = append(command.Env, extra...)
	stdout, stderr := &fsKitInspectorOutput{}, &fsKitInspectorOutput{}
	command.Stdout, command.Stderr, command.WaitDelay = stdout, stderr, 2*time.Second
	if err := command.Run(); err != nil || stdout.truncated || stderr.truncated {
		t.Fatalf("bounded fixture Git %v failed: %v; stderr=%s truncated=%v", args, err, auth.RedactLogString(string(stderr.data)), stdout.truncated || stderr.truncated)
	}
	return stdout.data
}

func fsKitStorageRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fsKitStorageOIDs(blobs map[string][]byte) []string {
	oids := make([]string, 0, len(blobs))
	for oid := range blobs {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	return oids
}

func fsKitStorageRequireMissing(output []byte, expected []string) error {
	lines := bytes.Split(bytes.TrimSuffix(output, []byte("\n")), []byte("\n"))
	if len(lines) != len(expected) || len(expected) == 0 {
		return fmt.Errorf("batch-check reported %d rows for %d expected cold blobs", len(lines), len(expected))
	}
	for index, oid := range expected {
		if !bytes.Equal(lines[index], []byte(oid+" missing")) {
			return fmt.Errorf("blob %s is not verifiably absent without lazy fetching", oid)
		}
	}
	return nil
}

func fsKitStorageTree(t *testing.T, repo string, files map[string][]byte, head string) {
	t.Helper()
	for name, value := range files {
		fsKitReadEqual(t, filepath.Join(repo, name), value)
	}
	if target, err := os.Readlink(filepath.Join(repo, "link")); err != nil || target != "tracked.txt" {
		t.Fatalf("offline/reacquired symlink target=%q error=%v", target, err)
	}
	fsKitReadEqual(t, filepath.Join(repo, "link"), files["tracked.txt"])
	if got := strings.TrimSpace(fsKitGit(t, repo, "rev-parse", "HEAD")); got != head {
		t.Fatalf("offline/reacquired HEAD=%s want %s", got, head)
	}
	fsKitCleanGit(t, repo)
}

func TestFSKitColdMissingBlobEvidence(t *testing.T) {
	for _, test := range []struct {
		name, output string
		expected     []string
		valid        bool
	}{
		{"all missing", "one missing\ntwo missing\n", []string{"one", "two"}, true},
		{"already warm", "one blob 8\n", []string{"one"}, false},
		{"partial report", "one missing\n", []string{"one", "two"}, false},
		{"wrong order or duplicate", "one missing\none missing\n", []string{"one", "two"}, false},
		{"unexpected rows", "one missing\ntwo missing\n", []string{"one"}, false},
		{"no blobs cannot prove cold", "", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := fsKitStorageRequireMissing([]byte(test.output), test.expected); (err == nil) != test.valid {
				t.Fatalf("cold evidence accepted=%v want %v error=%v", err == nil, test.valid, err)
			}
		})
	}
}

func TestFSKitColdHTTPGate(t *testing.T) {
	for _, test := range []struct {
		name, method, target string
		online               bool
		status               int
		backend              bool
	}{
		{"upload advertisement", "GET", "/remote.git/info/refs?service=git-upload-pack", true, 200, true},
		{"upload request", "POST", "/remote.git/git-upload-pack", true, 200, true},
		{"receive advertisement refused", "GET", "/remote.git/info/refs?service=git-receive-pack", true, 403, false},
		{"receive request refused", "POST", "/remote.git/git-receive-pack", true, 403, false},
		{"duplicate service refused", "GET", "/remote.git/info/refs?service=git-upload-pack&service=git-receive-pack", true, 403, false},
		{"private state refused", "GET", "/state/catalogue.json", true, 403, false},
		{"other repository refused", "GET", "/other.git/info/refs?service=git-upload-pack", true, 403, false},
		{"offline holds transport URL", "GET", "/remote.git/info/refs?service=git-upload-pack", false, 503, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			transport := &fsKitStorageTransport{backend: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})}
			transport.online.Store(test.online)
			recorder := httptest.NewRecorder()
			transport.ServeHTTP(recorder, httptest.NewRequest(test.method, test.target, nil))
			wantDenied := int64(0)
			if !test.online {
				wantDenied = 1
			}
			if recorder.Code != test.status || called != test.backend || transport.requests.Load() != 1 || transport.denied.Load() != wantDenied {
				t.Fatalf("HTTP gate status=%d backend=%v requests=%d denied=%d", recorder.Code, called, transport.requests.Load(), transport.denied.Load())
			}
		})
	}
}

func TestFSKitColdCGIHeaders(t *testing.T) {
	for _, test := range []struct {
		name, headers string
		status        int
		valid         bool
	}{
		{"ordinary binary Git response", "Content-Type: application/x-git-upload-pack-result\r\n\r\n", 200, true},
		{"explicit unavailable response", "Status: 503 Offline\nContent-Type: text/plain\n\n", 503, true},
		{"invalid status", "Status: 99 bad\n\n", 0, false},
		{"malformed header", "bad header\n\n", 0, false},
		{"missing separator", "Content-Type: text/plain", 0, false},
		{"oversized header line", "Content-Type: " + strings.Repeat("x", 8192) + "\n\n", 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte{0, 255, 128, '\r', '\n', 0, 1}
			reader := bufio.NewReader(bytes.NewReader(append([]byte(test.headers), body...)))
			headers, status, err := fsKitStorageCGIHeaders(reader)
			if (err == nil) != test.valid || status != test.status {
				t.Fatalf("CGI header accepted=%v status=%d want valid=%v status=%d error=%v", err == nil, status, test.valid, test.status, err)
			}
			if test.valid {
				if headers.Get("Status") != "" {
					t.Fatal("CGI status escaped into HTTP headers")
				}
				remaining, err := io.ReadAll(reader)
				if err != nil || !bytes.Equal(remaining, body) {
					t.Fatalf("CGI metadata parsing consumed/changed binary body: %v", err)
				}
			}
		})
	}
}
