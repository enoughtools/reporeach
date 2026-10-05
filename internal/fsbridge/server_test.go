//go:build !windows

package fsbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jacobsa/fuse/fuseops"
)

func bridgeSourceDirectory(t *testing.T) string {
	t.Helper()
	// Darwin limits Unix socket paths to roughly 104 bytes. Its usual test
	// temporary directory can exhaust that bound before adding bridge.sock.
	directory, err := os.MkdirTemp("/tmp", "afs-bridge-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func bridgeServerCleanup(t *testing.T, server *Server) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.CloseDrain(ctx); err != nil {
			_ = server.http.Close()
			t.Errorf("bridge cleanup failed: %v", err)
		}
	})
}

func bridgeDescriptor(t *testing.T, directory string) Descriptor {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "connection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var descriptor Descriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		t.Fatal("bridge descriptor is not valid JSON")
	}
	token, err := hex.DecodeString(descriptor.Token)
	if err != nil || len(token) != 32 || len(descriptor.Token) != 64 {
		t.Fatal("bridge capability is not a 32-byte hex value")
	}
	if descriptor.Version != Version || descriptor.Socket != filepath.Join(directory, "bridge.sock") {
		t.Fatal("bridge descriptor does not identify this session")
	}
	return descriptor
}

func bridgeSocketClient(t *testing.T, descriptor Descriptor) *http.Client {
	t.Helper()
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", descriptor.Socket)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

type bridgeHTTPResult struct {
	status int
	body   []byte
	err    error
}

func bridgeSocketRequest(client *http.Client, descriptor Descriptor, method, path, body string) bridgeHTTPResult {
	request, err := http.NewRequest(method, "http://bridge"+path, bytes.NewBufferString(body))
	if err != nil {
		return bridgeHTTPResult{err: err}
	}
	request.Header.Set("Authorization", "Bearer "+descriptor.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return bridgeHTTPResult{err: err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return bridgeHTTPResult{status: response.StatusCode, body: data, err: err}
}

func bridgeMetadataResult(t *testing.T, result bridgeHTTPResult, status int, code syscall.Errno) Response {
	t.Helper()
	if result.err != nil {
		t.Fatal(result.err)
	}
	var response Response
	if err := json.Unmarshal(result.body, &response); err != nil {
		t.Fatal("bridge response is not valid JSON")
	}
	if result.status != status || response.Version != Version || response.Errno != int(code) {
		t.Fatalf("bridge response: status %d, version %d, errno %d", result.status, response.Version, response.Errno)
	}
	return response
}

func bridgeDrain(t *testing.T, server *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.CloseDrain(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.Done():
	case <-ctx.Done():
		t.Fatal("bridge serving loop did not stop after draining")
	}
	if err := server.Err(); err != nil {
		t.Fatal(err)
	}
}

func assertBridgePathsAbsent(t *testing.T, directory string) {
	t.Helper()
	for _, name := range []string{"bridge.sock", "connection.json"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("session path %s still exists or cannot be checked", name)
		}
	}
}

func TestServerPrivateSocketCapabilityAndStartupContext(t *testing.T) {
	directory := bridgeSourceDirectory(t)
	filesystem := &protocolFilesystem{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := Start(ctx, directory, filesystem)
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, server)
	if server.SourceDirectory() != directory {
		t.Fatal("server did not retain its source directory")
	}
	for name, kind := range map[string]os.FileMode{"bridge.sock": os.ModeSocket, "connection.json": 0} {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if info.Mode().Type() != kind || info.Mode().Perm() != 0o600 || !ok || int(stat.Uid) != os.Getuid() {
			t.Fatalf("%s is not private and owned by the current user", name)
		}
	}
	descriptor := bridgeDescriptor(t, directory)
	client := bridgeSocketClient(t, descriptor)
	bridgeMetadataResult(t, bridgeSocketRequest(client, descriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusOK, 0)
	cancel()
	bridgeMetadataResult(t, bridgeSocketRequest(client, descriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusOK, 0)
	select {
	case <-server.Done():
		t.Fatal("cancelling startup stopped an active bridge session")
	default:
	}
	bridgeDrain(t, server)
	assertBridgePathsAbsent(t, directory)
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatal("draining removed or changed the private source directory")
	}
	second, err := Start(context.Background(), directory, filesystem)
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, second)
	newDescriptor := bridgeDescriptor(t, directory)
	if newDescriptor.Token == descriptor.Token {
		t.Fatal("a new session reused the previous capability")
	}
	newClient := bridgeSocketClient(t, newDescriptor)
	before := filesystem.calls.Load()
	bridgeMetadataResult(t, bridgeSocketRequest(newClient, descriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusUnauthorized, syscall.EACCES)
	if filesystem.calls.Load() != before {
		t.Fatal("an expired capability reached the filesystem")
	}
	bridgeMetadataResult(t, bridgeSocketRequest(newClient, newDescriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusOK, 0)
	bridgeDrain(t, second)
}

func TestServerRefusesUnsafeOrOccupiedSourcesWithoutChanges(t *testing.T) {
	for _, name := range []string{"canceled", "relative", "source_symlink", "nonprivate_directory", "socket", "occupied_socket_path", "descriptor_symlink", "nonprivate_descriptor"} {
		t.Run(name, func(t *testing.T) {
			directory := bridgeSourceDirectory(t)
			input := directory
			ctx := context.Background()
			const sentinel = "preserve this existing file"
			untouched := filepath.Join(directory, "untouched")
			if err := os.WriteFile(untouched, []byte(sentinel), 0o600); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "relative":
				input = "."
			case "source_symlink":
				input = directory + "-link"
				if err := os.Symlink(directory, input); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(input) })
			case "nonprivate_directory":
				if err := os.Chmod(directory, 0o755); err != nil {
					t.Fatal(err)
				}
			case "socket":
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(directory, "bridge.sock"), Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				listener.SetUnlinkOnClose(false)
				t.Cleanup(func() { _ = listener.Close() })
			case "occupied_socket_path":
				if err := os.WriteFile(filepath.Join(directory, "bridge.sock"), []byte(sentinel), 0o600); err != nil {
					t.Fatal(err)
				}
			case "descriptor_symlink":
				if err := os.Symlink(untouched, filepath.Join(directory, "connection.json")); err != nil {
					t.Fatal(err)
				}
			case "nonprivate_descriptor":
				path := filepath.Join(directory, "connection.json")
				if err := os.WriteFile(path, []byte(sentinel), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			before := make(map[string]os.FileInfo, len(entries))
			for _, entry := range entries {
				info, err := os.Lstat(filepath.Join(directory, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				before[entry.Name()] = info
			}
			server, err := Start(ctx, input, &protocolFilesystem{})
			if server != nil {
				bridgeServerCleanup(t, server)
			}
			if err == nil || server != nil {
				t.Fatal("unsafe or occupied source was accepted")
			}
			after, err := os.ReadDir(directory)
			if err != nil || len(after) != len(before) {
				t.Fatal("refused startup changed the source directory")
			}
			for _, entry := range after {
				path := filepath.Join(directory, entry.Name())
				info, err := os.Lstat(path)
				original := before[entry.Name()]
				if err != nil || original == nil || !os.SameFile(original, info) || original.Mode() != info.Mode() {
					t.Fatal("refused startup changed an existing session path")
				}
				if info.Mode().IsRegular() {
					data, err := os.ReadFile(path)
					if err != nil || string(data) != sentinel {
						t.Fatal("refused startup overwrote an existing file")
					}
				}
			}
		})
	}
}

func TestServerDrainDeadlinePreservesInflightResources(t *testing.T) {
	for _, operation := range []string{"read", "getattr"} {
		t.Run(operation, func(t *testing.T) {
			directory := bridgeSourceDirectory(t)
			entered, gate := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			defer release()
			filesystem := &protocolFilesystem{}
			if operation == "read" {
				filesystem.read = func(_ context.Context, op *fuseops.ReadFileOp) error {
					close(entered)
					<-gate
					op.Data, op.BytesRead = [][]byte{{0, 255, 1, 254}}, 4
					return nil
				}
			} else {
				filesystem.getattr = func(_ context.Context, op *fuseops.GetInodeAttributesOp) error {
					close(entered)
					<-gate
					op.Attributes = fuseops.InodeAttributes{Mode: 0o640, Nlink: 1, Size: 4}
					return nil
				}
			}
			server, err := Start(context.Background(), directory, filesystem)
			if err != nil {
				t.Fatal(err)
			}
			bridgeServerCleanup(t, server)
			server.handler.register(9, 4, false)
			server.handler.remember(Node{Inode: 4})
			server.handler.remember(Node{Inode: 4})
			descriptor := bridgeDescriptor(t, directory)
			client := bridgeSocketClient(t, descriptor)
			completed := make(chan bridgeHTTPResult, 1)
			go func() {
				if operation == "read" {
					completed <- bridgeSocketRequest(client, descriptor, http.MethodGet, "/v1/fs/read?inode=4&handle=9&offset=0&size=4", "")
				} else {
					completed <- bridgeSocketRequest(client, descriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":4}`)
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not reach the filesystem")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			err = server.CloseDrain(ctx)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("drain did not report its deadline while an RPC was active")
			}
			if filesystem.releases.Load() != 0 || filesystem.forgets.Load() != 0 {
				t.Fatal("drain released resources belonging to an active request")
			}
			server.handler.mu.Lock()
			retained := len(server.handler.handles) == 1 && server.handler.lookups[4] == 2
			server.handler.mu.Unlock()
			if !retained {
				t.Fatal("drain lost handle or lookup bookkeeping")
			}
			for _, name := range []string{"bridge.sock", "connection.json"} {
				if _, err := os.Lstat(filepath.Join(directory, name)); err != nil {
					t.Fatal("drain removed an active session path")
				}
			}
			release()
			select {
			case result := <-completed:
				if operation == "read" {
					if result.err != nil || result.status != http.StatusOK || !bytes.Equal(result.body, []byte{0, 255, 1, 254}) {
						t.Fatal("draining interrupted an active binary read")
					}
				} else {
					bridgeMetadataResult(t, result, http.StatusOK, 0)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("active request did not finish after its gate opened")
			}
			bridgeDrain(t, server)
			assertBridgePathsAbsent(t, directory)
			if filesystem.releases.Load() != 1 || filesystem.forgets.Load() != 1 {
				t.Fatal("successful drain did not release each retained resource once")
			}
			bridgeDrain(t, server)
			if filesystem.releases.Load() != 1 || filesystem.forgets.Load() != 1 {
				t.Fatal("repeated drain released resources more than once")
			}
		})
	}
}

func TestServerDrainPreservesReplacedSessionPaths(t *testing.T) {
	for _, name := range []string{"bridge.sock", "connection.json"} {
		for _, replace := range []bool{false, true} {
			state := "removed"
			if replace {
				state = "replaced"
			}
			t.Run(name+"/"+state, func(t *testing.T) {
				directory := bridgeSourceDirectory(t)
				server, err := Start(context.Background(), directory, &protocolFilesystem{})
				if err != nil {
					t.Fatal(err)
				}
				bridgeServerCleanup(t, server)
				path := filepath.Join(directory, name)
				const sentinel = "replacement belongs to another session"
				var replacement os.FileInfo
				if replace {
					// Create the replacement while the original still exists, so the
					// test cannot accidentally reuse the descriptor's old inode.
					newPath := filepath.Join(directory, "replacement")
					if err := os.WriteFile(newPath, []byte(sentinel), 0o600); err != nil {
						t.Fatal(err)
					}
					replacement, err = os.Lstat(newPath)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(newPath, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err = server.CloseDrain(ctx)
				cancel()
				if replace {
					if err == nil {
						t.Fatal("drain accepted a replaced session path")
					}
					info, statErr := os.Lstat(path)
					data, readErr := os.ReadFile(path)
					if statErr != nil || readErr != nil || !os.SameFile(replacement, info) || string(data) != sentinel {
						t.Fatal("drain removed or changed a replacement path")
					}
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					bridgeDrain(t, server)
				} else if err != nil {
					t.Fatal(err)
				}
				assertBridgePathsAbsent(t, directory)
			})
		}
	}
}

func TestServerDrainRetriesFailedLookupRelease(t *testing.T) {
	directory := bridgeSourceDirectory(t)
	var attempts atomic.Int64
	filesystem := &protocolFilesystem{forget: func(_ context.Context, op *fuseops.ForgetInodeOp) error {
		if op.Inode != 7 || op.N != 2 {
			return syscall.EINVAL
		}
		if attempts.Add(1) == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}}
	server, err := Start(context.Background(), directory, filesystem)
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, server)
	server.handler.register(9, 7, false)
	server.handler.remember(Node{Inode: 7})
	server.handler.remember(Node{Inode: 7})
	if err := server.CloseDrain(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("failed lookup cleanup was not returned by drain")
	}
	server.handler.mu.Lock()
	retained := server.handler.lookups[7] == 2 && len(server.handler.handles) == 0
	server.handler.mu.Unlock()
	if !retained || filesystem.releases.Load() != 1 {
		t.Fatal("failed cleanup lost lookup references or retained a released handle")
	}
	for _, name := range []string{"bridge.sock", "connection.json"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err != nil {
			t.Fatal("failed cleanup removed a recoverable session path")
		}
	}
	bridgeDrain(t, server)
	assertBridgePathsAbsent(t, directory)
	if attempts.Load() != 2 || filesystem.releases.Load() != 1 {
		t.Fatal("retry did not release only the remaining lookup references")
	}
}
