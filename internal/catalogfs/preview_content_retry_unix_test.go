//go:build !windows

package catalogfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/jacobsa/fuse/fuseops"
)

type retryPromotionHydrator struct {
	paths   map[string]string
	calls   atomic.Int64
	failure error
}

var _ model.Hydrator = (*retryPromotionHydrator)(nil)

func (*retryPromotionHydrator) Enqueue(model.HydrationTask)        {}
func (*retryPromotionHydrator) EnqueueBatch([]model.HydrationTask) {}
func (*retryPromotionHydrator) QueueDepth(model.RepoID) int        { return 0 }

func (h *retryPromotionHydrator) EnsureHydrated(ctx context.Context, _ model.RepoConfig, node model.BaseNode) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if h.calls.Add(1) == 2 {
		return "", 0, h.failure
	}
	info, err := os.Stat(h.paths[node.Path])
	if err != nil {
		return "", 0, err
	}
	return h.paths[node.Path], info.Size(), nil
}

func (h *retryPromotionHydrator) OpenHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	path, size, err := h.EnsureHydrated(ctx, repo, node)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	return file, size, err
}

func (h *retryPromotionHydrator) ReadBlob(ctx context.Context, _ model.RepoConfig, node model.BaseNode, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(h.paths[node.Path])
	if err == nil && int64(len(data)) > maxBytes {
		return nil, syscall.EFBIG
	}
	return data, err
}

func TestReadOnlyPreviewRetriesPartialPromotionWithSameAdapter(t *testing.T) {
	for _, failure := range []error{syscall.EIO, context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			cfg := model.RepoConfig{ID: model.RepoID(testEntries[0].ID), Name: "project",
				GitDir: filepath.Join(root, "git"), OverlayDir: filepath.Join(root, "overlay"),
				BlobCacheDir: filepath.Join(root, "cache"), MetaDBPath: filepath.Join(root, "snapshot.db"), OverlayDBPath: filepath.Join(root, "overlay.db")}
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
			contents := map[string][]byte{"first.bin": {0, 255, 1, 2, 0}, "second.bin": {254, 0, 3, 4, 128, 5, 0}}
			hydrator := &retryPromotionHydrator{paths: map[string]string{}, failure: failure}
			nodes := []model.BaseNode{{RepoID: cfg.ID, Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"}}
			for _, name := range []string{"first.bin", "second.bin"} {
				path := filepath.Join(root, name)
				if err := os.WriteFile(path, contents[name], 0o600); err != nil {
					t.Fatal(err)
				}
				hydrator.paths[name] = path
				nodes = append(nodes, model.BaseNode{RepoID: cfg.ID, Path: name, Type: "file", Mode: 0o644,
					ObjectOID: name, SizeState: "known", SizeBytes: int64(len(contents[name]))})
			}
			gen, err := snap.PublishGeneration(ctx, "commit", "main", nodes)
			if err != nil {
				t.Fatal(err)
			}
			resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
			resolver.SetGeneration(gen)
			engine := &fusefs.Engine{Repo: cfg, Resolver: resolver, Overlay: ov, Hydrator: hydrator}
			var activations atomic.Int64
			fs, err := NewWithPreviewContent(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
				activations.Add(1)
				// The desktop creates a fresh adapter for every callback. An old
				// adapter's local inode numbers cannot be reused in a later one.
				return fusefs.NewArtifactFuse(cfg, resolver, engine), nil
			}, nil, func(context.Context, Entry, string) (PreviewDirectory, error) {
				return PreviewDirectory{Revision: "commit", Entries: nodes[1:]}, nil
			}, func(_ context.Context, _ Entry, path, _ string) (*os.File, error) {
				return os.Open(hydrator.paths[path])
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(fs.Destroy)
			repository := repoRoot(t, fs, "alice")
			inodes := map[string]fuseops.InodeID{}
			readers := map[string]fuseops.HandleID{}
			for _, name := range []string{"first.bin", "second.bin"} {
				inodes[name] = lookup(t, fs, repository, name)
				readers[name] = openReadPreview(t, fs, inodes[name])
				if got := readFile(t, fs, inodes[name], readers[name]); !bytes.Equal(got, contents[name]) {
					t.Fatalf("initial immutable reader %s changed bytes", name)
				}
			}
			writer := &fuseops.OpenFileOp{Inode: inodes["first.bin"], OpenFlags: syscall.O_RDWR}
			wantFailure := error(syscall.EIO)
			if errors.Is(failure, context.Canceled) {
				wantFailure = syscall.EINTR
			}
			if err := fs.OpenFile(ctx, writer); !errors.Is(err, wantFailure) {
				t.Fatalf("second reader promotion did not fail as injected: got %v want %v", err, wantFailure)
			}
			if hydrator.calls.Load() != 2 || activations.Load() != 1 {
				t.Fatal("failure did not occur after exactly one successful reader promotion")
			}
			if err := fs.OpenFile(ctx, writer); err != nil {
				t.Fatalf("promotion retry failed: %v", err)
			}
			if activations.Load() != 1 {
				t.Fatal("partial promotion retry acquired a different adapter")
			}
			changed := []byte{128, 0, 255, 0, 9}
			if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: inodes["first.bin"], Handle: writer.Handle, Data: changed}); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, fs, inodes["first.bin"], readers["first.bin"]); !bytes.Equal(got, changed) {
				t.Fatal("reader promoted before the failure missed the later write or aliased another path")
			}
			if got := readFile(t, fs, inodes["second.bin"], readers["second.bin"]); !bytes.Equal(got, contents["second.bin"]) {
				t.Fatal("promotion retry aliased the second reader to the written file")
			}
			for _, name := range []string{"first.bin", "second.bin"} {
				if err := fs.Unlink(ctx, &fuseops.UnlinkOp{Parent: repository, Name: name}); err != nil {
					t.Fatal(err)
				}
				want := contents[name]
				if name == "first.bin" {
					want = changed
				}
				if got := readFile(t, fs, inodes[name], readers[name]); !bytes.Equal(got, want) {
					t.Fatalf("retained reader %s lost bytes after unlink", name)
				}
				if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: readers[name]}); err != nil {
					t.Fatal(err)
				}
			}
			if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: writer.Handle}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadOnlyPreviewHandleCanChangeMetadataWithoutGainingWriteAccess(t *testing.T) {
	fs, fixture, activations, contentCalls := contentBrowsingFixture(t, nil)
	ctx := context.Background()
	file := lookup(t, fs, repoRoot(t, fs, "alice"), "README.md")
	reader := openReadPreview(t, fs, file)
	mode, modified := os.FileMode(0o640), time.Unix(1_700_000_000, 123_000_000)
	if err := fs.SetFileHandleAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: file, Handle: &reader, Mode: &mode, Mtime: &modified}); err != nil {
		t.Fatalf("valid metadata change through a read-only preview descriptor failed: %v", err)
	}
	attrs, err := fs.GetFileHandleAttributes(ctx, file, reader)
	if err != nil || attrs.Mode.Perm() != mode || !attrs.Mtime.Equal(modified) {
		t.Fatalf("read-only descriptor lost updated metadata after promotion: %+v %v", attrs, err)
	}
	if activations.Load() != 1 || contentCalls.Load() != 0 {
		t.Fatal("metadata mutation did not promote once directly to the writable backend")
	}
	size := uint64(0)
	if err := fs.SetFileHandleAttributes(ctx, &fuseops.SetInodeAttributesOp{Inode: file, Handle: &reader, Size: &size}); err != syscall.EBADF {
		t.Fatalf("read-only descriptor gained truncation access: %v", err)
	}
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: file, Handle: reader, Data: []byte("bad")}); err != syscall.EBADF {
		t.Fatalf("read-only descriptor gained data write access: %v", err)
	}
	if got := readFile(t, fs, file, reader); !bytes.Equal(got, fixture.content) {
		t.Fatal("metadata change or refused truncation changed binary file content")
	}
	if err := fs.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: reader}); err != nil {
		t.Fatal(err)
	}
}
