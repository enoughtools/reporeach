package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// macOS limits Unix socket paths to 104 bytes. Go's normal per-test temporary
// directory includes the entire test name and can exceed that limit.
func shortSocketDirectory(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rr-http-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove socket fixture: %v", err)
		}
	})
	return dir
}

func desktopHTTPOptions(t *testing.T) Options {
	t.Helper()
	dir := shortSocketDirectory(t)
	gh := fakeGitHub(t, "exit 4")
	return Options{StateDir: filepath.Join(dir, "state"), Socket: filepath.Join(dir, "control.sock"), MountRoot: filepath.Join(dir, "repositories"), GHPath: gh.path}
}

func serveExpectedFailure(t *testing.T, opts Options) error {
	t.Helper()
	// A regression that starts listening instead of refusing the path must fail
	// this test within a bounded interval, rather than hang the entire suite.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return Serve(ctx, opts)
}

type desktopHTTPFixture struct {
	opts   Options
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	err    error
}

func startDesktopHTTP(t *testing.T, opts Options) *desktopHTTPFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fixture := &desktopHTTPFixture{opts: opts, cancel: cancel, done: make(chan error, 1)}
	go func() { fixture.done <- Serve(ctx, opts) }()
	t.Cleanup(func() { fixture.stop(t) })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-fixture.done:
			fixture.done <- err
			t.Fatalf("desktop HTTP server exited during startup: %v", err)
		default:
		}
		if info, err := os.Lstat(opts.Socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			// An intentionally stale socket may already exist. Readiness requires
			// an actual status response from the replacement service.
			requestCtx, stopRequest := context.WithTimeout(context.Background(), 200*time.Millisecond)
			_, status, requestErr := Request(requestCtx, opts.Socket, http.MethodGet, "/v1/status", nil)
			stopRequest()
			if requestErr == nil && status == http.StatusOK {
				return fixture
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("desktop HTTP server did not create its control socket")
	return nil
}

func (f *desktopHTTPFixture) stop(t *testing.T) {
	t.Helper()
	f.once.Do(func() {
		f.cancel()
		select {
		case f.err = <-f.done:
		case <-time.After(3 * time.Second):
			f.err = errors.New("desktop HTTP server did not stop after cancellation")
		}
	})
	if f.err != nil {
		t.Error(f.err)
	}
}

func TestServePrivateSocketLifecycleAndSingleOwner(t *testing.T) {
	opts := desktopHTTPOptions(t)
	fixture := startDesktopHTTP(t, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data, status, err := Request(ctx, opts.Socket, http.MethodGet, "/v1/status", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status request = %d, %s, error = %v", status, data, err)
	}
	var response Status
	if err := json.Unmarshal(data, &response); err != nil || response.Version != Version || response.MountRoot != opts.MountRoot {
		t.Fatalf("unexpected status response: %+v, error = %v", response, err)
	}
	for path, mode := range map[string]os.FileMode{opts.StateDir: 0o700, opts.Socket: 0o600, filepath.Join(opts.StateDir, "service.lock"): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private path %q has wrong permissions, error = %v", filepath.Base(path), err)
		}
	}
	duplicateState := opts
	duplicateState.Socket = filepath.Join(filepath.Dir(opts.Socket), "second.sock")
	if err := serveExpectedFailure(t, duplicateState); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("duplicate state owner error = %v", err)
	}
	duplicateSocket := opts
	duplicateSocket.StateDir = filepath.Join(filepath.Dir(opts.Socket), "other-state")
	if err := serveExpectedFailure(t, duplicateSocket); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("duplicate socket owner error = %v", err)
	}
	fixture.stop(t)
	if _, err := os.Lstat(opts.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shutdown retained control socket: %v", err)
	}
	// Shutdown releases the state lock, so restarting uses the same persistent
	// catalogue instead of requiring manual lock-file or socket removal.
	restarted := startDesktopHTTP(t, opts)
	if _, status, err := Request(ctx, opts.Socket, http.MethodGet, "/v1/status", nil); err != nil || status != http.StatusOK {
		t.Fatalf("restarted status = %d, error = %v", status, err)
	}
	restarted.stop(t)
}

func TestServeReplacesStaleOwnedSocket(t *testing.T) {
	opts := desktopHTTPOptions(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: opts.Socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(opts.Socket); err != nil {
		t.Fatal("fixture did not retain stale socket")
	}
	fixture := startDesktopHTTP(t, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, status, err := Request(ctx, opts.Socket, http.MethodGet, "/v1/status", nil); err != nil || status != http.StatusOK {
		t.Fatalf("replacement socket status = %d, error = %v", status, err)
	}
	fixture.stop(t)
}

func TestServeRefusesNonSocketAndUnsafeLockPaths(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "directory", "lock symlink", "lock directory", "public socket directory"} {
		t.Run(kind, func(t *testing.T) {
			opts := desktopHTTPOptions(t)
			if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			protected := opts.Socket
			switch kind {
			case "file":
				if err := os.WriteFile(opts.Socket, []byte("user data"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(opts.StateDir, opts.Socket); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(opts.Socket, 0o700); err != nil {
					t.Fatal(err)
				}
			case "lock symlink":
				protected = filepath.Join(opts.StateDir, "service.lock")
				if err := os.Symlink(opts.StateDir, protected); err != nil {
					t.Fatal(err)
				}
			case "lock directory":
				protected = filepath.Join(opts.StateDir, "service.lock")
				if err := os.Mkdir(protected, 0o700); err != nil {
					t.Fatal(err)
				}
			case "public socket directory":
				protected = filepath.Dir(opts.Socket)
				if err := os.Chmod(protected, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(protected)
			if err != nil {
				t.Fatal(err)
			}
			if err := serveExpectedFailure(t, opts); err == nil {
				t.Fatal("unsafe service path was accepted")
			}
			after, err := os.Lstat(protected)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("rejected startup replaced an existing path")
			}
			if kind == "file" {
				data, err := os.ReadFile(opts.Socket)
				if err != nil || string(data) != "user data" {
					t.Fatal("rejected startup changed user's data")
				}
			}
		})
	}
}

func TestServeRequiresAbsoluteStateAndSocket(t *testing.T) {
	for _, change := range []func(*Options){func(o *Options) { o.StateDir = "state" }, func(o *Options) { o.Socket = "service.sock" }} {
		opts := desktopHTTPOptions(t)
		change(&opts)
		if err := serveExpectedFailure(t, opts); err == nil {
			t.Fatal("relative service path was accepted")
		}
	}
}

func TestHandlerStatusAndErrorsAreStructuredUncachedJSON(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	tests := []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/v1/status", "", http.StatusOK},
		{http.MethodGet, "/v1/auth/status", "", http.StatusOK},
		{http.MethodGet, "/v1/status?token=secret", "", http.StatusBadRequest},
		{http.MethodDelete, "/v1/status", "", http.StatusNotFound},
		{http.MethodGet, "/v1/discover", "", http.StatusNotFound},
		{http.MethodGet, "/v1/unknown", "", http.StatusNotFound},
		{http.MethodPost, "/v1/discover", "", http.StatusBadRequest},
		{http.MethodPost, "/v1/settings", `{"mountRoot":"relative"}`, http.StatusBadRequest},
		{http.MethodPost, "/v1/repositories/action", `{"id":"missing/repo","action":"keep"}`, http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, request)
			if response.Code != test.status || response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" || !json.Valid(response.Body.Bytes()) {
				t.Fatalf("unexpected HTTP response: %d, %+v, %s", response.Code, response.Header(), response.Body.String())
			}
			if test.status >= 400 {
				var apiError map[string]string
				if err := json.Unmarshal(response.Body.Bytes(), &apiError); err != nil || apiError["error"] == "" {
					t.Fatal("HTTP failure did not return a structured error")
				}
			}
		})
	}
}

func TestHandlerRejectsMalformedObjectsWithoutSideEffects(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	seedDesktopCatalogue(t, s, catalogueRepository("octocat", "repo"))
	before := s.Status()
	bodies := []string{"", "{", "null", "[]", `"text"`, `{"unknown":true}`, `{} {}`, `{} trailing`, `{"mountRoot":123}`, `{"mountRoot":"` + strings.Repeat("a", requestBodyLimit) + `"}`}
	for _, path := range []string{"/v1/settings", "/v1/repositories/action"} {
		for index, body := range bodies {
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
			if response.Code != http.StatusBadRequest || !json.Valid(response.Body.Bytes()) {
				t.Fatalf("malformed body %d at %s returned %d", index, path, response.Code)
			}
		}
	}
	after := s.Status()
	if after.MountRoot != before.MountRoot || len(after.Repositories) != len(before.Repositories) || len(after.Operations) != 0 {
		t.Fatalf("malformed requests changed service state: %+v", after)
	}
}

func TestHandlerAcceptsRepositoryActionAndReturnsOperation(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo := catalogueRepository("octocat", "repo")
	seedDesktopCatalogue(t, s, repo)
	unlock, err := s.lockRepo(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/repositories/action", strings.NewReader(`{"id":"octocat/repo","action":"keep"}`)))
	var result struct {
		Operation Operation `json:"operation"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusAccepted || result.Operation.RepositoryID != repo.ID || result.Operation.Status != "running" {
		t.Fatalf("action response = %d, %+v, error = %v", response.Code, result, err)
	}
	if _, err := s.Action(repo.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	awaitDesktop(t, func() bool { return s.Status().Operations[0].Status == "canceled" })
}

func TestReadJSONRejectsUnsupportedFieldsTrailingDataAndOversize(t *testing.T) {
	for _, body := range []string{"", "{", "null", "[]", `"value"`, `{"unknown":"value"}`, `{} {}`, `{} false`, `{} trailing`, `{"value":"` + strings.Repeat("a", requestBodyLimit) + `"}`} {
		var value struct {
			Value string `json:"value"`
		}
		if err := readJSON(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/settings", strings.NewReader(body)), &value); err == nil {
			t.Fatal("malformed or oversized JSON was accepted")
		}
	}
	var value struct {
		Value string `json:"value"`
	}
	if err := readJSON(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/settings", strings.NewReader(" {\"value\":\"ok\"} \n")), &value); err != nil || value.Value != "ok" {
		t.Fatalf("valid object error = %v, value = %q", err, value.Value)
	}
}

func serveUnixResponse(t *testing.T, handler http.Handler) string {
	t.Helper()
	socket := filepath.Join(shortSocketDirectory(t), "response.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("close response fixture: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("response fixture did not stop")
		}
	})
	return socket
}

func TestRequestPreservesStructuredAPIErrorsAndUsesLocalTransport(t *testing.T) {
	socket := serveUnixResponse(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/settings" || r.Host != "localhost" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("unexpected native client request framing")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != `{"mountRoot":"/tmp/repos"}` {
			t.Error("native client did not send exact JSON body")
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"operation in progress"}`)
	}))
	data, status, err := Request(context.Background(), socket, http.MethodPost, "/v1/settings", []byte(`{"mountRoot":"/tmp/repos"}`))
	if err != nil || status != http.StatusConflict || string(data) != `{"error":"operation in progress"}` {
		t.Fatalf("native API error = %d, %s, error = %v", status, data, err)
	}
}

func TestRequestValidatesMethodPathAndBodyBeforeConnecting(t *testing.T) {
	socket := filepath.Join(shortSocketDirectory(t), "absent.sock")
	tests := []struct {
		socket, method, path string
		body                 []byte
	}{
		{socket, http.MethodPut, "/v1/status", nil},
		{"relative.sock", http.MethodGet, "/v1/status", nil},
		{socket, http.MethodGet, "/status", nil},
		{socket, http.MethodGet, "/v1/status?token=secret", nil},
		{socket, http.MethodGet, "/v1/status#fragment", nil},
		{socket, http.MethodGet, "/v1/status\r\nInjected: value", nil},
		{socket, http.MethodPost, "/v1/settings", []byte("{")},
		{socket, http.MethodPost, "/v1/settings", []byte(strings.Repeat(" ", requestBodyLimit+1))},
	}
	for index, test := range tests {
		if _, _, err := Request(context.Background(), test.socket, test.method, test.path, test.body); err == nil || strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("invalid request %d reached socket inspection: %v", index, err)
		}
	}
	if _, _, err := Request(context.Background(), socket, http.MethodGet, "/v1/status", nil); err == nil {
		t.Fatal("missing control socket was accepted")
	}
}

func TestRequestRefusesInsecureSocketAndNonSocketPaths(t *testing.T) {
	socket := serveUnixResponse(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("request reached an insecure socket") }))
	if err := os.Chmod(socket, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Request(context.Background(), socket, http.MethodGet, "/v1/status", nil); err == nil {
		t.Fatal("public control socket was accepted")
	}
	file := filepath.Join(shortSocketDirectory(t), "regular-file")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Request(context.Background(), file, http.MethodGet, "/v1/status", nil); err == nil {
		t.Fatal("regular file was accepted as control socket")
	}
	link := filepath.Join(filepath.Dir(file), "symlink")
	if err := os.Symlink(socket, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Request(context.Background(), link, http.MethodGet, "/v1/status", nil); err == nil {
		t.Fatal("symlink was accepted as control socket")
	}
}

func TestRequestRejectsMalformedAndOversizedResponses(t *testing.T) {
	for _, body := range []string{"", "not JSON", `{} {}`, `{ "error": `} {
		socket := serveUnixResponse(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		if _, _, err := Request(context.Background(), socket, http.MethodGet, "/v1/status", nil); err == nil || !strings.Contains(err.Error(), "invalid response") {
			t.Fatalf("malformed response was accepted: %v", err)
		}
	}
	block := strings.Repeat("a", 64<<10)
	socket := serveUnixResponse(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `"`)
		for range 513 {
			if _, err := io.WriteString(w, block); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"`)
	}))
	if _, _, err := Request(context.Background(), socket, http.MethodGet, "/v1/status", nil); err == nil || !strings.Contains(err.Error(), "invalid response") {
		t.Fatalf("oversized response was accepted: %v", err)
	}
}

func TestRequestHonorsCancellation(t *testing.T) {
	socket := serveUnixResponse(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, _, err := Request(ctx, socket, http.MethodGet, "/v1/status", nil); err == nil {
		t.Fatal("canceled request succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("canceled request did not return promptly")
	}
}

type desktopFileInfoWithOwner struct {
	os.FileInfo
	owner uint32
}

func (f desktopFileInfoWithOwner) Sys() any { return &syscall.Stat_t{Uid: f.owner} }

func TestOwnedByCurrentUserRejectsForeignOwner(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !ownedByCurrentUser(desktopFileInfoWithOwner{FileInfo: info, owner: uint32(os.Getuid())}) {
		t.Fatal("current user's path rejected")
	}
	if ownedByCurrentUser(desktopFileInfoWithOwner{FileInfo: info, owner: uint32(os.Getuid() + 1)}) {
		t.Fatal("foreign-owned path accepted")
	}
}
