//go:build !windows

package catalogfs

import (
	"context"
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
)

func TestGitTreePreviewDirectoriesRemainAccessibleAcrossPromotion(t *testing.T) {
	for _, gitMode := range []uint32{0o040000, 0o160000, 0o040755} {
		t.Run(fmt.Sprintf("git_mode_%o", gitMode), func(t *testing.T) {
			ctx := context.Background()
			// GitHub returns raw Git modes for trees and submodules. Persisted
			// preview receipts retain these modes, so the presentation boundary
			// must handle them without acquiring a new snapshot.
			children := []model.BaseNode{
				{Path: "laser galvo", Type: "dir", Mode: gitMode, SizeState: "known"},
				{Path: "laser galvo/control", Type: "dir", Mode: 0o040000, SizeState: "known"},
				{Path: "laser galvo/control/README.md", Type: "file", Mode: 0o100644, SizeState: "known", SizeBytes: 4},
			}
			f := repositoryFixtureWithNodes(t, "alice/project", children)
			var activations atomic.Int64
			fs, err := NewWithPreview(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
				activations.Add(1)
				return f.backend, nil
			}, nil, func(_ context.Context, _ Entry, path string) (PreviewDirectory, error) {
				directory := PreviewDirectory{Revision: "commit"}
				if path == "." {
					directory.GitFileSize = uint64(len("gitdir: " + f.config.GitDir + "\n"))
					directory.Entries = append([]model.BaseNode{
						{Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "blob", SizeState: "known", SizeBytes: int64(len(f.content))},
					}, children[0])
				} else if path == "laser galvo" {
					directory.Entries = children[1:2]
				} else if path == "laser galvo/control" {
					directory.Entries = children[2:]
				}
				return directory, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(fs.Destroy)
			root := repoRoot(t, fs, "alice")
			lookupDirectory := &fuseops.LookUpInodeOp{Parent: root, Name: "laser galvo"}
			if known, err := fs.LookUpMetadata(ctx, lookupDirectory); err != nil || !known || !lookupDirectory.Entry.Attributes.Mode.IsDir() || lookupDirectory.Entry.Attributes.Mode.Perm() != 0o755 {
				t.Fatalf("Finder directory lookup has inaccessible permissions: known=%t mode=%v err=%v", known, lookupDirectory.Entry.Attributes.Mode, err)
			}
			directoryID := lookupDirectory.Entry.Child
			stat := &fuseops.GetInodeAttributesOp{Inode: directoryID}
			if known, err := fs.GetMetadataAttributes(ctx, stat); err != nil || !known || stat.Attributes.Mode.Perm() != 0o755 {
				t.Fatalf("preview directory stat mode=%v err=%v", stat.Attributes.Mode, err)
			}
			rootHandle := openDir(t, fs, root)
			listing, err := fs.ReadDirectoryEntries(ctx, rootHandle, 0, 4096)
			if err != nil || len(listing) != 3 || listing[2].Entry.Child != directoryID || listing[2].Entry.Attributes.Mode.Perm() != 0o755 {
				t.Fatalf("preview directory enumeration lost accessible identity: %+v err=%v", listing, err)
			}
			previewHandle := openDir(t, fs, directoryID)
			before, err := fs.ReadDirectoryEntries(ctx, previewHandle, 0, 4096)
			if err != nil || len(before) != 1 || before[0].Name != "control" || before[0].Entry.Attributes.Mode.Perm() != 0o755 {
				t.Fatalf("nested preview directory inaccessible: %+v err=%v", before, err)
			}
			if activations.Load() != 0 || f.hydration.calls.Load() != 0 {
				t.Fatal("checking directory access activated or hydrated repository")
			}
			git := lookup(t, fs, root, ".git")
			opened := &fuseops.OpenFileOp{Inode: git, OpenFlags: syscall.O_RDONLY}
			if err := fs.OpenFile(ctx, opened); err != nil || activations.Load() != 1 {
				t.Fatalf("runtime promotion: %v", err)
			}
			if known, err := fs.LookUpMetadata(ctx, lookupDirectory); err != nil || !known || lookupDirectory.Entry.Child != directoryID || lookupDirectory.Entry.Attributes.Mode.Perm() != 0o755 {
				t.Fatalf("promotion changed directory access or identity: %+v err=%v", lookupDirectory.Entry, err)
			}
			actualHandle := openDir(t, fs, directoryID)
			after, err := fs.ReadDirectoryEntries(ctx, actualHandle, 0, 4096)
			if err != nil || len(after) != 1 || after[0].Name != "control" || after[0].Entry.Child != before[0].Entry.Child || after[0].Entry.Attributes.Mode.Perm() != 0o755 {
				t.Fatalf("promoted nested directory access or identity changed: %+v err=%v", after, err)
			}
			if f.hydration.calls.Load() != 0 {
				t.Fatal("opening promoted directories hydrated a blob")
			}
		})
	}
}
