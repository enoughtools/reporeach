//go:build !windows

package fusefs

import (
	"context"
	"os"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
)

func TestMetadataAttributesLeaveBaseBlobSizesUnknown(t *testing.T) {
	for _, name := range []string{"tracked.txt", "link"} {
		t.Run(name, func(t *testing.T) {
			f := newXattrFixture(t)
			node := f.snapshot.nodes[name]
			node.SizeState, node.SizeBytes = "unknown", 0
			f.snapshot.nodes[name] = node
			ctx := context.Background()
			lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: name}
			known, err := f.fs.LookUpMetadata(ctx, lookup)
			if err != nil || known || lookup.Entry.Attributes.Size != 0 {
				t.Fatalf("unknown metadata lookup: known=%t attrs=%+v err=%v", known, lookup.Entry.Attributes, err)
			}
			id := lookup.Entry.Child
			stat := &fuseops.GetInodeAttributesOp{Inode: id}
			known, err = f.fs.GetMetadataAttributes(ctx, stat)
			if err != nil || known || stat.Attributes.Size != 0 || stat.Attributes.Mode != lookup.Entry.Attributes.Mode || !stat.Attributes.Mtime.Equal(lookup.Entry.Attributes.Mtime) {
				t.Fatalf("unknown metadata stat: known=%t attrs=%+v err=%v", known, stat.Attributes, err)
			}
			if f.hydrator.calls != 0 || f.hydrator.readBlobCalls != 0 {
				t.Fatal("metadata acquired blob bytes")
			}
			xattrSet(t, f.fs, id, []byte{0, 255})
			known, err = f.fs.GetMetadataAttributes(ctx, stat)
			if err != nil || known || !stat.Attributes.Ctime.After(lookup.Entry.Attributes.Ctime) || !stat.Attributes.Mtime.Equal(lookup.Entry.Attributes.Mtime) {
				t.Fatal("metadata stat lost local xattr ctime or changed base mtime")
			}
			exact := &fuseops.GetInodeAttributesOp{Inode: id}
			if err := f.fs.GetInodeAttributes(ctx, exact); err != nil {
				t.Fatal(err)
			}
			want := uint64(f.hydrator.size)
			if name == "link" {
				want = uint64(len(f.hydrator.data))
			}
			if exact.Attributes.Size != want || f.hydrator.calls+f.hydrator.readBlobCalls != 1 {
				t.Fatal("ordinary stat no longer resolves exact size")
			}
			second := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: name}
			if known, err := f.fs.LookUpMetadata(ctx, second); err != nil || known || second.Entry.Child != id {
				t.Fatal("metadata lookup changed retained inode identity")
			}
			if err := f.fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: id, N: 2}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.fs.GetMetadataAttributes(ctx, stat); err != syscall.ESTALE {
				t.Fatalf("metadata lookup reference leak: %v", err)
			}
		})
	}
}

func TestMetadataAttributesAreExactForDirectoriesGitAndLocalOverlay(t *testing.T) {
	f := newXattrFixture(t)
	ctx := context.Background()
	f.snapshot.nodes["directory"] = model.BaseNode{Path: "directory", Type: "dir", Mode: 0o755, SizeState: "unknown"}
	if _, err := f.store.CreateFile(ctx, "tracked.txt", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Truncate(ctx, "tracked.txt", 57); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		size uint64
		mode os.FileMode
	}{
		{"directory", 4096, os.ModeDir | 0o755},
		{".git", uint64(len("gitdir: " + f.repo.GitDir + "\n")), 0o644},
		{"tracked.txt", 57, 0o600},
	} {
		lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: tc.name}
		known, err := f.fs.LookUpMetadata(ctx, lookup)
		if err != nil || !known || lookup.Entry.Attributes.Size != tc.size || lookup.Entry.Attributes.Mode != tc.mode {
			t.Fatalf("exact metadata %s: known=%t attrs=%+v err=%v", tc.name, known, lookup.Entry.Attributes, err)
		}
		stat := &fuseops.GetInodeAttributesOp{Inode: lookup.Entry.Child}
		if known, err := f.fs.GetMetadataAttributes(ctx, stat); err != nil || !known || stat.Attributes.Size != tc.size {
			t.Fatalf("exact metadata stat %s: known=%t attrs=%+v err=%v", tc.name, known, stat.Attributes, err)
		}
		if err := f.fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: lookup.Entry.Child, N: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if f.hydrator.calls != 0 || f.hydrator.readBlobCalls != 0 {
		t.Fatal("known metadata acquired blob bytes")
	}
}

func TestMetadataLookupFailureDoesNotGrantReference(t *testing.T) {
	fs, metadata, handle, hydrator := typedDirectoryFixture(t)
	t.Cleanup(func() { _ = fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: handle}) })
	metadata.path = "a.txt"
	op := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "a.txt"}
	if _, err := fs.LookUpMetadata(context.Background(), op); err == nil {
		t.Fatal("failed metadata lookup succeeded")
	}
	if fs.pathToInode["a.txt"] != 0 || hydrator.calls != 0 {
		t.Fatal("failed metadata lookup leaked an inode or acquired bytes")
	}
	metadata.path = ""
	if known, err := fs.LookUpMetadata(context.Background(), op); err != nil || known {
		t.Fatalf("metadata retry: known=%t err=%v", known, err)
	}
	if ref := fs.inodes[op.Entry.Child]; ref.Refcnt != 1 {
		t.Fatalf("metadata retry granted %d references", ref.Refcnt)
	}
}
