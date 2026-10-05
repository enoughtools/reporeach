//go:build darwin

package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
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
	path := "/Users/example/Library/Application Support/RepoReach #1?check/FSKit"
	fileURL := (&url.URL{Scheme: "file", Path: path}).String()
	for _, test := range []struct {
		name, source string
		want         bool
	}{
		{"exact path", path, true},
		{"encoded file URL", fileURL, true},
		{"localhost URL", "file://localhost" + strings.TrimPrefix(fileURL, "file://"), true},
		{"different path", path + "-other", false},
		{"different URL path", fileURL + "-other", false},
		{"trailing slash", fileURL + "/", false},
		{"query", fileURL + "?token=x", false},
		{"empty query", fileURL + "?", false},
		{"fragment", fileURL + "#fragment", false},
		{"empty fragment", fileURL + "#", false},
		{"empty query and fragment", fileURL + "?#", false},
		{"userinfo", "file://user@localhost" + strings.TrimPrefix(fileURL, "file://"), false},
		{"remote host", "file://other" + strings.TrimPrefix(fileURL, "file://"), false},
		{"localhost port", "file://localhost:123" + strings.TrimPrefix(fileURL, "file://"), false},
		{"different scheme", "https://example" + strings.TrimPrefix(fileURL, "file://"), false},
		{"opaque file URL", "file:opaque", false},
		{"malformed escape", fileURL + "%zz", false},
		{"prefix substring", "unrelated " + fileURL, false},
		{"unverified FSKit source decoration", "RepoReach -- " + fileURL, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := mountSourceMatches(test.source, path); got != test.want {
				t.Fatalf("source %q matched=%v, want %v", test.source, got, test.want)
			}
		})
	}
}

func TestNativeFSKitURLSourceOwnsNormalLifecycle(t *testing.T) {
	f := newFakeFSKitMount(t)
	f.service.opts.StateDir = filepath.Join(f.service.opts.StateDir, "RepoReach #1?check")
	f.command = func(_ context.Context, program string, _ ...string) error {
		if program == "/sbin/mount" {
			identity := f.ownIdentity()
			identity.source = (&url.URL{Scheme: "file", Path: identity.source}).String()
			f.setMounts(identity)
		} else {
			f.setMounts()
		}
		return nil
	}
	m := f.mount(t)
	if !m.verified || f.bridge.closeCount() != 0 {
		t.Fatal("encoded file URL was not retained as the live mount")
	}
	if err := m.Unmount(); err != nil {
		t.Fatal(err)
	}
	if err := m.Join(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.commandCount() != 2 || f.bridge.closeCount() != 1 {
		t.Fatalf("commands=%d drains=%d", f.commandCount(), f.bridge.closeCount())
	}
}

func TestNativeFSKitExtraURLSyntaxDoesNotAuthorizeUnmount(t *testing.T) {
	for _, suffix := range []string{"?", "?token=x", "#", "#fragment", "?#"} {
		t.Run(suffix, func(t *testing.T) {
			f := newFakeFSKitMount(t)
			f.command = func(context.Context, string, ...string) error {
				identity := f.ownIdentity()
				identity.source = (&url.URL{Scheme: "file", Path: identity.source}).String() + suffix
				f.setMounts(identity)
				return nil
			}
			mounted, err := f.service.mountNativeFSKit(context.Background(), f.root, nil, f.ops)
			if mounted == nil || !errors.Is(err, errFSKitMountOwnership) || f.bridge.closeCount() != 0 {
				t.Fatalf("mounted=%v err=%v drains=%d", mounted, err, f.bridge.closeCount())
			}
			if err := mounted.Unmount(); !errors.Is(err, errFSKitMountOwnership) {
				t.Fatal(err)
			}
			if f.commandCount() != 1 || f.bridge.closeCount() != 0 {
				t.Fatalf("unidentified source was released: commands=%d drains=%d", f.commandCount(), f.bridge.closeCount())
			}
		})
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

func TestNativeFSKitCommandFailureDiagnosticsAreBoundedAndRedacted(t *testing.T) {
	for _, test := range []struct {
		name, script, wantDiagnostic string
		truncated                    bool
		secrets                      []string
	}{
		{
			name: "large stderr with credentials",
			script: "printf '%s\\n' 'mount: transport denied https://credential-user:credential-password@example.invalid/path?token=credential-token#credential-fragment' >&2; " +
				"printf '%16384s' '' >&2; exit 7",
			wantDiagnostic: "transport denied", truncated: true,
			secrets: []string{"credential-user", "credential-password", "credential-token", "credential-fragment"},
		},
		{
			name: "truncated credential URL",
			script: "printf '%s\\n' 'mount: truncated remote' >&2; printf 'https://partial-user:' >&2; " +
				"i=0; while [ \"$i\" -lt 1000 ]; do printf 'partial-secret' >&2; i=$((i + 1)); done; printf '@example.invalid\\n' >&2; exit 7",
			wantDiagnostic: "truncated remote", truncated: true,
			secrets: []string{"partial-user", "partial-secret"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			err := runFSKitMountCommandLogged(context.Background(), logger, "/bin/sh", "-c", test.script, "native-fskit-test", "argument-secret-marker")
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 7 {
				t.Fatalf("command error=%v", err)
			}
			var record struct {
				Command   string `json:"command"`
				ExitCode  int    `json:"exit_code"`
				Stderr    string `json:"stderr"`
				Truncated bool   `json:"stderr_truncated"`
			}
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record.Command != "sh" || record.ExitCode != 7 || record.Truncated != test.truncated ||
				len(record.Stderr) > fsKitStderrLimit || !strings.Contains(record.Stderr, test.wantDiagnostic) {
				t.Fatalf("incomplete or unbounded failure diagnostic: %+v", record)
			}
			for _, secret := range append(test.secrets, "argument-secret-marker") {
				if strings.Contains(output.String(), secret) {
					t.Fatalf("command diagnostic exposed credential or argument %q", secret)
				}
			}
		})
	}
}

func TestNativeFSKitCommandInheritedStderrIsBounded(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// This child only tests an inherited stderr pipe; it never owns a mount.
	// Clean up its known PID even if a failed assertion ends the test early.
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 || pid == os.Getpid() {
			t.Errorf("invalid inherited-pipe fixture PID: %v", err)
			return
		}
		child, err := os.FindProcess(pid)
		if err == nil {
			_ = child.Kill()
		}
	})
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	ctx, cancel := context.WithTimeout(context.Background(), fsKitCommandWaitDelay+2*time.Second)
	defer cancel()
	start := time.Now()
	err := runFSKitMountCommandLogged(ctx, logger, "/bin/sh", "-c",
		"sleep 30 & printf '%s\\n' \"$!\" > \"$1\"; printf 'mount: parent exited\\n' >&2; exit 9", "native-fskit-test", pidFile)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 9 || ctx.Err() != nil || time.Since(start) > fsKitCommandWaitDelay+time.Second {
		t.Fatalf("inherited stderr delayed failure: err=%v elapsed=%v context=%v", err, time.Since(start), ctx.Err())
	}
	if !strings.Contains(output.String(), "parent exited") {
		t.Fatal("inherited stderr suppressed the parent's failure diagnostic")
	}
}
