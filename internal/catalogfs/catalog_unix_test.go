//go:build !windows

package catalogfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
)

var testEntries = []Entry{{ID: "alice/project", Owner: "alice", Name: "project"}, {ID: "team/project", Owner: "team", Name: "project"}}

type fixture struct {
	backend   *fusefs.ArtifactFuse
	hydration *testHydrator
	content   []byte
}

type testHydrator struct {
	path    string
	content []byte
	calls   atomic.Int64
}

func (h *testHydrator) Enqueue(model.HydrationTask)        {}
func (h *testHydrator) EnqueueBatch([]model.HydrationTask) {}
func (h *testHydrator) QueueDepth(model.RepoID) int        { return 0 }
func (h *testHydrator) EnsureHydrated(context.Context, model.RepoConfig, model.BaseNode) (string, int64, error) {
	h.calls.Add(1)
	return h.path, int64(len(h.content)), nil
}
func (h *testHydrator) OpenHydrated(context.Context, model.RepoConfig, model.BaseNode) (*os.File, int64, error) {
	h.calls.Add(1)
	f, err := os.Open(h.path)
	return f, int64(len(h.content)), err
}
func (h *testHydrator) ReadBlob(context.Context, model.RepoConfig, model.BaseNode, int64) ([]byte, error) {
	h.calls.Add(1)
	return bytes.Clone(h.content), nil
}

func repositoryFixture(t *testing.T, id string) fixture {
	t.Helper()
	root := t.TempDir()
	ctx := context.Background()
	cfg := model.RepoConfig{ID: model.RepoID(id), Name: "project", GitDir: filepath.Join(root, "git"), OverlayDir: filepath.Join(root, "overlay"), BlobCacheDir: filepath.Join(root, "cache"), MetaDBPath: filepath.Join(root, "snapshot.db"), OverlayDBPath: filepath.Join(root, "overlay.db")}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snap.Close() })
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	content := append([]byte(id), 0, 0xff, 0x01)
	gen, err := snap.PublishGeneration(ctx, "commit", "main", []model.BaseNode{{RepoID: cfg.ID, Path: ".", Type: "dir", Mode: 0o755, SizeState: "known", SizeBytes: 4096}, {RepoID: cfg.ID, Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "blob", SizeState: "known", SizeBytes: int64(len(content))}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "blob")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	h := &testHydrator{path: path, content: content}
	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	engine := &fusefs.Engine{Repo: cfg, Resolver: resolver, Overlay: ov, Hydrator: h}
	return fixture{backend: fusefs.NewArtifactFuse(cfg, resolver, engine), hydration: h, content: content}
}

func lookup(t *testing.T, fs *FileSystem, parent fuseops.InodeID, name string) fuseops.InodeID {
	t.Helper()
	op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
	if err := fs.LookUpInode(context.Background(), op); err != nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	return op.Entry.Child
}
func repoRoot(t *testing.T, fs *FileSystem, owner string) fuseops.InodeID {
	t.Helper()
	return lookup(t, fs, lookup(t, fs, fuseops.RootInodeID, owner), "project")
}
func openDir(t *testing.T, fs *FileSystem, id fuseops.InodeID) fuseops.HandleID {
	t.Helper()
	op := &fuseops.OpenDirOp{Inode: id}
	if err := fs.OpenDir(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	return op.Handle
}
func readFile(t *testing.T, fs *FileSystem, id fuseops.InodeID, handle fuseops.HandleID) []byte {
	t.Helper()
	op := &fuseops.ReadFileOp{Inode: id, Handle: handle, Size: 1024}
	if err := fs.ReadFile(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	var result []byte
	for _, chunk := range op.Data {
		result = append(result, chunk...)
	}
	return result
}

type directoryRecord struct {
	name  string
	inode fuseops.InodeID
}

func readRecords(t *testing.T, data []byte, plus bool) []directoryRecord {
	t.Helper()
	var records []directoryRecord
	// FUSE's wire format uses native byte order and an aligned fixed header.
	const entryOut = 128
	for len(data) > 0 {
		extra := 0
		if plus {
			extra = entryOut
		}
		if len(data) < extra+24 {
			t.Fatal("truncated directory record")
		}
		id := fuseops.InodeID(binary.NativeEndian.Uint64(data[extra:]))
		n := int(binary.NativeEndian.Uint32(data[extra+16:]))
		if len(data) < extra+24+n {
			t.Fatal("truncated directory name")
		}
		records = append(records, directoryRecord{name: string(data[extra+24 : extra+24+n]), inode: id})
		data = data[extra+24+(n+7)/8*8:]
	}
	return records
}

func TestCatalogueBrowsingDoesNotActivateOrHydrate(t *testing.T) {
	var activations atomic.Int64
	fs, err := New(testEntries, func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return nil, errors.New("unexpected activation")
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rootHandle := openDir(t, fs, fuseops.RootInodeID)
	read := &fuseops.ReadDirOp{Inode: fuseops.RootInodeID, Handle: rootHandle, Dst: make([]byte, 4096)}
	if err := fs.ReadDir(ctx, read); err != nil {
		t.Fatal(err)
	}
	records := readRecords(t, read.Dst[:read.BytesRead], false)
	if len(records) != 2 || records[0].name != "alice" || records[1].name != "team" {
		t.Fatalf("owners: %+v", records)
	}
	for _, owner := range []string{"alice", "team"} {
		id := repoRoot(t, fs, owner)
		attrs := &fuseops.GetInodeAttributesOp{Inode: id}
		if err := fs.GetInodeAttributes(ctx, attrs); err != nil || !attrs.Attributes.Mode.IsDir() {
			t.Fatalf("repo placeholder: %v", err)
		}
		if err := fs.GetXattr(ctx, &fuseops.GetXattrOp{Inode: id, Name: "com.apple.FinderInfo"}); err != fuse.ENOATTR {
			t.Fatal(err)
		}
	}
	if activations.Load() != 0 {
		t.Fatal("catalogue browsing activated repositories")
	}
}

func TestRepositoriesWithSameNameHaveIndependentInodesHandlesAndContents(t *testing.T) {
	fixtures := map[string]fixture{}
	for _, e := range testEntries {
		fixtures[e.ID] = repositoryFixture(t, e.ID)
	}
	var activations atomic.Int64
	fs, err := New(testEntries, func(_ context.Context, e Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return fixtures[e.ID].backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []fuseops.InodeID
	var handles []fuseops.HandleID
	for _, e := range testEntries {
		root := repoRoot(t, fs, e.Owner)
		_ = openDir(t, fs, root)
		dirRead := &fuseops.ReadDirOp{Inode: root, Handle: openDir(t, fs, root), Dst: make([]byte, 4096)}
		if err := fs.ReadDir(context.Background(), dirRead); err != nil {
			t.Fatal(err)
		}
		records := readRecords(t, dirRead.Dst[:dirRead.BytesRead], false)
		seen := map[fuseops.InodeID]bool{}
		for _, record := range records {
			if record.inode == 0 || seen[record.inode] {
				t.Fatal("ordinary readdir child identities are missing or aliased")
			}
			seen[record.inode] = true
			if fs.inodes[record.inode].refs != 0 {
				t.Fatal("ordinary readdir granted a lookup reference")
			}
		}
		for _, record := range records {
			id := lookup(t, fs, root, record.name)
			if id != record.inode {
				t.Fatal("ordinary readdir identity disagrees with lookup")
			}
			if err := fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: id, N: 1}); err != nil {
				t.Fatal(err)
			}
		}
		if fixtures[e.ID].hydration.calls.Load() != 0 {
			t.Fatal("directory entry hydrated a blob")
		}
		id := lookup(t, fs, root, "README.md")
		op := &fuseops.OpenFileOp{Inode: id}
		if err := fs.OpenFile(context.Background(), op); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, fs, id, op.Handle); !bytes.Equal(got, fixtures[e.ID].content) {
			t.Fatalf("wrong repository content: %x", got)
		}
		ids = append(ids, id)
		handles = append(handles, op.Handle)
	}
	if ids[0] == ids[1] || handles[0] == handles[1] {
		t.Fatal("repositories alias inode or handle namespaces")
	}
	if activations.Load() != 2 {
		t.Fatalf("activation count: %d", activations.Load())
	}
	for i, id := range ids {
		if err := fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: id, N: 1}); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, fs, id, handles[i]); !bytes.Equal(got, fixtures[testEntries[i].ID].content) {
			t.Fatal("forget invalidated open handle")
		}
	}
}

func TestRepositoryMutationsUseWritableArtifactFSOverlay(t *testing.T) {
	fixture := repositoryFixture(t, "alice/project")
	fs, err := New(testEntries, func(context.Context, Entry) (*fusefs.ArtifactFuse, error) { return fixture.backend, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	root := repoRoot(t, fs, "alice")
	create := &fuseops.CreateFileOp{Parent: root, Name: "local.txt", Mode: 0o644, OpenFlags: syscall.O_RDWR}
	if err := fs.CreateFile(ctx, create); err != nil {
		t.Fatal(err)
	}
	data := []byte{0, 1, 0xff, 'x'}
	legalName := &fuseops.CreateFileOp{Parent: root, Name: `a\b`, Mode: 0o644}
	if err := fs.CreateFile(ctx, legalName); err != nil {
		t.Fatalf("legal Unix filename: %v", err)
	}
	if got := lookup(t, fs, root, `a\b`); got != legalName.Entry.Child {
		t.Fatal("legal filename lookup failed")
	}
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: create.Entry.Child, Handle: create.Handle, Data: data}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, create.Entry.Child, create.Handle); !bytes.Equal(got, data) {
		t.Fatalf("written bytes: %x", got)
	}
	if err := fs.SyncFile(ctx, &fuseops.SyncFileOp{Inode: create.Entry.Child, Handle: create.Handle}); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rename(ctx, &fuseops.RenameOp{OldParent: root, NewParent: root, OldName: "local.txt", NewName: "renamed.txt"}); err != nil {
		t.Fatal(err)
	}
	if renamed := lookup(t, fs, root, "renamed.txt"); renamed != create.Entry.Child {
		t.Fatal("rename changed inode")
	}
	otherRoot := repoRoot(t, fs, "team")
	if err := fs.Rename(ctx, &fuseops.RenameOp{OldParent: root, NewParent: otherRoot, OldName: "renamed.txt", NewName: "copied.txt"}); err != syscall.EXDEV {
		t.Fatalf("cross-repo rename: %v", err)
	}
	if err := fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: root, Name: "renamed.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fs, create.Entry.Child, create.Handle); !bytes.Equal(got, data) {
		t.Fatal("unlinked open file lost its contents")
	}
	mkdir := &fuseops.MkDirOp{Parent: root, Name: "empty", Mode: 0o755}
	if err := fs.MkDir(ctx, mkdir); err != nil {
		t.Fatal(err)
	}
	if err := fs.RmDir(ctx, &fuseops.RmDirOp{Parent: root, Name: "empty"}); err != nil {
		t.Fatal(err)
	}
	link := &fuseops.CreateSymlinkOp{Parent: root, Name: "link", Target: "README.md"}
	if err := fs.CreateSymlink(ctx, link); err != nil {
		t.Fatal(err)
	}
	target := &fuseops.ReadSymlinkOp{Inode: link.Entry.Child}
	if err := fs.ReadSymlink(ctx, target); err != nil || target.Target != "README.md" {
		t.Fatalf("symlink: %s %v", target.Target, err)
	}
}

func TestActivationSerializesConcurrentEntryAndCanRetryFailure(t *testing.T) {
	fixture := repositoryFixture(t, "alice/project")
	var calls atomic.Int64
	gate := make(chan struct{})
	started := make(chan struct{})
	fs, err := New(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-gate
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t, fs, "alice")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() { errs <- fs.OpenDir(context.Background(), &fuseops.OpenDirOp{Inode: root}) })
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fs.OpenDir(ctx, &fuseops.OpenDirOp{Inode: root}); err != syscall.EINTR {
		t.Fatalf("cancelled waiter: %v", err)
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent activation count: %d", calls.Load())
	}
	var attempts atomic.Int64
	retry, err := New(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("temporary acquisition failure")
		}
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	retryRoot := repoRoot(t, retry, "alice")
	if err := retry.OpenDir(context.Background(), &fuseops.OpenDirOp{Inode: retryRoot}); err != syscall.EIO {
		t.Fatalf("first activation: %v", err)
	}
	_ = openDir(t, retry, retryRoot)
	if attempts.Load() != 2 {
		t.Fatal("activation error was permanently cached")
	}
}

func TestReadDirPlusTranslatesNamespacesAndDoesNotLeakFailedEntries(t *testing.T) {
	if runtime.GOOS == "darwin" {
		fs, err := New(testEntries, func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
			t.Fatal("READDIRPLUS activated repo on Darwin")
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := fs.ReadDirPlus(context.Background(), &fuseops.ReadDirPlusOp{}); err != syscall.ENOSYS {
			t.Fatalf("unsupported Darwin wire format: %v", err)
		}
		return
	}
	fixtures := map[string]fixture{}
	for _, e := range testEntries {
		fixtures[e.ID] = repositoryFixture(t, e.ID)
	}
	fs, err := New(testEntries, func(_ context.Context, e Entry) (*fusefs.ArtifactFuse, error) { return fixtures[e.ID].backend, nil })
	if err != nil {
		t.Fatal(err)
	}
	var allIDs []fuseops.InodeID
	for _, e := range testEntries {
		root := repoRoot(t, fs, e.Owner)
		h := openDir(t, fs, root)
		before := len(fs.childInodes)
		short := &fuseops.ReadDirPlusOp{ReadDirOp: fuseops.ReadDirOp{Inode: root, Handle: h, Dst: make([]byte, 1)}}
		if err := fs.ReadDirPlus(context.Background(), short); err != nil {
			t.Fatal(err)
		}
		if short.BytesRead != 0 || len(fs.childInodes) != before {
			t.Fatal("short buffer leaked translated lookup")
		}
		read := &fuseops.ReadDirPlusOp{ReadDirOp: fuseops.ReadDirOp{Inode: root, Handle: h, Dst: make([]byte, 4096)}}
		if err := fs.ReadDirPlus(context.Background(), read); err != nil {
			t.Fatal(err)
		}
		for _, record := range readRecords(t, read.Dst[:read.BytesRead], true) {
			id := lookup(t, fs, root, record.name)
			if id != record.inode {
				t.Fatalf("readdirplus/lookup mismatch %s: %d != %d", record.name, record.inode, id)
			}
			if err := fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: id, N: 2}); err != nil {
				t.Fatal(err)
			}
			if err := fs.GetInodeAttributes(context.Background(), &fuseops.GetInodeAttributesOp{Inode: id}); err != syscall.ESTALE {
				t.Fatalf("forgotten inode: %v", err)
			}
			allIDs = append(allIDs, id)
		}
	}
	seen := map[fuseops.InodeID]bool{}
	for _, id := range allIDs {
		if seen[id] {
			t.Fatal("directory entries alias across repositories")
		}
		seen[id] = true
	}
}

func TestDiscoveryRefreshIsAtomicAndPreservesOpenHandles(t *testing.T) {
	fixture := repositoryFixture(t, "alice/project")
	fs, err := New(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) { return fixture.backend, nil })
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t, fs, "alice")
	id := lookup(t, fs, root, "README.md")
	open := &fuseops.OpenFileOp{Inode: id}
	if err := fs.OpenFile(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetEntries([]Entry{{ID: "broken", Owner: "../oops", Name: "repo"}}); err == nil {
		t.Fatal("invalid refresh succeeded")
	}
	if still := repoRoot(t, fs, "alice"); still != root {
		t.Fatal("failed refresh replaced catalogue")
	}
	if err := fs.SetEntries(testEntries[1:]); err != nil {
		t.Fatal(err)
	}
	if err := fs.GetInodeAttributes(context.Background(), &fuseops.GetInodeAttributesOp{Inode: root}); err != syscall.ESTALE {
		t.Fatalf("removed placeholder: %v", err)
	}
	if got := readFile(t, fs, id, open.Handle); !bytes.Equal(got, fixture.content) {
		t.Fatal("discovery removed open content")
	}
	if err := fs.SetEntries(testEntries); err != nil {
		t.Fatal(err)
	}
	newRoot := repoRoot(t, fs, "alice")
	if newRoot == root {
		t.Fatal("readded path reused stale inode")
	}
}

func TestInvalidCataloguePathsAndMutationsAreRejected(t *testing.T) {
	noop := func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		t.Fatal("unexpected activation")
		return nil, nil
	}
	invalid := []Entry{{ID: "a", Owner: "..", Name: "repo"}, {ID: "a", Owner: "owner", Name: "/repo"}, {ID: "a", Owner: "owner", Name: "repo/child"}, {ID: "a", Owner: "owner", Name: "repo\\child"}, {ID: "a", Owner: "owner", Name: "repo\x00"}, {ID: "a", Owner: "owner", Name: "."}, {ID: "", Owner: "owner", Name: "repo"}}
	for _, e := range invalid {
		if _, err := New([]Entry{e}, noop); err == nil {
			t.Fatalf("accepted %+v", e)
		}
	}
	for _, entries := range [][]Entry{{testEntries[0], testEntries[0]}, {{ID: "1", Owner: "Org", Name: "repo"}, {ID: "2", Owner: "org", Name: "repo2"}}, {{ID: "1", Owner: "org", Name: "Repo"}, {ID: "2", Owner: "org", Name: "repo"}}} {
		if _, err := New(entries, noop); err == nil {
			t.Fatalf("accepted collision %+v", entries)
		}
	}
	fs, err := New(testEntries, noop)
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t, fs, "alice")
	for _, name := range []string{"..", ".", "../secret", "nested/file", "bad\x00name"} {
		if err := fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{Parent: root, Name: name}); err != syscall.EINVAL {
			t.Fatalf("invalid child %q: %v", name, err)
		}
		if err := fs.CreateFile(context.Background(), &fuseops.CreateFileOp{Parent: root, Name: name}); err != syscall.EINVAL {
			t.Fatalf("invalid create %q: %v", name, err)
		}
	}
	if err := fs.MkDir(context.Background(), &fuseops.MkDirOp{Parent: fuseops.RootInodeID, Name: "new"}); err != syscall.EROFS {
		t.Fatalf("catalogue mutation: %v", err)
	}
	if err := fs.OpenFile(context.Background(), &fuseops.OpenFileOp{Inode: root}); err != syscall.EISDIR {
		t.Fatalf("open repo directory as file: %v", err)
	}
	if !reflect.DeepEqual(fs.repositories["alice/project"].entry, testEntries[0]) {
		t.Fatal("entry unexpectedly mutated")
	}
}

func TestDiscoveryChurnReclaimsRetiredPlaceholders(t *testing.T) {
	fs, err := New(nil, func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		t.Fatal("metadata refresh activated a repository")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		entry := Entry{ID: fmt.Sprintf("repo-%d", i), Owner: fmt.Sprintf("owner-%d", i), Name: "project"}
		if err := fs.SetEntries([]Entry{entry}); err != nil {
			t.Fatal(err)
		}
		if len(fs.inodes) != 3 || len(fs.repositories) != i+1 {
			t.Fatalf("discovery leaked placeholder nodes or lost ID tombstones: inodes=%d repos=%d", len(fs.inodes), len(fs.repositories))
		}
	}
}

func TestRefreshPreservesActivationIdentityForAlreadyRoutedOperations(t *testing.T) {
	fixture := repositoryFixture(t, "alice/project")
	var calls atomic.Int64
	gate := make(chan struct{})
	started := make(chan struct{})
	fs, err := New(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-gate
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t, fs, "alice")
	// Suspend a request after routing but before starting acquisition.
	routed, err := fs.node(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.SetEntries(nil); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetEntries(testEntries[:1]); err != nil {
		t.Fatal(err)
	}
	newRoot := repoRoot(t, fs, "alice")
	current, err := fs.node(newRoot)
	if err != nil {
		t.Fatal(err)
	}
	if current.repo != routed.repo {
		t.Fatal("refresh changed identity for a routed acquisition")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Go(func() { _, err := fs.activateRepo(context.Background(), routed.repo); errs <- err })
	<-started
	wg.Go(func() { errs <- fs.OpenDir(context.Background(), &fuseops.OpenDirOp{Inode: newRoot}) })
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh duplicated acquisition: %d", calls.Load())
	}
}

func TestReadDirOnlyIdentitiesLiveUntilLastDirectoryHandleCloses(t *testing.T) {
	fixture := repositoryFixture(t, "alice/project")
	fs, err := New(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) { return fixture.backend, nil })
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t, fs, "alice")
	h1, h2 := openDir(t, fs, root), openDir(t, fs, root)
	read := func(h fuseops.HandleID) []directoryRecord {
		t.Helper()
		op := &fuseops.ReadDirOp{Inode: root, Handle: h, Dst: make([]byte, 4096)}
		if err := fs.ReadDir(context.Background(), op); err != nil {
			t.Fatal(err)
		}
		return readRecords(t, op.Dst[:op.BytesRead], false)
	}
	first, second := read(h1), read(h2)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("open directories disagree on child identities: %v %v", first, second)
	}
	readme := lookup(t, fs, root, "README.md")
	var git fuseops.InodeID
	for _, record := range first {
		if record.name == ".git" {
			git = record.inode
		}
	}
	gitNode, err := fs.node(git)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: h1}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.node(git); err != nil {
		t.Fatal("first directory release discarded another handle's identity")
	}
	if err := fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: h2}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.node(git); err != syscall.ESTALE {
		t.Fatalf("unlooked-up catalogue identity retained after closing directories: %v", err)
	}
	if err := fixture.backend.GetInodeAttributes(context.Background(), &fuseops.GetInodeAttributesOp{Inode: gitNode.local}); err != syscall.ESTALE {
		t.Fatalf("unlooked-up backend identity retained after closing directories: %v", err)
	}
	if err := fs.GetInodeAttributes(context.Background(), &fuseops.GetInodeAttributesOp{Inode: readme}); err != nil {
		t.Fatalf("directory release discarded looked-up identity: %v", err)
	}
	if err := fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: readme, N: 1}); err != nil {
		t.Fatal(err)
	}
	if len(fs.childInodes) != 0 {
		t.Fatal("finished directory/lookup lifecycle retained inode mappings")
	}
}
