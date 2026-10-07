//go:build darwin

package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fsbridge"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

var errNativeRecoveryUnproven = errors.New("this virtual folder does not have a matching recovery record; its existing session was retained")
var errNativeRecoveryBusy = errors.New("virtual folders are still in use or their disconnection could not be confirmed; close files and Finder windows using them, then try recovery again")

type nativeRecoveryLease interface {
	CleanupDetached() error
	Close() error
}

type nativeMountRecoveryOperations struct {
	mounts        func() ([]fsKitMountIdentity, error)
	fsids         func() ([][2]int32, error)
	boot          func() (string, error)
	read          func() (*nativeMountSessionReceipt, error)
	validate      func(*nativeMountSessionReceipt) error
	lease         func(*nativeMountSessionReceipt) (nativeRecoveryLease, error)
	retire        func(*nativeMountSessionReceipt) error
	command       func(context.Context, string, ...string) error
	poll, timeout time.Duration
}

func (s *Service) nativeRecoveryOperations() nativeMountRecoveryOperations {
	root := s.cachedVirtualRoot
	return nativeMountRecoveryOperations{
		mounts: cachedDarwinMounts, fsids: nativeMountFSIDs, boot: nativeBootSessionUUID,
		read:     func() (*nativeMountSessionReceipt, error) { return readNativeMountSession(s.opts.StateDir, root) },
		validate: func(receipt *nativeMountSessionReceipt) error { return s.validateNativeRecoveryReceipt(receipt) },
		lease: func(receipt *nativeMountSessionReceipt) (nativeRecoveryLease, error) {
			return fsbridge.AcquireRecoveryLease(receipt.Bridge.SourceDirectory, receipt.Bridge.SocketDirectory, receipt.Bridge)
		},
		retire: func(receipt *nativeMountSessionReceipt) error {
			return removeNativeMountSession(s.opts.StateDir, root, *receipt)
		},
		command: func(ctx context.Context, command string, args ...string) error {
			return runFSKitMountCommandLogged(ctx, s.logger, command, args...)
		},
		poll: 100 * time.Millisecond, timeout: fsKitMountTimeout,
	}
}

func (s *Service) validateNativeRecoveryReceipt(receipt *nativeMountSessionReceipt) error {
	if receipt == nil || s.cachedVirtualRoot == "" || receipt.Mount.Root != s.cachedVirtualRoot || receipt.Mount.Owner != uint32(os.Geteuid()) {
		return errNativeRecoveryUnproven
	}
	state, err := filepath.EvalSymlinks(s.opts.StateDir)
	if err != nil || receipt.Bridge.SourceDirectory != filepath.Join(state, "FSKit") || receipt.Bridge.SocketDirectory != s.opts.FSKitSocketDir {
		return errNativeRecoveryUnproven
	}
	for _, directory := range []string{s.opts.StateDir, receipt.Bridge.SourceDirectory, receipt.Bridge.SocketDirectory} {
		if nativePrivateReceiptDirectory(directory, false) != nil {
			return errNativeRecoveryUnproven
		}
	}
	rootReceipt, err := readNativeRootReceipt(nativeRootReceiptPath(s.opts.StateDir, s.cachedVirtualRoot))
	if err != nil || rootReceipt == nil || !rootReceipt.VerifiedNativeMount || rootReceipt.Root != s.cachedVirtualRoot || rootReceipt.Identity != receipt.RootIdentity {
		return errNativeRecoveryUnproven
	}
	return nil
}

func inspectNativeRecovery(ops nativeMountRecoveryOperations) (*nativeMountSessionReceipt, []fsKitMountIdentity, error) {
	receipt, err := ops.read()
	if err != nil || receipt == nil || ops.validate(receipt) != nil {
		return nil, nil, errNativeRecoveryUnproven
	}
	boot, err := ops.boot()
	if err != nil || boot == "" {
		return nil, nil, errNativeRecoveryUnproven
	}
	mounts, err := ops.mounts()
	if err != nil {
		return nil, nil, errNativeRecoveryBusy
	}
	identity := receipt.identity()
	related := 0
	for _, mounted := range mounts {
		if mounted.fsid == identity.fsid || mounted.root == identity.root || mountSourceMatches(mounted.source, receipt.Bridge.SourceDirectory) {
			// An expired receipt cannot authorize detaching any current mount,
			// including an unrelated volume that reused the old kernel FSID.
			if boot != receipt.BootUUID || mounted != identity {
				return nil, nil, errNativeRecoveryUnproven
			}
			related++
		}
	}
	if related > 1 {
		return nil, nil, errNativeRecoveryUnproven
	}
	return receipt, mounts, nil
}

func nativeRecoveryState(ops nativeMountRecoveryOperations, receipt *nativeMountSessionReceipt, mounts []fsKitMountIdentity) (present, absent bool, err error) {
	fsids, err := ops.fsids()
	if err != nil || !fsKitInventoryComplete(mounts, fsids) {
		return false, false, errNativeRecoveryBusy
	}
	identity := receipt.identity()
	for _, mounted := range mounts {
		if mounted == identity {
			present = true
		}
	}
	listed := false
	for _, fsid := range fsids {
		if fsid == identity.fsid {
			listed = true
		}
	}
	if present && !listed {
		return false, false, errNativeRecoveryBusy
	}
	return present, fsKitSessionAbsent(mounts, fsids, identity, identity.root, receipt.Bridge.SourceDirectory), nil
}

func recoverNativeSession(ctx context.Context, ops nativeMountRecoveryOperations) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	boot, err := ops.boot()
	if err != nil || boot == "" {
		return errNativeRecoveryUnproven
	}
	readBoot := ops.boot
	ops.boot = func() (string, error) {
		current, err := readBoot()
		if err != nil || current != boot {
			return "", errNativeRecoveryUnproven
		}
		return current, nil
	}
	receipt, _, err := inspectNativeRecovery(ops)
	if err != nil {
		return err
	}
	lease, err := ops.lease(receipt)
	if err != nil {
		return errNativeRecoveryBusy
	}
	defer func() {
		if err := lease.Close(); retErr == nil && err != nil {
			retErr = errNativeRecoveryBusy
		}
	}()
	// Re-read every private and kernel binding under the original source lease.
	current, mounts, err := inspectNativeRecovery(ops)
	if err != nil {
		return err
	}
	if !receipt.sameSession(*current) || receipt.fileIdentity != current.fileIdentity {
		return errNativeRecoveryUnproven
	}
	identity := receipt.identity()
	firstPresent, firstAbsent, err := nativeRecoveryState(ops, receipt, mounts)
	if err != nil {
		return err
	}
	if boot != receipt.BootUUID && !firstAbsent {
		return errNativeRecoveryBusy
	}
	if !waitMountPoll(ctx, ops.poll) {
		return ctx.Err()
	}
	current, mounts, err = inspectNativeRecovery(ops)
	if err != nil {
		return err
	}
	if !receipt.sameSession(*current) || receipt.fileIdentity != current.fileIdentity {
		return errNativeRecoveryUnproven
	}
	present, absent, err := nativeRecoveryState(ops, receipt, mounts)
	if err != nil {
		return err
	}
	if present != firstPresent {
		return errNativeRecoveryBusy
	}
	if boot != receipt.BootUUID && !absent {
		return errNativeRecoveryBusy
	}
	if present {
		if err := ctx.Err(); err != nil {
			return err
		}
		commandCtx, cancel := context.WithTimeout(ctx, ops.timeout)
		err := ops.command(commandCtx, "/sbin/umount", identity.root)
		cancel()
		if err != nil {
			return errNativeRecoveryBusy
		}
	}
	verifyCtx, cancel := context.WithTimeout(ctx, ops.timeout)
	defer cancel()
	absences := 0
	for {
		current, mounts, err = inspectNativeRecovery(ops)
		if err != nil {
			return err
		}
		if !receipt.sameSession(*current) || receipt.fileIdentity != current.fileIdentity {
			return errNativeRecoveryUnproven
		}
		_, absent, err := nativeRecoveryState(ops, receipt, mounts)
		if err != nil {
			return err
		}
		if absent {
			absences++
		} else {
			absences = 0
		}
		if absences == 2 {
			break
		}
		if !waitMountPoll(verifyCtx, ops.poll) {
			return errNativeRecoveryBusy
		}
	}
	if err := verifyCtx.Err(); err != nil {
		return err
	}
	if _, err := ops.boot(); err != nil {
		return errNativeRecoveryUnproven
	}
	if err := lease.CleanupDetached(); err != nil {
		return errNativeRecoveryBusy
	}
	if _, err := ops.boot(); err != nil {
		return errNativeRecoveryUnproven
	}
	if err := ops.retire(receipt); err != nil {
		return errNativeRecoveryBusy
	}
	return nil
}

func (s *Service) platformRecoverMountCatalogue(ctx context.Context) error {
	return recoverNativeSession(ctx, s.nativeRecoveryOperations())
}

func (s *Service) platformOwnedMountNeedsRecovery(mounted fusefs.MountedFS) bool {
	native, ok := mounted.(*nativeFSKitMount)
	if !ok {
		return false
	}
	select {
	case <-native.bridge.Done():
		return true
	default:
		return false
	}
}

func (s *Service) platformPreflightMountRecovery() error {
	if !s.hybridCatalogue || s.cachedVirtualRoot == "" {
		return nil
	}
	ops := s.nativeRecoveryOperations()
	mounts, err := ops.mounts()
	if err != nil {
		return errNativeRecoveryBusy
	}
	fsids, err := ops.fsids()
	if err != nil || !fsKitInventoryComplete(mounts, fsids) {
		return errNativeRecoveryBusy
	}
	source := filepath.Join(filepath.Dir(filepath.Dir(s.cachedVirtualRoot)), "FSKit")
	stale := false
	for _, mounted := range mounts {
		if mounted.root == s.cachedVirtualRoot || mountSourceMatches(mounted.source, source) {
			stale = true
		}
	}
	receipt, readErr := ops.read()
	if readErr != nil {
		return errNativeRecoveryUnproven
	}
	if stale || receipt != nil {
		if s.platformMountRecoveryAvailable() {
			return errors.New("virtual folders need recovery; choose Recover Virtual Folders to reconnect them safely")
		}
		return errNativeRecoveryUnproven
	}
	return nil
}

func (s *Service) platformMountRecoveryAvailable() bool {
	if !s.hybridCatalogue || s.cachedVirtualRoot == "" {
		return false
	}
	ops := s.nativeRecoveryOperations()
	receipt, mounts, err := inspectNativeRecovery(ops)
	if err != nil {
		return false
	}
	boot, err := ops.boot()
	if err != nil {
		return false
	}
	if boot != receipt.BootUUID {
		_, absent, err := nativeRecoveryState(ops, receipt, mounts)
		if err != nil || !absent {
			return false
		}
	}
	lease, err := ops.lease(receipt)
	if err != nil {
		return false
	}
	return lease.Close() == nil
}

func (s *Service) recordNativeMountSession(identity fsKitMountIdentity, bridge platformBridge, prepared *nativeMountRootReceipt) (func() error, error) {
	if prepared == nil || identity.root != s.cachedVirtualRoot {
		return nil, errNativeRecoveryUnproven
	}
	server, ok := bridge.(*fsbridge.Server)
	if !ok {
		return nil, errNativeRecoveryUnproven
	}
	boot, err := nativeBootSessionUUID()
	if err != nil {
		return nil, err
	}
	files, err := server.SessionIdentity()
	if err != nil {
		return nil, err
	}
	receipt := newNativeMountSessionReceipt(boot, identity, prepared.Identity, files)
	if err := s.validateNativeRecoveryReceipt(&receipt); err != nil {
		return nil, err
	}
	if err := writeNativeMountSession(s.opts.StateDir, receipt); err != nil {
		return nil, err
	}
	saved, err := readNativeMountSession(s.opts.StateDir, identity.root)
	if err != nil || saved == nil || !saved.sameSession(receipt) {
		return nil, errNativeRecoveryUnproven
	}
	return func() error { return removeNativeMountSession(s.opts.StateDir, identity.root, *saved) }, nil
}
