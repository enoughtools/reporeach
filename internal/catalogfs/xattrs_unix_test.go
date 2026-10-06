//go:build !windows

package catalogfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"golang.org/x/sys/unix"
)

func catalogueMetadataConfig(t *testing.T) model.RepoConfig {
	t.Helper()
	root := t.TempDir()
	return model.RepoConfig{ID: "catalogue", OverlayDir: filepath.Join(root, "overlay"), OverlayDBPath: filepath.Join(root, "metadata.db")}
}

func catalogueMetadataStore(t *testing.T, cfg model.RepoConfig) *overlay.Store {
	t.Helper()
	store, err := overlay.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func setCatalogueXattr(t *testing.T, fs *FileSystem, id fuseops.InodeID, name string, value []byte) {
	t.Helper()
	if err := fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: id, Name: name, Value: value}); err != nil {
		t.Fatalf("set xattr %s: %v", name, err)
	}
}

func catalogueXattr(t *testing.T, fs *FileSystem, id fuseops.InodeID, name string) []byte {
	t.Helper()
	query := &fuseops.GetXattrOp{Inode: id, Name: name}
	if err := fs.GetXattr(context.Background(), query); err != nil {
		t.Fatalf("xattr size %s: %v", name, err)
	}
	read := &fuseops.GetXattrOp{Inode: id, Name: name, Dst: make([]byte, query.BytesRead)}
	if err := fs.GetXattr(context.Background(), read); err != nil {
		t.Fatalf("xattr read %s: %v", name, err)
	}
	return read.Dst[:read.BytesRead]
}

func TestSyntheticXattrsRemainLocalAndPreserveBinaryValues(t *testing.T) {
	store := catalogueMetadataStore(t, catalogueMetadataConfig(t))
	var activations atomic.Int64
	fs, err := NewWithMetadata(testEntries, func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return nil, syscall.EIO
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := lookup(t, fs, fuseops.RootInodeID, "alice")
	repo := lookup(t, fs, owner, "project")
	value := []byte{0, 0xff, 1, 0, 0x80}
	for _, id := range []fuseops.InodeID{fuseops.RootInodeID, owner, repo} {
		if err := fs.GetXattr(ctx, &fuseops.GetXattrOp{Inode: id, Name: "user.binary"}); err != fuse.ENOATTR {
			t.Fatalf("missing attribute: %v", err)
		}
		setCatalogueXattr(t, fs, id, "user.binary", value)
		setCatalogueXattr(t, fs, id, "user.empty", nil)
		if got := catalogueXattr(t, fs, id, "user.binary"); !bytes.Equal(got, value) {
			t.Fatalf("binary value changed: %x", got)
		}
		if got := catalogueXattr(t, fs, id, "user.empty"); len(got) != 0 {
			t.Fatalf("empty attribute returned %x", got)
		}
		dst := []byte{0x66, 0x66}
		short := &fuseops.GetXattrOp{Inode: id, Name: "user.binary", Dst: dst}
		if err := fs.GetXattr(ctx, short); err != syscall.ERANGE || short.BytesRead != len(value) || !bytes.Equal(dst, []byte{0x66, 0x66}) {
			t.Fatalf("short get buffer: %d %x %v", short.BytesRead, dst, err)
		}
		query := &fuseops.ListXattrOp{Inode: id}
		if err := fs.ListXattr(ctx, query); err != nil || query.BytesRead != len("user.binary\x00user.empty\x00") {
			t.Fatalf("list size: %d %v", query.BytesRead, err)
		}
		list := &fuseops.ListXattrOp{Inode: id, Dst: make([]byte, query.BytesRead)}
		if err := fs.ListXattr(ctx, list); err != nil || !bytes.Equal(list.Dst, []byte("user.binary\x00user.empty\x00")) {
			t.Fatalf("list encoding: %q %v", list.Dst, err)
		}
		shortList := &fuseops.ListXattrOp{Inode: id, Dst: []byte{0x66}}
		if err := fs.ListXattr(ctx, shortList); err != syscall.ERANGE || shortList.BytesRead != query.BytesRead || shortList.Dst[0] != 0x66 {
			t.Fatalf("short list buffer: %+v %v", shortList, err)
		}
		if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.binary", Flags: unix.XATTR_CREATE}); err != syscall.EEXIST {
			t.Fatalf("create existing: %v", err)
		}
		if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.missing", Flags: unix.XATTR_REPLACE}); err != fuse.ENOATTR {
			t.Fatalf("replace missing: %v", err)
		}
		if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.binary", Value: nil, Flags: unix.XATTR_REPLACE}); err != nil {
			t.Fatal(err)
		}
		if got := catalogueXattr(t, fs, id, "user.binary"); len(got) != 0 {
			t.Fatal("replace did not preserve empty value")
		}
		if err := fs.RemoveXattr(ctx, &fuseops.RemoveXattrOp{Inode: id, Name: "user.empty"}); err != nil {
			t.Fatal(err)
		}
		if err := fs.RemoveXattr(ctx, &fuseops.RemoveXattrOp{Inode: id, Name: "user.empty"}); err != fuse.ENOATTR {
			t.Fatalf("remove missing: %v", err)
		}
		n, err := fs.node(id)
		if err != nil {
			t.Fatal(err)
		}
		object, found, err := store.MetadataObject(ctx, n.metadataID)
		if err != nil || !found {
			t.Fatalf("known object: %v %v", found, err)
		}
		attrs := &fuseops.GetInodeAttributesOp{Inode: id}
		if err := fs.GetInodeAttributes(ctx, attrs); err != nil || attrs.Attributes.Ctime.UnixNano() != object.CtimeUnixNs {
			t.Fatalf("persisted metadata ctime: %v %v", attrs.Attributes.Ctime, err)
		}
	}
	if activations.Load() != 0 {
		t.Fatalf("synthetic metadata activated %d repositories", activations.Load())
	}
}

func TestCatalogueXattrIdentitySurvivesRenameHideAndRestart(t *testing.T) {
	cfg := catalogueMetadataConfig(t)
	store := catalogueMetadataStore(t, cfg)
	activate := func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		t.Fatal("catalogue metadata activated a repository")
		return nil, nil
	}
	entry := Entry{ID: "opaque/../id", Owner: "alice", Name: "project"}
	fs, err := NewWithMetadata([]Entry{entry}, activate, store)
	if err != nil {
		t.Fatal(err)
	}
	old := repoRoot(t, fs, "alice")
	setCatalogueXattr(t, fs, fuseops.RootInodeID, "user.root", []byte("root"))
	setCatalogueXattr(t, fs, lookup(t, fs, fuseops.RootInodeID, "alice"), "user.owner", []byte("owner"))
	setCatalogueXattr(t, fs, old, "user.repo", []byte("repo"))
	entry.Owner, entry.Name = "team", "renamed"
	if err := fs.SetEntries([]Entry{entry}); err != nil {
		t.Fatal(err)
	}
	renamed := lookup(t, fs, lookup(t, fs, fuseops.RootInodeID, "team"), "renamed")
	if got := catalogueXattr(t, fs, renamed, "user.repo"); !bytes.Equal(got, []byte("repo")) {
		t.Fatal("rename lost stable repository metadata")
	}
	if err := fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: old, Name: "user.repo"}); err != syscall.ESTALE {
		t.Fatalf("retired placeholder remained usable: %v", err)
	}
	if err := fs.SetEntries(nil); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetEntries([]Entry{entry}); err != nil {
		t.Fatal(err)
	}
	shown := lookup(t, fs, lookup(t, fs, fuseops.RootInodeID, "team"), "renamed")
	if got := catalogueXattr(t, fs, shown, "user.repo"); !bytes.Equal(got, []byte("repo")) {
		t.Fatal("hide/show lost stable repository metadata")
	}
	replacement := entry
	replacement.ID = "opaque/id" // would alias the original if raw IDs were normalized
	if err := fs.SetEntries([]Entry{replacement}); err != nil {
		t.Fatal(err)
	}
	newID := lookup(t, fs, lookup(t, fs, fuseops.RootInodeID, "team"), "renamed")
	if err := fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: newID, Name: "user.repo"}); err != fuse.ENOATTR {
		t.Fatalf("replacement inherited prior repository metadata: %v", err)
	}
	if err := fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: shown, Name: "user.repo", Value: []byte("stale")}); err != syscall.ESTALE {
		t.Fatalf("stale placeholder mutated replacement: %v", err)
	}
	fs.Destroy()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = catalogueMetadataStore(t, cfg)
	entry.Owner, entry.Name = "alice", "project"
	restarted, err := NewWithMetadata([]Entry{entry}, activate, store)
	if err != nil {
		t.Fatal(err)
	}
	for _, read := range []struct {
		id         fuseops.InodeID
		name, want string
	}{
		{fuseops.RootInodeID, "user.root", "root"},
		{lookup(t, restarted, fuseops.RootInodeID, "alice"), "user.owner", "owner"},
		{repoRoot(t, restarted, "alice"), "user.repo", "repo"},
	} {
		if got := catalogueXattr(t, restarted, read.id, read.name); !bytes.Equal(got, []byte(read.want)) {
			t.Fatalf("restart lost %s metadata", read.name)
		}
	}
}

func TestCatalogueXattrValidationAndMetadataFreeCompatibility(t *testing.T) {
	fs, err := New(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		t.Fatal("metadata-free probes activated a repository")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	id := repoRoot(t, fs, "alice")
	ctx := context.Background()
	if err := fs.GetXattr(ctx, &fuseops.GetXattrOp{Inode: id, Name: "user.empty"}); err != fuse.ENOATTR {
		t.Fatalf("metadata-free missing getter: %v", err)
	}
	list := &fuseops.ListXattrOp{Inode: id, Dst: []byte{0x66}, BytesRead: 99}
	if err := fs.ListXattr(ctx, list); err != nil || list.BytesRead != 0 || list.Dst[0] != 0x66 {
		t.Fatalf("metadata-free list: %+v %v", list, err)
	}
	if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.empty"}); err != syscall.ENOTSUP {
		t.Fatalf("metadata-free set: %v", err)
	}
	if err := fs.RemoveXattr(ctx, &fuseops.RemoveXattrOp{Inode: id, Name: "user.empty"}); err != syscall.ENOTSUP {
		t.Fatalf("metadata-free remove: %v", err)
	}
	for _, tc := range []struct {
		name string
		want error
	}{
		{"", syscall.EINVAL}, {"bad\x00name", syscall.EINVAL}, {string([]byte{0xff}), syscall.EINVAL},
		{strings.Repeat("a", model.MaxXattrNameBytes+1), syscall.ENAMETOOLONG},
		{strings.Repeat("é", 64), syscall.ENAMETOOLONG},
	} {
		if err := fs.GetXattr(ctx, &fuseops.GetXattrOp{Inode: id, Name: tc.name}); err != tc.want {
			t.Fatalf("invalid getter name: %v, want %v", err, tc.want)
		}
		if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: tc.name}); err != tc.want {
			t.Fatalf("invalid setter name: %v, want %v", err, tc.want)
		}
		if err := fs.RemoveXattr(ctx, &fuseops.RemoveXattrOp{Inode: id, Name: tc.name}); err != tc.want {
			t.Fatalf("invalid removal name: %v, want %v", err, tc.want)
		}
	}
	invalidFlags := []uint32{unix.XATTR_CREATE | unix.XATTR_REPLACE, 1 << 31, ^uint32(0)}
	for _, flags := range invalidFlags {
		if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.value", Flags: flags}); err != syscall.EINVAL {
			t.Fatalf("unsupported flags %d: %v", flags, err)
		}
	}
	if err := fs.SetXattr(ctx, &fuseops.SetXattrOp{Inode: id, Name: "user.value", Value: make([]byte, model.MaxXattrValueBytes+1)}); err != syscall.E2BIG {
		t.Fatalf("oversized value: %v", err)
	}
}

func TestRepositoryBodyXattrsTranslateInodesAndRetainHandleIdentity(t *testing.T) {
	store := catalogueMetadataStore(t, catalogueMetadataConfig(t))
	fixtures := map[string]fixture{}
	for _, entry := range testEntries {
		fixtures[entry.ID] = repositoryFixture(t, entry.ID)
	}
	fs, err := NewWithMetadata(testEntries, func(_ context.Context, entry Entry) (*fusefs.ArtifactFuse, error) {
		return fixtures[entry.ID].backend, nil
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, entry := range testEntries {
		root := repoRoot(t, fs, entry.Owner)
		setCatalogueXattr(t, fs, root, "user.identity", []byte("catalogue-root"))
		id := lookup(t, fs, root, "README.md")
		value := append([]byte(entry.ID), 0, 0xff)
		setCatalogueXattr(t, fs, id, "user.identity", value)
		if got := catalogueXattr(t, fs, id, "user.identity"); !bytes.Equal(got, value) {
			t.Fatal("translated body metadata aliased another repository")
		}
		short := &fuseops.GetXattrOp{Inode: id, Name: "user.identity", Dst: []byte{0x66}}
		if err := fs.GetXattr(ctx, short); err != syscall.ERANGE || short.BytesRead != len(value) || short.Inode != id {
			t.Fatalf("forwarded short read did not preserve required size/global inode: %+v %v", short, err)
		}
		shortList := &fuseops.ListXattrOp{Inode: id, Dst: []byte{0x66}}
		if err := fs.ListXattr(ctx, shortList); err != syscall.ERANGE || shortList.BytesRead != len("user.identity\x00") || shortList.Inode != id {
			t.Fatalf("forwarded short list did not preserve required size/global inode: %+v %v", shortList, err)
		}
		if err := fixtures[entry.ID].backend.GetXattr(ctx, &fuseops.GetXattrOp{Inode: fuseops.RootInodeID, Name: "user.identity"}); err != fuse.ENOATTR {
			t.Fatalf("active repo root inherited catalogue-store attribute: %v", err)
		}
		if fixtures[entry.ID].hydration.calls.Load() != 0 {
			t.Fatal("metadata operations hydrated repository content")
		}
		open := &fuseops.OpenFileOp{Inode: id}
		if err := fs.OpenFile(ctx, open); err != nil {
			t.Fatal(err)
		}
		if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: id, N: 1}); err != nil {
			t.Fatal(err)
		}
		if err := fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: root, Name: "README.md"}); err != nil {
			t.Fatal(err)
		}
		replacement := &fuseops.CreateFileOp{Parent: root, Name: "README.md", Mode: 0o644}
		if err := fs.CreateFile(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		hydrationCalls := fixtures[entry.ID].hydration.calls.Load()
		setCatalogueXattr(t, fs, id, "user.retained", []byte("retained"))
		if got := catalogueXattr(t, fs, id, "user.identity"); !bytes.Equal(got, value) {
			t.Fatal("forgotten lookup lost retained handle metadata")
		}
		if err := fs.GetXattr(ctx, &fuseops.GetXattrOp{Inode: replacement.Entry.Child, Name: "user.retained"}); err != fuse.ENOATTR {
			t.Fatalf("retained old handle tagged its replacement: %v", err)
		}
		if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: replacement.Handle}); err != nil {
			t.Fatal(err)
		}
		if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: open.Handle}); err != nil {
			t.Fatal(err)
		}
		if err := fs.GetXattr(ctx, &fuseops.GetXattrOp{Inode: id, Name: "user.identity"}); err != syscall.ESTALE {
			t.Fatalf("closed forgotten object remained addressable: %v", err)
		}
		if fixtures[entry.ID].hydration.calls.Load() != hydrationCalls {
			t.Fatal("retained metadata operations hydrated repository content")
		}
	}
}

func TestRepositoryDirectoryXattrsRetainForgottenOpenIdentity(t *testing.T) {
	fixture := repositoryFixture(t, "alice/project")
	fs, err := New(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	root := repoRoot(t, fs, "alice")
	directory := &fuseops.MkDirOp{Parent: root, Name: "directory", Mode: 0o755}
	if err := fs.MkDir(ctx, directory); err != nil {
		t.Fatal(err)
	}
	id := directory.Entry.Child
	setCatalogueXattr(t, fs, id, "user.directory", []byte("directory"))
	h := openDir(t, fs, id)
	if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: id, N: 1}); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetEntries(nil); err != nil {
		t.Fatal(err)
	}
	setCatalogueXattr(t, fs, id, "user.retained", []byte("retained"))
	if got := catalogueXattr(t, fs, id, "user.directory"); !bytes.Equal(got, []byte("directory")) {
		t.Fatal("hide/forget lost retained directory identity")
	}
	if err := fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: h}); err != nil {
		t.Fatal(err)
	}
	if err := fs.GetXattr(ctx, &fuseops.GetXattrOp{Inode: id, Name: "user.directory"}); err != syscall.ESTALE {
		t.Fatalf("closed forgotten directory remained addressable: %v", err)
	}
}

type gatedMetadataStore struct {
	model.OverlayStore
	once    sync.Once
	path    string
	started chan struct{}
	release chan struct{}
}

func (s *gatedMetadataStore) BindMetadata(ctx context.Context, path, nodeType string) (model.MetadataObject, error) {
	if path == s.path {
		s.once.Do(func() {
			close(s.started)
			<-s.release
		})
	}
	return s.OverlayStore.BindMetadata(ctx, path, nodeType)
}

func TestInFlightSyntheticXattrCannotTagReplacement(t *testing.T) {
	store := catalogueMetadataStore(t, catalogueMetadataConfig(t))
	gated := &gatedMetadataStore{OverlayStore: store, started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(gated.release) }) }
	t.Cleanup(unblock)
	fs, err := NewWithMetadata(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		t.Fatal("synthetic metadata activated a repository")
		return nil, nil
	}, gated)
	if err != nil {
		t.Fatal(err)
	}
	oldID := repoRoot(t, fs, "alice")
	oldNode, err := fs.node(oldID)
	if err != nil {
		t.Fatal(err)
	}
	gated.path = oldNode.metadataPath
	written := make(chan error, 1)
	go func() {
		written <- fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: oldID, Name: "user.inflight", Value: []byte("old")})
	}()
	select {
	case <-gated.started:
	case <-time.After(3 * time.Second):
		t.Fatal("metadata request did not reach the store")
	}
	refreshed := make(chan error, 1)
	go func() { refreshed <- fs.SetEntries([]Entry{{ID: "replacement", Owner: "alice", Name: "project"}}) }()
	select {
	case err := <-refreshed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("metadata store access blocked catalogue refresh")
	}
	newID := repoRoot(t, fs, "alice")
	newNode, err := fs.node(newID)
	if err != nil || newNode.metadataID == "" || newNode.metadataID == oldNode.metadataID {
		t.Fatalf("replacement did not bind an independent identity: %+v %v", newNode, err)
	}
	replacementObjectID := newNode.metadataID
	unblock()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("metadata write did not finish")
	}
	newNode, err = fs.node(newID)
	if err != nil || newNode.metadataID != replacementObjectID {
		t.Fatalf("old operation cached an ID on its replacement: %+v %v", newNode, err)
	}
	if err := fs.GetXattr(context.Background(), &fuseops.GetXattrOp{Inode: newID, Name: "user.inflight"}); err != fuse.ENOATTR {
		t.Fatalf("in-flight old operation tagged replacement: %v", err)
	}
	object, err := store.BindMetadata(context.Background(), oldNode.metadataPath, "dir")
	if err != nil {
		t.Fatal(err)
	}
	value, found, err := store.GetMetadataXattr(context.Background(), object.ID, "user.inflight")
	if err != nil || !found || !bytes.Equal(value, []byte("old")) {
		t.Fatalf("in-flight operation lost old identity: %x %v %v", value, found, err)
	}
}

func TestMissingSyntheticXattrProbePreservesCtime(t *testing.T) {
	store := catalogueMetadataStore(t, catalogueMetadataConfig(t))
	fs, err := NewWithMetadata(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		t.Fatal("synthetic attribute lookup activated a repository")
		return nil, nil
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "alice"}
	if err := fs.LookUpInode(ctx, owner); err != nil || !owner.Entry.Attributes.Ctime.Equal(fs.created) {
		t.Fatalf("new owner lookup ctime: %v %v", owner.Entry.Attributes.Ctime, err)
	}
	repo := &fuseops.LookUpInodeOp{Parent: owner.Entry.Child, Name: "project"}
	if err := fs.LookUpInode(ctx, repo); err != nil || !repo.Entry.Attributes.Ctime.Equal(fs.created) {
		t.Fatalf("new repo lookup ctime: %v %v", repo.Entry.Attributes.Ctime, err)
	}
	for _, id := range []fuseops.InodeID{fuseops.RootInodeID, owner.Entry.Child, repo.Entry.Child} {
		before := &fuseops.GetInodeAttributesOp{Inode: id}
		if err := fs.GetInodeAttributes(ctx, before); err != nil || !before.Attributes.Ctime.Equal(fs.created) {
			t.Fatalf("initial synthetic ctime: %v %v", before.Attributes.Ctime, err)
		}
		if err := fs.GetXattr(ctx, &fuseops.GetXattrOp{Inode: id, Name: "user.missing"}); err != fuse.ENOATTR {
			t.Fatalf("missing probe: %v", err)
		}
		if err := fs.ListXattr(ctx, &fuseops.ListXattrOp{Inode: id}); err != nil {
			t.Fatal(err)
		}
		after := &fuseops.GetInodeAttributesOp{Inode: id}
		if err := fs.GetInodeAttributes(ctx, after); err != nil || !after.Attributes.Ctime.Equal(before.Attributes.Ctime) {
			t.Fatalf("missing probe changed ctime: %v to %v, %v", before.Attributes.Ctime, after.Attributes.Ctime, err)
		}
	}
	if runtime.GOOS != "darwin" {
		for _, id := range []fuseops.InodeID{fuseops.RootInodeID, owner.Entry.Child} {
			assertSyntheticDirPlusCtime(t, fs, id, fs.created.UnixNano())
		}
	}
}

func TestSyntheticMutationCtimeSurvivesRestartBeforeMetadataProbe(t *testing.T) {
	cfg := catalogueMetadataConfig(t)
	store := catalogueMetadataStore(t, cfg)
	activate := func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		t.Fatal("synthetic attribute lookup activated a repository")
		return nil, nil
	}
	fs, err := NewWithMetadata(testEntries[:1], activate, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ownerID := lookup(t, fs, fuseops.RootInodeID, "alice")
	repoID := lookup(t, fs, ownerID, "project")
	var persisted [3]int64
	for i, id := range []fuseops.InodeID{fuseops.RootInodeID, ownerID, repoID} {
		setCatalogueXattr(t, fs, id, "user.persisted", []byte("value"))
		n, err := fs.node(id)
		if err != nil {
			t.Fatal(err)
		}
		object, found, err := store.MetadataObject(ctx, n.metadataID)
		if err != nil || !found || object.CtimeUnixNs == 0 {
			t.Fatalf("mutation did not persist ctime: %+v %v %v", object, found, err)
		}
		persisted[i] = object.CtimeUnixNs
	}
	fs.Destroy()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = catalogueMetadataStore(t, cfg)
	restarted, err := NewWithMetadata(testEntries[:1], activate, store)
	if err != nil {
		t.Fatal(err)
	}
	// Make the new mount's fallback deliberately later. Persistent mutation
	// times must remain exact rather than being clamped to a new session time.
	restarted.created = time.Unix(0, persisted[2]+int64(time.Second))
	root := &fuseops.GetInodeAttributesOp{Inode: fuseops.RootInodeID}
	if err := restarted.GetInodeAttributes(ctx, root); err != nil || root.Attributes.Ctime.UnixNano() != persisted[0] {
		t.Fatalf("first root getattr lost prior mutation ctime: %v %v", root.Attributes.Ctime, err)
	}
	owner := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "alice"}
	if err := restarted.LookUpInode(ctx, owner); err != nil || owner.Entry.Attributes.Ctime.UnixNano() != persisted[1] {
		t.Fatalf("first owner lookup lost prior mutation ctime: %v %v", owner.Entry.Attributes.Ctime, err)
	}
	repo := &fuseops.LookUpInodeOp{Parent: owner.Entry.Child, Name: "project"}
	if err := restarted.LookUpInode(ctx, repo); err != nil || repo.Entry.Attributes.Ctime.UnixNano() != persisted[2] {
		t.Fatalf("first repo lookup lost prior mutation ctime: %v %v", repo.Entry.Attributes.Ctime, err)
	}
	for i, id := range []fuseops.InodeID{fuseops.RootInodeID, owner.Entry.Child, repo.Entry.Child} {
		attrs := &fuseops.GetInodeAttributesOp{Inode: id}
		if err := restarted.GetInodeAttributes(ctx, attrs); err != nil || attrs.Attributes.Ctime.UnixNano() != persisted[i] {
			t.Fatalf("restarted getattr lost prior mutation ctime: %v %v", attrs.Attributes.Ctime, err)
		}
	}
	if runtime.GOOS != "darwin" {
		assertSyntheticDirPlusCtime(t, restarted, fuseops.RootInodeID, persisted[1])
		assertSyntheticDirPlusCtime(t, restarted, owner.Entry.Child, persisted[2])
	}
}

func assertSyntheticDirPlusCtime(t *testing.T, fs *FileSystem, id fuseops.InodeID, want int64) {
	t.Helper()
	handle := openDir(t, fs, id)
	t.Cleanup(func() { _ = fs.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: handle}) })
	op := &fuseops.ReadDirPlusOp{ReadDirOp: fuseops.ReadDirOp{Inode: id, Handle: handle, Dst: make([]byte, 4096)}}
	if err := fs.ReadDirPlus(context.Background(), op); err != nil || op.BytesRead < 152 {
		t.Fatalf("synthetic readdirplus: %d %v", op.BytesRead, err)
	}
	// Linux FUSE entry_out has a 40-byte prefix followed by fuse_attr. Ctime
	// sits at attribute offset 40, and its nanoseconds at attribute offset 56.
	seconds := binary.NativeEndian.Uint64(op.Dst[80:88])
	nanoseconds := binary.NativeEndian.Uint32(op.Dst[96:100])
	if got := int64(seconds)*int64(time.Second) + int64(nanoseconds); got != want {
		t.Fatalf("synthetic readdirplus ctime: %d, want %d", got, want)
	}
}
