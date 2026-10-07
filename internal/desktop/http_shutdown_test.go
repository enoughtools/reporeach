package desktop

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/jacobsa/fuse/fuseops"
)

type shutdownTestMount struct {
	*desktopFakeMount
	attempted chan struct{}
}

func (m *shutdownTestMount) Unmount() error {
	err := m.desktopFakeMount.Unmount()
	m.attempted <- struct{}{}
	return err
}

type shutdownHTTPFixture struct {
	service  *Service
	mount    *shutdownTestMount
	listener net.Listener
	socket   string
	cancel   context.CancelFunc
	done     chan error
}

func startShutdownHTTP(t *testing.T, refusal error) *shutdownHTTPFixture {
	t.Helper()
	opts := desktopHTTPOptions(t)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := New(ctx, opts)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s.hybridCatalogue = false
	s.quiescentCatalogue = false
	s.dependencyReady = func() bool { return true }
	mount := &shutdownTestMount{desktopFakeMount: newDesktopFakeMount(), attempted: make(chan struct{}, 16)}
	mount.unmountErr = refusal
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		return mount, nil
	}
	if err := s.Mount(context.Background()); err != nil {
		cancel()
		_ = s.Close()
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", opts.Socket)
	if err != nil {
		cancel()
		_ = s.Close()
		t.Fatal(err)
	}
	if err := os.Chmod(opts.Socket, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &shutdownHTTPFixture{service: s, mount: mount, listener: listener, socket: opts.Socket, cancel: cancel, done: make(chan error, 1)}
	go func() { f.done <- serveControlService(ctx, listener, s) }()
	t.Cleanup(func() {
		mount.mu.Lock()
		mount.unmountErr = nil
		mount.mu.Unlock()
		cancel()
		_ = s.PrepareQuit(context.Background())
		select {
		case <-f.done:
		case <-time.After(3 * time.Second):
			t.Error("retained shutdown fixture did not stop after normal detachment")
		}
		_ = listener.Close()
		_ = s.Close()
	})
	f.request(t, http.MethodGet, "/v1/status", http.StatusOK)
	return f
}

func (f *shutdownHTTPFixture) request(t *testing.T, method, path string, wantStatus int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data, status, err := Request(ctx, f.socket, method, path, nil)
	if err != nil || status != wantStatus {
		t.Fatalf("%s %s = %d, %s, %v", method, path, status, data, err)
	}
	return data
}

func (f *shutdownHTTPFixture) awaitAttempt(t *testing.T) {
	t.Helper()
	select {
	case <-f.mount.attempted:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not attempt normal detachment")
	}
}

func (f *shutdownHTTPFixture) assertRetained(t *testing.T) {
	t.Helper()
	select {
	case err := <-f.done:
		f.done <- err
		t.Fatalf("failed normal detachment terminated the owner: %v", err)
	default:
	}
	if err := f.service.ctx.Err(); err != nil {
		t.Fatalf("failed shutdown canceled the live filesystem: %v", err)
	}
	f.service.mu.Lock()
	closing, closed, prepared := f.service.closing, f.service.closed, f.service.quitPrepared
	f.service.mu.Unlock()
	if closing || closed || prepared || !f.service.Status().Mounted {
		t.Fatalf("failed shutdown changed live ownership: closing=%v, closed=%v, prepared=%v", closing, closed, prepared)
	}
	select {
	case <-f.service.quitReady:
		t.Fatal("failed shutdown signaled successful quit preparation")
	default:
	}
}

func TestControlShutdownRefusalRetainsFilesystemAndManualRetry(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"busy writer", syscall.EBUSY},
		{"fsync failure", syscall.EIO},
		{"uncertain ownership", errors.New("mount ownership could not be verified")},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := startShutdownHTTP(t, test.err)
			fs, metadata := f.service.catalog, f.service.catalogMetadata
			f.cancel()
			f.awaitAttempt(t)
			f.assertRetained(t)
			// A request after signal cancellation must still carry a live context.
			// PrepareQuit checks it before issuing the next normal-unmount attempt.
			f.request(t, http.MethodPost, "/v1/prepare-quit", http.StatusBadRequest)
			f.awaitAttempt(t)
			f.request(t, http.MethodGet, "/v1/status", http.StatusOK)
			value := []byte{0, 0xff, 1}
			if err := fs.SetXattr(f.service.ctx, &fuseops.SetXattrOp{Inode: fuseops.RootInodeID, Name: "user.catalogue", Value: value}); err != nil {
				t.Fatalf("retained filesystem lost writable metadata: %v", err)
			}
			if got := readDesktopCatalogueXattr(t, fs, fuseops.RootInodeID); !bytes.Equal(got, value) {
				t.Fatalf("retained filesystem data = %x", got)
			}
			if _, err := f.service.engine.ListRepos(f.service.ctx); err != nil {
				t.Fatalf("retained daemon lost its database: %v", err)
			}
			select {
			case <-f.mount.attempted:
				t.Fatal("shutdown retried detachment without an explicit request")
			case <-time.After(30 * time.Millisecond):
			}
			f.mount.mu.Lock()
			f.mount.unmountErr = nil
			f.mount.mu.Unlock()
			// Shutdown must drain this handler so the successful retry response is
			// delivered before it closes the transport and catalogue database.
			f.request(t, http.MethodPost, "/v1/prepare-quit", http.StatusOK)
			select {
			case err := <-f.done:
				f.done <- err
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("successful explicit retry did not finish shutdown")
			}
			if _, err := metadata.HasMetadataXattrs(context.Background()); err == nil {
				t.Fatal("successful detachment retained the catalogue database")
			}
			if !errors.Is(f.service.ctx.Err(), context.Canceled) {
				t.Fatal("successful shutdown did not end the service lifetime")
			}
		})
	}
}

func TestControlListenerFailureRetainsBusyFilesystemOwner(t *testing.T) {
	f := startShutdownHTTP(t, syscall.EBUSY)
	if err := f.listener.Close(); err != nil {
		t.Fatal(err)
	}
	f.awaitAttempt(t)
	f.assertRetained(t)
	if _, err := f.service.catalogMetadata.HasMetadataXattrs(f.service.ctx); err != nil {
		t.Fatalf("listener failure closed live filesystem storage: %v", err)
	}
	f.mount.mu.Lock()
	f.mount.unmountErr = nil
	f.mount.mu.Unlock()
	if err := f.service.PrepareQuit(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-f.done:
		f.done <- err
		if err == nil {
			t.Fatal("fatal listener error was suppressed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("prepared owner did not finish after listener failure")
	}
}

func TestControlPreparedQuitFinishesWithoutSignal(t *testing.T) {
	f := startShutdownHTTP(t, nil)
	f.request(t, http.MethodPost, "/v1/prepare-quit", http.StatusOK)
	select {
	case err := <-f.done:
		f.done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("successful terminal preparation waited for an additional signal")
	}
	f.mount.mu.Lock()
	defer f.mount.mu.Unlock()
	if f.mount.unmountCalls != 1 {
		t.Fatalf("terminal preparation attempted %d normal unmounts", f.mount.unmountCalls)
	}
}

func TestNewRejectsCanceledStartupBeforeCreatingState(t *testing.T) {
	opts := desktopHTTPOptions(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if service, err := New(ctx, opts); !errors.Is(err, context.Canceled) || service != nil {
		t.Fatalf("canceled startup = %v, %v", service, err)
	}
	if _, err := os.Lstat(filepath.Join(opts.StateDir, "catalogue.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled startup wrote state: %v", err)
	}
}

func TestCanceledPreparationAfterDetachDoesNotSignalQuitReady(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mount := &catalogueChangeMount{desktopFakeMount: newDesktopFakeMount(), onUnmount: cancel}
	s.dependencyReady = func() bool { return true }
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		return mount, nil
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareQuit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled preparation = %v", err)
	}
	select {
	case <-s.quitReady:
		t.Fatal("canceled preparation authorized process exit")
	default:
	}
	if err := s.ctx.Err(); err != nil {
		t.Fatalf("canceled request ended service lifetime: %v", err)
	}
	if err := s.PrepareQuit(context.Background()); err != nil {
		t.Fatalf("explicit retry after canceled preparation = %v", err)
	}
	select {
	case <-s.quitReady:
	default:
		t.Fatal("successful explicit retry did not signal quit readiness")
	}
}
