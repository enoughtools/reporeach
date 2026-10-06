//go:build !windows

package catalogfs

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
)

func browsingFixture(t *testing.T, unknown bool) (*FileSystem, fixture, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	f := repositoryFixture(t, "alice/project")
	var activations, acquisitions atomic.Int64
	fs, err := NewWithPreview(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return f.backend, nil
	}, nil, func(_ context.Context, _ Entry, path string) (PreviewDirectory, error) {
		acquisitions.Add(1)
		if path != "." {
			return PreviewDirectory{Revision: "commit"}, nil
		}
		sizeState := "known"
		if unknown {
			sizeState = "unknown"
		}
		return PreviewDirectory{Revision: "commit", Entries: []model.BaseNode{
			{Path: "README.md", Type: "file", Mode: 0o644, SizeState: sizeState, SizeBytes: int64(len(f.content)), ObjectOID: "blob"},
			{Path: ".DS_Store", Type: "file", Mode: 0o644, SizeState: "known", SizeBytes: 6},
			{Path: "Icon\r", Type: "file", Mode: 0o644, SizeState: "known", SizeBytes: 3},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fs, f, &activations, &acquisitions
}

func TestBrowsingPreviewMetadataProbesAndListingNeverActivate(t *testing.T) {
	fs, f, activations, acquisitions := browsingFixture(t, true)
	root := repoRoot(t, fs, "alice")
	ctx := context.Background()
	for _, name := range []string{".DS_Store", "Icon\r", "README.md"} {
		op := &fuseops.LookUpInodeOp{Parent: root, Name: name}
		known, err := fs.LookUpMetadata(ctx, op)
		if err != nil || known != (name != "README.md") {
			t.Fatalf("metadata lookup %q: known=%v err=%v", name, known, err)
		}
		attrs := &fuseops.GetInodeAttributesOp{Inode: op.Entry.Child}
		if got, err := fs.GetMetadataAttributes(ctx, attrs); err != nil || got != known {
			t.Fatalf("metadata attributes %q: known=%v err=%v", name, got, err)
		}
		if err := fs.GetXattr(ctx, &fuseops.GetXattrOp{Inode: op.Entry.Child, Name: "com.apple.FinderInfo"}); err != fuse.ENOATTR {
			t.Fatalf("immutable Git xattr: %v", err)
		}
	}
	missing := &fuseops.LookUpInodeOp{Parent: root, Name: "._README.md"}
	if _, err := fs.LookUpMetadata(ctx, missing); err != syscall.ENOENT {
		t.Fatalf("authoritative missing name: %v", err)
	}
	h := openDir(t, fs, root)
	entries, err := fs.ReadDirectoryEntries(ctx, h, 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
		if e.Name == "README.md" && (e.SizeKnown || e.Entry.Attributes.Size != 0) {
			t.Fatalf("unknown blob represented as authoritative size: %+v", e)
		}
	}
	if !reflect.DeepEqual(names, []string{".DS_Store", "Icon\r", "README.md"}) {
		t.Fatalf("complete committed names: %q", names)
	}
	if activations.Load() != 0 || f.hydration.calls.Load() != 0 || acquisitions.Load() != 1 {
		t.Fatalf("browsing work: activation=%d hydration=%d metadata acquisitions=%d", activations.Load(), f.hydration.calls.Load(), acquisitions.Load())
	}
	if err := fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: h}); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewPromotionRetainsIdentityAndBalancesMixedReferences(t *testing.T) {
	fs, f, activations, _ := browsingFixture(t, true)
	ctx := context.Background()
	root := repoRoot(t, fs, "alice")
	var id fuseops.InodeID
	for i := 0; i < 3; i++ {
		op := &fuseops.LookUpInodeOp{Parent: root, Name: "README.md"}
		if _, err := fs.LookUpMetadata(ctx, op); err != nil {
			t.Fatal(err)
		}
		if id != 0 && id != op.Entry.Child {
			t.Fatal("preview lookups changed identity")
		}
		id = op.Entry.Child
	}
	open := &fuseops.OpenFileOp{Inode: id, OpenFlags: syscall.O_RDONLY}
	if err := fs.OpenFile(ctx, open); err != nil {
		t.Fatal(err)
	}
	if activations.Load() != 1 {
		t.Fatalf("open did not activate once: %d", activations.Load())
	}
	if actual := lookup(t, fs, root, "README.md"); actual != id {
		t.Fatalf("promotion changed ID: preview=%d actual=%d", id, actual)
	}
	n, err := fs.node(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: id, N: 3}); err != nil {
		t.Fatal(err)
	}
	if err := f.backend.GetInodeAttributes(ctx, &fuseops.GetInodeAttributesOp{Inode: n.local}); err != nil {
		t.Fatalf("old preview forgets retired remaining real identity: %v", err)
	}
	if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: id, N: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.node(id); err != syscall.ESTALE {
		t.Fatalf("forgotten preview mapping retained: %v", err)
	}
	if err := f.backend.GetInodeAttributes(ctx, &fuseops.GetInodeAttributesOp{Inode: n.local}); err != syscall.ESTALE {
		t.Fatalf("promotion leaked backend references: %v", err)
	}
	// The file descriptor still owns its backend handle after inode forget.
	if _, err := fs.GetFileHandleAttributes(ctx, id, open.Handle); err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: open.Handle}); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewConcurrentLookupsAcquireOnceAndKeepOneIdentity(t *testing.T) {
	fs, _, activations, acquisitions := browsingFixture(t, true)
	root := repoRoot(t, fs, "alice")
	ids := make(chan fuseops.InodeID, 24)
	errors := make(chan error, 24)
	var group sync.WaitGroup
	for i := 0; i < cap(ids); i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			op := &fuseops.LookUpInodeOp{Parent: root, Name: "README.md"}
			_, err := fs.LookUpMetadata(context.Background(), op)
			ids <- op.Entry.Child
			errors <- err
		}()
	}
	group.Wait()
	close(ids)
	close(errors)
	var first fuseops.InodeID
	for id := range ids {
		if first != 0 && first != id {
			t.Fatal("concurrent lookup aliases")
		}
		first = id
	}
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if acquisitions.Load() != 1 || activations.Load() != 0 {
		t.Fatalf("duplicated work: metadata=%d runtime=%d", acquisitions.Load(), activations.Load())
	}
	if err := fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: first, N: 24}); err != nil {
		t.Fatal(err)
	}
	if len(fs.previewInodes) != 0 {
		t.Fatal("finished preview references retained identity")
	}
}

func TestPreviewErrorsRevisionMismatchAndWritableFallbackAreExplicit(t *testing.T) {
	f := repositoryFixture(t, "alice/project")
	var calls atomic.Int64
	fs, err := NewWithPreview(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		calls.Add(1)
		return f.backend, nil
	}, nil, func(_ context.Context, _ Entry, path string) (PreviewDirectory, error) {
		if path == "." {
			return PreviewDirectory{Revision: "first", Entries: []model.BaseNode{{Path: "nested", Type: "dir", Mode: 0o755, SizeState: "known"}}}, nil
		}
		return PreviewDirectory{Revision: "changed"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t, fs, "alice")
	child := lookup(t, fs, root, "nested")
	if err := fs.OpenDir(context.Background(), &fuseops.OpenDirOp{Inode: child}); !errors.Is(err, syscall.ESTALE) {
		t.Fatalf("revision drift reported as empty directory: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("failed immutable metadata activated writable engine")
	}
	fs, err = NewWithPreview(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		calls.Add(1)
		return f.backend, nil
	}, nil, func(context.Context, Entry, string) (PreviewDirectory, error) {
		return PreviewDirectory{}, ErrPreviewUnavailable
	})
	if err != nil {
		t.Fatal(err)
	}
	root = repoRoot(t, fs, "alice")
	lookup(t, fs, root, "README.md")
	if calls.Load() != 1 {
		t.Fatal("existing writable state did not bypass source preview")
	}
}

func TestPreviewDirectoryReferencesSurvivePromotionAndDrainExactly(t *testing.T) {
	fs, f, _, _ := browsingFixture(t, true)
	ctx := context.Background()
	root := repoRoot(t, fs, "alice")
	previewHandle := openDir(t, fs, root)
	previewEntries, err := fs.ReadDirectoryEntries(ctx, previewHandle, 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var id fuseops.InodeID
	for _, entry := range previewEntries {
		if entry.Name == "README.md" {
			id = entry.Entry.Child
		}
		if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: entry.Entry.Child, N: 1}); err != nil {
			t.Fatal(err)
		}
	}
	open := &fuseops.OpenFileOp{Inode: id, OpenFlags: syscall.O_RDONLY}
	if err := fs.OpenFile(ctx, open); err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: open.Handle}); err != nil {
		t.Fatal(err)
	}
	realHandle := openDir(t, fs, root)
	realEntries, err := fs.ReadDirectoryEntries(ctx, realHandle, 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range realEntries {
		if entry.Name == "README.md" && entry.Entry.Child != id {
			t.Fatal("new writable directory did not retain preview identity")
		}
		if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: entry.Entry.Child, N: 1}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := fs.node(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: realHandle}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.node(id); err != nil {
		t.Fatal("real directory close retired still-open preview snapshot")
	}
	if err := fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: previewHandle}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.node(id); err != syscall.ESTALE {
		t.Fatalf("preview directory retained mapping after final close: %v", err)
	}
	if err := f.backend.GetInodeAttributes(ctx, &fuseops.GetInodeAttributesOp{Inode: n.local}); err != syscall.ESTALE {
		t.Fatalf("mixed directory lifecycle leaked promotion references: %v", err)
	}
}

func TestInFlightPreviewCannotReplacePromotedWorkingTree(t *testing.T) {
	f := repositoryFixture(t, "alice/project")
	started, complete := make(chan struct{}), make(chan struct{})
	fs, err := NewWithPreview(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		return f.backend, nil
	}, nil, func(context.Context, Entry, string) (PreviewDirectory, error) {
		close(started)
		<-complete
		return PreviewDirectory{Revision: "outdated", Entries: []model.BaseNode{{Path: "README.md", Type: "file", Mode: 0o644, SizeState: "known", SizeBytes: 987654}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t, fs, "alice")
	result := make(chan error, 1)
	go func() {
		op := &fuseops.LookUpInodeOp{Parent: root, Name: "README.md"}
		_, err := fs.LookUpMetadata(context.Background(), op)
		if err == nil && op.Entry.Attributes.Size != uint64(len(f.content)) {
			err = errors.New("in-flight source preview overwrote writable attributes")
		}
		result <- err
	}()
	<-started
	creation := &fuseops.CreateFileOp{Parent: root, Name: "promotion.txt", Mode: 0o644, OpenFlags: syscall.O_RDWR}
	if err := fs.CreateFile(context.Background(), creation); err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: creation.Handle}); err != nil {
		t.Fatal(err)
	}
	close(complete)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	repo := fs.repositories[testEntries[0].ID]
	if len(repo.previewDirs) != 0 {
		t.Fatal("source preview published after working-tree activation")
	}
}

func TestFrozenRepositoryRejectsEveryImplementedMutation(t *testing.T) {
	fs, _, _, _ := browsingFixture(t, false)
	ctx := context.Background()
	root := repoRoot(t, fs, "alice")
	id := lookup(t, fs, root, "README.md")
	open := &fuseops.OpenFileOp{Inode: id, OpenFlags: syscall.O_RDWR}
	if err := fs.OpenFile(ctx, open); err != nil {
		t.Fatal(err)
	}
	release, err := fs.FreezeRepositoryWrites(testEntries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	size := uint64(0)
	mutations := []struct {
		name string
		run  func() error
	}{
		{"create", func() error { return fs.CreateFile(ctx, &fuseops.CreateFileOp{Parent: root, Name: "new", Mode: 0o644}) }},
		{"mkdir", func() error { return fs.MkDir(ctx, &fuseops.MkDirOp{Parent: root, Name: "new", Mode: 0o755}) }},
		{"symlink", func() error {
			return fs.CreateSymlink(ctx, &fuseops.CreateSymlinkOp{Parent: root, Name: "new", Target: "README.md"})
		}},
		{"unlink", func() error { return fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: root, Name: "README.md"}) }},
		{"rmdir", func() error { return fs.RmDir(ctx, &fuseops.RmDirOp{Parent: root, Name: "new"}) }},
		{"rename", func() error {
			return fs.Rename(ctx, &fuseops.RenameOp{OldParent: root, NewParent: root, OldName: "README.md", NewName: "new"})
		}},
		{"setattr", func() error { return fs.SetInodeAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: id, Size: &size}) }},
		{"handle setattr", func() error {
			return fs.SetFileHandleAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: id, Handle: &open.Handle, Size: &size})
		}},
		{"write", func() error {
			return fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: id, Handle: open.Handle, Data: []byte("change")})
		}},
		{"write open", func() error { return fs.OpenFile(ctx, &fuseops.OpenFileOp{Inode: id, OpenFlags: syscall.O_WRONLY}) }},
		{"truncate open", func() error {
			return fs.OpenFile(ctx, &fuseops.OpenFileOp{Inode: id, OpenFlags: syscall.O_RDONLY | syscall.O_TRUNC})
		}},
		{"set xattr", func() error {
			return fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.test", Value: []byte("change")})
		}},
		{"remove xattr", func() error { return fs.RemoveXattr(ctx, &fuseops.RemoveXattrOp{Inode: id, Name: "user.test"}) }},
		{"root xattr", func() error {
			return fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: root, Name: "user.test", Value: []byte("change")})
		}},
	}
	for _, mutation := range mutations {
		if err := mutation.run(); err != syscall.EBUSY {
			t.Errorf("%s: got %v want busy", mutation.name, err)
		}
	}
	if err := fs.GetInodeAttributes(ctx, &fuseops.GetInodeAttributesOp{Inode: id}); err != nil {
		t.Fatalf("freeze prevented reads: %v", err)
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: open.Handle}); err != nil {
		t.Fatalf("freeze prevented normal handle drain: %v", err)
	}
}

type cancelOnSecondPreviewCheck struct {
	context.Context
	cancel context.CancelFunc
	checks atomic.Int64
}

func (ctx *cancelOnSecondPreviewCheck) Err() error {
	if ctx.checks.Add(1) >= 2 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestCancelledPreviewPageReturnsNoEntriesOrLookupReferences(t *testing.T) {
	fs, _, _, _ := browsingFixture(t, true)
	root := repoRoot(t, fs, "alice")
	handleID := openDir(t, fs, root)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelOnSecondPreviewCheck{Context: base, cancel: cancel}
	entries, err := fs.ReadDirectoryEntries(ctx, handleID, 0, 4096)
	if !errors.Is(err, context.Canceled) || len(entries) != 0 {
		t.Fatalf("cancelled page entries=%d err=%v", len(entries), err)
	}
	handle, err := fs.getHandle(handleID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range handle.entries {
		n, err := fs.node(entry.inode)
		if err != nil || n.refs != 0 {
			t.Fatalf("cancelled page leaked lookup on %q: node=%+v err=%v", entry.name, n, err)
		}
	}
	if err := fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: handleID}); err != nil {
		t.Fatal(err)
	}
	if len(fs.previewInodes) != 0 {
		t.Fatal("cancelled directory retained zero-reference identities")
	}
}

func TestDestroyReleasesPromotionBindingsEvenWithoutKernelForget(t *testing.T) {
	fs, f, _, _ := browsingFixture(t, true)
	root := repoRoot(t, fs, "alice")
	op := &fuseops.LookUpInodeOp{Parent: root, Name: "README.md"}
	if _, err := fs.LookUpMetadata(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	open := &fuseops.OpenFileOp{Inode: op.Entry.Child, OpenFlags: syscall.O_RDONLY}
	if err := fs.OpenFile(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	n, err := fs.node(op.Entry.Child)
	if err != nil {
		t.Fatal(err)
	}
	fs.Destroy()
	fs.Destroy()
	if err := f.backend.GetInodeAttributes(context.Background(), &fuseops.GetInodeAttributesOp{Inode: n.local}); err != syscall.ESTALE {
		t.Fatalf("teardown leaked promotion binding: %v", err)
	}
	if _, err := fs.node(op.Entry.Child); err != syscall.ESTALE {
		t.Fatal("destroyed preview accepted new metadata operations")
	}
}

func TestPreviewGitPointerMetadataStaysColdUntilContentOpen(t *testing.T) {
	f := repositoryFixture(t, "alice/project")
	gitContent := []byte("gitdir: " + f.config.GitDir + "\n")
	var activations atomic.Int64
	fs, err := NewWithPreview(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return f.backend, nil
	}, nil, func(context.Context, Entry, string) (PreviewDirectory, error) {
		return PreviewDirectory{Revision: "commit", GitFileSize: uint64(len(gitContent)), Entries: []model.BaseNode{
			{Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "blob", SizeState: "known", SizeBytes: int64(len(f.content))},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	root := repoRoot(t, fs, "alice")
	git := &fuseops.LookUpInodeOp{Parent: root, Name: ".git"}
	if known, err := fs.LookUpMetadata(ctx, git); err != nil || !known {
		t.Fatalf("cold Git lookup known=%v err=%v", known, err)
	}
	attrs := &fuseops.GetInodeAttributesOp{Inode: git.Entry.Child}
	if err := fs.GetInodeAttributes(ctx, attrs); err != nil || attrs.Attributes.Size != uint64(len(gitContent)) || !attrs.Attributes.Mode.IsRegular() {
		t.Fatalf("cold exact Git metadata=%+v err=%v", attrs.Attributes, err)
	}
	handle := openDir(t, fs, root)
	entries, err := fs.ReadDirectoryEntries(ctx, handle, 0, 4096)
	if err != nil || len(entries) != 2 || entries[0].Name != ".git" || entries[0].Entry.Child != git.Entry.Child || !entries[0].SizeKnown {
		t.Fatalf("cold names changed synthesized Git identity: entries=%+v err=%v", entries, err)
	}
	if activations.Load() != 0 || f.hydration.calls.Load() != 0 {
		t.Fatal("Git discovery prepared writable checkout or fetched content")
	}
	open := &fuseops.OpenFileOp{Inode: git.Entry.Child, OpenFlags: syscall.O_RDONLY}
	if err := fs.OpenFile(ctx, open); err != nil {
		t.Fatal(err)
	}
	read := &fuseops.ReadFileOp{Inode: git.Entry.Child, Handle: open.Handle, Size: int64(len(gitContent)), Dst: make([]byte, len(gitContent))}
	if err := fs.ReadFile(ctx, read); err != nil {
		t.Fatal(err)
	}
	var content []byte
	for _, part := range read.Data {
		content = append(content, part...)
	}
	if len(read.Data) == 0 {
		content = read.Dst[:read.BytesRead]
	}
	if string(content) != string(gitContent) || activations.Load() != 1 || f.hydration.calls.Load() != 0 {
		t.Fatalf("actual Git pointer content=%q activations=%d hydration=%d", content, activations.Load(), f.hydration.calls.Load())
	}
	if actual := lookup(t, fs, root, ".git"); actual != git.Entry.Child {
		t.Fatal("Git pointer promotion replaced native identity")
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: open.Handle}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: entry.Entry.Child, N: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: git.Entry.Child, N: 2}); err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: handle}); err != nil {
		t.Fatal(err)
	}
	if len(fs.previewInodes) != 0 {
		t.Fatal("Git pointer promotion leaked references")
	}
}
