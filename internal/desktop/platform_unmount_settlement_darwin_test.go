//go:build darwin

package desktop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newSettlementFSKitMount(t *testing.T) (*fakeFSKitMount, *nativeFSKitMount) {
	t.Helper()
	f := newFakeFSKitMount(t)
	f.ops.timeout = 250 * time.Millisecond
	f.ops.poll = 2 * time.Millisecond
	f.ops.unmountRetryWindow = 50 * time.Millisecond
	return f, f.mount(t)
}

func assertSettlementNormalUnmount(t *testing.T, root, program string, args []string) {
	t.Helper()
	if program != "/sbin/umount" || len(args) != 1 || args[0] != root {
		t.Errorf("settlement command = %q %q; want normal unmount of the captured root", program, args)
	}
}

func TestNativeFSKitUnmountSettlementRetriesTransientFailures(t *testing.T) {
	f, mounted := newSettlementFSKitMount(t)
	var commands atomic.Int32
	f.command = func(ctx context.Context, program string, args ...string) error {
		assertSettlementNormalUnmount(t, f.root, program, args)
		if err := ctx.Err(); err != nil {
			t.Errorf("unmount invoked with an expired context: %v", err)
		}
		if commands.Add(1) <= 2 {
			return errors.New("fixture: resource temporarily busy")
		}
		f.setMounts()
		return nil
	}
	if err := mounted.Unmount(); err != nil {
		t.Fatalf("transient failures did not settle: %v", err)
	}
	if commands.Load() != 3 || f.bridge.closeCount() != 1 {
		t.Fatalf("commands=%d drains=%d; want three normal attempts and one drain", commands.Load(), f.bridge.closeCount())
	}
}

func TestNativeFSKitUnmountSettlementPersistentBusyKeepsSessionRetryable(t *testing.T) {
	f, mounted := newSettlementFSKitMount(t)
	mounted.ops.unmountRetryWindow = 50 * time.Millisecond
	var commands atomic.Int32
	f.command = func(_ context.Context, program string, args ...string) error {
		assertSettlementNormalUnmount(t, f.root, program, args)
		commands.Add(1)
		return errors.New("fixture: held file remains busy")
	}
	started := time.Now()
	if err := mounted.Unmount(); err == nil {
		t.Fatal("persistent busy reported successful detachment")
	}
	if time.Since(started) > time.Second {
		t.Fatal("persistent busy did not obey the short retry window")
	}
	if commands.Load() < 2 || f.bridge.closeCount() != 0 {
		t.Fatalf("commands=%d drains=%d; want retries without releasing a busy bridge", commands.Load(), f.bridge.closeCount())
	}
	identity, verified := mounted.verifiedIdentity()
	mounts, err := mounted.ops.mounts()
	if err != nil || !verified || len(mounts) != 1 || mounts[0] != identity {
		t.Fatal("busy settlement changed the captured native session")
	}
	before := commands.Load()
	f.command = func(_ context.Context, program string, args ...string) error {
		assertSettlementNormalUnmount(t, f.root, program, args)
		commands.Add(1)
		f.setMounts()
		return nil
	}
	if err := mounted.Unmount(); err != nil {
		t.Fatalf("later explicit retry did not detach: %v", err)
	}
	if commands.Load() != before+1 || f.bridge.closeCount() != 1 {
		t.Fatalf("commands=%d drains=%d; explicit retry should detach and drain once", commands.Load(), f.bridge.closeCount())
	}
}

func TestNativeFSKitUnmountSettlementRechecksIdentityAfterWaiting(t *testing.T) {
	for _, name := range []string{"old gone replacement at root", "old moved replacement at root", "same FSID changed source", "same FSID changed owner", "same FSID changed type", "inventory failure"} {
		t.Run(name, func(t *testing.T) {
			f, mounted := newSettlementFSKitMount(t)
			var commands atomic.Int32
			f.command = func(_ context.Context, program string, args ...string) error {
				assertSettlementNormalUnmount(t, f.root, program, args)
				commands.Add(1)
				return errors.New("fixture: initial busy response")
			}
			inventory := mounted.ops.mounts
			var afterFailure atomic.Int32
			mounted.ops.mounts = func() ([]fsKitMountIdentity, error) {
				mounts, err := inventory()
				if err != nil || commands.Load() == 0 || afterFailure.Add(1) != 1 {
					return mounts, err
				}
				// Return the original post-command snapshot once. Change the
				// inventory after taking it, so reusing it for a later command
				// would target a replacement or an unidentified filesystem.
				original := f.ownIdentity()
				replacement := original
				replacement.fsid = [2]int32{300, 400}
				replacement.source = "/other/source"
				switch name {
				case "old gone replacement at root":
					f.setMounts(replacement)
				case "old moved replacement at root":
					original.root = "/somewhere/else"
					f.setMounts(original, replacement)
				case "same FSID changed source":
					original.source = "/other/source"
					f.setMounts(original)
				case "same FSID changed owner":
					original.owner++
					f.setMounts(original)
				case "same FSID changed type":
					original.typeName = "another-filesystem"
					f.setMounts(original)
				case "inventory failure":
					f.mu.Lock()
					f.inspectErr = errors.New("fixture: mount inventory unavailable")
					f.mu.Unlock()
				}
				return mounts, nil
			}
			err := mounted.Unmount()
			wantDrains := 0
			if name == "old gone replacement at root" {
				wantDrains = 1
				if err != nil {
					t.Fatalf("confirmed old-session detach did not drain: %v", err)
				}
			} else if !errors.Is(err, errFSKitMountOwnership) {
				t.Fatalf("changed or unavailable inventory error = %v; want ownership refusal", err)
			}
			if commands.Load() != 1 || f.bridge.closeCount() != wantDrains {
				t.Fatalf("commands=%d drains=%d; a changed inventory must prevent another unmount", commands.Load(), f.bridge.closeCount())
			}
		})
	}
}

func TestNativeFSKitUnmountSettlementDeadlineBeforeRetryPreservesBridge(t *testing.T) {
	for _, tc := range []struct {
		name           string
		timeout, retry time.Duration
	}{
		{name: "retry window expires", timeout: 250 * time.Millisecond, retry: 15 * time.Millisecond},
		{name: "outer deadline cancels retry", timeout: 15 * time.Millisecond, retry: 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, mounted := newSettlementFSKitMount(t)
			mounted.ops.timeout = tc.timeout
			mounted.ops.unmountRetryWindow = tc.retry
			mounted.ops.poll = 50 * time.Millisecond
			var commands atomic.Int32
			f.command = func(_ context.Context, program string, args ...string) error {
				assertSettlementNormalUnmount(t, f.root, program, args)
				commands.Add(1)
				return errors.New("fixture: busy before deadline")
			}
			if err := mounted.Unmount(); err == nil {
				t.Fatal("expired retry reported successful detachment")
			}
			if commands.Load() != 1 || f.bridge.closeCount() != 0 {
				t.Fatalf("commands=%d drains=%d; deadline should prevent any retry or drain", commands.Load(), f.bridge.closeCount())
			}
		})
	}
}

func TestNativeFSKitUnmountSettlementCommandUsesRemainingRetryDeadline(t *testing.T) {
	f, mounted := newSettlementFSKitMount(t)
	mounted.ops.poll = 5 * time.Millisecond
	var commands atomic.Int32
	var initialDeadline, retryDeadline time.Time
	var retryResult error
	f.command = func(ctx context.Context, program string, args ...string) error {
		assertSettlementNormalUnmount(t, f.root, program, args)
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("unmount command has no deadline")
			return errors.New("fixture: unbounded command")
		}
		if commands.Add(1) == 1 {
			initialDeadline = deadline
			return errors.New("fixture: initial busy response")
		}
		retryDeadline = deadline
		if remaining := time.Until(deadline); remaining <= 0 || remaining >= mounted.ops.unmountRetryWindow {
			t.Errorf("retry deadline has %v remaining; want the window reduced by the preceding wait", remaining)
		}
		<-ctx.Done()
		retryResult = ctx.Err()
		return retryResult
	}
	if err := mounted.Unmount(); err == nil {
		t.Fatal("expired retry command reported successful detachment")
	}
	if commands.Load() != 2 || f.bridge.closeCount() != 0 {
		t.Fatalf("commands=%d drains=%d; timed-out retry should preserve the mounted bridge", commands.Load(), f.bridge.closeCount())
	}
	if !retryDeadline.Before(initialDeadline) || !errors.Is(retryResult, context.DeadlineExceeded) {
		t.Fatalf("retry did not expire within its independent window: initial=%v retry=%v result=%v", initialDeadline, retryDeadline, retryResult)
	}
}

func TestNativeFSKitUnmountSettlementCancellationWhileSettlingPreservesBridge(t *testing.T) {
	f, mounted := newSettlementFSKitMount(t)
	mounted.ops.poll = 50 * time.Millisecond
	mounted.ops.unmountRetryWindow = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var commands atomic.Int32
	f.command = func(_ context.Context, program string, args ...string) error {
		assertSettlementNormalUnmount(t, f.root, program, args)
		commands.Add(1)
		return errors.New("fixture: busy before cancellation")
	}
	inventory := mounted.ops.mounts
	postFailureObserved := make(chan struct{})
	var observed sync.Once
	mounted.ops.mounts = func() ([]fsKitMountIdentity, error) {
		mounts, err := inventory()
		if commands.Load() != 0 {
			observed.Do(func() { close(postFailureObserved) })
		}
		return mounts, err
	}
	done := make(chan error, 1)
	go func() { done <- mounted.unmount(ctx) }()
	select {
	case <-postFailureObserved:
	case <-time.After(time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("initial failed unmount was not observed")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled unmount error = %v; want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled retry wait did not stop")
	}
	if commands.Load() != 1 || f.bridge.closeCount() != 0 {
		t.Fatalf("commands=%d drains=%d; cancellation must preserve the original session", commands.Load(), f.bridge.closeCount())
	}
}

func TestNativeFSKitUnmountSettlementNeverRetriesReportedCancellation(t *testing.T) {
	for _, reported := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(reported.Error(), func(t *testing.T) {
			f, mounted := newSettlementFSKitMount(t)
			var commands atomic.Int32
			f.command = func(_ context.Context, program string, args ...string) error {
				assertSettlementNormalUnmount(t, f.root, program, args)
				commands.Add(1)
				return reported
			}
			if err := mounted.Unmount(); !errors.Is(err, reported) {
				t.Fatalf("reported cancellation error = %v; want %v", err, reported)
			}
			if commands.Load() != 1 || f.bridge.closeCount() != 0 {
				t.Fatalf("commands=%d drains=%d; reported cancellation must not retry or drain", commands.Load(), f.bridge.closeCount())
			}
		})
	}
}

func TestNativeFSKitUnmountSettlementDetachedAfterRetryExpiryUsesOuterDrainContext(t *testing.T) {
	f, mounted := newSettlementFSKitMount(t)
	var commands atomic.Int32
	var retryDeadline time.Time
	f.command = func(ctx context.Context, program string, args ...string) error {
		assertSettlementNormalUnmount(t, f.root, program, args)
		if commands.Add(1) == 1 {
			return errors.New("fixture: initial busy response")
		}
		var ok bool
		retryDeadline, ok = ctx.Deadline()
		if !ok {
			t.Error("retry command has no deadline")
			return errors.New("fixture: unbounded retry")
		}
		<-ctx.Done()
		f.setMounts()
		return ctx.Err()
	}
	f.bridge.close = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			t.Errorf("detached session received expired drain context: %v", err)
		}
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.After(retryDeadline) {
			t.Error("detached session did not retain the later outer ownership deadline")
		}
		return nil
	}
	if err := mounted.Unmount(); err != nil {
		t.Fatalf("detached session did not drain after retry expiry: %v", err)
	}
	if commands.Load() != 2 || f.bridge.closeCount() != 1 {
		t.Fatalf("commands=%d drains=%d; detached session should drain once with its outer context", commands.Load(), f.bridge.closeCount())
	}
}

func TestNativeFSKitUnmountSettlementDoesNotReissueSuccessfulCommand(t *testing.T) {
	for _, eventualDetach := range []bool{true, false} {
		name := "successful command still mounted"
		if eventualDetach {
			name = "successful command settles during observation"
		}
		t.Run(name, func(t *testing.T) {
			f, mounted := newSettlementFSKitMount(t)
			mounted.ops.timeout = 20 * time.Millisecond
			var commands atomic.Int32
			f.command = func(_ context.Context, program string, args ...string) error {
				assertSettlementNormalUnmount(t, f.root, program, args)
				commands.Add(1)
				return nil
			}
			if eventualDetach {
				inventory := mounted.ops.mounts
				var observations atomic.Int32
				mounted.ops.mounts = func() ([]fsKitMountIdentity, error) {
					if commands.Load() != 0 && observations.Add(1) == 2 {
						f.setMounts()
					}
					return inventory()
				}
			}
			err := mounted.Unmount()
			wantDrains := 0
			if eventualDetach {
				wantDrains = 1
				if err != nil {
					t.Fatalf("observation did not notice completed detach: %v", err)
				}
			} else if err == nil {
				t.Fatal("command exit alone was treated as proof of detachment")
			}
			if commands.Load() != 1 || f.bridge.closeCount() != wantDrains {
				t.Fatalf("commands=%d drains=%d; successful command must only be observed", commands.Load(), f.bridge.closeCount())
			}
		})
	}
}
