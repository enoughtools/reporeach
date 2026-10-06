//go:build !windows

package fusefs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"golang.org/x/sys/unix"
)

type xattrFixture struct {
	fs       *ArtifactFuse
	store    *overlaystore.Store
	repo     model.RepoConfig
	snapshot *fakeSnapshot
	hydrator *fakeLookupHydrator
}

func newXattrFixture(t *testing.T) *xattrFixture {
	t.Helper()
	root := t.TempDir()
	repo := model.RepoConfig{ID: "repo", GitDir: filepath.Join(root, "actual.git"), OverlayDir: filepath.Join(root, "overlay"), OverlayDBPath: filepath.Join(root, "overlay.db")}
	if err := os.Mkdir(repo.GitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := &fakeSnapshot{nodes: map[string]model.BaseNode{
		".":           {Path: ".", Type: "dir", Mode: 0o755, SizeState: "known", SizeBytes: 4096},
		"tracked.txt": {Path: "tracked.txt", Type: "file", Mode: 0o644, ObjectOID: "tracked", SizeState: "known", SizeBytes: 4},
		"target.txt":  {Path: "target.txt", Type: "file", Mode: 0o644, ObjectOID: "target", SizeState: "known", SizeBytes: 4},
		"link":        {Path: "link", Type: "symlink", Mode: 0o120000, ObjectOID: "link", SizeState: "known", SizeBytes: 11},
	}}
	store, err := overlaystore.New(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cache := filepath.Join(root, "blob")
	if err := os.WriteFile(cache, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &fakeLookupHydrator{path: cache, size: 4, data: []byte("tracked.txt")}
	resolver := &Resolver{Snapshot: snap, Overlay: store}
	resolver.SetGeneration(1)
	resolver.SetCommitTime(100)
	engine := &Engine{Repo: repo, Resolver: resolver, Overlay: store, Hydrator: h}
	return &xattrFixture{fs: NewArtifactFuse(repo, resolver, engine), store: store, repo: repo, snapshot: snap, hydrator: h}
}

func xattrLookup(t *testing.T, fs *ArtifactFuse, name string) fuseops.InodeID {
	t.Helper()
	op := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: name}
	if err := fs.LookUpInode(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	return op.Entry.Child
}

func xattrSet(t *testing.T, fs *ArtifactFuse, id fuseops.InodeID, value []byte) {
	t.Helper()
	if err := fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: id, Name: "user.local", Value: value}); err != nil {
		t.Fatal(err)
	}
}

func xattrGet(t *testing.T, fs *ArtifactFuse, id fuseops.InodeID) []byte {
	t.Helper()
	op := &fuseops.GetXattrOp{Inode: id, Name: "user.local", Dst: make([]byte, model.MaxXattrValueBytes)}
	if err := fs.GetXattr(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	return op.Dst[:op.BytesRead]
}

func TestXattrsAreBinaryEmptyAndMetadataOnly(t *testing.T) {
	f := newXattrFixture(t)
	id := xattrLookup(t, f.fs, "tracked.txt")
	before := &fuseops.GetInodeAttributesOp{Inode: id}
	if err := f.fs.GetInodeAttributes(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	node := f.snapshot.nodes["tracked.txt"]
	node.SizeState = "unknown"
	f.snapshot.nodes[node.Path] = node
	value := []byte{0, 255, 128, 13, 10, 0, 42}
	xattrSet(t, f.fs, id, value)
	value[1] = 0 // caller mutation must not alter persisted bytes
	want := []byte{0, 255, 128, 13, 10, 0, 42}
	if got := xattrGet(t, f.fs, id); !bytes.Equal(got, want) {
		t.Fatalf("binary bytes = %v", got)
	}
	query := &fuseops.GetXattrOp{Inode: id, Name: "user.local"}
	if err := f.fs.GetXattr(context.Background(), query); err != nil || query.BytesRead != len(want) {
		t.Fatalf("size query: %+v %v", query, err)
	}
	small := &fuseops.GetXattrOp{Inode: id, Name: "user.local", Dst: bytes.Repeat([]byte{99}, 2)}
	if err := f.fs.GetXattr(context.Background(), small); err != syscall.ERANGE || small.BytesRead != len(want) || !bytes.Equal(small.Dst, []byte{99, 99}) {
		t.Fatalf("small buffer: %+v %v", small, err)
	}
	if err := f.fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: id, Name: "user.local", Value: nil, Flags: unix.XATTR_REPLACE}); err != nil {
		t.Fatal(err)
	}
	if got := xattrGet(t, f.fs, id); len(got) != 0 {
		t.Fatalf("empty attr = %v", got)
	}
	if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: id, Name: "user.absent"}); err != fuse.ENOATTR {
		t.Fatalf("missing attr: %v", err)
	}
	if err := f.fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: id, Name: "a", Value: []byte{1}, Flags: unix.XATTR_CREATE}); err != nil {
		t.Fatal(err)
	}
	list := &fuseops.ListXattrOp{Inode: id, Dst: make([]byte, 128)}
	if err := f.fs.ListXattr(context.Background(), list); err != nil || string(list.Dst[:list.BytesRead]) != "a\x00user.local\x00" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if f.hydrator.calls != 0 || f.hydrator.readBlobCalls != 0 {
		t.Fatalf("xattrs hydrated: %+v", f.hydrator)
	}
	if count, err := f.store.DirtyCount(context.Background()); err != nil || count != 0 {
		t.Fatalf("data dirty = %d %v", count, err)
	}
	if entries, err := f.store.ListByPrefix(context.Background(), "."); err != nil || len(entries) != 0 {
		t.Fatalf("metadata created data overlays: %+v %v", entries, err)
	}
	entries, err := os.ReadDir(filepath.Join(f.repo.OverlayDir, "upper"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("metadata COW: %v %v", entries, err)
	}
	node.SizeState = "known"
	f.snapshot.nodes[node.Path] = node
	after := &fuseops.GetInodeAttributesOp{Inode: id}
	if err := f.fs.GetInodeAttributes(context.Background(), after); err != nil {
		t.Fatal(err)
	}
	if !after.Attributes.Mtime.Equal(before.Attributes.Mtime) || !after.Attributes.Ctime.After(before.Attributes.Ctime) {
		t.Fatalf("mtime/ctime before=%+v after=%+v", before.Attributes, after.Attributes)
	}
}

func TestXattrPoliciesAndBoundsRejectWithoutChangingValue(t *testing.T) {
	f := newXattrFixture(t)
	id := xattrLookup(t, f.fs, "tracked.txt")
	xattrSet(t, f.fs, id, []byte("before"))
	for _, tc := range []struct {
		name  string
		flags uint32
		value []byte
		want  error
	}{
		{"user.local", unix.XATTR_CREATE, []byte("bad"), syscall.EEXIST}, {"absent", unix.XATTR_REPLACE, nil, fuse.ENOATTR}, {"user.local", unix.XATTR_CREATE | unix.XATTR_REPLACE, nil, syscall.EINVAL}, {"user.local", 1 << 20, nil, syscall.EINVAL},
		{"", 0, nil, syscall.EINVAL}, {"bad\x00name", 0, nil, syscall.EINVAL}, {string([]byte{255}), 0, nil, syscall.EINVAL},
		{strings.Repeat("a", 128), 0, nil, syscall.ENAMETOOLONG}, {"user.local", 0, make([]byte, model.MaxXattrValueBytes+1), syscall.E2BIG},
	} {
		if err := f.fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: id, Name: tc.name, Flags: tc.flags, Value: tc.value}); err != tc.want {
			t.Fatalf("%q flag%d: %v want%v", tc.name, tc.flags, err, tc.want)
		}
	}
	if got := xattrGet(t, f.fs, id); string(got) != "before" {
		t.Fatalf("failed set changed value %q", got)
	}
	if err := f.fs.RemoveXattr(context.Background(), &fuseops.RemoveXattrOp{Inode: id, Name: "absent"}); err != fuse.ENOATTR {
		t.Fatal(err)
	}
	if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: 9999, Name: "user.local"}); err != syscall.ESTALE {
		t.Fatal(err)
	}
}

func TestXattrsFollowRenameAndRetainedReplacedInodes(t *testing.T) {
	f := newXattrFixture(t)
	source := xattrLookup(t, f.fs, "tracked.txt")
	target := xattrLookup(t, f.fs, "target.txt")
	xattrSet(t, f.fs, source, []byte("source"))
	xattrSet(t, f.fs, target, []byte("target"))
	open := &fuseops.OpenFileOp{Inode: target, OpenFlags: syscall.O_RDONLY}
	if err := f.fs.OpenFile(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: open.Handle})
	})
	if err := f.fs.Rename(context.Background(), &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "tracked.txt", NewParent: fuseops.RootInodeID, NewName: "target.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := xattrGet(t, f.fs, target); string(got) != "target" {
		t.Fatalf("old target attrs=%q", got)
	}
	if got := xattrGet(t, f.fs, source); string(got) != "source" {
		t.Fatalf("renamed source attrs=%q", got)
	}
	xattrSet(t, f.fs, target, []byte("retained"))
	if got := xattrGet(t, f.fs, xattrLookup(t, f.fs, "target.txt")); string(got) != "source" {
		t.Fatalf("retained write changed replacement=%q", got)
	}
	if err := f.fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: target, N: 1}); err != nil {
		t.Fatal(err)
	}
	if got := xattrGet(t, f.fs, target); string(got) != "retained" {
		t.Fatalf("forgotten open target attrs=%q", got)
	}
	if err := f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: open.Handle}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: target, Name: "user.local"}); err != syscall.ESTALE {
		t.Fatalf("released forgotten item=%v", err)
	}
}

func TestXattrsSurviveUnlinkUntilReferencesDrainWithoutResurrection(t *testing.T) {
	f := newXattrFixture(t)
	id := xattrLookup(t, f.fs, "tracked.txt")
	xattrSet(t, f.fs, id, []byte("old"))
	open := &fuseops.OpenFileOp{Inode: id, OpenFlags: syscall.O_RDONLY}
	if err := f.fs.OpenFile(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Unlink(context.Background(), &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "tracked.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: id, N: 1}); err != nil {
		t.Fatal(err)
	}
	xattrSet(t, f.fs, id, []byte("detached"))
	entry, found, err := f.store.Lookup(context.Background(), "tracked.txt")
	if err != nil || !found || !entry.IsDeleted() {
		t.Fatalf("xattr resurrected deleted path: %+v %v %v", entry, found, err)
	}
	create := &fuseops.CreateFileOp{Parent: fuseops.RootInodeID, Name: "tracked.txt", Mode: 0o644, OpenFlags: syscall.O_RDWR}
	if err := f.fs.CreateFile(context.Background(), create); err != nil {
		t.Fatal(err)
	}
	if create.Entry.Child == id {
		t.Fatal("replacement reused old inode")
	}
	if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: create.Entry.Child, Name: "user.local"}); err != fuse.ENOATTR {
		t.Fatalf("replacement inherited attrs=%v", err)
	}
	if got := xattrGet(t, f.fs, id); string(got) != "detached" {
		t.Fatalf("old attrs=%q", got)
	}
	_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: open.Handle})
	_ = f.fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: create.Handle})
}

func TestXattrsPersistAcrossReopenOnRootSymlinkAndVirtualGitfile(t *testing.T) {
	f := newXattrFixture(t)
	ids := []fuseops.InodeID{fuseops.RootInodeID, xattrLookup(t, f.fs, "link"), xattrLookup(t, f.fs, ".git")}
	for i, id := range ids {
		xattrSet(t, f.fs, id, []byte{byte(i), 0, 255})
	}
	attrs := &fuseops.GetInodeAttributesOp{Inode: ids[1]}
	if err := f.fs.GetInodeAttributes(context.Background(), attrs); err != nil {
		t.Fatal(err)
	}
	ctime := attrs.Attributes.Ctime
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := overlaystore.New(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r := &Resolver{Snapshot: f.snapshot, Overlay: store}
	r.SetGeneration(1)
	r.SetCommitTime(100)
	fs := NewArtifactFuse(f.repo, r, &Engine{Repo: f.repo, Resolver: r, Overlay: store, Hydrator: f.hydrator})
	link := xattrLookup(t, fs, "link")
	attrs = &fuseops.GetInodeAttributesOp{Inode: link}
	if err := fs.GetInodeAttributes(context.Background(), attrs); err != nil {
		t.Fatal(err)
	}
	if !attrs.Attributes.Ctime.Equal(ctime) {
		t.Fatalf("restart lost ctime before xattr/open: %v !=%v", attrs.Attributes.Ctime, ctime)
	}
	for i, id := range []fuseops.InodeID{fuseops.RootInodeID, link, xattrLookup(t, fs, ".git")} {
		if got := xattrGet(t, fs, id); !bytes.Equal(got, []byte{byte(i), 0, 255}) {
			t.Fatalf("persisted attrs=%v", got)
		}
	}
	if entries, err := os.ReadDir(f.repo.GitDir); err != nil || len(entries) != 0 {
		t.Fatalf("virtual.git metadata touched actualGitDir: %v %v", entries, err)
	}
	if count, err := store.DirtyCount(context.Background()); err != nil || count != 0 {
		t.Fatalf("restart metadata dirty=%d %v", count, err)
	}
}

func TestXattrHEADDisappearanceAndTypeReplacementDetachBindings(t *testing.T) {
	for _, replacementType := range []string{"file", "symlink"} {
		t.Run(replacementType, func(t *testing.T) {
			f := newXattrFixture(t)
			old := xattrLookup(t, f.fs, "tracked.txt")
			xattrSet(t, f.fs, old, []byte("old"))
			if err := f.fs.resolver.Transition(func() error {
				delete(f.snapshot.nodes, "tracked.txt")
				if err := f.store.ReconcileChecked(context.Background(), func(path string) (model.BaseNode, bool, error) { n, ok := f.snapshot.nodes[path]; return n, ok, nil }); err != nil {
					return err
				}
				f.fs.resolver.SetGeneration(2)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			f.snapshot.nodes["tracked.txt"] = model.BaseNode{Path: "tracked.txt", Type: replacementType, Mode: 0o644, SizeState: "known", SizeBytes: 0}
			f.fs.resolver.SetGeneration(3)
			fresh := xattrLookup(t, f.fs, "tracked.txt")
			if fresh == old {
				t.Fatal("HEAD recreation reused retired inode")
			}
			if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: fresh, Name: "user.local"}); err != fuse.ENOATTR {
				t.Fatalf("HEAD recreation inherited attrs=%v", err)
			}
			xattrSet(t, f.fs, old, []byte("retained"))
			if got := xattrGet(t, f.fs, old); string(got) != "retained" {
				t.Fatal(got)
			}
			if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: fresh, Name: "user.local"}); err != fuse.ENOATTR {
				t.Fatalf("retired write changed recreatedHEAD=%v", err)
			}
		})
	}
}

func TestMissingXattrProbesDoNotChangeVisibleTimes(t *testing.T) {
	f := newXattrFixture(t)
	id := xattrLookup(t, f.fs, "tracked.txt")
	before := &fuseops.GetInodeAttributesOp{Inode: id}
	if err := f.fs.GetInodeAttributes(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: id, Name: "com.apple.provenance"}); !errors.Is(err, fuse.ENOATTR) {
		t.Fatal(err)
	}
	if err := f.fs.ListXattr(context.Background(), &fuseops.ListXattrOp{Inode: id}); err != nil {
		t.Fatal(err)
	}
	after := &fuseops.GetInodeAttributesOp{Inode: id}
	if err := f.fs.GetInodeAttributes(context.Background(), after); err != nil {
		t.Fatal(err)
	}
	if !before.Attributes.Ctime.Equal(after.Attributes.Ctime) || !before.Attributes.Mtime.Equal(after.Attributes.Mtime) || before.Attributes.Ctime.Unix() != 100 {
		t.Fatalf("probe changed times before=%+v after=%+v", before.Attributes, after.Attributes)
	}
}

func xattrSnapshotChildren(f *xattrFixture) {
	f.snapshot.kids = map[string][]model.BaseNode{".": {}}
	for path, node := range f.snapshot.nodes {
		if path != "." {
			f.snapshot.kids["."] = append(f.snapshot.kids["."], node)
		}
	}
}

func xattrDirectoryIDs(t *testing.T, f *xattrFixture, handle fuseops.HandleID) map[string]fuseops.InodeID {
	t.Helper()
	ids := make(map[string]fuseops.InodeID)
	op := &fuseops.ReadDirOp{Inode: fuseops.RootInodeID, Handle: handle, Dst: make([]byte, 4096)}
	if err := f.fs.ReadDirWithInodeMapping(context.Background(), op, func(id fuseops.InodeID) fuseops.InodeID {
		ref, err := f.fs.metadataRef(id)
		if err != nil {
			t.Fatal(err)
		}
		ids[ref.Path] = id
		return id
	}); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestCachedOldHEADInodeCannotOpenOrMutateReplacement(t *testing.T) {
	for _, typ := range []string{"file", "dir"} {
		t.Run(typ, func(t *testing.T) {
			f := newXattrFixture(t)
			old := xattrLookup(t, f.fs, "tracked.txt")
			if err := f.fs.resolver.Transition(func() error {
				delete(f.snapshot.nodes, "tracked.txt")
				return f.store.ReconcileChecked(context.Background(), func(path string) (model.BaseNode, bool, error) { n, ok := f.snapshot.nodes[path]; return n, ok, nil })
			}); err != nil {
				t.Fatal(err)
			}
			f.snapshot.nodes["tracked.txt"] = model.BaseNode{Path: "tracked.txt", Type: typ, Mode: 0o755, SizeState: "known", SizeBytes: 0}
			if err := f.fs.OpenFile(context.Background(), &fuseops.OpenFileOp{Inode: old, OpenFlags: syscall.O_RDWR}); err != syscall.ESTALE {
				t.Fatalf("old inode opened HEAD replacement: %v", err)
			}
			if err := f.fs.OpenDir(context.Background(), &fuseops.OpenDirOp{Inode: old}); err != syscall.ESTALE {
				t.Fatalf("old inode opened replacement directory: %v", err)
			}
			size := uint64(0)
			if err := f.fs.SetInodeAttributes(context.Background(), &fuseops.SetInodeAttributesOp{Inode: old, Size: &size}); err != syscall.ESTALE {
				t.Fatalf("old inode modified replacement: %v", err)
			}
			if count, err := f.store.DirtyCount(context.Background()); err != nil || count != 0 {
				t.Fatalf("stale request dirtied data: %d %v", count, err)
			}
		})
	}
}

func TestDirectorySnapshotRetainsOldMetadataWithoutAliasingFreshHEAD(t *testing.T) {
	f := newXattrFixture(t)
	old := xattrLookup(t, f.fs, "tracked.txt")
	xattrSet(t, f.fs, old, []byte("historical"))
	xattrSnapshotChildren(f)
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := f.fs.OpenDir(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	defer f.fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: open.Handle})
	oldIDs := xattrDirectoryIDs(t, f, open.Handle)
	if oldIDs["tracked.txt"] != old {
		t.Fatalf("initial listing changed inode: %v", oldIDs)
	}
	dh, err := f.fs.dirHandle(open.Handle)
	if err != nil {
		t.Fatal(err)
	}
	var oldEntry ReaddirEntry
	for _, e := range dh.entries {
		if e.Name == "tracked.txt" {
			oldEntry = e
		}
	}
	metadata, found, err := f.store.MetadataObject(context.Background(), oldEntry.MetadataID)
	if err != nil || !found {
		t.Fatalf("snapshot lost metadata %v %v", found, err)
	}
	plus, err := f.fs.childEntryFromReaddir(context.Background(), dh, oldEntry)
	if err != nil || plus.Attributes.Ctime.UnixNano() != metadata.CtimeUnixNs {
		t.Fatalf("dir-plus ignored persisted ctime: %v %v", plus.Attributes.Ctime, err)
	}
	if err := f.fs.resolver.Transition(func() error {
		delete(f.snapshot.nodes, "tracked.txt")
		f.fs.resolver.SetGeneration(2)
		return f.store.ReconcileChecked(context.Background(), func(path string) (model.BaseNode, bool, error) { n, ok := f.snapshot.nodes[path]; return n, ok, nil })
	}); err != nil {
		t.Fatal(err)
	}
	f.snapshot.nodes["tracked.txt"] = model.BaseNode{Path: "tracked.txt", Type: "file", Mode: 0o644, SizeState: "known", SizeBytes: 0}
	f.fs.resolver.SetGeneration(3)
	xattrSnapshotChildren(f)
	freshOpen := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := f.fs.OpenDir(context.Background(), freshOpen); err != nil {
		t.Fatal(err)
	}
	defer f.fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: freshOpen.Handle})
	fresh := xattrDirectoryIDs(t, f, freshOpen.Handle)["tracked.txt"]
	if fresh == old {
		t.Fatal("fresh plain READDIR reused retired inode")
	}
	if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: fresh, Name: "user.local"}); err != fuse.ENOATTR {
		t.Fatalf("fresh listing inherited old attrs: %v", err)
	}
	if got := xattrDirectoryIDs(t, f, open.Handle)["tracked.txt"]; got != old {
		t.Fatalf("historical listing lost original identity: %d", got)
	}
	xattrSet(t, f.fs, old, []byte("still historical"))
	if got := xattrGet(t, f.fs, old); string(got) != "still historical" {
		t.Fatal(got)
	}
	if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: fresh, Name: "user.local"}); err != fuse.ENOATTR {
		t.Fatalf("old listing changed fresh HEAD metadata: %v", err)
	}
	plus, err = f.fs.childEntryFromReaddir(context.Background(), dh, oldEntry)
	if err != nil || plus.Child != old || plus.Attributes.Size != 4 {
		t.Fatalf("historical dir-plus lost snapshot: %+v %v", plus, err)
	}
	if f.fs.pathToInode["tracked.txt"] != fresh {
		t.Fatal("historical dir-plus displaced current namespace")
	}
}

func TestReadDirOnlyMissingDestinationDoesNotBlockValidRename(t *testing.T) {
	f := newXattrFixture(t)
	xattrSnapshotChildren(f)
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := f.fs.OpenDir(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	defer f.fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: open.Handle})
	ids := xattrDirectoryIDs(t, f, open.Handle)
	if ids["target.txt"] == 0 {
		t.Fatal("missing READDIR-only identity")
	}
	if err := f.fs.resolver.Transition(func() error {
		delete(f.snapshot.nodes, "target.txt")
		f.fs.resolver.SetGeneration(2)
		return f.store.ReconcileChecked(context.Background(), func(path string) (model.BaseNode, bool, error) { n, ok := f.snapshot.nodes[path]; return n, ok, nil })
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.Rename(context.Background(), &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "tracked.txt", NewParent: fuseops.RootInodeID, NewName: "target.txt"}); err != nil {
		t.Fatalf("retired READDIR-only destination blocked rename: %v", err)
	}
	if _, err := f.fs.resolver.ResolvePath("target.txt"); err != nil {
		t.Fatal(err)
	}
}

type failingXattrCtimeStore struct{ model.OverlayStore }

func (s *failingXattrCtimeStore) MetadataObject(context.Context, model.MetadataObjectID) (model.MetadataObject, bool, error) {
	return model.MetadataObject{}, false, context.Canceled
}

func TestLookupReleasesReferenceIfFinalMetadataReplyFails(t *testing.T) {
	for _, name := range []string{"tracked.txt", ".git"} {
		t.Run(name, func(t *testing.T) {
			f := newXattrFixture(t)
			f.fs.engine.Overlay = &failingXattrCtimeStore{OverlayStore: f.store}
			op := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: name}
			if err := f.fs.LookUpInode(context.Background(), op); err != syscall.EINTR {
				t.Fatalf("lookup final error: %v", err)
			}
			if f.fs.pathToInode[name] != 0 || len(f.fs.inodes) != 1 {
				t.Fatalf("failed lookup leaked inode: %+v", f.fs.inodes)
			}
		})
	}
}

func TestReadDirTypeReplacementAndReferenceDrainPreserveMetadataIdentity(t *testing.T) {
	f := newXattrFixture(t)
	old := xattrLookup(t, f.fs, "tracked.txt")
	xattrSet(t, f.fs, old, []byte("old file"))
	xattrSnapshotChildren(f)
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := f.fs.OpenDir(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	if id := xattrDirectoryIDs(t, f, open.Handle)["tracked.txt"]; id != old {
		t.Fatal(id)
	}
	if err := f.fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: old, N: 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.resolver.Transition(func() error {
		f.snapshot.nodes["tracked.txt"] = model.BaseNode{Path: "tracked.txt", Type: "symlink", Mode: 0o120000, SizeState: "known", SizeBytes: 0}
		f.fs.resolver.SetGeneration(2)
		return f.store.ReconcileChecked(context.Background(), func(path string) (model.BaseNode, bool, error) { n, ok := f.snapshot.nodes[path]; return n, ok, nil })
	}); err != nil {
		t.Fatal(err)
	}
	xattrSnapshotChildren(f)
	freshOpen := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := f.fs.OpenDir(context.Background(), freshOpen); err != nil {
		t.Fatal(err)
	}
	defer f.fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: freshOpen.Handle})
	fresh := xattrDirectoryIDs(t, f, freshOpen.Handle)["tracked.txt"]
	if fresh == old {
		t.Fatal("type replacement reused file inode")
	}
	if got := xattrGet(t, f.fs, old); string(got) != "old file" {
		t.Fatalf("retained old file attr=%q", got)
	}
	if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: fresh, Name: "user.local"}); err != fuse.ENOATTR {
		t.Fatalf("new symlink inherited old file attr: %v", err)
	}
	if err := f.fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: open.Handle}); err != nil {
		t.Fatal(err)
	}
	if err := f.fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: old, Name: "user.local"}); err != syscall.ESTALE {
		t.Fatalf("old dirent lifetime didn't drain: %v", err)
	}
}
