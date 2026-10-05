//go:build darwin

package desktop

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
)

type fakePlatformBridge struct {
	source string
	done   chan struct{}
	mu     sync.Mutex
	err    error
	closes int
	close  func(context.Context) error
}

func (b *fakePlatformBridge) SourceDirectory() string { return b.source }
func (b *fakePlatformBridge) Done() <-chan struct{}   { return b.done }
func (b *fakePlatformBridge) Err() error              { b.mu.Lock(); defer b.mu.Unlock(); return b.err }
func (b *fakePlatformBridge) CloseDrain(ctx context.Context) error {
	b.mu.Lock()
	b.closes++
	closeFn := b.close
	b.mu.Unlock()
	if closeFn != nil {
		return closeFn(ctx)
	}
	return nil
}
func (b *fakePlatformBridge) closeCount() int { b.mu.Lock(); defer b.mu.Unlock(); return b.closes }

type fakeFSKitMount struct {
	service    *Service
	root       string
	bridge     *fakePlatformBridge
	ops        fsKitMountOperations
	mu         sync.Mutex
	mounts     []fsKitMountIdentity
	inspectErr error
	commands   [][]string
	command    func(context.Context, string, ...string) error
	starts     int
}

func newFakeFSKitMount(t *testing.T) *fakeFSKitMount {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFSKitMount{service: &Service{opts: Options{StateDir: t.TempDir()}}, root: root,
		bridge: &fakePlatformBridge{done: make(chan struct{})}}
	f.ops = fsKitMountOperations{
		ready: func() bool { return true },
		start: func(ctx context.Context, source string, fs *catalogfs.FileSystem) (platformBridge, error) {
			f.starts++
			f.bridge.source = source
			if ctx.Done() != nil {
				t.Error("bridge inherited request cancellation")
			}
			return f.bridge, nil
		},
		command: func(ctx context.Context, program string, args ...string) error {
			f.mu.Lock()
			f.commands = append(f.commands, append([]string{program}, args...))
			command := f.command
			f.mu.Unlock()
			if command != nil {
				return command(ctx, program, args...)
			}
			if program == "/sbin/mount" {
				f.setMounts(f.ownIdentity())
			} else {
				f.setMounts()
			}
			return nil
		},
		rootFSID: func(string) ([2]int32, error) { return [2]int32{7, 8}, nil },
		mounts: func() ([]fsKitMountIdentity, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]fsKitMountIdentity(nil), f.mounts...), f.inspectErr
		},
		poll: time.Millisecond, timeout: 35 * time.Millisecond, verifyDelay: 4 * time.Millisecond,
	}
	return f
}

func (f *fakeFSKitMount) ownIdentity() fsKitMountIdentity {
	// A system FSKit broker may report its own uid and "lifs". Neither an
	// assumed type label nor caller uid is the session proof.
	return fsKitMountIdentity{fsid: [2]int32{101, 202}, owner: 0, typeName: "lifs", root: f.root, source: f.bridge.source}
}
func (f *fakeFSKitMount) setMounts(mounts ...fsKitMountIdentity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounts = append([]fsKitMountIdentity(nil), mounts...)
}
func (f *fakeFSKitMount) commandCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.commands) }
func (f *fakeFSKitMount) mount(t *testing.T) *nativeFSKitMount {
	t.Helper()
	mounted, err := f.service.mountNativeFSKit(context.Background(), f.root, nil, f.ops)
	if err != nil {
		t.Fatal(err)
	}
	return mounted.(*nativeFSKitMount)
}

func TestNativeFSKitOSAvailability(t *testing.T) {
	for release, want := range map[string]bool{"24.4.0": false, "25.0.0": true, "26.1.2": true, "25": true, "": false, "invalid": false, "25x.0": false, "-25.0": false} {
		if got := supportsNativeFSKit(release); got != want {
			t.Errorf("release %q: %v, want %v", release, got, want)
		}
	}
	if !strings.Contains(platformDependencyMessage(), "macOS 26") || strings.Contains(platformDependencyMessage(), "macFUSE") {
		t.Fatal(platformDependencyMessage())
	}
}

func TestNativeFSKitSourceIdentity(t *testing.T) {
	path := "/Users/example/Library/Application Support/RepoReach/FSKit"
	fileURL := (&url.URL{Scheme: "file", Path: path}).String()
	for _, source := range []string{path, fileURL, "file://localhost" + strings.TrimPrefix(fileURL, "file://")} {
		if !mountSourceMatches(source, path) {
			t.Errorf("rejected source %q", source)
		}
	}
	for _, source := range []string{path + "-other", fileURL + "?token=x", fileURL + "#fragment", "file://other" + path, "https://example" + path, "file:opaque"} {
		if mountSourceMatches(source, path) {
			t.Errorf("accepted different source %q", source)
		}
	}
}

func TestNativeFSKitMountChecksBeforeStartingBridge(t *testing.T) {
	t.Run("unsupported OS", func(t *testing.T) {
		f := newFakeFSKitMount(t)
		f.ops.ready = func() bool { return false }
		mounted, err := f.service.mountNativeFSKit(context.Background(), f.root, nil, f.ops)
		if mounted != nil || err == nil || f.starts != 0 || f.commandCount() != 0 {
			t.Fatalf("mounted=%v err=%v starts=%d", mounted, err, f.starts)
		}
		if _, err := os.Stat(filepath.Join(f.service.opts.StateDir, "FSKit")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unsupported OS created source directory")
		}
	})
	t.Run("already mounted", func(t *testing.T) {
		f := newFakeFSKitMount(t)
		f.setMounts(fsKitMountIdentity{root: f.root, fsid: [2]int32{10, 20}})
		mounted, err := f.service.mountNativeFSKit(context.Background(), f.root, nil, f.ops)
		if mounted != nil || err == nil || f.starts != 0 || f.commandCount() != 0 {
			t.Fatalf("mounted=%v err=%v starts=%d", mounted, err, f.starts)
		}
	})
	t.Run("canceled request", func(t *testing.T) {
		f := newFakeFSKitMount(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		mounted, err := f.service.mountNativeFSKit(ctx, f.root, nil, f.ops)
		if mounted != nil || !errors.Is(err, context.Canceled) || f.starts != 0 {
			t.Fatalf("mounted=%v err=%v starts=%d", mounted, err, f.starts)
		}
	})
}

func TestNativeFSKitMountAndNormalDrain(t *testing.T) {
	f := newFakeFSKitMount(t)
	m := f.mount(t)
	info, err := os.Stat(f.bridge.source)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private source directory: %v %v", info, err)
	}
	want := []string{"/sbin/mount", "-F", "-t", "reporeach", f.bridge.source, f.root}
	if !reflect.DeepEqual(f.commands[0], want) {
		t.Fatalf("command %v, want %v", f.commands[0], want)
	}
	if !m.verified || f.bridge.closeCount() != 0 {
		t.Fatal("mount was not retained")
	}
	if err := m.Unmount(); err != nil {
		t.Fatal(err)
	}
	if err := m.Join(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.bridge.closeCount() != 1 {
		t.Fatalf("drains=%d", f.bridge.closeCount())
	}
	if !reflect.DeepEqual(f.commands[1], []string{"/sbin/umount", f.root}) {
		t.Fatal(f.commands[1])
	}
}

func TestNativeFSKitFailedCommandRetainsAttachedMount(t *testing.T) {
	f := newFakeFSKitMount(t)
	f.command = func(context.Context, string, ...string) error {
		f.setMounts(f.ownIdentity())
		return errors.New("sensitive backend diagnostic")
	}
	mounted, err := f.service.mountNativeFSKit(context.Background(), f.root, nil, f.ops)
	if mounted == nil || err == nil || strings.Contains(err.Error(), "sensitive") || f.bridge.closeCount() != 0 {
		t.Fatalf("mounted=%v err=%v drains=%d", mounted, err, f.bridge.closeCount())
	}
	f.command = nil
	if err := mounted.Unmount(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeFSKitCancelledCommandRetainsAttachedMount(t *testing.T) {
	f := newFakeFSKitMount(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.command = func(ctx context.Context, _ string, _ ...string) error {
		f.setMounts(f.ownIdentity())
		cancel()
		return ctx.Err()
	}
	mounted, err := f.service.mountNativeFSKit(ctx, f.root, nil, f.ops)
	if mounted == nil || err == nil || f.bridge.closeCount() != 0 {
		t.Fatalf("mounted=%v err=%v drains=%d", mounted, err, f.bridge.closeCount())
	}
	f.command = nil
	if err := mounted.Unmount(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeFSKitInterruptedSetupRetainsLateAttachment(t *testing.T) {
	f := newFakeFSKitMount(t)
	f.command = func(context.Context, string, ...string) error { return context.DeadlineExceeded }
	mounted, err := f.service.mountNativeFSKit(context.Background(), f.root, nil, f.ops)
	if mounted == nil || err == nil || f.bridge.closeCount() != 0 {
		t.Fatalf("mounted=%v err=%v drains=%d", mounted, err, f.bridge.closeCount())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Millisecond)
	defer cancel()
	if err := mounted.Join(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := mounted.Unmount(); !errors.Is(err, errFSKitMountOwnership) {
		t.Fatal(err)
	}
	if f.bridge.closeCount() != 0 || f.commandCount() != 1 {
		t.Fatal("unobserved broker task was released")
	}
	// A broker attachment arriving after the mount command exited must still
	// have its connection. Once observed, normal identity-safe cleanup works.
	f.setMounts(f.ownIdentity())
	f.command = nil
	if err := mounted.Unmount(); err != nil {
		t.Fatal(err)
	}
	if f.bridge.closeCount() != 1 || f.commandCount() != 2 {
		t.Fatalf("drains=%d commands=%d", f.bridge.closeCount(), f.commandCount())
	}
}

func TestNativeFSKitUncertainAttachmentRetainsBridge(t *testing.T) {
	for _, kind := range []string{"inspection error", "unknown source", "unchanged FSID", "moved resource"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeFSKitMount(t)
			f.command = func(context.Context, string, ...string) error {
				identity := f.ownIdentity()
				if kind == "unknown source" {
					identity.source = "unverified-resource"
				}
				if kind == "unchanged FSID" {
					identity.fsid = [2]int32{7, 8}
				}
				if kind == "moved resource" {
					identity.root = "/somewhere/else"
				}
				f.setMounts(identity)
				if kind == "inspection error" {
					f.mu.Lock()
					f.inspectErr = errors.New("unavailable")
					f.mu.Unlock()
				}
				return nil
			}
			mounted, err := f.service.mountNativeFSKit(context.Background(), f.root, nil, f.ops)
			if mounted == nil || !errors.Is(err, errFSKitMountOwnership) || f.bridge.closeCount() != 0 {
				t.Fatalf("mounted=%v err=%v drains=%d", mounted, err, f.bridge.closeCount())
			}
			if err := mounted.Unmount(); !errors.Is(err, errFSKitMountOwnership) || f.commandCount() != 1 || f.bridge.closeCount() != 0 {
				t.Fatalf("err=%v commands=%d drains=%d", err, f.commandCount(), f.bridge.closeCount())
			}
		})
	}
}

func TestNativeFSKitCancelledBeforeCommandRetainsFailedDrain(t *testing.T) {
	f := newFakeFSKitMount(t)
	ctx, cancel := context.WithCancel(context.Background())
	start := f.ops.start
	f.ops.start = func(ctx context.Context, source string, fs *catalogfs.FileSystem) (platformBridge, error) {
		bridge, err := start(ctx, source, fs)
		cancel()
		return bridge, err
	}
	f.bridge.close = func(context.Context) error { return errors.New("drain failed") }
	mounted, err := f.service.mountNativeFSKit(ctx, f.root, nil, f.ops)
	if mounted == nil || !errors.Is(err, context.Canceled) || f.commandCount() != 0 {
		t.Fatalf("mounted=%v err=%v commands=%d", mounted, err, f.commandCount())
	}
	f.bridge.close = nil
	if err := mounted.Unmount(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeFSKitUnmountCommandFailureAfterDetachIsSafe(t *testing.T) {
	f := newFakeFSKitMount(t)
	m := f.mount(t)
	f.command = func(context.Context, string, ...string) error {
		f.setMounts()
		return errors.New("command exited after detach")
	}
	if err := m.Unmount(); err != nil {
		t.Fatal(err)
	}
	if f.bridge.closeCount() != 1 {
		t.Fatal("detached mount was not drained")
	}
}

func TestNativeFSKitJoinInspectionFailureDoesNotReleaseStores(t *testing.T) {
	f := newFakeFSKitMount(t)
	m := f.mount(t)
	f.mu.Lock()
	f.inspectErr = errors.New("inspection unavailable")
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := m.Join(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if f.bridge.closeCount() != 0 {
		t.Fatal("uncertain mount was drained")
	}
}

func TestNativeFSKitCommandSuccessRequiresActualAttachment(t *testing.T) {
	f := newFakeFSKitMount(t)
	f.command = func(context.Context, string, ...string) error { return nil }
	mounted, err := f.service.mountNativeFSKit(context.Background(), f.root, nil, f.ops)
	if mounted != nil || err == nil || f.bridge.closeCount() != 1 {
		t.Fatalf("mounted=%v err=%v drains=%d", mounted, err, f.bridge.closeCount())
	}
}

func TestNativeFSKitBusyOrIncompleteUnmountRetainsBridge(t *testing.T) {
	for _, commandErr := range []error{errors.New("busy"), nil} {
		t.Run(map[bool]string{true: "busy", false: "successful command still mounted"}[commandErr != nil], func(t *testing.T) {
			f := newFakeFSKitMount(t)
			m := f.mount(t)
			f.command = func(context.Context, string, ...string) error { return commandErr }
			start := time.Now()
			if err := m.Unmount(); err == nil {
				t.Fatal("unmount reported success with live mount")
			}
			if time.Since(start) > time.Second || f.bridge.closeCount() != 0 {
				t.Fatal("unmount did not retain bridge promptly")
			}
			f.command = nil
			if err := m.Unmount(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeFSKitUnmountNeverTargetsObservedReplacement(t *testing.T) {
	t.Run("old identity gone", func(t *testing.T) {
		f := newFakeFSKitMount(t)
		m := f.mount(t)
		replacement := f.ownIdentity()
		replacement.fsid = [2]int32{300, 400}
		replacement.source = "/other/source"
		f.setMounts(replacement)
		if err := m.Unmount(); err != nil {
			t.Fatal(err)
		}
		if f.commandCount() != 1 || f.bridge.closeCount() != 1 {
			t.Fatalf("commands=%d drains=%d", f.commandCount(), f.bridge.closeCount())
		}
	})
	t.Run("old identity moved", func(t *testing.T) {
		f := newFakeFSKitMount(t)
		m := f.mount(t)
		original := f.ownIdentity()
		original.root = "/somewhere/else"
		replacement := f.ownIdentity()
		replacement.fsid = [2]int32{300, 400}
		replacement.source = "/other/source"
		f.setMounts(original, replacement)
		if err := m.Unmount(); !errors.Is(err, errFSKitMountOwnership) {
			t.Fatal(err)
		}
		if f.commandCount() != 1 || f.bridge.closeCount() != 0 {
			t.Fatalf("commands=%d drains=%d", f.commandCount(), f.bridge.closeCount())
		}
	})
	t.Run("same FSID different source", func(t *testing.T) {
		f := newFakeFSKitMount(t)
		m := f.mount(t)
		replacement := f.ownIdentity()
		replacement.source = "/other/source"
		f.setMounts(replacement)
		if err := m.Unmount(); !errors.Is(err, errFSKitMountOwnership) {
			t.Fatal(err)
		}
		if f.commandCount() != 1 || f.bridge.closeCount() != 0 {
			t.Fatalf("commands=%d drains=%d", f.commandCount(), f.bridge.closeCount())
		}
	})
}

func TestNativeFSKitJoinCancellationAndBridgeFailureKeepOwnership(t *testing.T) {
	f := newFakeFSKitMount(t)
	m := f.mount(t)
	f.bridge.mu.Lock()
	f.bridge.err = errors.New("bridge stopped")
	f.bridge.mu.Unlock()
	close(f.bridge.done)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if err := m.Join(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if f.bridge.closeCount() != 0 {
		t.Fatal("observation closed a mounted bridge")
	}
	f.setMounts()
	if err := m.Join(context.Background()); err == nil || err.Error() != "bridge stopped" {
		t.Fatalf("lost bridge failure after safe detach: %v", err)
	}
	if f.bridge.closeCount() != 1 {
		t.Fatal("detached bridge was not drained")
	}
}

func TestNativeFSKitNormalCleanupSuppressesHistoricalBridgeFailure(t *testing.T) {
	f := newFakeFSKitMount(t)
	m := f.mount(t)
	f.bridge.mu.Lock()
	f.bridge.err = errors.New("previous connection failure")
	f.bridge.mu.Unlock()
	close(f.bridge.done)
	if err := m.Unmount(); err != nil {
		t.Fatal(err)
	}
	if err := m.Join(context.Background()); err != nil {
		t.Fatalf("completed cleanup failed forever: %v", err)
	}
	if f.bridge.closeCount() != 1 {
		t.Fatal("cleanup was not fully drained")
	}
}

func TestNativeFSKitDrainFailureRemainsRetryable(t *testing.T) {
	f := newFakeFSKitMount(t)
	m := f.mount(t)
	f.bridge.close = func(context.Context) error { return errors.New("pending request") }
	if err := m.Unmount(); err == nil {
		t.Fatal("ignored failed drain")
	}
	if f.bridge.closeCount() != 1 {
		t.Fatal("expected first drain attempt")
	}
	f.bridge.close = nil
	if err := m.Unmount(); err != nil {
		t.Fatal(err)
	}
	if f.bridge.closeCount() != 2 || f.commandCount() != 2 {
		t.Fatalf("drains=%d commands=%d", f.bridge.closeCount(), f.commandCount())
	}
}

func TestNativeFSKitDrainSerializationHonorsCancellation(t *testing.T) {
	f := newFakeFSKitMount(t)
	m := f.mount(t)
	f.setMounts()
	entered := make(chan struct{})
	f.bridge.close = func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.drain(ctx) }()
	<-entered
	start := time.Now()
	if err := m.Unmount(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked drain cancellation: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("unmount waited on drain without its deadline")
	}
	cancel()
	<-done
}

func TestNativeFSKitConcurrentUnmountIsSerialized(t *testing.T) {
	f := newFakeFSKitMount(t)
	m := f.mount(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.command = func(ctx context.Context, _ string, _ ...string) error {
		close(entered)
		select {
		case <-release:
			f.setMounts()
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	first := make(chan error, 1)
	go func() { first <- m.Unmount() }()
	<-entered
	second := make(chan error, 1)
	go func() { second <- m.Unmount() }()
	// Releasing the first command also lets the second inspect a detached
	// session. It must not issue another path-based unmount.
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if f.commandCount() != 2 || f.bridge.closeCount() != 1 {
		t.Fatalf("commands=%d drains=%d", f.commandCount(), f.bridge.closeCount())
	}
}

func TestNativeFSKitMountCommandCancellationIsBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := runFSKitMountCommand(ctx, "/bin/sh", "-c", "sleep 30 & wait")
	if err == nil || ctx.Err() == nil || time.Since(start) > time.Second {
		t.Fatalf("canceled command: err=%v elapsed=%v", err, time.Since(start))
	}
}
