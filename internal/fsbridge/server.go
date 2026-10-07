//go:build !windows

package fsbridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/jacobsa/fuse/fuseutil"
)

// Server owns one filesystem RPC session, independently of the desktop
// management socket. Its startup context does not determine its lifetime: the
// desktop service must prove the kernel mount has detached before CloseDrain.
type Server struct {
	directory      string
	socket         string
	descriptorPath string
	socketInfo     os.FileInfo
	descriptorInfo os.FileInfo
	sourceInfo     os.FileInfo
	socketDirInfo  os.FileInfo
	lease          *os.File
	http           *http.Server
	handler        *Handler
	done           chan struct{}
	mu             sync.Mutex
	err            error
	closeMu        sync.Mutex
	closed         bool
}

// Start requires an existing private directory owned by the current user. It
// never removes an existing socket: recovery must first establish that no
// surviving mount or bridge owns that session.
func Start(ctx context.Context, sourceDir string, filesystem fuseutil.FileSystem) (*Server, error) {
	return start(ctx, sourceDir, sourceDir, "bridge.sock", filesystem)
}

// StartWithSocketDirectory keeps the descriptor and FSKit resource in sourceDir,
// while placing the Unix listener directly in a shared app-group container.
// A canonical source identity gives each private state root its own short name.
func StartWithSocketDirectory(ctx context.Context, sourceDir, socketDir string, filesystem fuseutil.FileSystem) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := bridgeDirectory(sourceDir); err != nil {
		return nil, err
	}
	if _, err := bridgeDirectory(socketDir); err != nil {
		return nil, err
	}
	canonicalSource, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		return nil, errors.New("filesystem bridge source directory could not be resolved")
	}
	canonicalSocket, err := filepath.EvalSymlinks(socketDir)
	if err != nil {
		return nil, errors.New("filesystem bridge socket directory could not be resolved")
	}
	digest := sha256.Sum256([]byte(canonicalSource))
	return start(ctx, sourceDir, canonicalSocket, "b"+hex.EncodeToString(digest[:8]), filesystem)
}

func bridgeDirectory(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("filesystem bridge directories must be absolute private directories")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ok || int(stat.Uid) != os.Getuid() {
		return nil, errors.New("filesystem bridge directories must be private real directories owned by the current user")
	}
	return info, nil
}

func start(ctx context.Context, sourceDir, socketDir, socketName string, filesystem fuseutil.FileSystem) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sourceInfo, err := bridgeDirectory(sourceDir)
	if err != nil {
		return nil, err
	}
	socketDirectoryInfo, err := bridgeDirectory(socketDir)
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(socketDir, socketName)
	if len(socket) >= len(syscall.RawSockaddrUnix{}.Path) {
		return nil, errors.New("filesystem bridge socket path exceeds the Unix socket limit")
	}
	descriptorPath := filepath.Join(sourceDir, "connection.json")
	if err := checkSessionPaths(sourceDir, socket, descriptorPath); err != nil {
		return nil, err
	}
	lease, err := acquireSourceLease(sourceDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if lease != nil {
			_ = lease.Close()
		}
	}()
	// Validation before the lease leaves refused unsafe paths untouched. Repeat
	// under the lease so concurrent starts cannot replace a live descriptor.
	for path, expected := range map[string]os.FileInfo{sourceDir: sourceInfo, socketDir: socketDirectoryInfo} {
		current, err := bridgeDirectory(path)
		if err != nil || !os.SameFile(current, expected) {
			return nil, errors.New("filesystem bridge directory changed during startup")
		}
	}
	if err := checkSessionPaths(sourceDir, socket, descriptorPath); err != nil {
		return nil, err
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, errors.New("could not create filesystem bridge capability")
	}
	handler, err := New(filesystem, token)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	socketInfo, err := os.Lstat(socket)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	cleanup := func() { _ = listener.Close(); _ = removeOwned(socket, socketInfo) }
	if err := os.Chmod(socket, 0o600); err != nil {
		cleanup()
		return nil, err
	}
	privateSocketInfo, err := os.Lstat(socket)
	if err != nil || !os.SameFile(privateSocketInfo, socketInfo) {
		cleanup()
		return nil, errors.New("filesystem bridge socket changed while securing it")
	}
	socketInfo = privateSocketInfo
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, err
	}
	descriptor := Descriptor{Version: Version, Socket: socket, Token: hex.EncodeToString(token)}
	descriptorInfo, err := publishDescriptor(sourceDir, descriptorPath, descriptor)
	if err != nil {
		cleanup()
		return nil, err
	}
	server := &Server{directory: sourceDir, socket: socket, descriptorPath: descriptorPath, socketInfo: socketInfo, descriptorInfo: descriptorInfo, sourceInfo: sourceInfo, socketDirInfo: socketDirectoryInfo, lease: lease, handler: handler, done: make(chan struct{})}
	lease = nil
	server.http = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10}
	go func() {
		err := server.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		server.mu.Lock()
		server.err = err
		server.mu.Unlock()
		close(server.done)
	}()
	return server, nil
}

func checkSessionPaths(sourceDir, socket, descriptorPath string) error {
	for _, path := range []string{socket, filepath.Join(sourceDir, "bridge.sock")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return err
			}
			return errors.New("filesystem bridge socket already exists")
		}
	}
	fd, err := syscall.Open(descriptorPath, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("filesystem bridge descriptor is unavailable or unsafe")
	}
	file := os.NewFile(uintptr(fd), descriptorPath)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return errors.New("filesystem bridge descriptor could not be checked")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || int(owner.Uid) != os.Getuid() || info.Size() <= 0 || info.Size() > 16<<10 {
		return errors.New("filesystem bridge descriptor must be a bounded private regular file owned by the current user")
	}
	data, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	var descriptor Descriptor
	if err != nil || len(data) > 16<<10 || json.Unmarshal(data, &descriptor) != nil || descriptor.Version != Version || len(descriptor.Token) != 64 {
		return errors.New("filesystem bridge descriptor is invalid; preserve the session until its mount is safely detached")
	}
	for _, value := range descriptor.Token {
		if !(value >= '0' && value <= '9' || value >= 'a' && value <= 'f') {
			return errors.New("filesystem bridge descriptor is invalid; preserve the session until its mount is safely detached")
		}
	}
	if descriptor.Socket != socket {
		return errors.New("filesystem bridge descriptor belongs to a different socket location; preserve the session until its mount is safely detached")
	}
	return nil
}

func acquireSourceLease(sourceDir string) (*os.File, error) {
	path := filepath.Join(sourceDir, ".bridge.lock")
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, errors.New("filesystem bridge session lease is unavailable or unsafe")
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || int(owner.Uid) != os.Getuid() {
		_ = file.Close()
		return nil, errors.New("filesystem bridge session lease must be a private regular file owned by the current user")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("filesystem bridge resource already has an active session")
		}
		return nil, errors.New("filesystem bridge session lease could not be acquired")
	}
	// Never unlink this inode: another process could then lock a different file
	// at the same path while this session still holds its original lease.
	return file, nil
}

func publishDescriptor(directory, path string, descriptor Descriptor) (os.FileInfo, error) {
	data, err := json.Marshal(descriptor)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(directory, ".connection-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	dir, err := os.Open(directory)
	if err != nil {
		_ = removeOwned(path, info)
		return nil, err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil || closeErr != nil {
		_ = removeOwned(path, info)
		return nil, errors.Join(err, closeErr)
	}
	return info, nil
}

func (s *Server) SourceDirectory() string { return s.directory }
func (s *Server) Done() <-chan struct{}   { return s.done }
func (s *Server) Err() error              { s.mu.Lock(); defer s.mu.Unlock(); return s.err }

// CloseDrain waits for all RPC calls to finish before releasing their handles
// and lookup references. A timeout leaves those resources intact; calling this
// method again can finish draining. It deliberately does not destroy the
// filesystem or close daemon-owned stores.
func (s *Server) CloseDrain(ctx context.Context) error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	if err := s.http.Shutdown(ctx); err != nil {
		return err
	}
	if err := s.handler.closeResources(ctx); err != nil {
		return err
	}
	var result error
	result = errors.Join(result, removeOwned(s.socket, s.socketInfo))
	result = errors.Join(result, removeOwned(s.descriptorPath, s.descriptorInfo))
	if result != nil {
		return result
	}
	if s.lease != nil {
		err := s.lease.Close()
		s.lease = nil
		if err != nil {
			return err
		}
	}
	s.closed = true
	return nil
}

func removeOwned(path string, expected os.FileInfo) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(info, expected) {
		return fmt.Errorf("filesystem bridge session path was replaced: %s", path)
	}
	return os.Remove(path)
}
