//go:build !windows

package catalogfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/jacobsa/fuse/fuseops"
)

func TestActivateRepositoryForExportBindsColdPreviewBeforeInventory(t *testing.T) {
	ctx := context.Background()
	f := repositoryFixtureWithNodes(t, "alice/project", []model.BaseNode{
		{Path: "unvisited", Type: "dir", Mode: 0o755, SizeState: "known"},
		{Path: "unvisited/nested.bin", Type: "file", Mode: 0o644, ObjectOID: "nested", SizeState: "known", SizeBytes: 4},
	})
	var activations, nestedPreviews atomic.Int64
	fs, err := NewWithPreviewContent(testEntries[:1], func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return f.backend, nil
	}, nil, func(_ context.Context, _ Entry, path string) (PreviewDirectory, error) {
		if path != "." {
			nestedPreviews.Add(1)
			// Keep changes service state before staging. The immutable preview
			// callback is unavailable then; a lazy traversal would activate here.
			return PreviewDirectory{}, ErrPreviewUnavailable
		}
		return PreviewDirectory{Revision: "commit", GitFileSize: 42, Entries: []model.BaseNode{
			{Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "blob", SizeState: "known", SizeBytes: int64(len(f.content))},
			{Path: "unvisited", Type: "dir", Mode: 0o040000, SizeState: "known"},
		}}, nil
	}, func(context.Context, Entry, string, string) (*os.File, error) {
		return os.Open(f.hydration.path)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fs.Destroy)
	fs.created = time.Unix(1_900_000_000, 0)
	root := repoRoot(t, fs, "alice")
	readme := lookup(t, fs, root, "README.md")
	reader := openReadPreview(t, fs, readme)
	if got := readFile(t, fs, readme, reader); !bytes.Equal(got, f.content) {
		t.Fatal("initial immutable preview bytes differ")
	}
	cold := &fuseops.GetInodeAttributesOp{Inode: readme}
	if known, err := fs.GetMetadataAttributes(ctx, cold); err != nil || !known {
		t.Fatalf("cold attributes: %v", err)
	}
	if activations.Load() != 0 || nestedPreviews.Load() != 0 {
		t.Fatal("cold browsing activated the runtime or visited the nested directory")
	}

	// A prepared engine may already contain native Git/index and dirty overlay
	// state. Export binding must retain them rather than rebuilding a checkout.
	if err := os.MkdirAll(f.config.GitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitFiles := map[string][]byte{"HEAD": []byte("ref: refs/heads/local-branch\n"), "index": {0, 255, 127, 0, 1}}
	for name, data := range gitFiles {
		if err := os.WriteFile(filepath.Join(f.config.GitDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ov, err := overlay.New(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	defer ov.Close()
	dirty := []byte{0xff, 0, 1, 128, 'd', 'i', 'r', 't', 'y'}
	if _, err := ov.CreateFile(ctx, "README.md", 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ov.WriteFile(ctx, "README.md", 0, dirty); err != nil {
		t.Fatal(err)
	}
	if err := ov.SetMtime(ctx, "README.md", time.Unix(1_700_000_000, 123)); err != nil {
		t.Fatal(err)
	}
	beforeOverlay, err := ov.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	release, err := fs.FreezeRepositoryWrites(testEntries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := fs.ActivateRepositoryForExport(ctx, testEntries[0].ID); err != nil {
		t.Fatal(err)
	}
	first := exportAttributeInventory(t, fs, root)
	second := exportAttributeInventory(t, fs, root)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("unvisited nested directory changed attributes between export inventories")
	}
	if first["README.md"].Mtime.Equal(cold.Attributes.Mtime) || first["README.md"].Size != uint64(len(dirty)) || first["README.md"].Mode.Perm() != 0o640 {
		t.Fatalf("export retained stale preview attributes: %+v", first["README.md"])
	}
	if _, ok := first["unvisited/nested.bin"]; !ok {
		t.Fatal("export inventory omitted the unvisited nested file")
	}
	if activations.Load() != 1 || nestedPreviews.Load() != 0 {
		t.Fatal("export inventory lazily activated or acquired a nested immutable preview")
	}
	if lookup(t, fs, root, "README.md") != readme {
		t.Fatal("export binding replaced the retained preview inode")
	}
	if got := readFile(t, fs, readme, reader); !bytes.Equal(got, dirty) {
		t.Fatal("retained reader did not join the existing dirty working tree")
	}
	if err := fs.WriteFile(ctx, &fuseops.WriteFileOp{Inode: readme, Handle: reader, Data: []byte("write")}); err != syscall.EBADF {
		t.Fatalf("export binding made a retained reader writable: %v", err)
	}
	if err := fs.OpenFile(ctx, &fuseops.OpenFileOp{Inode: readme, OpenFlags: syscall.O_RDWR}); err != syscall.EBUSY {
		t.Fatalf("export binding admitted a writable open through its frozen repository: %v", err)
	}
	for name, before := range gitFiles {
		after, err := os.ReadFile(filepath.Join(f.config.GitDir, name))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("binding changed native %s: %v", name, err)
		}
	}
	afterOverlay, err := ov.ListAll(ctx)
	if err != nil || !reflect.DeepEqual(beforeOverlay, afterOverlay) {
		t.Fatalf("binding changed dirty overlay state: %v", err)
	}
	if err := fs.ActivateRepositoryForExport(ctx, testEntries[0].ID); err != nil || activations.Load() != 1 {
		t.Fatalf("repeated export binding rebuilt the backend: %v", err)
	}
}

func exportAttributeInventory(t *testing.T, fs *FileSystem, root fuseops.InodeID) map[string]fuseops.InodeAttributes {
	t.Helper()
	ctx := context.Background()
	result := make(map[string]fuseops.InodeAttributes)
	var visit func(fuseops.InodeID, string)
	visit = func(id fuseops.InodeID, path string) {
		op := &fuseops.GetInodeAttributesOp{Inode: id}
		if known, err := fs.GetMetadataAttributes(ctx, op); err != nil || !known {
			t.Fatalf("inventory attributes for %q: %v", path, err)
		}
		result[path] = op.Attributes
		if !op.Attributes.Mode.IsDir() {
			return
		}
		h := openDir(t, fs, id)
		defer fs.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: h})
		entries, err := fs.ReadDirectoryEntries(ctx, h, 0, 64<<10)
		if err != nil {
			t.Fatalf("inventory directory %q: %v", path, err)
		}
		for _, entry := range entries {
			if path == "." && entry.Name == ".git" {
				continue
			}
			visit(entry.Entry.Child, filepath.Join(path, entry.Name))
		}
	}
	visit(root, ".")
	return result
}

func TestActivateRepositoryForExportRequiresLiveFrozenRepository(t *testing.T) {
	fs, _, activations, _ := contentBrowsingFixture(t, nil)
	ctx := context.Background()
	if err := fs.ActivateRepositoryForExport(ctx, testEntries[0].ID); err != syscall.EBUSY || activations.Load() != 0 {
		t.Fatalf("unfrozen activation: %v", err)
	}
	if err := fs.ActivateRepositoryForExport(ctx, "missing"); err != syscall.ENOENT {
		t.Fatalf("missing repository: %v", err)
	}
	release, err := fs.FreezeRepositoryWrites(testEntries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := fs.ActivateRepositoryForExport(canceled, testEntries[0].ID); !errors.Is(err, context.Canceled) || activations.Load() != 0 {
		t.Fatalf("canceled activation: %v", err)
	}
	fs.Destroy()
	if err := fs.ActivateRepositoryForExport(ctx, testEntries[0].ID); err != syscall.ESTALE || activations.Load() != 0 {
		t.Fatalf("destroyed repository: %v", err)
	}
}
