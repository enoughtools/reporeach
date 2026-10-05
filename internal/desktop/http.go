package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const requestBodyLimit = 1 << 20

// Serve owns one state directory and private socket until ctx is canceled.
// HTTP is only a local framing protocol: no TCP listener is opened.
func Serve(ctx context.Context, opts Options) (retErr error) {
	if !filepath.IsAbs(opts.StateDir) || !filepath.IsAbs(opts.Socket) {
		return errors.New("state directory and socket paths must be absolute")
	}
	if err := privateDirectory(opts.StateDir, true); err != nil {
		return err
	}
	if err := privateDirectory(filepath.Dir(opts.Socket), false); err != nil {
		return fmt.Errorf("socket directory: %w", err)
	}
	lockPath := filepath.Join(opts.StateDir, "service.lock")
	if info, err := os.Lstat(lockPath); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return errors.New("service lock must be a regular file")
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("RepoReach is already running for this state directory")
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if info, err := os.Lstat(opts.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(info) {
			return errors.New("refusing to replace an unowned or non-socket path")
		}
		connection, dialErr := net.DialTimeout("unix", opts.Socket, 200*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return errors.New("RepoReach is already listening on this socket")
		}
		if err := os.Remove(opts.Socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", opts.Socket)
	if err != nil {
		return fmt.Errorf("create local control socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(opts.Socket)
	if err := os.Chmod(opts.Socket, 0o600); err != nil {
		return err
	}
	service, err := New(ctx, opts)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, service.Close()) }()
	service.Restore()
	server := &http.Server{
		Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 6 * time.Minute,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stop := make(chan struct{})
	shutdownDone := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				_ = server.Close()
			}
		case <-stop:
		}
	}()
	err = server.Serve(listener)
	if ctx.Err() != nil {
		// Drain handlers before closing the engine they use. Shutdown is bounded
		// and closes active transports if a handler cannot finish in time.
		<-shutdownDone
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.RawQuery != "" {
			jsonError(w, http.StatusBadRequest, errors.New("query parameters are not supported"))
			return
		}
		ctx := r.Context()
		var response any
		var err error
		code := http.StatusOK
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/status":
			response = s.Status()
		case "GET /v1/auth/status":
			response = s.github.AuthStatus(ctx)
		case "POST /v1/auth/start":
			response, err = s.github.StartAuth(ctx)
		case "POST /v1/discover":
			response, err = s.Discover(ctx)
		case "POST /v1/repositories/adopt":
			var adoption AdoptionRequest
			err = readJSON(w, r, &adoption)
			if err == nil {
				response, err = s.Adopt(ctx, adoption)
			}
		case "POST /v1/organizations/settings":
			var setting struct {
				Owner   string `json:"owner"`
				Enabled *bool  `json:"enabled"`
			}
			err = readJSON(w, r, &setting)
			if err == nil && setting.Enabled == nil {
				err = errors.New("enabled must be a boolean")
			}
			if err == nil {
				err = s.SetOrganizationEnabled(ctx, setting.Owner, *setting.Enabled)
			}
			response = s.Status()
		case "POST /v1/repositories/visibility":
			var setting struct {
				ID      string `json:"id"`
				Enabled *bool  `json:"enabled"`
			}
			err = readJSON(w, r, &setting)
			if err == nil && setting.Enabled == nil {
				err = errors.New("enabled must be a boolean")
			}
			if err == nil {
				err = s.SetRepositoryEnabled(ctx, setting.ID, *setting.Enabled)
			}
			response = s.Status()
		case "POST /v1/settings":
			var settings struct {
				MountRoot string `json:"mountRoot"`
			}
			err = readJSON(w, r, &settings)
			if err == nil {
				err = s.Settings(ctx, settings.MountRoot)
			}
			response = s.Status()
		case "POST /v1/mount":
			err = s.Mount(ctx)
			response = s.Status()
		case "POST /v1/unmount":
			err = s.Unmount(ctx)
			response = s.Status()
		case "POST /v1/prepare-quit":
			err = s.PrepareQuit(ctx)
			response = s.Status()
		case "POST /v1/repositories/action":
			var action struct {
				ID     string `json:"id"`
				Action string `json:"action"`
			}
			err = readJSON(w, r, &action)
			if err == nil {
				var operation Operation
				operation, err = s.Action(action.ID, action.Action)
				response = struct {
					Operation Operation `json:"operation"`
				}{operation}
				code = http.StatusAccepted
			}
		default:
			jsonError(w, http.StatusNotFound, errors.New("unknown API endpoint or method"))
			return
		}
		if err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(response)
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, requestBodyLimit)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil || len(raw) == 0 || raw[0] != '{' {
		return errors.New("request body must be a valid JSON object with supported fields")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("request body has trailing data or is too large")
	}
	object := json.NewDecoder(bytes.NewReader(raw))
	object.DisallowUnknownFields()
	if err := object.Decode(dst); err != nil {
		return errors.New("request body must be a valid JSON object with supported fields")
	}
	return nil
}

func jsonError(w http.ResponseWriter, status int, err error) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": safeError(err)})
}

// Request sends one JSON request through the private socket. It returns the
// response body even for API failures so a native client gets structured errors.
func Request(ctx context.Context, socket, method, path string, body []byte) ([]byte, int, error) {
	if method != http.MethodGet && method != http.MethodPost {
		return nil, 0, errors.New("method must be GET or POST")
	}
	if !filepath.IsAbs(socket) || !strings.HasPrefix(path, "/v1/") || strings.ContainsAny(path, "?#\r\n") {
		return nil, 0, errors.New("use an absolute socket path and a /v1/ API path")
	}
	if len(body) > requestBodyLimit || (len(body) > 0 && !json.Valid(body)) {
		return nil, 0, errors.New("body must be valid JSON smaller than 1 MiB")
	}
	if err := privateDirectory(filepath.Dir(socket), false); err != nil {
		return nil, 0, errors.New("RepoReach's control socket directory is not private")
	}
	info, err := os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 {
		return nil, 0, errors.New("RepoReach's private control socket is unavailable")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		}, DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 6 * time.Minute}
	request, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errors.New("invalid API request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, errors.New("could not connect to RepoReach's background service")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20+1))
	if err != nil || len(data) > 32<<20 || !json.Valid(data) {
		return nil, 0, errors.New("background service returned an invalid response")
	}
	return data, response.StatusCode, nil
}

func privateDirectory(path string, makePrivate bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && makePrivate {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
		return errors.New("directory must be a real folder owned by the current user")
	}
	if makePrivate {
		return os.Chmod(path, 0o700)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("directory must be accessible only by the current user")
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}
