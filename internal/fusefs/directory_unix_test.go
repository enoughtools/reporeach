//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
)

type directoryMetadataFailure struct {
	*fakeOverlay
	path string
}

func (s *directoryMetadataFailure) BindMetadata(ctx context.Context, path, typ string) (model.MetadataObject, error) {
	if path == s.path {
		return model.MetadataObject{}, errors.New("directory metadata unavailable")
	}
	return s.fakeOverlay.BindMetadata(ctx, path, typ)
}

func typedDirectoryFixture(t *testing.T) (*ArtifactFuse, *directoryMetadataFailure, fuseops.HandleID, *fakeLookupHydrator) {
	t.Helper()
	repo := model.RepoConfig{ID: "repo", GitDir: "/unused/repository.git"}
	nodes := []model.BaseNode{
		{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "a", SizeState: "unknown"},
		{Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: "b", SizeState: "unknown"},
	}
	snapshot := &fakeSnapshot{nodes: map[string]model.BaseNode{".": {Path: ".", Type: "dir"}}, kids: map[string][]model.BaseNode{".": nodes}}
	for _, node := range nodes {
		snapshot.nodes[node.Path] = node
	}
	metadata := &directoryMetadataFailure{fakeOverlay: &fakeOverlay{entries: map[string]model.OverlayEntry{}}}
	resolver := &Resolver{Snapshot: snapshot, Overlay: metadata}
	resolver.SetGeneration(1)
	hydrator := &fakeLookupHydrator{size: 11}
	fs := NewArtifactFuse(repo, resolver, &Engine{Resolver: resolver, Repo: repo, Overlay: metadata, Hydrator: hydrator})
	op := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := fs.OpenDir(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	return fs, metadata, op.Handle, hydrator
}

func TestTypedDirectoryPageCookiesAndReferences(t *testing.T) {
	fs, _, handle, hydrator := typedDirectoryFixture(t)
	ctx := context.Background()
	var offset fuseops.DirOffset
	var names []string
	var ids []fuseops.InodeID
	for {
		page, err := fs.ReadDirectoryEntries(ctx, handle, offset, 32)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) != 1 || page[0].Offset != offset+1 {
			t.Fatalf("cookie=%d page=%+v", offset, page)
		}
		entry := page[0]
		names = append(names, entry.Name)
		ids = append(ids, entry.Entry.Child)
		if entry.SizeKnown != (entry.Name == ".git") {
			t.Fatalf("size-known %q = %t", entry.Name, entry.SizeKnown)
		}
		ref := fs.inodes[entry.Entry.Child]
		if ref.Refcnt != 1 || ref.DirRefs != 1 {
			t.Fatalf("references before forget = %+v", ref)
		}
		if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: ref.ID, N: 1}); err != nil {
			t.Fatal(err)
		}
		if fs.inodes[ref.ID] == nil || ref.Refcnt != 0 || ref.DirRefs != 1 {
			t.Fatal("directory handle did not retain forgotten entry")
		}
		// Replaying a cookie returns the same ID, with exactly one new lookup.
		replay, err := fs.ReadDirectoryEntries(ctx, handle, offset, 32)
		if err != nil || len(replay) != 1 || replay[0].Entry.Child != ref.ID || ref.Refcnt != 1 {
			t.Fatalf("cookie replay = %+v, refs=%+v err=%v", replay, ref, err)
		}
		fs.dropInodeLookup(ref.ID)
		offset = entry.Offset
	}
	if len(names) != 3 || names[0] != ".git" || names[1] != "a.txt" || names[2] != "b.txt" || hydrator.calls != 0 {
		t.Fatalf("names=%v hydrations=%d", names, hydrator.calls)
	}
	if err := fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: handle}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if fs.inodes[id] != nil {
			t.Fatalf("inode %d survived directory and lookup release", id)
		}
	}
}

func TestTypedDirectoryFailedPageDropsOnlyAcquiredLookups(t *testing.T) {
	fs, metadata, handle, hydrator := typedDirectoryFixture(t)
	metadata.path = "b.txt"
	page, err := fs.ReadDirectoryEntries(context.Background(), handle, 0, 4096)
	if err == nil || page != nil {
		t.Fatalf("failed page = %+v err=%v", page, err)
	}
	for _, name := range []string{".git", "a.txt"} {
		ref := fs.inodes[fs.pathToInode[name]]
		if ref == nil || ref.Refcnt != 0 || ref.DirRefs != 1 {
			t.Fatalf("failed-page reference %q = %+v", name, ref)
		}
	}
	metadata.path = ""
	page, err = fs.ReadDirectoryEntries(context.Background(), handle, 0, 4096)
	if err != nil || len(page) != 3 || hydrator.calls != 0 {
		t.Fatalf("retry page=%+v error=%v hydrations=%d", page, err, hydrator.calls)
	}
	for _, entry := range page {
		if ref := fs.inodes[entry.Entry.Child]; ref.Refcnt != 1 || ref.DirRefs != 1 {
			t.Fatalf("retry references = %+v", ref)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fs.ReadDirectoryEntries(ctx, handle, 0, 4096); err != context.Canceled {
		t.Fatalf("canceled enumeration error = %v", err)
	}
	if _, err := fs.ReadDirectoryEntries(context.Background(), handle, 0, 31); err != syscall.EINVAL {
		t.Fatalf("undersized page error = %v", err)
	}
}
