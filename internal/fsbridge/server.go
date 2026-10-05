//go:build !windows

package fsbridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(sourceDir) {
		return nil, errors.New("filesystem bridge source must be an absolute private directory")
	}
	info, err := os.Lstat(sourceDir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ok || int(stat.Uid) != os.Getuid() {
		return nil, errors.New("filesystem bridge source must be a private directory owned by the current user")
	}
	socket := filepath.Join(sourceDir, "bridge.sock")
	descriptorPath := filepath.Join(sourceDir, "connection.json")
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("filesystem bridge socket already exists")
	}
	if old, err := os.Lstat(descriptorPath); err == nil {
		owner, ok := old.Sys().(*syscall.Stat_t)
		if !old.Mode().IsRegular() || old.Mode().Perm()&0o077 != 0 || !ok || int(owner.Uid) != os.Getuid() {
			return nil, errors.New("filesystem bridge descriptor must be a private regular file owned by the current user")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
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
	server := &Server{directory: sourceDir, socket: socket, descriptorPath: descriptorPath, socketInfo: socketInfo, descriptorInfo: descriptorInfo, handler: handler, done: make(chan struct{})}
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
