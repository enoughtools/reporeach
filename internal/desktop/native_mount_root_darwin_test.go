//go:build darwin

package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"golang.org/x/sys/unix"
)

func nativeReceiptFixture(t *testing.T) (*Service, string, *nativeMountRootReceipt) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Service{opts: Options{StateDir: state}}
	prepared, err := s.prepareNativeMountRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if prepared == nil {
		fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			t.Fatal(err)
		}
		uuid, queryErr := nativeBackingVolumeUUID(fd)
		_ = unix.Close(fd)
		if queryErr != nil {
			t.Skipf("disposable test volume cannot provide a persistent UUID: %v", queryErr)
		}
		t.Fatalf("working volume UUID %q did not produce an eligible empty-root receipt", uuid)
	}
	return s, root, prepared
}

func TestNativeRootReceiptNeedsSuccessfulNativeVerification(t *testing.T) {
	s, root, prepared := nativeReceiptFixture(t)
	path := nativeRootReceiptPath(s.opts.StateDir, root)
	loaded, err := readNativeRootReceipt(path)
	if err != nil || loaded == nil || loaded.Root != root || !filepath.IsAbs(loaded.Root) || !loaded.PreparedEmpty || loaded.VerifiedNativeMount || loaded.SystemDirectory != nil {
		t.Fatalf("initial receipt grants unexpected eligibility: %+v %v", loaded, err)
	}
	if err := verifyNativeMountRoot(s.opts.StateDir, prepared); err != nil {
		t.Fatal(err)
	}
	loaded, err = readNativeRootReceipt(path)
	if err != nil || !loaded.VerifiedNativeMount || loaded.Identity != prepared.Identity {
		t.Fatalf("successful mount did not retain exact root proof: %+v %v", loaded, err)
	}
	rootStat := unix.Stat_t{Dev: 12, Ino: loaded.Identity.Inode, Uid: loaded.Identity.UID,
		Btim: unix.Timespec{Sec: loaded.Identity.BirthSec, Nsec: loaded.Identity.BirthNSec}}
	systemStat := unix.Stat_t{Dev: 12, Ino: 1001, Uid: 0, Mode: unix.S_IFDIR | 0o700,
		Btim: unix.Timespec{Sec: 1000, Nsec: 3}}
	if err := validateNativeSystemDirectory(loaded, &rootStat, &systemStat, false, nil); err != nil {
		t.Fatal(err)
	}
	bound := nativeRootIdentity(&systemStat, loaded.Identity.VolumeUUID)
	loaded.SystemDirectory = &bound
	if err := writeNativeRootReceipt(path, *loaded); err != nil {
		t.Fatal(err)
	}
	reopened, err := readNativeRootReceipt(path)
	if err != nil || !reflect.DeepEqual(reopened.SystemDirectory, &bound) {
		t.Fatalf("restart lost system directory binding: %+v %v", reopened, err)
	}
	if err := validateNativeSystemDirectory(reopened, &rootStat, &systemStat, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSystemDirectoryRejectsUnprovedOrChangedObjects(t *testing.T) {
	root := unix.Stat_t{Dev: 19, Ino: 500, Uid: 501, Btim: unix.Timespec{Sec: 2000, Nsec: 7}}
	system := unix.Stat_t{Dev: 19, Ino: 600, Uid: 0, Mode: unix.S_IFDIR | 0o700, Btim: unix.Timespec{Sec: 2001, Nsec: 9}}
	base := nativeMountRootReceipt{Version: 1, Root: "/fixture", PreparedEmpty: true, VerifiedNativeMount: true,
		Identity: nativeRootIdentity(&root, "0102030405060708090a0b0c0d0e0f10")}
	bound := nativeRootIdentity(&system, base.Identity.VolumeUUID)
	base.SystemDirectory = &bound
	cases := []struct {
		name   string
		mutate func(*nativeMountRootReceipt, *unix.Stat_t, *unix.Stat_t)
		acl    bool
		aclErr error
	}{
		{name: "never prepared empty", mutate: func(r *nativeMountRootReceipt, _, _ *unix.Stat_t) { r.PreparedEmpty = false }},
		{name: "intent only", mutate: func(r *nativeMountRootReceipt, _, _ *unix.Stat_t) { r.VerifiedNativeMount = false }},
		{name: "root replaced", mutate: func(_ *nativeMountRootReceipt, root, _ *unix.Stat_t) { root.Ino++ }},
		{name: "root inode reused", mutate: func(_ *nativeMountRootReceipt, root, _ *unix.Stat_t) { root.Btim.Nsec++ }},
		{name: "root owner changed", mutate: func(_ *nativeMountRootReceipt, root, _ *unix.Stat_t) { root.Uid++ }},
		{name: "directory replaced", mutate: func(_ *nativeMountRootReceipt, _, system *unix.Stat_t) { system.Ino++ }},
		{name: "directory inode reused", mutate: func(_ *nativeMountRootReceipt, _, system *unix.Stat_t) { system.Btim.Sec++ }},
		{name: "different device", mutate: func(_ *nativeMountRootReceipt, _, system *unix.Stat_t) { system.Dev++ }},
		{name: "user owned", mutate: func(_ *nativeMountRootReceipt, _, system *unix.Stat_t) { system.Uid = 501 }},
		{name: "symlink", mutate: func(_ *nativeMountRootReceipt, _, system *unix.Stat_t) { system.Mode = unix.S_IFLNK | 0o700 }},
		{name: "regular file", mutate: func(_ *nativeMountRootReceipt, _, system *unix.Stat_t) { system.Mode = unix.S_IFREG | 0o700 }},
		{name: "group writable", mutate: func(_ *nativeMountRootReceipt, _, system *unix.Stat_t) { system.Mode |= 0o020 }},
		{name: "other writable", mutate: func(_ *nativeMountRootReceipt, _, system *unix.Stat_t) { system.Mode |= 0o002 }},
		{name: "unexpected permission pattern", mutate: func(_ *nativeMountRootReceipt, _, system *unix.Stat_t) { system.Mode |= 0o055 }},
		{name: "special mode bits", mutate: func(_ *nativeMountRootReceipt, _, system *unix.Stat_t) { system.Mode |= unix.S_ISGID }},
		{name: "unknown birthtime", mutate: func(_ *nativeMountRootReceipt, _, system *unix.Stat_t) { system.Btim = unix.Timespec{} }},
		{name: "ACL present", acl: true},
		{name: "ACL unavailable", aclErr: unix.EACCES},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receipt, currentRoot, currentSystem := base, root, system
			if tc.mutate != nil {
				tc.mutate(&receipt, &currentRoot, &currentSystem)
			}
			if err := validateNativeSystemDirectory(&receipt, &currentRoot, &currentSystem, tc.acl, tc.aclErr); !errors.Is(err, errMountRootNotEmpty) {
				t.Fatalf("unproved object accepted: %v", err)
			}
		})
	}
}

func TestNativeRootReceiptDoesNotTransferToAnotherFolder(t *testing.T) {
	s, root, prepared := nativeReceiptFixture(t)
	if err := verifyNativeMountRoot(s.opts.StateDir, prepared); err != nil {
		t.Fatal(err)
	}
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.prepareNativeMountRoot(other)
	if err != nil || second == nil || second.VerifiedNativeMount || second.Identity == prepared.Identity {
		t.Fatalf("new root inherited eligibility: %+v %v", second, err)
	}
	first, err := readNativeRootReceipt(nativeRootReceiptPath(s.opts.StateDir, root))
	if err != nil || !first.VerifiedNativeMount {
		t.Fatalf("preparing a new root lost rollback history: %+v %v", first, err)
	}
	if err := s.recheckNativeMountRoot(other, prepared); !errors.Is(err, errNativeRootChanged) {
		t.Fatalf("different root passed pre-command recheck: %v", err)
	}
}

func TestNativeRootKeepsUserEntriesAndUnreceiptedSystemName(t *testing.T) {
	for _, name := range []string{"notes.txt", ".DS_Store", ".fseventsd"} {
		t.Run(name, func(t *testing.T) {
			s := &Service{opts: Options{StateDir: t.TempDir()}}
			root := t.TempDir()
			path := filepath.Join(root, name)
			value := []byte{0, 0xff, 1, 2, 0}
			if name == ".fseventsd" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(path, "preserved")
			}
			if err := os.WriteFile(path, value, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.checkMountDirectory(root); !errors.Is(err, errMountRootNotEmpty) {
				t.Fatalf("existing entry was accepted: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, value) {
				t.Fatalf("refusal changed user bytes: %v %v", got, err)
			}
		})
	}
}

func TestNativeRootRejectsUserDirectoryEvenWithVerifiedHistory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the synthetic UID501 policy cases cover non-root ownership; this fixture must be user-owned")
	}
	s, root, prepared := nativeReceiptFixture(t)
	if err := verifyNativeMountRoot(s.opts.StateDir, prepared); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".fseventsd")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.checkMountDirectory(root); !errors.Is(err, errMountRootNotEmpty) {
		t.Fatalf("user-created system name accepted: %v", err)
	}
	loaded, err := readNativeRootReceipt(nativeRootReceiptPath(s.opts.StateDir, root))
	if err != nil || loaded.SystemDirectory != nil {
		t.Fatalf("untrusted directory was bound: %+v %v", loaded, err)
	}
}

func TestNativeRootReceiptRejectsUnsafeFilesWithoutBlocking(t *testing.T) {
	for _, name := range []string{"FIFO", "symlink", "hardlink", "permissions", "malformed", "unprivate parent", "symlink parent"} {
		t.Run(name, func(t *testing.T) {
			s, root, _ := nativeReceiptFixture(t)
			path := nativeRootReceiptPath(s.opts.StateDir, root)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "FIFO":
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "private"), path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				target := filepath.Join(t.TempDir(), "linked")
				if err := os.WriteFile(target, []byte("preserved"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(target, path); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unprivate parent":
				if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink parent":
				if err := os.Remove(filepath.Dir(path)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Dir(path)); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { _, err := readNativeRootReceipt(path); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("unsafe history accepted")
				}
			case <-time.After(time.Second):
				// Release a buggy FIFO reader so the disposable test can clean up.
				if name == "FIFO" {
					fd, _ := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0)
					if fd >= 0 {
						_ = unix.Close(fd)
					}
				}
				t.Fatal("unsafe history blocked before its type check")
			}
		})
	}
}

func TestNativeRootReceiptRefusesChangedPreparationAndKeepsOldBytes(t *testing.T) {
	s, root, prepared := nativeReceiptFixture(t)
	path := nativeRootReceiptPath(s.opts.StateDir, root)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wrong := *prepared
	wrong.Identity.Inode++
	if err := verifyNativeMountRoot(s.opts.StateDir, &wrong); err == nil {
		t.Fatal("changed proof was committed")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("verification refusal changed durable preparation")
	}
	var saved nativeMountRootReceipt
	if err := json.Unmarshal(after, &saved); err != nil || saved.VerifiedNativeMount {
		t.Fatal("failed native setup grants exception")
	}
}

func TestNativeRootRetiresAbsentSystemBindingOnlyAfterSuccessfulEmptyMount(t *testing.T) {
	s, root, first := nativeReceiptFixture(t)
	if err := verifyNativeMountRoot(s.opts.StateDir, first); err != nil {
		t.Fatal(err)
	}
	path := nativeRootReceiptPath(s.opts.StateDir, root)
	stored, err := readNativeRootReceipt(path)
	if err != nil {
		t.Fatal(err)
	}
	previousDirectory := nativeRootObjectIdentity{VolumeUUID: stored.Identity.VolumeUUID, Inode: 99001, BirthSec: 1000, BirthNSec: 4, UID: 0}
	stored.SystemDirectory = &previousDirectory
	if err := writeNativeRootReceipt(path, *stored); err != nil {
		t.Fatal(err)
	}
	// The root is actually empty. Preparation must retain the historical pin
	// until this new empty native session is positively verified.
	next, err := s.prepareNativeMountRoot(root)
	if err != nil || next == nil || !next.wasEmpty || next.SystemDirectory == nil {
		t.Fatalf("empty preparation lost the previous binding: %+v %v", next, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wrong := *next
	wrong.Identity.BirthNSec++
	if err := verifyNativeMountRoot(s.opts.StateDir, &wrong); err == nil {
		t.Fatal("failed verification retired a system binding")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed empty verification changed the bound receipt")
	}
	if err := s.recheckNativeMountRoot(root, next); err != nil {
		t.Fatal(err)
	}
	if err := verifyNativeMountRoot(s.opts.StateDir, next); err != nil {
		t.Fatal(err)
	}
	retired, err := readNativeRootReceipt(path)
	if err != nil || !retired.VerifiedNativeMount || retired.SystemDirectory != nil {
		t.Fatalf("successful empty native session did not retire the absent binding: %+v %v", retired, err)
	}
}

func TestNativeMountKeepsLiveOwnerWhenReceiptVerificationFails(t *testing.T) {
	f := newFakeFSKitMount(t)
	f.ops.verifyRoot = func(string, *nativeMountRootReceipt) error { return errors.New("fixture: durable history unavailable") }
	owner, err := f.service.mountNativeFSKit(context.Background(), f.root, nil, f.ops)
	if err == nil || owner == nil || f.bridge.closeCount() != 0 || !strings.Contains(err.Error(), "attached") {
		t.Fatalf("receipt failure orphaned live mount: owner=%v err=%v drains=%d", owner, err, f.bridge.closeCount())
	}
	if identity, verified := owner.(*nativeFSKitMount).verifiedIdentity(); !verified || identity != f.ownIdentity() {
		t.Fatal("receipt failure lost captured native identity")
	}
	if err := owner.Unmount(); err != nil {
		t.Fatal(err)
	}
	if f.bridge.closeCount() != 1 {
		t.Fatal("later normal unmount did not drain once")
	}
}

func TestNativeMountRejectsRootReplacementBeforeCommand(t *testing.T) {
	f := newFakeFSKitMount(t)
	if err := os.Chmod(f.service.opts.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if prepared, err := f.service.prepareNativeMountRoot(f.root); err != nil || prepared == nil {
		t.Fatalf("replacement fixture must begin with an eligible empty root: %+v %v", prepared, err)
	}
	start := f.ops.start
	f.ops.start = func(ctx context.Context, source, socketDir string, fs *catalogfs.FileSystem) (platformBridge, error) {
		bridge, err := start(ctx, source, socketDir, fs)
		if err != nil {
			return bridge, err
		}
		if err := os.Rename(f.root, f.root+"-preserved"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(f.root + "-preserved") })
		if err := os.Mkdir(f.root, 0o700); err != nil {
			t.Fatal(err)
		}
		return bridge, nil
	}
	owner, err := f.service.mountNativeFSKit(context.Background(), f.root, nil, f.ops)
	if !errors.Is(err, errNativeRootChanged) || owner != nil || f.commandCount() != 0 || f.bridge.closeCount() != 1 {
		t.Fatalf("changed root reached mount command: owner=%v err=%v commands=%d drains=%d", owner, err, f.commandCount(), f.bridge.closeCount())
	}
}
