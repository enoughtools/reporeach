//go:build !windows

package fusefs

import (
	"context"
	"syscall"
	"testing"

	"github.com/jacobsa/fuse/fuseops"
)

func TestPreparedHandleMetadataRemainsLazyAndRetainsRenamedUnlinkedAuthority(t *testing.T) {
	f := newXattrFixture(t)
	node := f.snapshot.nodes["tracked.txt"]
	node.SizeState, node.SizeBytes = "unknown", 0
	f.snapshot.nodes[node.Path] = node
	ctx := context.Background()
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "tracked.txt"}
	if known, err := f.fs.LookUpMetadata(ctx, lookup); err != nil || known {
		t.Fatalf("metadata lookup: %v %v", known, err)
	}
	id := lookup.Entry.Child
	opened := &fuseops.OpenFileOp{Inode: id, OpenFlags: syscall.O_RDONLY}
	if err := f.fs.OpenFile(ctx, opened); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: opened.Handle}) })
	// Ordinary prepared opens already retain their binary backing descriptor.
	// Metadata must not perform an additional exact-size resolution.
	f.hydrator.calls = 0
	attrs, known, err := f.fs.GetFileHandleMetadataAttributes(ctx, id, opened.Handle)
	if err != nil || known || attrs.Size != 0 || f.hydrator.calls != 0 {
		t.Fatalf("prepared metadata acquired exact size: known=%v size=%d calls=%d err=%v", known, attrs.Size, f.hydrator.calls, err)
	}
	if err := f.fs.Rename(ctx, &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "tracked.txt", NewParent: fuseops.RootInodeID, NewName: "moved.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: id, N: 1}); err != nil {
		t.Fatal(err)
	}
	attrs, known, err = f.fs.GetFileHandleMetadataAttributes(ctx, id, opened.Handle)
	if err != nil || !known || attrs.Size != 4 {
		t.Fatalf("renamed descriptor lost current authority: known=%v size=%d err=%v", known, attrs.Size, err)
	}
	if err := f.fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "moved.txt"}); err != nil {
		t.Fatal(err)
	}
	replacement := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "moved.txt", Mode: 0o600, OpenFlags: syscall.O_RDWR}
	if err := f.fs.CreateFile(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: replacement.Handle}) })
	if err := f.fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: replacement.Entry.Child, Handle: replacement.Handle, Data: []byte("replacement bytes")}); err != nil {
		t.Fatal(err)
	}
	attrs, known, err = f.fs.GetFileHandleMetadataAttributes(ctx, id, opened.Handle)
	if err != nil || !known || attrs.Size != 4 || attrs.Nlink != 0 {
		t.Fatalf("unlinked descriptor borrowed replacement metadata: known=%v attrs=%+v err=%v", known, attrs, err)
	}
}
