//go:build !windows

package fsbridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jacobsa/fuse/fuseops"
)

func splitBridgeSocketPath(t *testing.T, source, socketDirectory string) string {
	t.Helper()
	physicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	physicalSockets, err := filepath.EvalSymlinks(socketDirectory)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(physicalSource))
	return filepath.Join(physicalSockets, "b"+hex.EncodeToString(sum[:])[:16])
}

func splitBridgeDescriptor(t *testing.T, source, socketDirectory string) Descriptor {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(source, "connection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var descriptor Descriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		t.Fatal("split bridge descriptor is not valid JSON")
	}
	token, err := hex.DecodeString(descriptor.Token)
	if err != nil || len(token) != 32 || len(descriptor.Token) != 64 || descriptor.Token != strings.ToLower(descriptor.Token) {
		t.Fatal("split bridge capability is not a lowercase 32-byte hex value")
	}
	if descriptor.Version != Version || descriptor.Socket != splitBridgeSocketPath(t, source, socketDirectory) {
		t.Fatal("split bridge descriptor does not identify the canonical socket for this source")
	}
	return descriptor
}

func assertSplitBridgeAbsent(t *testing.T, source, socket string) {
	t.Helper()
	for _, path := range []string{socket, filepath.Join(source, "connection.json")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("drained split bridge session path still exists or cannot be checked")
		}
	}
}

type splitBridgeSavedPath struct {
	info os.FileInfo
	data []byte
}

func saveSplitBridgeDirectory(t *testing.T, directory string) map[string]splitBridgeSavedPath {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	saved := make(map[string]splitBridgeSavedPath, len(entries))
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		state := splitBridgeSavedPath{info: info}
		if info.Mode().IsRegular() {
			state.data, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
		}
		saved[entry.Name()] = state
	}
	return saved
}

func assertSplitBridgeDirectoryPreserved(t *testing.T, directory string, saved map[string]splitBridgeSavedPath, allowNewLease bool) {
	t.Helper()
	after := saveSplitBridgeDirectory(t, directory)
	if allowNewLease {
		if _, existed := saved[".bridge.lock"]; !existed {
			delete(after, ".bridge.lock")
		}
	}
	if len(saved) != len(after) {
		t.Fatal("refused startup changed the directory's session paths")
	}
	for name, before := range saved {
		now, ok := after[name]
		if !ok || !os.SameFile(before.info, now.info) || before.info.Mode() != now.info.Mode() || !bytes.Equal(before.data, now.data) {
			t.Fatal("refused startup changed an existing path or its contents")
		}
	}
}

func writeSplitBridgeSentinel(t *testing.T, directory string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, "untouched"), []byte("preserve unrelated user data"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertSplitBridgeSourceLease(t *testing.T, source string, held bool) {
	t.Helper()
	fd, err := syscall.Open(filepath.Join(source, ".bridge.lock"), syscall.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal("persistent source lease cannot be opened safely")
	}
	defer syscall.Close(fd)
	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if held {
		if err == nil {
			_ = syscall.Flock(fd, syscall.LOCK_UN)
			t.Fatal("active or incompletely drained session does not hold the source lease")
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			t.Fatal("source lease contention failed for an unrelated reason")
		}
		return
	}
	if err != nil {
		t.Fatal("completed session did not release the persistent source lease")
	}
	if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
}

func TestServerSplitSocketDirectoryPrivateRPCAndCapabilityRotation(t *testing.T) {
	source, sockets := bridgeSourceDirectory(t), bridgeSourceDirectory(t)
	writeSplitBridgeSentinel(t, source)
	writeSplitBridgeSentinel(t, sockets)
	sourceBefore, socketsBefore := saveSplitBridgeDirectory(t, source), saveSplitBridgeDirectory(t, sockets)
	filesystem := &protocolFilesystem{}
	server, err := StartWithSocketDirectory(context.Background(), source, sockets, filesystem)
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, server)
	assertSplitBridgeSourceLease(t, source, true)
	if server.SourceDirectory() != source {
		t.Fatal("split transport changed the resource's source directory")
	}
	descriptor := splitBridgeDescriptor(t, source, sockets)
	for path, kind := range map[string]os.FileMode{
		descriptor.Socket:                        os.ModeSocket,
		filepath.Join(source, "connection.json"): 0,
		filepath.Join(source, ".bridge.lock"):    0,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if info.Mode().Type() != kind || info.Mode().Perm() != 0o600 || !ok || int(stat.Uid) != os.Getuid() {
			t.Fatal("split session path is not private and owned by the current user")
		}
	}
	lockBefore, err := os.Lstat(filepath.Join(source, ".bridge.lock"))
	if err != nil {
		t.Fatal(err)
	}
	client := bridgeSocketClient(t, descriptor)
	bridgeMetadataResult(t, bridgeSocketRequest(client, descriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusOK, 0)
	bridgeDrain(t, server)
	assertSplitBridgeSourceLease(t, source, false)
	assertSplitBridgeAbsent(t, source, descriptor.Socket)
	lockAfter, err := os.Lstat(filepath.Join(source, ".bridge.lock"))
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("successful drain removed or replaced the persistent source lease")
	}
	assertSplitBridgeDirectoryPreserved(t, source, sourceBefore, true)
	assertSplitBridgeDirectoryPreserved(t, sockets, socketsBefore, false)
	for _, directory := range []string{source, sockets} {
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatal("draining changed one of the private session directories")
		}
	}
	second, err := StartWithSocketDirectory(context.Background(), source, sockets, filesystem)
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, second)
	current := splitBridgeDescriptor(t, source, sockets)
	if current.Token == descriptor.Token {
		t.Fatal("split session reused an expired capability")
	}
	client = bridgeSocketClient(t, current)
	before := filesystem.calls.Load()
	bridgeMetadataResult(t, bridgeSocketRequest(client, descriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusUnauthorized, syscall.EACCES)
	if filesystem.calls.Load() != before {
		t.Fatal("an expired split capability reached the filesystem")
	}
	bridgeMetadataResult(t, bridgeSocketRequest(client, current, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusOK, 0)
	bridgeDrain(t, second)
}

func TestServerSplitSocketDirectoryLeaseExcludesOtherRootsAndLegacyStart(t *testing.T) {
	source, sockets, otherSockets := bridgeSourceDirectory(t), bridgeSourceDirectory(t), bridgeSourceDirectory(t)
	server, err := StartWithSocketDirectory(context.Background(), source, sockets, &protocolFilesystem{})
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, server)
	assertSplitBridgeSourceLease(t, source, true)
	sourceBefore, socketsBefore, otherBefore := saveSplitBridgeDirectory(t, source), saveSplitBridgeDirectory(t, sockets), saveSplitBridgeDirectory(t, otherSockets)
	for _, legacy := range []bool{false, true} {
		var refused *Server
		var err error
		if legacy {
			refused, err = Start(context.Background(), source, &protocolFilesystem{})
		} else {
			refused, err = StartWithSocketDirectory(context.Background(), source, otherSockets, &protocolFilesystem{})
		}
		if refused != nil {
			bridgeServerCleanup(t, refused)
		}
		if err == nil || refused != nil {
			t.Fatal("another transport acquired an actively owned source")
		}
		assertSplitBridgeDirectoryPreserved(t, source, sourceBefore, false)
		assertSplitBridgeDirectoryPreserved(t, sockets, socketsBefore, false)
		assertSplitBridgeDirectoryPreserved(t, otherSockets, otherBefore, false)
	}
	bridgeDrain(t, server)
	assertSplitBridgeSourceLease(t, source, false)
	legacy, err := Start(context.Background(), source, &protocolFilesystem{})
	if err != nil {
		t.Fatal("successful split drain did not release the source for the legacy API")
	}
	bridgeServerCleanup(t, legacy)
	bridgeDescriptor(t, source)
	bridgeDrain(t, legacy)
}

func TestServerSplitSocketDirectoryDistinctSourcesShareRoot(t *testing.T) {
	firstSource, secondSource, sockets := bridgeSourceDirectory(t), bridgeSourceDirectory(t), bridgeSourceDirectory(t)
	first, err := StartWithSocketDirectory(context.Background(), firstSource, sockets, &protocolFilesystem{})
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, first)
	firstDescriptor := splitBridgeDescriptor(t, firstSource, sockets)
	firstSocket, err := os.Lstat(firstDescriptor.Socket)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StartWithSocketDirectory(context.Background(), secondSource, sockets, &protocolFilesystem{})
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, second)
	secondDescriptor := splitBridgeDescriptor(t, secondSource, sockets)
	if firstDescriptor.Socket == secondDescriptor.Socket {
		t.Fatal("different sources collided in a shared socket directory")
	}
	after, err := os.Lstat(firstDescriptor.Socket)
	if err != nil || !os.SameFile(firstSocket, after) || splitBridgeDescriptor(t, firstSource, sockets) != firstDescriptor {
		t.Fatal("a second source replaced the first session")
	}
	for _, descriptor := range []Descriptor{firstDescriptor, secondDescriptor} {
		bridgeMetadataResult(t, bridgeSocketRequest(bridgeSocketClient(t, descriptor), descriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusOK, 0)
	}
	bridgeDrain(t, first)
	bridgeMetadataResult(t, bridgeSocketRequest(bridgeSocketClient(t, secondDescriptor), secondDescriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusOK, 0)
	bridgeDrain(t, second)
}

func TestServerSplitSocketDirectoryCanonicalAncestorsAndSourceIdentity(t *testing.T) {
	root := bridgeSourceDirectory(t)
	alias := root + "-alias"
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })
	for _, name := range []string{"source", "sockets", "other-sockets"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source, sockets := filepath.Join(alias, "source"), filepath.Join(alias, "sockets")
	server, err := StartWithSocketDirectory(context.Background(), source, sockets, &protocolFilesystem{})
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, server)
	if server.SourceDirectory() != source {
		t.Fatal("canonical transport changed the caller's source identity")
	}
	descriptor := splitBridgeDescriptor(t, source, sockets)
	bridgeMetadataResult(t, bridgeSocketRequest(bridgeSocketClient(t, descriptor), descriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusOK, 0)
	refused, err := StartWithSocketDirectory(context.Background(), filepath.Join(root, "source"), filepath.Join(root, "other-sockets"), &protocolFilesystem{})
	if refused != nil {
		bridgeServerCleanup(t, refused)
	}
	if err == nil || refused != nil {
		t.Fatal("alternate spelling bypassed physical source ownership")
	}
	bridgeDrain(t, server)
}

func TestServerSplitSocketDirectoryRefusesUnsafePathsAndOccupiedSockets(t *testing.T) {
	for _, name := range []string{"canceled", "relative_source", "relative_sockets", "source_symlink", "sockets_symlink", "nonprivate_source", "nonprivate_sockets", "source_file", "sockets_file", "missing_sockets", "socket_file", "live_socket", "legacy_socket", "lease_symlink", "nonprivate_lease"} {
		t.Run(name, func(t *testing.T) {
			source, sockets := bridgeSourceDirectory(t), bridgeSourceDirectory(t)
			writeSplitBridgeSentinel(t, source)
			writeSplitBridgeSentinel(t, sockets)
			inputSource, inputSockets := source, sockets
			ctx := context.Background()
			socket := splitBridgeSocketPath(t, source, sockets)
			switch name {
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			case "relative_source":
				inputSource = "."
			case "relative_sockets":
				inputSockets = "."
			case "source_symlink", "sockets_symlink":
				target := source
				if name == "sockets_symlink" {
					target = sockets
				}
				link := target + "-link"
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(link) })
				if name == "source_symlink" {
					inputSource = link
				} else {
					inputSockets = link
				}
			case "nonprivate_source":
				if err := os.Chmod(source, 0o755); err != nil {
					t.Fatal(err)
				}
			case "nonprivate_sockets":
				if err := os.Chmod(sockets, 0o755); err != nil {
					t.Fatal(err)
				}
			case "missing_sockets":
				inputSockets = filepath.Join(sockets, "missing")
			case "source_file":
				inputSource = filepath.Join(source, "untouched")
			case "sockets_file":
				inputSockets = filepath.Join(sockets, "untouched")
			case "socket_file", "legacy_socket":
				if name == "legacy_socket" {
					socket = filepath.Join(source, "bridge.sock")
				}
				if err := os.WriteFile(socket, []byte("foreign session"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "live_socket":
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				listener.SetUnlinkOnClose(false)
				t.Cleanup(func() { _ = listener.Close() })
			case "lease_symlink":
				if err := os.Symlink(filepath.Join(source, "untouched"), filepath.Join(source, ".bridge.lock")); err != nil {
					t.Fatal(err)
				}
			case "nonprivate_lease":
				path := filepath.Join(source, ".bridge.lock")
				if err := os.WriteFile(path, []byte("foreign lease"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			sourceBefore, socketsBefore := saveSplitBridgeDirectory(t, source), saveSplitBridgeDirectory(t, sockets)
			server, err := StartWithSocketDirectory(ctx, inputSource, inputSockets, &protocolFilesystem{})
			if server != nil {
				bridgeServerCleanup(t, server)
			}
			if err == nil || server != nil {
				t.Fatal("unsafe or occupied split session was accepted")
			}
			assertSplitBridgeDirectoryPreserved(t, source, sourceBefore, true)
			assertSplitBridgeDirectoryPreserved(t, sockets, socketsBefore, false)
		})
	}
}

func TestServerSplitSocketDirectoryPreviousDescriptorPolicy(t *testing.T) {
	for _, name := range []string{"same_socket", "different_root", "legacy_socket", "corrupt_json", "oversized_json", "bad_version", "bad_token", "uppercase_token", "descriptor_symlink", "nonprivate_descriptor"} {
		t.Run(name, func(t *testing.T) {
			source, sockets, otherSockets := bridgeSourceDirectory(t), bridgeSourceDirectory(t), bridgeSourceDirectory(t)
			writeSplitBridgeSentinel(t, source)
			writeSplitBridgeSentinel(t, sockets)
			previous := Descriptor{Version: Version, Socket: splitBridgeSocketPath(t, source, sockets), Token: strings.Repeat("a", 64)}
			switch name {
			case "different_root":
				previous.Socket = splitBridgeSocketPath(t, source, otherSockets)
			case "legacy_socket":
				previous.Socket = filepath.Join(source, "bridge.sock")
			case "bad_version":
				previous.Version++
			case "bad_token":
				previous.Token = strings.Repeat("g", 64)
			case "uppercase_token":
				previous.Token = strings.Repeat("A", 64)
			}
			data, err := json.Marshal(previous)
			if err != nil {
				t.Fatal(err)
			}
			if name == "corrupt_json" {
				data = []byte("{")
			}
			if name == "oversized_json" {
				data = append(data, bytes.Repeat([]byte(" "), MaxMetadataSize+1)...)
			}
			path := filepath.Join(source, "connection.json")
			if name == "descriptor_symlink" {
				if err := os.Symlink(filepath.Join(source, "untouched"), path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if name == "nonprivate_descriptor" {
					if err := os.Chmod(path, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			sourceBefore, socketsBefore, otherBefore := saveSplitBridgeDirectory(t, source), saveSplitBridgeDirectory(t, sockets), saveSplitBridgeDirectory(t, otherSockets)
			filesystem := &protocolFilesystem{}
			server, err := StartWithSocketDirectory(context.Background(), source, sockets, filesystem)
			if server != nil {
				bridgeServerCleanup(t, server)
			}
			if name != "same_socket" {
				if err == nil || server != nil {
					t.Fatal("startup replaced an unsafe or mismatched previous descriptor")
				}
				assertSplitBridgeDirectoryPreserved(t, source, sourceBefore, true)
				assertSplitBridgeDirectoryPreserved(t, sockets, socketsBefore, false)
				assertSplitBridgeDirectoryPreserved(t, otherSockets, otherBefore, false)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			current := splitBridgeDescriptor(t, source, sockets)
			if current.Token == previous.Token {
				t.Fatal("stale descriptor did not rotate its capability")
			}
			client := bridgeSocketClient(t, current)
			bridgeMetadataResult(t, bridgeSocketRequest(client, previous, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusUnauthorized, syscall.EACCES)
			if filesystem.calls.Load() != 0 {
				t.Fatal("a stale descriptor's capability reached the filesystem")
			}
			bridgeMetadataResult(t, bridgeSocketRequest(client, current, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusOK, 0)
			bridgeDrain(t, server)
		})
	}
}

func TestServerSplitSocketDirectoryDrainTimeoutKeepsSourceLease(t *testing.T) {
	source, sockets, otherSockets := bridgeSourceDirectory(t), bridgeSourceDirectory(t), bridgeSourceDirectory(t)
	entered, gate := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	filesystem := &protocolFilesystem{getattr: func(_ context.Context, op *fuseops.GetInodeAttributesOp) error {
		close(entered)
		<-gate
		op.Attributes = fuseops.InodeAttributes{Mode: 0o640, Nlink: 1}
		return nil
	}}
	server, err := StartWithSocketDirectory(context.Background(), source, sockets, filesystem)
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, server)
	server.handler.register(9, 4, false)
	server.handler.remember(Node{Inode: 4})
	descriptor := splitBridgeDescriptor(t, source, sockets)
	client := bridgeSocketClient(t, descriptor)
	completed := make(chan bridgeHTTPResult, 1)
	go func() {
		completed <- bridgeSocketRequest(client, descriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":4}`)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("split request did not reach the filesystem")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err = server.CloseDrain(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || filesystem.releases.Load() != 0 || filesystem.forgets.Load() != 0 {
		t.Fatal("deadline did not preserve active split session resources")
	}
	assertSplitBridgeSourceLease(t, source, true)
	sourceBefore, socketsBefore := saveSplitBridgeDirectory(t, source), saveSplitBridgeDirectory(t, sockets)
	if _, err := os.Lstat(descriptor.Socket); err != nil {
		t.Fatal("drain timeout removed the split socket path")
	}
	if _, err := os.Lstat(filepath.Join(source, "connection.json")); err != nil {
		t.Fatal("drain timeout removed the source descriptor")
	}
	refused, err := StartWithSocketDirectory(context.Background(), source, otherSockets, &protocolFilesystem{})
	if refused != nil {
		bridgeServerCleanup(t, refused)
	}
	if err == nil || refused != nil {
		t.Fatal("drain timeout released source ownership before active calls completed")
	}
	assertSplitBridgeDirectoryPreserved(t, source, sourceBefore, false)
	assertSplitBridgeDirectoryPreserved(t, sockets, socketsBefore, false)
	release()
	select {
	case result := <-completed:
		bridgeMetadataResult(t, result, http.StatusOK, 0)
	case <-time.After(5 * time.Second):
		t.Fatal("split request did not finish after its gate opened")
	}
	bridgeDrain(t, server)
	assertSplitBridgeSourceLease(t, source, false)
	assertSplitBridgeAbsent(t, source, descriptor.Socket)
	if filesystem.releases.Load() != 1 || filesystem.forgets.Load() != 1 {
		t.Fatal("successful retry did not release retained resources once")
	}
	second, err := StartWithSocketDirectory(context.Background(), source, otherSockets, &protocolFilesystem{})
	if err != nil {
		t.Fatal("successful drain retry did not release the source lease")
	}
	bridgeServerCleanup(t, second)
	bridgeDrain(t, second)
}

func TestServerSplitSocketDirectoryReplacedPathsKeepLeaseUntilCleanup(t *testing.T) {
	for _, name := range []string{"socket", "descriptor"} {
		t.Run(name, func(t *testing.T) {
			source, sockets, otherSockets := bridgeSourceDirectory(t), bridgeSourceDirectory(t), bridgeSourceDirectory(t)
			server, err := StartWithSocketDirectory(context.Background(), source, sockets, &protocolFilesystem{})
			if err != nil {
				t.Fatal(err)
			}
			bridgeServerCleanup(t, server)
			descriptor := splitBridgeDescriptor(t, source, sockets)
			path := descriptor.Socket
			if name == "descriptor" {
				path = filepath.Join(source, "connection.json")
			}
			replacement := filepath.Join(filepath.Dir(path), "replacement")
			data := []byte("replacement belongs to a different owner")
			if err := os.WriteFile(replacement, data, 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(replacement)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
			if err := server.CloseDrain(context.Background()); err == nil {
				t.Fatal("drain accepted a replaced split session path")
			}
			assertSplitBridgeSourceLease(t, source, true)
			after, statErr := os.Lstat(path)
			contents, readErr := os.ReadFile(path)
			if statErr != nil || readErr != nil || !os.SameFile(info, after) || !bytes.Equal(contents, data) {
				t.Fatal("drain removed or changed a replacement path")
			}
			refused, err := StartWithSocketDirectory(context.Background(), source, otherSockets, &protocolFilesystem{})
			if refused != nil {
				bridgeServerCleanup(t, refused)
			}
			if err == nil || refused != nil {
				t.Fatal("failed path cleanup released the source lease")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			bridgeDrain(t, server)
			assertSplitBridgeSourceLease(t, source, false)
			assertSplitBridgeAbsent(t, source, descriptor.Socket)
			second, err := StartWithSocketDirectory(context.Background(), source, otherSockets, &protocolFilesystem{})
			if err != nil {
				t.Fatal("completed path cleanup did not release the source lease")
			}
			bridgeServerCleanup(t, second)
			bridgeDrain(t, second)
		})
	}
}

func TestServerSplitSocketDirectoryPathBoundsBeforeSessionCreation(t *testing.T) {
	root, sockets := bridgeSourceDirectory(t), bridgeSourceDirectory(t)
	longSource := filepath.Join(root, strings.Repeat("s", 180))
	if err := os.Mkdir(longSource, 0o700); err != nil {
		t.Fatal(err)
	}
	server, err := StartWithSocketDirectory(context.Background(), longSource, sockets, &protocolFilesystem{})
	if err != nil {
		t.Fatal("short socket directory did not permit a long source path")
	}
	bridgeServerCleanup(t, server)
	descriptor := splitBridgeDescriptor(t, longSource, sockets)
	bridgeMetadataResult(t, bridgeSocketRequest(bridgeSocketClient(t, descriptor), descriptor, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`), http.StatusOK, 0)
	bridgeDrain(t, server)
	for _, tc := range []struct{ name, component string }{{"ascii", strings.Repeat("x", 180)}, {"multibyte", strings.Repeat("é", 50)}} {
		t.Run(tc.name, func(t *testing.T) {
			source := bridgeSourceDirectory(t)
			longSockets := filepath.Join(root, tc.component)
			if err := os.Mkdir(longSockets, 0o700); err != nil {
				t.Fatal(err)
			}
			writeSplitBridgeSentinel(t, source)
			writeSplitBridgeSentinel(t, longSockets)
			sourceBefore, socketsBefore := saveSplitBridgeDirectory(t, source), saveSplitBridgeDirectory(t, longSockets)
			refused, err := StartWithSocketDirectory(context.Background(), source, longSockets, &protocolFilesystem{})
			if refused != nil {
				bridgeServerCleanup(t, refused)
			}
			if err == nil || refused != nil {
				t.Fatal("an overlong canonical Unix socket path was accepted")
			}
			assertSplitBridgeDirectoryPreserved(t, source, sourceBefore, false)
			assertSplitBridgeDirectoryPreserved(t, longSockets, socketsBefore, false)
		})
	}
}

func TestServerSplitSocketDirectoryFailedResourceCleanupKeepsSourceLease(t *testing.T) {
	source, sockets := bridgeSourceDirectory(t), bridgeSourceDirectory(t)
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
	server, err := StartWithSocketDirectory(context.Background(), source, sockets, filesystem)
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, server)
	server.handler.register(9, 7, false)
	server.handler.remember(Node{Inode: 7})
	server.handler.remember(Node{Inode: 7})
	descriptor := splitBridgeDescriptor(t, source, sockets)
	if err := server.CloseDrain(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("failed lookup cleanup was not returned by split drain")
	}
	assertSplitBridgeSourceLease(t, source, true)
	for _, path := range []string{descriptor.Socket, filepath.Join(source, "connection.json")} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatal("failed cleanup removed a recoverable split session path")
		}
	}
	bridgeDrain(t, server)
	assertSplitBridgeSourceLease(t, source, false)
	assertSplitBridgeAbsent(t, source, descriptor.Socket)
	if attempts.Load() != 2 || filesystem.releases.Load() != 1 {
		t.Fatal("split cleanup retry did not release only remaining resources")
	}
}

func TestServerSplitSocketDirectoryFailedStartupCanRetry(t *testing.T) {
	source, sockets := bridgeSourceDirectory(t), bridgeSourceDirectory(t)
	server, err := StartWithSocketDirectory(context.Background(), source, sockets, nil)
	if server != nil {
		bridgeServerCleanup(t, server)
	}
	if err == nil || server != nil {
		t.Fatal("startup accepted a missing filesystem")
	}
	assertSplitBridgeAbsent(t, source, splitBridgeSocketPath(t, source, sockets))
	server, err = StartWithSocketDirectory(context.Background(), source, sockets, &protocolFilesystem{})
	if err != nil {
		t.Fatal("failed startup retained ownership and prevented a valid retry")
	}
	bridgeServerCleanup(t, server)
	assertSplitBridgeSourceLease(t, source, true)
	bridgeDrain(t, server)
	assertSplitBridgeSourceLease(t, source, false)
}
