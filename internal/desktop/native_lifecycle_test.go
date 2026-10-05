package desktop

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

func TestPartialMountErrorRetainsLifecycleOwner(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return true }
	mount := newDesktopFakeMount()
	verificationErr := errors.New("mount identity has not been verified")
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		return mount, verificationErr
	}
	if err := s.Mount(context.Background()); !errors.Is(err, verificationErr) {
		t.Fatalf("Mount error = %v", err)
	}
	if !s.Status().Mounted || s.catalog == nil {
		t.Fatal("failed verification discarded a possibly live mount")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	mount.mu.Lock()
	defer mount.mu.Unlock()
	if mount.unmountCalls != 1 {
		t.Fatalf("Close attempted %d unmounts", mount.unmountCalls)
	}
}

func TestPrepareQuitRetainsDesiredMountAndRefusesBusyVolume(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return true }
	mount := newDesktopFakeMount()
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		return mount, nil
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	mount.mu.Lock()
	mount.unmountErr = syscall.EBUSY
	mount.mu.Unlock()
	if err := s.PrepareQuit(context.Background()); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("busy quit error = %v", err)
	}
	if !s.Status().Mounted || s.catalog == nil {
		t.Fatal("busy quit discarded the active filesystem")
	}
	assertDesired := func() {
		t.Helper()
		state, err := readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
		if err != nil || !state.MountDesired {
			t.Fatalf("quit changed persisted mount intent: %v, %+v", err, state)
		}
	}
	assertDesired()
	mount.mu.Lock()
	mount.unmountErr = nil
	mount.mu.Unlock()
	if err := s.PrepareQuit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Status().Mounted {
		t.Fatal("successful quit preparation retained a mounted volume")
	}
	assertDesired()
	if err := s.Mount(context.Background()); err == nil {
		t.Fatal("a control request remounted after successful quit preparation")
	}
	if _, err := s.Action("missing", "prepare"); err == nil || err.Error() != "service is closing" {
		t.Fatalf("new work after quit preparation = %v", err)
	}
	if err := s.PrepareQuit(context.Background()); err != nil {
		t.Fatalf("idempotent quit preparation = %v", err)
	}
}

func TestPrepareQuitCanceledRequestLeavesMountAttached(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return true }
	mount := newDesktopFakeMount()
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		return mount, nil
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.PrepareQuit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled quit error = %v", err)
	}
	if !s.Status().Mounted {
		t.Fatal("canceled quit detached the volume")
	}
	mount.mu.Lock()
	defer mount.mu.Unlock()
	if mount.unmountCalls != 0 {
		t.Fatal("canceled request attempted unmount")
	}
}

func TestCloseCanRetryAfterBusyUnmount(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return true }
	mount := newDesktopFakeMount()
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		return mount, nil
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	mount.mu.Lock()
	mount.unmountErr = syscall.EBUSY
	mount.mu.Unlock()
	if err := s.Close(); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("first Close = %v", err)
	}
	if !s.Status().Mounted || s.closed {
		t.Fatal("failed Close discarded the active filesystem")
	}
	mount.mu.Lock()
	mount.unmountErr = nil
	mount.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatalf("retry Close = %v", err)
	}
	if s.Status().Mounted || !s.closed {
		t.Fatal("retry Close did not finish detaching")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	mount.mu.Lock()
	defer mount.mu.Unlock()
	if mount.unmountCalls != 2 {
		t.Fatalf("Close calls = %d, want two attempts", mount.unmountCalls)
	}
}
