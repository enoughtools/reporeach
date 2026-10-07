//go:build darwin

package desktop

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Every operation that could access a mount or invoke the OS is replaced here.
// The receipt fixture records nonexistent paths and never mounts a filesystem.
type nativeRecoveryFixture struct {
	t       *testing.T
	receipt nativeMountSessionReceipt
	other   fsKitMountIdentity
	ops     nativeMountRecoveryOperations
	events  []string

	reads, inventories, directReads, commands, cleanups, retirements, closes int
	verificationReads                                                        int
	directTimes                                                              []time.Time
	verificationTimes                                                        []time.Time
	currentMounts                                                            []fsKitMountIdentity
	readHook                                                                 func(int) (*nativeMountSessionReceipt, error)
	mountHook                                                                func(int) ([]fsKitMountIdentity, error)
	fsidHook                                                                 func(int) ([][2]int32, error)
	commandHook                                                              func(context.Context) error
	leaseErr, cleanupErr, retireErr, closeErr                                error
}

type nativeRecoveryTestLease struct{ fixture *nativeRecoveryFixture }

func (lease nativeRecoveryTestLease) CleanupDetached() error {
	f := lease.fixture
	f.cleanups++
	f.events = append(f.events, "cleanup")
	return f.cleanupErr
}

func (lease nativeRecoveryTestLease) Close() error {
	f := lease.fixture
	f.closes++
	f.events = append(f.events, "close")
	return f.closeErr
}

func newNativeRecoveryFixture(t *testing.T) *nativeRecoveryFixture {
	t.Helper()
	_, receipt := nativeSessionFixture(t)
	receipt.fileIdentity = nativeMountSessionFileIdentity{device: 1, inode: 71,
		volumeUUID: "0102030405060708090a0b0c0d0e0f10", birthSec: 1000, birthNSec: 4}
	f := &nativeRecoveryFixture{t: t, receipt: receipt,
		other: fsKitMountIdentity{fsid: [2]int32{18, 2}, owner: receipt.Mount.Owner, typeName: "apfs", root: "/unrelated", source: "/dev/unrelated"}}
	f.ops = nativeMountRecoveryOperations{
		read: func() (*nativeMountSessionReceipt, error) {
			f.reads++
			if f.readHook != nil {
				return f.readHook(f.reads)
			}
			copy := f.receipt
			return &copy, nil
		},
		validate: func(*nativeMountSessionReceipt) error { return nil },
		boot:     func() (string, error) { return f.receipt.BootUUID, nil },
		mounts: func() ([]fsKitMountIdentity, error) {
			f.inventories++
			if f.mountHook != nil {
				mounts, err := f.mountHook(f.inventories)
				f.currentMounts = mounts
				return mounts, err
			}
			mounts := []fsKitMountIdentity{f.other}
			if f.commands == 0 {
				mounts = append(mounts, f.receipt.identity())
			}
			f.currentMounts = mounts
			return mounts, nil
		},
		fsids: func() ([][2]int32, error) {
			f.directReads++
			f.directTimes = append(f.directTimes, time.Now())
			if f.commands == 0 {
				fsids := make([][2]int32, len(f.currentMounts))
				for index, mounted := range f.currentMounts {
					fsids[index] = mounted.fsid
				}
				return fsids, nil
			}
			f.verificationReads++
			f.verificationTimes = append(f.verificationTimes, time.Now())
			if f.fsidHook != nil {
				return f.fsidHook(f.verificationReads)
			}
			return [][2]int32{f.other.fsid}, nil
		},
		lease: func(receipt *nativeMountSessionReceipt) (nativeRecoveryLease, error) {
			if !f.receipt.sameSession(*receipt) {
				t.Error("lease requested for a different session")
			}
			if f.leaseErr != nil {
				return nil, f.leaseErr
			}
			f.events = append(f.events, "lease")
			return nativeRecoveryTestLease{fixture: f}, nil
		},
		command: func(ctx context.Context, command string, args ...string) error {
			f.commands++
			f.events = append(f.events, "command")
			if command != "/sbin/umount" || !reflect.DeepEqual(args, []string{f.receipt.Mount.Root}) {
				t.Errorf("recovery invoked an unauthorized command: %q %#v", command, args)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("recovery command has no deadline")
			}
			if f.commandHook != nil {
				return f.commandHook(ctx)
			}
			return nil
		},
		retire: func(receipt *nativeMountSessionReceipt) error {
			f.retirements++
			f.events = append(f.events, "retire")
			if !f.receipt.sameSession(*receipt) {
				t.Error("retiring a different session")
			}
			return f.retireErr
		},
		poll: 3 * time.Millisecond, timeout: 40 * time.Millisecond,
	}
	return f
}

func (f *nativeRecoveryFixture) assertRetained(t *testing.T, commands, closes int) {
	t.Helper()
	if f.commands != commands || f.cleanups != 0 || f.retirements != 0 || f.closes != closes {
		t.Fatalf("unproven session was mutated: commands=%d cleanup=%d retire=%d closes=%d events=%v", f.commands, f.cleanups, f.retirements, f.closes, f.events)
	}
}

func TestNativeRecoveryRejectsChangedOrDuplicateKernelIdentity(t *testing.T) {
	for _, name := range []string{"fsid", "owner", "filesystem", "root", "source", "equivalent source spelling", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			f := newNativeRecoveryFixture(t)
			identity := f.receipt.identity()
			changed := identity
			switch name {
			case "fsid":
				changed.fsid[0]++
			case "owner":
				changed.owner++
			case "filesystem":
				changed.typeName = "apfs"
			case "root":
				changed.root += "-moved"
			case "source":
				changed.source += "other"
			case "equivalent source spelling":
				changed.source = f.receipt.Bridge.SourceDirectory
			}
			f.mountHook = func(int) ([]fsKitMountIdentity, error) {
				if name == "duplicate" {
					return []fsKitMountIdentity{identity, identity}, nil
				}
				return []fsKitMountIdentity{changed}, nil
			}
			if err := recoverNativeSession(context.Background(), f.ops); !errors.Is(err, errNativeRecoveryUnproven) {
				t.Fatalf("changed kernel tuple was authorized: %v", err)
			}
			f.assertRetained(t, 0, 0)
		})
	}
}

func TestNativeRecoveryRequiresBoundReceiptAndExclusiveLease(t *testing.T) {
	for _, name := range []string{"missing receipt", "unreadable receipt", "unbound receipt", "expired receipt with related mount", "unavailable boot", "active lease", "unavailable inventory"} {
		t.Run(name, func(t *testing.T) {
			f := newNativeRecoveryFixture(t)
			expected := errNativeRecoveryUnproven
			switch name {
			case "missing receipt":
				f.readHook = func(int) (*nativeMountSessionReceipt, error) { return nil, nil }
			case "unreadable receipt":
				f.readHook = func(int) (*nativeMountSessionReceipt, error) { return nil, unix.EIO }
			case "unbound receipt":
				f.ops.validate = func(*nativeMountSessionReceipt) error { return unix.EACCES }
			case "expired receipt with related mount":
				f.ops.boot = func() (string, error) { return "aabbccddeeff00112233445566778899", nil }
			case "unavailable boot":
				f.ops.boot = func() (string, error) { return "", unix.EPERM }
			case "active lease":
				f.leaseErr = unix.EWOULDBLOCK
				expected = errNativeRecoveryBusy
			case "unavailable inventory":
				f.mountHook = func(int) ([]fsKitMountIdentity, error) { return nil, unix.EIO }
				expected = errNativeRecoveryBusy
			}
			if err := recoverNativeSession(context.Background(), f.ops); !errors.Is(err, expected) {
				t.Fatalf("unproven recovery did not fail closed: %v", err)
			}
			f.assertRetained(t, 0, 0)
		})
	}
}

func expiredNativeRecoveryFixture(t *testing.T) *nativeRecoveryFixture {
	t.Helper()
	f := newNativeRecoveryFixture(t)
	f.ops.boot = func() (string, error) { return "aabbccddeeff00112233445566778899", nil }
	f.mountHook = func(int) ([]fsKitMountIdentity, error) { return []fsKitMountIdentity{f.other}, nil }
	return f
}

func (f *nativeRecoveryFixture) assertExpiredSessionRetained(t *testing.T) {
	t.Helper()
	f.assertRetained(t, 0, f.closes)
	leases := 0
	for _, event := range f.events {
		if event == "lease" {
			leases++
		}
	}
	if leases != f.closes || leases > 1 {
		t.Fatalf("failed expired recovery did not release its one lease: %v", f.events)
	}
}

func TestNativeRecoveryExpiredDetachedReceiptCleansOnlyPrivateSession(t *testing.T) {
	f := expiredNativeRecoveryFixture(t)
	if err := recoverNativeSession(context.Background(), f.ops); err != nil {
		t.Fatal(err)
	}
	if want := []string{"lease", "cleanup", "retire", "close"}; !reflect.DeepEqual(f.events, want) {
		t.Fatalf("expired recovery invoked a command or reordered retirement: got %v want %v", f.events, want)
	}
	if f.commands != 0 || f.cleanups != 1 || f.retirements != 1 || f.closes != 1 || f.directReads < 2 {
		t.Fatalf("expired detached recovery counts: commands=%d cleanup=%d retire=%d close=%d observations=%d", f.commands, f.cleanups, f.retirements, f.closes, f.directReads)
	}
	last := len(f.directTimes) - 1
	if f.directTimes[last].Sub(f.directTimes[last-1]) < f.ops.poll {
		t.Fatal("expired receipt cleanup preceded two separated absence observations")
	}
}

func TestNativeRecoveryExpiredReceiptCannotDetachRelatedCurrentResource(t *testing.T) {
	for _, name := range []string{"identical tuple", "reused fsid", "reused root", "reused source", "duplicate tuple", "resource appears after absence"} {
		t.Run(name, func(t *testing.T) {
			f := expiredNativeRecoveryFixture(t)
			identity := f.receipt.identity()
			current := identity
			switch name {
			case "reused fsid":
				current = f.other
				current.fsid = identity.fsid
			case "reused root", "resource appears after absence":
				current = f.other
				current.root = identity.root
			case "reused source":
				current = f.other
				current.source = identity.source
			}
			f.mountHook = func(int) ([]fsKitMountIdentity, error) {
				if name == "resource appears after absence" && f.directReads == 0 {
					return []fsKitMountIdentity{f.other}, nil
				}
				if name == "duplicate tuple" {
					return []fsKitMountIdentity{f.other, identity, identity}, nil
				}
				return []fsKitMountIdentity{current}, nil
			}
			if err := recoverNativeSession(context.Background(), f.ops); err == nil {
				t.Fatal("expired receipt authorized a related current resource")
			}
			f.assertExpiredSessionRetained(t)
		})
	}
}

func TestNativeRecoveryExpiredReceiptRequiresCompleteDirectAbsence(t *testing.T) {
	for _, name := range []string{"old fsid hidden", "unknown fsid hidden", "inventory error", "private binding changed", "lease held"} {
		t.Run(name, func(t *testing.T) {
			f := expiredNativeRecoveryFixture(t)
			switch name {
			case "old fsid hidden":
				f.ops.fsids = func() ([][2]int32, error) { return [][2]int32{f.other.fsid, f.receipt.identity().fsid}, nil }
			case "unknown fsid hidden":
				f.ops.fsids = func() ([][2]int32, error) { return [][2]int32{f.other.fsid, {901, 17}}, nil }
			case "inventory error":
				f.ops.fsids = func() ([][2]int32, error) { return nil, unix.EIO }
			case "private binding changed":
				f.ops.validate = func(*nativeMountSessionReceipt) error { return unix.ESTALE }
			case "lease held":
				f.leaseErr = unix.EWOULDBLOCK
			}
			if err := recoverNativeSession(context.Background(), f.ops); err == nil {
				t.Fatal("expired receipt cleanup accepted unproven absence or ownership")
			}
			f.assertExpiredSessionRetained(t)
		})
	}
}

func TestNativeRecoveryExpiredReceiptRequiresStableCurrentBootThroughCleanup(t *testing.T) {
	for _, stage := range []string{"between absence observations", "after final absence", "boot read fails before cleanup"} {
		t.Run(stage, func(t *testing.T) {
			f := expiredNativeRecoveryFixture(t)
			f.ops.boot = func() (string, error) {
				if (stage == "between absence observations" && f.directReads >= 1) ||
					(stage == "after final absence" && f.directReads >= 4) {
					return "bbccddee00112233445566778899aaff", nil
				}
				if stage == "boot read fails before cleanup" && f.directReads >= 4 {
					return "", unix.EIO
				}
				return "aabbccddeeff00112233445566778899", nil
			}
			if err := recoverNativeSession(context.Background(), f.ops); err == nil {
				t.Fatal("expired cleanup survived an unavailable or changed current boot")
			}
			f.assertExpiredSessionRetained(t)
		})
	}
}

func TestNativeRecoveryRechecksBindingsUnderLeaseBeforeCommand(t *testing.T) {
	for _, name := range []string{"receipt rotates after lease", "receipt rotates after poll", "receipt file replaced after lease", "receipt file replaced after poll", "mount changes after lease", "mount changes after poll"} {
		t.Run(name, func(t *testing.T) {
			f := newNativeRecoveryFixture(t)
			f.readHook = func(read int) (*nativeMountSessionReceipt, error) {
				copy := f.receipt
				if (name == "receipt rotates after lease" && read >= 2) || (name == "receipt rotates after poll" && read >= 3) {
					copy.Bridge.Socket.Inode++
				}
				if (name == "receipt file replaced after lease" && read >= 2) || (name == "receipt file replaced after poll" && read >= 3) {
					copy.fileIdentity.birthNSec++
					if !copy.sameSession(f.receipt) {
						t.Error("file identity fixture unexpectedly changed the receipt's JSON")
					}
				}
				return &copy, nil
			}
			f.mountHook = func(read int) ([]fsKitMountIdentity, error) {
				identity := f.receipt.identity()
				if (name == "mount changes after lease" && read >= 2) || (name == "mount changes after poll" && read >= 3) {
					identity.fsid[0]++
				}
				return []fsKitMountIdentity{identity}, nil
			}
			if err := recoverNativeSession(context.Background(), f.ops); !errors.Is(err, errNativeRecoveryUnproven) {
				t.Fatalf("rotated session was authorized: %v", err)
			}
			f.assertRetained(t, 0, 1)
		})
	}
}

func TestNativeRecoveryRejectsIdenticalReceiptFileReplacementAfterCommand(t *testing.T) {
	f := newNativeRecoveryFixture(t)
	f.readHook = func(int) (*nativeMountSessionReceipt, error) {
		copy := f.receipt
		if f.commands != 0 {
			copy.fileIdentity.birthNSec++
			if !copy.sameSession(f.receipt) {
				t.Error("file identity fixture unexpectedly changed the receipt's JSON")
			}
		}
		return &copy, nil
	}
	if err := recoverNativeSession(context.Background(), f.ops); !errors.Is(err, errNativeRecoveryUnproven) {
		t.Fatalf("replacement receipt inherited recovery authority: %v", err)
	}
	f.assertRetained(t, 1, 1)
}

func TestNativeRecoveryRequiresCompleteDirectInventoryBeforeCommand(t *testing.T) {
	for _, stage := range []int{1, 2} {
		for _, failure := range []string{"error", "captured fsid missing", "unknown fsid", "empty inventory"} {
			t.Run(failure+string(rune('0'+stage)), func(t *testing.T) {
				f := newNativeRecoveryFixture(t)
				read := 0
				f.ops.fsids = func() ([][2]int32, error) {
					read++
					fsids := [][2]int32{f.other.fsid, f.receipt.identity().fsid}
					if read == stage {
						switch failure {
						case "error":
							return nil, unix.EIO
						case "captured fsid missing":
							return [][2]int32{f.other.fsid}, nil
						case "unknown fsid":
							return append(fsids, [2]int32{901, 17}), nil
						case "empty inventory":
							return nil, nil
						}
					}
					return fsids, nil
				}
				if err := recoverNativeSession(context.Background(), f.ops); !errors.Is(err, errNativeRecoveryBusy) {
					t.Fatalf("uncertain pre-command direct inventory was accepted: %v", err)
				}
				f.assertRetained(t, 0, 1)
			})
		}
	}
}

func TestNativeRecoveryCommandFailuresRetainSession(t *testing.T) {
	for _, failure := range []error{unix.EBUSY, unix.EIO, unix.EPERM, errors.New("fsync failed")} {
		t.Run(failure.Error(), func(t *testing.T) {
			f := newNativeRecoveryFixture(t)
			f.commandHook = func(context.Context) error { return failure }
			if err := recoverNativeSession(context.Background(), f.ops); !errors.Is(err, errNativeRecoveryBusy) {
				t.Fatalf("command failure was accepted: %v", err)
			}
			f.assertRetained(t, 1, 1)
			if f.verificationReads != 0 {
				t.Fatal("failed command was reinterpreted as absence")
			}
		})
	}
}

func TestNativeRecoveryRequiresDirectAndCachedKernelAbsence(t *testing.T) {
	for _, name := range []string{"original fsid survives", "unknown fsid survives", "cached mount survives", "direct inventory fails", "cached inventory fails"} {
		t.Run(name, func(t *testing.T) {
			f := newNativeRecoveryFixture(t)
			if name == "cached mount survives" {
				f.mountHook = func(int) ([]fsKitMountIdentity, error) {
					return []fsKitMountIdentity{f.receipt.identity(), f.other}, nil
				}
			} else if name == "cached inventory fails" {
				f.mountHook = func(int) ([]fsKitMountIdentity, error) {
					if f.commands != 0 {
						return nil, unix.EIO
					}
					return []fsKitMountIdentity{f.receipt.identity(), f.other}, nil
				}
			} else {
				f.fsidHook = func(int) ([][2]int32, error) {
					switch name {
					case "original fsid survives":
						return [][2]int32{f.other.fsid, f.receipt.identity().fsid}, nil
					case "unknown fsid survives":
						return [][2]int32{f.other.fsid, {901, 17}}, nil
					default:
						return nil, unix.EIO
					}
				}
			}
			started := time.Now()
			if err := recoverNativeSession(context.Background(), f.ops); err == nil {
				t.Fatal("uncertain kernel absence was accepted")
			}
			f.assertRetained(t, 1, 1)
			if time.Since(started) > time.Second {
				t.Fatal("uncertain detach exceeded its verification bound")
			}
		})
	}
}

func TestNativeRecoveryNeedsTwoSeparatedAbsencesAfterPresence(t *testing.T) {
	f := newNativeRecoveryFixture(t)
	f.mountHook = func(int) ([]fsKitMountIdentity, error) {
		if f.commands == 0 || f.verificationReads == 1 {
			return []fsKitMountIdentity{f.other, f.receipt.identity()}, nil
		}
		return []fsKitMountIdentity{f.other}, nil
	}
	f.fsidHook = func(read int) ([][2]int32, error) {
		if read == 2 {
			return [][2]int32{f.other.fsid, f.receipt.identity().fsid}, nil
		}
		return [][2]int32{f.other.fsid}, nil
	}
	f.ops.retire = func(*nativeMountSessionReceipt) error {
		f.retirements++
		f.events = append(f.events, "retire")
		if f.verificationReads != 4 {
			t.Errorf("presence did not reset earlier absence: verification reads=%d", f.verificationReads)
		}
		return nil
	}
	if err := recoverNativeSession(context.Background(), f.ops); err != nil {
		t.Fatal(err)
	}
	if f.verificationReads != 4 || f.cleanups != 1 || f.retirements != 1 || f.commands != 1 || f.closes != 1 {
		t.Fatalf("unexpected recovery operations: %+v", f)
	}
	for index := 1; index < len(f.verificationTimes); index++ {
		if f.verificationTimes[index].Sub(f.verificationTimes[index-1]) < f.ops.poll {
			t.Fatal("absence observations were not separated by the poll interval")
		}
	}
}

func TestNativeRecoveryErrorAfterOneAbsenceCannotAuthorizeCleanup(t *testing.T) {
	for _, inventory := range []string{"direct", "cached", "receipt"} {
		t.Run(inventory, func(t *testing.T) {
			f := newNativeRecoveryFixture(t)
			switch inventory {
			case "direct":
				f.fsidHook = func(read int) ([][2]int32, error) {
					if read == 2 {
						return nil, unix.EIO
					}
					return [][2]int32{f.other.fsid}, nil
				}
			case "cached":
				f.mountHook = func(int) ([]fsKitMountIdentity, error) {
					if f.commands == 0 {
						return []fsKitMountIdentity{f.receipt.identity(), f.other}, nil
					}
					if f.verificationReads == 1 {
						return nil, unix.EIO
					}
					return []fsKitMountIdentity{f.other}, nil
				}
			case "receipt":
				f.readHook = func(int) (*nativeMountSessionReceipt, error) {
					if f.verificationReads == 1 {
						return nil, unix.EIO
					}
					copy := f.receipt
					return &copy, nil
				}
			}
			if err := recoverNativeSession(context.Background(), f.ops); err == nil {
				t.Fatal("an error completed a previous single absence")
			}
			f.assertRetained(t, 1, 1)
		})
	}
}

func TestNativeRecoveryOrdersNormalDetachAndPrivateRetirement(t *testing.T) {
	f := newNativeRecoveryFixture(t)
	originalRetire := f.ops.retire
	f.ops.retire = func(receipt *nativeMountSessionReceipt) error {
		if f.verificationReads != 2 || f.cleanups != 1 {
			t.Error("retirement preceded definitive detach and cleanup")
		}
		return originalRetire(receipt)
	}
	if err := recoverNativeSession(context.Background(), f.ops); err != nil {
		t.Fatal(err)
	}
	if want := []string{"lease", "command", "cleanup", "retire", "close"}; !reflect.DeepEqual(f.events, want) {
		t.Fatalf("recovery order: got %v want %v", f.events, want)
	}
	if f.commands != 1 || f.cleanups != 1 || f.retirements != 1 || f.closes != 1 || f.verificationReads != 2 {
		t.Fatalf("unexpected recovery counts: %+v", f)
	}
	if f.verificationTimes[1].Sub(f.verificationTimes[0]) < f.ops.poll {
		t.Fatal("cleanup followed consecutive unseparated observations")
	}
}

func TestNativeRecoveryRetiresAlreadyAbsentSessionWithoutOSCommand(t *testing.T) {
	f := newNativeRecoveryFixture(t)
	f.mountHook = func(int) ([]fsKitMountIdentity, error) { return []fsKitMountIdentity{f.other}, nil }
	if err := recoverNativeSession(context.Background(), f.ops); err != nil {
		t.Fatal(err)
	}
	if want := []string{"lease", "cleanup", "retire", "close"}; !reflect.DeepEqual(f.events, want) || f.directReads != 4 {
		t.Fatalf("already-absent recovery order/counts: %v direct reads=%d", f.events, f.directReads)
	}
}

func TestNativeRecoveryCleanupFailuresRetainRecoveryRecord(t *testing.T) {
	for _, stage := range []string{"cleanup", "retire"} {
		t.Run(stage, func(t *testing.T) {
			f := newNativeRecoveryFixture(t)
			if stage == "cleanup" {
				f.cleanupErr = unix.EIO
			} else {
				f.retireErr = unix.EIO
			}
			if err := recoverNativeSession(context.Background(), f.ops); !errors.Is(err, errNativeRecoveryBusy) {
				t.Fatalf("failed private retirement was accepted: %v", err)
			}
			if f.commands != 1 || f.cleanups != 1 || f.closes != 1 || (stage == "cleanup" && f.retirements != 0) || (stage == "retire" && f.retirements != 1) {
				t.Fatalf("failed cleanup/retirement order: %v", f.events)
			}
		})
	}
}

func TestNativeRecoveryReportsLeaseCloseFailure(t *testing.T) {
	f := newNativeRecoveryFixture(t)
	f.closeErr = unix.EIO
	if err := recoverNativeSession(context.Background(), f.ops); !errors.Is(err, errNativeRecoveryBusy) {
		t.Fatalf("failed recovery lease release was reported as success: %v", err)
	}
	if f.closes != 1 || f.cleanups != 1 || f.retirements != 1 {
		t.Fatalf("lease release failed at unexpected stage: %v", f.events)
	}
}

func TestNativeRecoveryCancellationPreservesSession(t *testing.T) {
	// The injected command cooperates with its context. This checks recovery's
	// cancellation behavior, not a hard time bound for an actual kernel unmount.
	for _, stage := range []string{"before inspection", "after lease", "final pre-command inventory", "during command", "command timeout", "during verification"} {
		t.Run(stage, func(t *testing.T) {
			f := newNativeRecoveryFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			commands, closes := 0, 0
			switch stage {
			case "before inspection":
				cancel()
			case "after lease":
				originalLease := f.ops.lease
				f.ops.lease = func(receipt *nativeMountSessionReceipt) (nativeRecoveryLease, error) {
					lease, err := originalLease(receipt)
					cancel()
					return lease, err
				}
				closes = 1
			case "final pre-command inventory":
				originalFSIDs := f.ops.fsids
				f.ops.fsids = func() ([][2]int32, error) {
					fsids, err := originalFSIDs()
					if f.directReads == 2 {
						cancel()
					}
					return fsids, err
				}
				closes = 1
			case "during command":
				f.commandHook = func(commandCtx context.Context) error {
					cancel()
					<-commandCtx.Done()
					return commandCtx.Err()
				}
				commands, closes = 1, 1
			case "command timeout":
				f.commandHook = func(commandCtx context.Context) error {
					<-commandCtx.Done()
					return commandCtx.Err()
				}
				commands, closes = 1, 1
			case "during verification":
				f.fsidHook = func(int) ([][2]int32, error) {
					cancel()
					return [][2]int32{f.other.fsid}, nil
				}
				commands, closes = 1, 1
			}
			started := time.Now()
			if err := recoverNativeSession(ctx, f.ops); err == nil {
				t.Fatal("cancelled recovery succeeded")
			}
			if time.Since(started) > time.Second {
				t.Fatal("cancelled recovery did not stop within its bound")
			}
			f.assertRetained(t, commands, closes)
		})
	}
}
