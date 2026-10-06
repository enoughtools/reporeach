//go:build !windows

package catalogfs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
)

func contentBrowsingFixture(t *testing.T, acquire func(context.Context)) (*FileSystem, fixture, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	f := repositoryFixture(t, "alice/project")
	var activations, contentCalls atomic.Int64
	fs, err := NewWithPreviewContent(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return f.backend, nil
	}, nil, func(context.Context, Entry, string) (PreviewDirectory, error) {
		return PreviewDirectory{Revision: "commit", GitFileSize: 42, Entries: []model.BaseNode{
			{Path: "README.md", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: int64(len(f.content))},
		}}, nil
	}, func(ctx context.Context, entry Entry, path, revision string) (*os.File, error) {
		if entry.ID != testEntries[0].ID || path != "README.md" || revision != "commit" {
			t.Errorf("content request lost immutable identity: %+v %q %q", entry, path, revision)
		}
		contentCalls.Add(1)
		if acquire != nil {
			acquire(ctx)
		}
		return os.Open(f.hydration.path)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fs.Destroy)
	return fs, f, &activations, &contentCalls
}

func openReadPreview(t *testing.T, fs *FileSystem, inode fuseops.InodeID) fuseops.HandleID {
	t.Helper()
	op := &fuseops.OpenFileOp{Inode: inode, OpenFlags: syscall.O_RDONLY}
	if err := fs.OpenFile(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	return op.Handle
}

func TestReadOnlyPreviewDefersContentAndSurvivesForgetAndHide(t *testing.T) {
	fs, f, activations, contentCalls := contentBrowsingFixture(t, nil)
	ctx := context.Background()
	root := repoRoot(t, fs, "alice")
	file := lookup(t, fs, root, "README.md")
	h := openReadPreview(t, fs, file)
	if attrs, err := fs.GetFileHandleAttributes(ctx, file, h); err != nil || attrs.Size != uint64(len(f.content)) {
		t.Fatalf("known fstat: %+v %v", attrs, err)
	}
	if contentCalls.Load() != 0 || activations.Load() != 0 || f.hydration.calls.Load() != 0 {
		t.Fatal("read-only open or known fstat acquired content")
	}
	if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: file, N: 1}); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetEntries(nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, file, h); !bytes.Equal(got, f.content) {
		t.Fatal("retained immutable descriptor lost binary bytes after hide/forget")
	}
	partial := &fuseops.ReadFileOp{Inode: file, Handle: h, Offset: 2, Size: 3}
	if err := fs.ReadFile(ctx, partial); err != nil || !bytes.Equal(partial.Data[0], f.content[2:5]) {
		t.Fatalf("partial binary read: %v %+v", err, partial.Data)
	}
	if err := fs.FlushFile(ctx, &fuseops.FlushFileOp{Inode: file, Handle: h}); err != nil {
		t.Fatal(err)
	}
	if err := fs.SyncFile(ctx, &fuseops.SyncFileOp{Inode: file, Handle: h}); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: file, Handle: h, Data: []byte("bad")}); err != syscall.EBADF {
		t.Fatalf("read-only write: %v", err)
	}
	if activations.Load() != 0 || contentCalls.Load() != 1 || f.hydration.calls.Load() != 0 {
		t.Fatal("read-only content prepared a writable backend or repeated acquisition")
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: h}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.node(file); err != syscall.ESTALE {
		t.Fatalf("closed preview retained forgotten identity: %v", err)
	}
}

func TestReadOnlyPreviewJoinsBackendBeforeWriteAndRetainsUnlinkedBytes(t *testing.T) {
	fs, f, activations, _ := contentBrowsingFixture(t, nil)
	ctx := context.Background()
	root := repoRoot(t, fs, "alice")
	file := lookup(t, fs, root, "README.md")
	reader := openReadPreview(t, fs, file)
	if got := readFile(t, fs, file, reader); !bytes.Equal(got, f.content) {
		t.Fatal("initial preview bytes")
	}
	writer := &fuseops.OpenFileOp{Inode: file, OpenFlags: syscall.O_RDWR}
	if err := fs.OpenFile(ctx, writer); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Repeat([]byte{0xff}, len(f.content))
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: file, Handle: writer.Handle, Data: changed}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, file, reader); !bytes.Equal(got, changed) {
		t.Fatal("existing preview descriptor missed later overlay write")
	}
	if activations.Load() != 1 {
		t.Fatal("promotion repeated writable activation")
	}
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: file, Handle: reader, Data: changed}); err != syscall.EBADF {
		t.Fatalf("promoted reader became writable: %v", err)
	}
	if err := fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: root, Name: "README.md"}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, file, reader); !bytes.Equal(got, changed) {
		t.Fatal("promoted open descriptor lost unlinked bytes")
	}
	for _, h := range []fuseops.HandleID{reader, writer.Handle} {
		if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: h}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadOnlyPreviewReadDrainsBeforeMutationPromotion(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	fs, f, _, _ := contentBrowsingFixture(t, func(context.Context) { close(started); <-finish })
	ctx := context.Background()
	file := lookup(t, fs, repoRoot(t, fs, "alice"), "README.md")
	reader := openReadPreview(t, fs, file)
	readDone := make(chan error, 1)
	go func() {
		op := &fuseops.ReadFileOp{Inode: file, Handle: reader, Size: int64(len(f.content))}
		err := fs.ReadFile(ctx, op)
		if err == nil && !bytes.Equal(op.Data[0], f.content) {
			err = syscall.EIO
		}
		readDone <- err
	}()
	<-started
	writer := &fuseops.OpenFileOp{Inode: file, OpenFlags: syscall.O_RDWR}
	openDone := make(chan error, 1)
	go func() { openDone <- fs.OpenFile(ctx, writer) }()
	select {
	case err := <-openDone:
		t.Fatalf("mutation passed an in-flight immutable reader: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(finish)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if err := <-openDone; err != nil {
		t.Fatal(err)
	}
	changed := bytes.Repeat([]byte{'x'}, len(f.content))
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: file, Handle: writer.Handle, Data: changed}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, file, reader); !bytes.Equal(got, changed) {
		t.Fatal("reader did not join the backend after its admitted read drained")
	}
}

func TestUnknownPreviewExactSizeReadsOnlyItsBlob(t *testing.T) {
	fs, f, activations, contentCalls := contentBrowsingFixture(t, nil)
	original := fs.preview
	fs.preview = func(ctx context.Context, entry Entry, path string) (PreviewDirectory, error) {
		directory, err := original(ctx, entry, path)
		directory.Entries[0].SizeState, directory.Entries[0].SizeBytes = "unknown", 0
		return directory, err
	}
	ctx := context.Background()
	root := repoRoot(t, fs, "alice")
	metadata := &fuseops.LookUpInodeOp{Parent: root, Name: "README.md"}
	if known, err := fs.LookUpMetadata(ctx, metadata); err != nil || known {
		t.Fatalf("metadata lookup invented size: %v %v", known, err)
	}
	if contentCalls.Load() != 0 {
		t.Fatal("unknown metadata lookup fetched bytes")
	}
	exact := &fuseops.GetInodeAttributesOp{Inode: metadata.Entry.Child}
	if err := fs.GetInodeAttributes(ctx, exact); err != nil || exact.Attributes.Size != uint64(len(f.content)) {
		t.Fatalf("exact stat: %d %v", exact.Attributes.Size, err)
	}
	lookup := &fuseops.LookUpInodeOp{Parent: root, Name: "README.md"}
	if err := fs.LookUpInode(ctx, lookup); err != nil || lookup.Entry.Attributes.Size != uint64(len(f.content)) || lookup.Entry.Child != metadata.Entry.Child {
		t.Fatalf("exact lookup: %+v %v", lookup.Entry, err)
	}
	if activations.Load() != 0 || f.hydration.calls.Load() != 0 || contentCalls.Load() != 2 {
		t.Fatal("exact size resolution activated the writable runtime")
	}
}

func TestPreviewReadlinkIsBoundedAndDoesNotActivate(t *testing.T) {
	ctx := context.Background()
	var activations, reads atomic.Int64
	target := "../target with spaces"
	cache := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(cache, []byte(target), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := NewWithPreviewContent(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return nil, syscall.EIO
	}, nil, func(context.Context, Entry, string) (PreviewDirectory, error) {
		return PreviewDirectory{Revision: "commit", Entries: []model.BaseNode{
			{Path: "link", Type: "symlink", Mode: 0o120000, SizeState: "known", SizeBytes: int64(len(target))},
			{Path: "huge", Type: "symlink", Mode: 0o120000, SizeState: "known", SizeBytes: model.MaxSymlinkTargetBytes + 1},
		}}, nil
	}, func(context.Context, Entry, string, string) (*os.File, error) { reads.Add(1); return os.Open(cache) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fs.Destroy)
	root := repoRoot(t, fs, "alice")
	op := &fuseops.ReadSymlinkOp{Inode: lookup(t, fs, root, "link")}
	if err := fs.ReadSymlink(ctx, op); err != nil || op.Target != target {
		t.Fatalf("readlink changed target: %q %v", op.Target, err)
	}
	if err := fs.ReadSymlink(ctx, &fuseops.ReadSymlinkOp{Inode: lookup(t, fs, root, "huge")}); err != syscall.ENAMETOOLONG {
		t.Fatalf("large readlink: %v", err)
	}
	if activations.Load() != 0 || reads.Load() != 1 {
		t.Fatal("readlink activated or fetched oversized target")
	}
}
