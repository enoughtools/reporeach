//go:build darwin

package fusefs

import (
	"context"
	"syscall"
	"testing"

	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"golang.org/x/sys/unix"
)

func TestDarwinNoFollowXattrsApplyToSymlinkIdentity(t *testing.T) {
	f := newXattrFixture(t)
	link, target := xattrLookup(t, f.fs, "link"), xattrLookup(t, f.fs, "tracked.txt")
	if err := f.fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: link, Name: "user.local", Value: []byte("link"), Flags: unix.XATTR_NOFOLLOW}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: link, Name: "user.local", Value: []byte("wrong"), Flags: unix.XATTR_NOFOLLOW | unix.XATTR_CREATE}); err != syscall.EEXIST {
		t.Fatalf("nofollow create bypassed existing check: %v", err)
	}
	if err := f.fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: link, Name: "user.local", Value: []byte("changed"), Flags: unix.XATTR_NOFOLLOW | unix.XATTR_REPLACE}); err != nil {
		t.Fatal(err)
	}
	if got := xattrGet(t, f.fs, link); string(got) != "changed" {
		t.Fatal(got)
	}
	if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: target, Name: "user.local"}); err != fuse.ENOATTR {
		t.Fatalf("nofollow xattr changed symlink target: %v", err)
	}
	if err := f.fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: link, Name: "fresh", Flags: unix.XATTR_NOFOLLOW | unix.XATTR_CREATE}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: link, Name: "missing", Flags: unix.XATTR_NOFOLLOW | unix.XATTR_REPLACE}); err != fuse.ENOATTR {
		t.Fatal(err)
	}
}

func TestDarwinResolvedSetterContextPreservesPolicyAndValidation(t *testing.T) {
	f := newXattrFixture(t)
	id := xattrLookup(t, f.fs, "tracked.txt")
	for _, flags := range []uint32{unix.XATTR_NOFOLLOW, xattrNoFollowAny, xattrResolveBeneath, unix.XATTR_NOSECURITY, unix.XATTR_NODEFAULT, unix.XATTR_NOFOLLOW | xattrNoFollowAny | xattrResolveBeneath | unix.XATTR_NOSECURITY | unix.XATTR_NODEFAULT} {
		op := &fuseops.SetXattrOp{Inode: id, Name: "user.context", Value: []byte{0, 255}, Flags: flags | unix.XATTR_CREATE}
		if err := f.fs.SetXattr(context.Background(), op); err != nil {
			t.Fatal(err)
		}
		if err := f.fs.SetXattr(context.Background(), op); err != syscall.EEXIST {
			t.Fatalf("context flag bypassed create: flags%x %v", flags, err)
		}
		op.Flags = flags | unix.XATTR_REPLACE
		op.Value = nil
		if err := f.fs.SetXattr(context.Background(), op); err != nil {
			t.Fatal(err)
		}
		op.Name = "missing"
		if err := f.fs.SetXattr(context.Background(), op); err != fuse.ENOATTR {
			t.Fatalf("context flag bypassed replace: flags%x %v", flags, err)
		}
		op.Name = "bad\x00name"
		if err := f.fs.SetXattr(context.Background(), op); err != syscall.EINVAL {
			t.Fatalf("context flag bypassed validation: %v", err)
		}
		op.Name = "user.context"
		op.Inode = 9999
		if err := f.fs.SetXattr(context.Background(), op); err != syscall.ESTALE {
			t.Fatalf("context flag bypassed inode: %v", err)
		}
		if err := f.fs.RemoveXattr(context.Background(), &fuseops.RemoveXattrOp{Inode: id, Name: "user.context"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, flags := range []uint32{unix.XATTR_CREATE | unix.XATTR_REPLACE, unix.XATTR_SHOWCOMPRESSION, 1 << 20} {
		if err := f.fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: id, Name: "user.context", Flags: flags}); err != syscall.EINVAL {
			t.Fatalf("accepted unsupported flags%x: %v", flags, err)
		}
	}
}
