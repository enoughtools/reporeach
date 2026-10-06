//go:build !windows

package catalogfs

import (
	"context"
	"syscall"
	"testing"

	"github.com/jacobsa/fuse/fuseops"
)

func TestPreviewHandleMetadataDefersUnknownSizeUntilExactStat(t *testing.T) {
	fs, f, activations, contentCalls := contentBrowsingFixture(t, nil)
	original := fs.preview
	fs.preview = func(ctx context.Context, entry Entry, path string) (PreviewDirectory, error) {
		directory, err := original(ctx, entry, path)
		directory.Entries[0].SizeState, directory.Entries[0].SizeBytes = "unknown", 0
		return directory, err
	}
	ctx := context.Background()
	lookup := &fuseops.LookUpInodeOp{Parent: repoRoot(t, fs, "alice"), Name: "README.md"}
	if known, err := fs.LookUpMetadata(ctx, lookup); err != nil || known {
		t.Fatalf("metadata lookup: known=%v err=%v", known, err)
	}
	id := lookup.Entry.Child
	h := openReadPreview(t, fs, id)
	attrs, known, err := fs.GetFileHandleMetadataAttributes(ctx, id, h)
	if err != nil || known || attrs.Size != 0 || contentCalls.Load() != 0 {
		t.Fatalf("metadata fstat fetched or invented size: known=%v size=%d calls=%d err=%v", known, attrs.Size, contentCalls.Load(), err)
	}
	if _, _, err := fs.GetFileHandleMetadataAttributes(ctx, id+100, h); err != syscall.EBADF {
		t.Fatalf("mismatched inode accepted: %v", err)
	}
	if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: id, N: 1}); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetEntries(nil); err != nil {
		t.Fatal(err)
	}
	exact, err := fs.GetFileHandleAttributes(ctx, id, h)
	if err != nil || exact.Size != uint64(len(f.content)) || contentCalls.Load() != 1 {
		t.Fatalf("exact retained fstat: size=%d calls=%d err=%v", exact.Size, contentCalls.Load(), err)
	}
	attrs, known, err = fs.GetFileHandleMetadataAttributes(ctx, id, h)
	if err != nil || !known || attrs.Size != exact.Size || contentCalls.Load() != 1 || activations.Load() != 0 || f.hydration.calls.Load() != 0 {
		t.Fatalf("cached fstat lost size or activated engine: known=%v size=%d calls=%d err=%v", known, attrs.Size, contentCalls.Load(), err)
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: h}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.GetFileHandleMetadataAttributes(ctx, id, h); err != syscall.EBADF {
		t.Fatalf("closed descriptor accepted: %v", err)
	}
}
