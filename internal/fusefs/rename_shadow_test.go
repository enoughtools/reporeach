package fusefs

import (
	"bytes"
	"context"
	"errors"
	iofs "io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
)

func TestRenameRecreatedSourcePreservesBaseDeletion(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		for _, sourceInBase := range []bool{false, true} {
			for _, destinationInBase := range []bool{false, true} {
				name := "file"
				if symlink {
					name = "symlink"
				}
				if sourceInBase {
					name += "/tracked-source"
				} else {
					name += "/new-source"
				}
				if destinationInBase {
					name += "/tracked-destination"
				} else {
					name += "/new-destination"
				}
				t.Run(name, func(t *testing.T) {
					ctx := context.Background()
					base := map[string]model.BaseNode{}
					if sourceInBase {
						base["source"] = model.BaseNode{Path: "source", Type: "file", Mode: 0o100644, ObjectOID: "source-object"}
					}
					if destinationInBase {
						base["destination"] = model.BaseNode{Path: "destination", Type: "file", Mode: 0o100755, ObjectOID: "destination-object"}
					}
					engine, ov := renameTestEngine(t, base)
					if sourceInBase {
						if err := engine.Unlink(ctx, "source"); err != nil {
							t.Fatal(err)
						}
					}
					payload := []byte{0, 0xff, 'n', 'e', 'w', 0, '\n'}
					const target = "../new-target"
					if symlink {
						if err := engine.Symlink(ctx, "source", target); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := engine.Create(ctx, "source", 0o640); err != nil {
							t.Fatal(err)
						}
						if _, err := engine.Write(ctx, "source", 0, payload); err != nil {
							t.Fatal(err)
						}
					}
					before, _, err := ov.Lookup(ctx, "source")
					if err != nil {
						t.Fatal(err)
					}
					if err := engine.Rename(ctx, "source", "destination"); err != nil {
						t.Fatal(err)
					}
					if _, err := engine.Resolver.ResolvePath("source"); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("source reappeared after rename: %v", err)
					}
					deleted, exists, err := ov.Lookup(ctx, "source")
					if err != nil {
						t.Fatal(err)
					}
					if sourceInBase {
						if !exists || !deleted.IsDeleted() || deleted.TargetPath != "" {
							t.Fatalf("source must retain a plain deletion: %+v, exists=%v", deleted, exists)
						}
					} else if exists {
						t.Fatalf("new source left an unnecessary whiteout: %+v", deleted)
					}
					moved, exists, err := ov.Lookup(ctx, "destination")
					if err != nil || !exists {
						t.Fatalf("destination lookup: %+v, exists=%v, err=%v", moved, exists, err)
					}
					if moved.Mode != before.Mode || moved.SizeBytes != before.SizeBytes || moved.MtimeUnixNs != before.MtimeUnixNs {
						t.Fatalf("rename changed content attributes: before=%+v, after=%+v", before, moved)
					}
					if destinationInBase {
						if moved.SourceOID != "destination-object" || moved.SourceMode != 0o100755 {
							t.Fatalf("destination provenance lost: %+v", moved)
						}
					} else if moved.SourceOID != "" || moved.SourceMode != 0 {
						t.Fatalf("new destination gained unrelated provenance: %+v", moved)
					}
					if symlink {
						if moved.Kind != model.OverlayKindSymlink || moved.TargetPath != target {
							t.Fatalf("symlink target lost: %+v", moved)
						}
					} else {
						got, err := engine.Read(ctx, "destination", 0, len(payload)+1)
						if err != nil || !bytes.Equal(got, payload) {
							t.Fatalf("binary contents changed: got=%x, err=%v", got, err)
						}
						if moved.BackingPath != before.BackingPath {
							t.Fatalf("rename changed backing identity: before=%q, after=%q", before.BackingPath, moved.BackingPath)
						}
					}
				})
			}
		}
	}
}

func TestRenameUsesMergedDestinationType(t *testing.T) {
	ctx := context.Background()
	engine, _ := renameTestEngine(t, map[string]model.BaseNode{
		"destination": {Path: "destination", Type: "dir", Mode: 0o40000},
	})
	if err := engine.Rmdir(ctx, "destination"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Create(ctx, "destination", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := engine.Create(ctx, "source", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Write(ctx, "source", 0, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if err := engine.Rename(ctx, "source", "destination"); err != nil {
		t.Fatalf("rename onto a file that replaced a base directory: %v", err)
	}
	got, err := engine.Read(ctx, "destination", 0, 32)
	if err != nil || !bytes.Equal(got, []byte("replacement")) {
		t.Fatalf("replacement contents: got=%x, err=%v", got, err)
	}
}

func TestRenameRejectsMergedDirectoryDestination(t *testing.T) {
	ctx := context.Background()
	engine, ov := renameTestEngine(t, map[string]model.BaseNode{
		"destination": {Path: "destination", Type: "file", Mode: 0o100644, ObjectOID: "old-file"},
	})
	if err := engine.Unlink(ctx, "destination"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Mkdir(ctx, "destination", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := engine.Create(ctx, "destination/child", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := engine.Create(ctx, "source", 0o644); err != nil {
		t.Fatal(err)
	}
	source, _, err := ov.Lookup(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Rename(ctx, "source", "destination"); !errors.Is(err, iofs.ErrInvalid) {
		t.Fatalf("rename onto current directory: %v, want invalid argument", err)
	}
	after, exists, err := ov.Lookup(ctx, "source")
	if err != nil || !exists || after != source {
		t.Fatalf("rejected rename changed source: %+v, exists=%v, err=%v", after, exists, err)
	}
	for _, path := range []string{"destination", "destination/child"} {
		if _, err := engine.Resolver.ResolvePath(path); err != nil {
			t.Fatalf("rejected rename changed %q: %v", path, err)
		}
	}
}

func renameTestEngine(t *testing.T, base map[string]model.BaseNode) (*Engine, *overlaystore.Store) {
	t.Helper()
	root := t.TempDir()
	cfg := model.RepoConfig{ID: "rename-test", OverlayDir: filepath.Join(root, "overlay"), OverlayDBPath: filepath.Join(root, "overlay.db")}
	ov, err := overlaystore.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	resolver := &Resolver{Snapshot: &fakeSnapshot{nodes: base}, Overlay: ov}
	resolver.SetGeneration(1)
	return &Engine{Repo: cfg, Resolver: resolver, Overlay: ov}, ov
}
