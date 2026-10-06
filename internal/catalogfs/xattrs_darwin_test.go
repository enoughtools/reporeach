//go:build darwin

package catalogfs

import (
	"bytes"
	"context"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"golang.org/x/sys/unix"
)

func TestDarwinNoFollowXattrsPreservePolicyAndSymlinkIdentity(t *testing.T) {
	fixture := repositoryFixture(t, "alice/project")
	store := catalogueMetadataStore(t, catalogueMetadataConfig(t))
	fs, err := NewWithMetadata(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	root := repoRoot(t, fs, "alice")
	link := &fuseops.CreateSymlinkOp{Parent: root, Name: "link", Target: "README.md"}
	if err := fs.CreateSymlink(ctx, link); err != nil {
		t.Fatal(err)
	}
	for _, id := range []fuseops.InodeID{root, link.Entry.Child} {
		initial := []byte{0, 0xff, 1}
		if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.nofollow", Value: initial, Flags: unix.XATTR_NOFOLLOW}); err != nil {
			t.Fatalf("nofollow upsert: %v", err)
		}
		if got := catalogueXattr(t, fs, id, "user.nofollow"); !bytes.Equal(got, initial) {
			t.Fatalf("nofollow upsert changed value: %x", got)
		}
		if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.nofollow", Flags: unix.XATTR_CREATE | unix.XATTR_NOFOLLOW}); err != syscall.EEXIST {
			t.Fatalf("nofollow create existing: %v", err)
		}
		if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.missing", Flags: unix.XATTR_REPLACE | unix.XATTR_NOFOLLOW}); err != fuse.ENOATTR {
			t.Fatalf("nofollow replace missing: %v", err)
		}
		if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.created", Value: []byte("created"), Flags: unix.XATTR_CREATE | unix.XATTR_NOFOLLOW}); err != nil {
			t.Fatalf("nofollow create: %v", err)
		}
		if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.created", Value: []byte("replaced"), Flags: unix.XATTR_REPLACE | unix.XATTR_NOFOLLOW}); err != nil {
			t.Fatalf("nofollow replace: %v", err)
		}
		if got := catalogueXattr(t, fs, id, "user.created"); !bytes.Equal(got, []byte("replaced")) {
			t.Fatal("nofollow did not preserve replace policy")
		}
		if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.created", Flags: unix.XATTR_CREATE | unix.XATTR_REPLACE | unix.XATTR_NOFOLLOW}); err != syscall.EINVAL {
			t.Fatalf("nofollow accepted contradictory policies: %v", err)
		}
	}
	target := lookup(t, fs, root, "README.md")
	if err := fs.GetXattr(ctx, &fuseops.GetXattrOp{Inode: target, Name: "user.nofollow"}); err != fuse.ENOATTR {
		t.Fatalf("symlink metadata tagged its target: %v", err)
	}
	if fixture.hydration.calls.Load() != 0 {
		t.Fatal("symlink metadata operations hydrated the target")
	}
}
