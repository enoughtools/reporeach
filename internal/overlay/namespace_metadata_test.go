package overlay

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

const namespaceMetadataAttribute = "com.reb.namespace-test"

func namespaceMetadata(t *testing.T, store *Store, path, nodeType string, value []byte) model.MetadataObject {
	t.Helper()
	object, err := store.BindMetadata(context.Background(), path, nodeType)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataXattr(context.Background(), object.ID, namespaceMetadataAttribute, value, model.XattrAlwaysSet); err != nil {
		t.Fatal(err)
	}
	return object
}

func assertNamespaceBinding(t *testing.T, store *Store, path string, want model.MetadataObjectID) {
	t.Helper()
	var got model.MetadataObjectID
	err := store.db.QueryRowContext(context.Background(), "SELECT object_id FROM metadata_bindings WHERE path=?", path).Scan(&got)
	if want == "" {
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("binding for %q = %q, err=%v; want absent", path, got, err)
		}
		return
	}
	if err != nil || got != want {
		t.Fatalf("binding for %q = %q, err=%v; want %q", path, got, err, want)
	}
}

func assertNamespaceAttribute(t *testing.T, store *Store, id model.MetadataObjectID, want []byte) {
	t.Helper()
	got, exists, err := store.GetMetadataXattr(context.Background(), id, namespaceMetadataAttribute)
	if err != nil || !exists || !bytes.Equal(got, want) {
		t.Fatalf("metadata %q attribute = %x, exists=%v, err=%v; want %x", id, got, exists, err, want)
	}
}

func TestNamespaceMetadataCreationDetachesPreviousIdentity(t *testing.T) {
	for _, kind := range []string{"file", "opened-file", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := testStore(t)
			ctx := context.Background()
			oldValue := []byte{0, 0xff, 'o', 'l', 'd'}
			childValue := []byte{0xff, 'c', 0}
			// These bindings represent a base directory and a metadata-only base child.
			old := namespaceMetadata(t, store, "node", "dir", oldValue)
			child := namespaceMetadata(t, store, "node/child", "file", childValue)
			wantType := kind
			switch kind {
			case "file":
				_, err := store.CreateFile(ctx, "node", 0o644)
				if err != nil {
					t.Fatal(err)
				}
			case "opened-file":
				wantType = "file"
				_, file, err := store.CreateFileOpened(ctx, "node", 0o644)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if _, err := store.CreateSymlink(ctx, "node", "../target"); err != nil {
					t.Fatal(err)
				}
			case "directory":
				wantType = "dir"
				if err := store.CreateDirectory(ctx, "node", 0o755); err != nil {
					t.Fatal(err)
				}
			}
			fresh, err := store.BindMetadata(ctx, "node", wantType)
			if err != nil || fresh.ID == old.ID || fresh.Type != wantType {
				t.Fatalf("new identity = %+v, err=%v; old=%+v", fresh, err, old)
			}
			assertNamespaceBinding(t, store, "node", fresh.ID)
			assertNamespaceBinding(t, store, "node/child", "")
			if value, exists, err := store.GetMetadataXattr(ctx, fresh.ID, namespaceMetadataAttribute); err != nil || exists {
				t.Fatalf("new object inherited old attribute: %x, exists=%v, err=%v", value, exists, err)
			}
			assertNamespaceAttribute(t, store, old.ID, oldValue)
			assertNamespaceAttribute(t, store, child.ID, childValue)
			// Retained old inode identities remain writable after namespace detachment.
			retainedValue := []byte{0xff, 0, 'r'}
			if err := store.SetMetadataXattr(ctx, old.ID, namespaceMetadataAttribute, retainedValue, model.XattrMustReplace); err != nil {
				t.Fatal(err)
			}
			assertNamespaceAttribute(t, store, old.ID, retainedValue)
			assertNamespaceBinding(t, store, "node", fresh.ID)
		})
	}
}

func TestNamespaceMetadataPromotionPreservesIdentity(t *testing.T) {
	for _, nodeType := range []string{"file", "symlink", "dir"} {
		t.Run(nodeType, func(t *testing.T) {
			store, cfg := testStore(t)
			ctx := context.Background()
			value := []byte{0, 0xff, 'm'}
			object := namespaceMetadata(t, store, "base", nodeType, value)
			if nodeType == "dir" {
				child := namespaceMetadata(t, store, "base/child", "file", value)
				if err := store.Mkdir(ctx, "base", 0o40000); err != nil {
					t.Fatal(err)
				}
				if err := store.SetMode(ctx, "base", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := store.SetMtime(ctx, "base", time.Unix(123, 0)); err != nil {
					t.Fatal(err)
				}
				assertNamespaceBinding(t, store, "base/child", child.ID)
			} else {
				data := []byte{0, 0xff, 'b', 'a', 's', 'e'}
				mode := uint32(0o100644)
				if nodeType == "symlink" {
					data = []byte("../base-target")
					mode = 0o120000
				}
				oid := testBlobOID(data)
				if err := os.WriteFile(filepath.Join(cfg.BlobCacheDir, oid), data, 0o644); err != nil {
					t.Fatal(err)
				}
				base := model.BaseNode{Path: "base", Type: nodeType, Mode: mode, ObjectOID: oid}
				if _, err := store.EnsureCopyOnWrite(ctx, cfg, "base", base); err != nil {
					t.Fatal(err)
				}
				if nodeType == "file" {
					if _, err := store.WriteFile(ctx, "base", 0, []byte{0xff, 0, 'w'}); err != nil {
						t.Fatal(err)
					}
					if err := store.Truncate(ctx, "base", 2); err != nil {
						t.Fatal(err)
					}
					if err := store.SetMode(ctx, "base", 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			assertNamespaceBinding(t, store, "base", object.ID)
			assertNamespaceAttribute(t, store, object.ID, value)
		})
	}
}

func namespaceRename(store *Store, ctx context.Context, variant, source, destination string) error {
	base := model.BaseNode{Path: destination, Type: "file", ObjectOID: "destination-base", Mode: 0o100755}
	switch variant {
	case "rename":
		return store.Rename(ctx, source, destination)
	case "source-whiteout":
		return store.RenameWithSourceWhiteout(ctx, source, destination, &base)
	case "destination-base":
		return store.RenameAndMarkModifiedFromBase(ctx, source, destination, base.ObjectOID, base.Mode)
	default:
		panic("unknown rename test variant")
	}
}

func TestNamespaceMetadataScalarRenameMovesSourceIdentity(t *testing.T) {
	for _, variant := range []string{"rename", "source-whiteout", "destination-base"} {
		for _, nodeType := range []string{"file", "symlink"} {
			t.Run(variant+"/"+nodeType, func(t *testing.T) {
				store, _ := testStore(t)
				ctx := context.Background()
				if nodeType == "symlink" {
					if _, err := store.CreateSymlink(ctx, "source", "../target"); err != nil {
						t.Fatal(err)
					}
				} else if _, err := store.CreateFile(ctx, "source", 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := store.CreateFile(ctx, "destination", 0o644); err != nil {
					t.Fatal(err)
				}
				sourceValue, destinationValue := []byte{0, 0xff, 's'}, []byte{0xff, 0, 'd'}
				source := namespaceMetadata(t, store, "source", nodeType, sourceValue)
				destination := namespaceMetadata(t, store, "destination", "file", destinationValue)
				if err := namespaceRename(store, ctx, variant, "source", "destination"); err != nil {
					t.Fatal(err)
				}
				assertNamespaceBinding(t, store, "source", "")
				assertNamespaceBinding(t, store, "destination", source.ID)
				assertNamespaceAttribute(t, store, source.ID, sourceValue)
				assertNamespaceAttribute(t, store, destination.ID, destinationValue)
				if err := namespaceRename(store, ctx, variant, "destination", "destination"); err != nil {
					t.Fatalf("same-path rename: %v", err)
				}
				assertNamespaceBinding(t, store, "destination", source.ID)
				assertNamespaceAttribute(t, store, source.ID, sourceValue)
			})
		}
	}
}

func TestNamespaceMetadataTreeRenameMovesMetadataOnlyChildren(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	const sourcePath, destinationPath = "目录_%", "moved_百分%"
	if err := store.Mkdir(ctx, sourcePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.Mkdir(ctx, destinationPath, 0o755); err != nil {
		t.Fatal(err)
	}
	value := []byte{0, 0xff, '%', '_'}
	source := namespaceMetadata(t, store, sourcePath, "dir", value)
	children := map[string]model.MetadataObject{}
	for _, suffix := range []string{"child", "nested/é_%", "百分%_"} {
		path := sourcePath + "/" + suffix
		children[suffix] = namespaceMetadata(t, store, path, "file", value)
		if _, exists := store.Get(path); exists {
			t.Fatalf("metadata-only child %q gained byte overlay", path)
		}
	}
	replaced := namespaceMetadata(t, store, destinationPath, "dir", value)
	replacedChild := namespaceMetadata(t, store, destinationPath+"/old-child", "file", value)
	outside := map[string]model.MetadataObject{}
	for _, path := range []string{"目录AX/child", sourcePath + "other/child", destinationPath + "other/child"} {
		outside[path] = namespaceMetadata(t, store, path, "file", value)
	}
	if err := store.RenameTree(ctx, sourcePath, destinationPath, []string{sourcePath}, nil); err != nil {
		t.Fatal(err)
	}
	assertNamespaceBinding(t, store, sourcePath, "")
	assertNamespaceBinding(t, store, destinationPath, source.ID)
	for suffix, object := range children {
		assertNamespaceBinding(t, store, sourcePath+"/"+suffix, "")
		assertNamespaceBinding(t, store, destinationPath+"/"+suffix, object.ID)
		assertNamespaceAttribute(t, store, object.ID, value)
		if _, exists := store.Get(destinationPath + "/" + suffix); exists {
			t.Fatalf("moving metadata-only child %q materialized byte overlay", suffix)
		}
	}
	assertNamespaceBinding(t, store, destinationPath+"/old-child", "")
	assertNamespaceAttribute(t, store, replaced.ID, value)
	assertNamespaceAttribute(t, store, replacedChild.ID, value)
	for path, object := range outside {
		assertNamespaceBinding(t, store, path, object.ID)
	}
}

func TestNamespaceMetadataRemoveDetachesSubtree(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	const root = "目录_%"
	if err := store.Mkdir(ctx, root, 0o755); err != nil {
		t.Fatal(err)
	}
	value := []byte{0xff, 0, 'u'}
	objects := map[string]model.MetadataObject{}
	for _, path := range []string{root, root + "/child", root + "/nested/child"} {
		nodeType := "file"
		if path == root {
			nodeType = "dir"
		}
		objects[path] = namespaceMetadata(t, store, path, nodeType, value)
	}
	outside := namespaceMetadata(t, store, root+"other/child", "file", value)
	if err := store.Remove(ctx, root); err != nil {
		t.Fatal(err)
	}
	for path, object := range objects {
		assertNamespaceBinding(t, store, path, "")
		assertNamespaceAttribute(t, store, object.ID, value)
	}
	assertNamespaceBinding(t, store, root+"other/child", outside.ID)
	if entry, exists := store.Get(root); !exists || !entry.IsDeleted() {
		t.Fatalf("removed root = %+v, exists=%v", entry, exists)
	}
}

func TestNamespaceMetadataReconcilePreservesCommittedIdentity(t *testing.T) {
	for _, operation := range []string{"create", "modify", "rename", "symlink", "directory"} {
		t.Run(operation, func(t *testing.T) {
			store, cfg := testStore(t)
			ctx := context.Background()
			path, nodeType := "committed", "file"
			data := []byte{0, 0xff, 'c', 'o', 'm', 'm', 'i', 't'}
			mode := uint32(0o100644)
			switch operation {
			case "create":
				if _, err := store.CreateFile(ctx, path, 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := store.WriteFile(ctx, path, 0, data); err != nil {
					t.Fatal(err)
				}
			case "modify", "rename":
				original := []byte("original")
				if operation == "rename" {
					original = data
					path = "source"
				}
				oid := testBlobOID(original)
				if err := os.WriteFile(filepath.Join(cfg.BlobCacheDir, oid), original, 0o644); err != nil {
					t.Fatal(err)
				}
				base := model.BaseNode{Path: path, Type: "file", Mode: mode, ObjectOID: oid}
				if _, err := store.EnsureCopyOnWrite(ctx, cfg, path, base); err != nil {
					t.Fatal(err)
				}
				if operation == "rename" {
					if err := store.Rename(ctx, path, "committed"); err != nil {
						t.Fatal(err)
					}
					path = "committed"
				} else {
					if err := store.Truncate(ctx, path, 0); err != nil {
						t.Fatal(err)
					}
					if _, err := store.WriteFile(ctx, path, 0, data); err != nil {
						t.Fatal(err)
					}
				}
			case "symlink":
				nodeType, mode = "symlink", 0o120000
				const target = "../committed-target"
				data = []byte(target)
				if _, err := store.CreateSymlink(ctx, path, target); err != nil {
					t.Fatal(err)
				}
			case "directory":
				nodeType, mode = "dir", 0o40000
				if err := store.Mkdir(ctx, path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			value := []byte{0xff, 0, 'x'}
			object := namespaceMetadata(t, store, path, nodeType, value)
			base := model.BaseNode{Path: path, Type: nodeType, Mode: mode, ObjectOID: testBlobOID(data)}
			if err := store.ReconcileChecked(ctx, func(requested string) (model.BaseNode, bool, error) {
				return base, requested == path, nil
			}); err != nil {
				t.Fatal(err)
			}
			if entry, exists := store.Get(path); exists {
				t.Fatalf("committed byte overlay survived: %+v", entry)
			}
			assertNamespaceBinding(t, store, path, object.ID)
			assertNamespaceAttribute(t, store, object.ID, value)
		})
	}
}

func TestNamespaceMetadataReconcileUsesFinalMergedNamespace(t *testing.T) {
	store, cfg := testStore(t)
	ctx := context.Background()
	value := []byte{0, 0xff, 'r'}
	detached := map[string]model.MetadataObject{}
	for _, path := range []string{"absent", "type-changed", "whiteout/child", "replacement/child", "missing-parent/child"} {
		detached[path] = namespaceMetadata(t, store, path, "file", value)
	}
	detached["replacement"] = namespaceMetadata(t, store, "replacement", "dir", value)
	root := namespaceMetadata(t, store, ".", "dir", value)
	gitfile := namespaceMetadata(t, store, ".git", "file", value)
	if _, err := store.CreateFile(ctx, "live-overlay", 0o644); err != nil {
		t.Fatal(err)
	}
	liveFile := namespaceMetadata(t, store, "live-overlay", "file", value)
	if _, err := store.CreateSymlink(ctx, "live-symlink", "../local"); err != nil {
		t.Fatal(err)
	}
	liveSymlink := namespaceMetadata(t, store, "live-symlink", "symlink", value)
	// Seed prior byte state directly so cleanup is tested independently of
	// Remove/CreateFile, which correctly detach bindings during the operation.
	if err := store.upsertEntry(ctx, model.OverlayEntry{Path: "whiteout", Kind: model.OverlayKindDelete}); err != nil {
		t.Fatal(err)
	}
	backing := filepath.Join(cfg.OverlayDir, "replacement-fixture")
	if err := os.WriteFile(backing, []byte{0, 0xff, 'f'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.upsertEntry(ctx, model.OverlayEntry{Path: "replacement", Kind: model.OverlayKindCreate, BackingPath: backing, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	base := map[string]model.BaseNode{
		"type-changed":         {Type: "symlink", Mode: 0o120000},
		"whiteout":             {Type: "dir", Mode: 0o40000},
		"whiteout/child":       {Type: "file", Mode: 0o100644},
		"replacement":          {Type: "dir", Mode: 0o40000},
		"replacement/child":    {Type: "file", Mode: 0o100644},
		"missing-parent/child": {Type: "file", Mode: 0o100644},
		"live-overlay":         {Type: "symlink", Mode: 0o120000},
		"live-symlink":         {Type: "file", Mode: 0o100644},
	}
	if err := store.ReconcileChecked(ctx, func(path string) (model.BaseNode, bool, error) {
		node, exists := base[path]
		return node, exists, nil
	}); err != nil {
		t.Fatal(err)
	}
	for path, object := range detached {
		assertNamespaceBinding(t, store, path, "")
		assertNamespaceAttribute(t, store, object.ID, value)
	}
	assertNamespaceBinding(t, store, ".", root.ID)
	assertNamespaceBinding(t, store, ".git", gitfile.ID)
	assertNamespaceBinding(t, store, "live-overlay", liveFile.ID)
	assertNamespaceBinding(t, store, "live-symlink", liveSymlink.ID)
	assertNamespaceAttribute(t, store, liveFile.ID, value)
	assertNamespaceAttribute(t, store, liveSymlink.ID, value)
}

func namespaceBindings(t *testing.T, store *Store) map[string]model.MetadataObjectID {
	t.Helper()
	rows, err := store.db.QueryContext(context.Background(), "SELECT path,object_id FROM metadata_bindings ORDER BY path")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	bindings := map[string]model.MetadataObjectID{}
	for rows.Next() {
		var path string
		var id model.MetadataObjectID
		if err := rows.Scan(&path, &id); err != nil {
			t.Fatal(err)
		}
		bindings[path] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return bindings
}

func TestNamespaceMetadataRenameFailureRollsBackBytesAndBindings(t *testing.T) {
	for _, variant := range []string{"rename", "source-whiteout", "destination-base", "tree"} {
		t.Run(variant, func(t *testing.T) {
			store, _ := testStore(t)
			ctx := context.Background()
			nodeType := "file"
			for _, path := range []string{"source", "destination"} {
				if variant == "tree" {
					nodeType = "dir"
					if err := store.Mkdir(ctx, path, 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := store.CreateFile(ctx, path, 0o644); err != nil {
						t.Fatal(err)
					}
					if _, err := store.WriteFile(ctx, path, 0, []byte{0, 0xff, path[0]}); err != nil {
						t.Fatal(err)
					}
				}
			}
			value := []byte{0xff, 0, 'a'}
			source := namespaceMetadata(t, store, "source", nodeType, value)
			destination := namespaceMetadata(t, store, "destination", nodeType, value)
			if variant == "tree" {
				namespaceMetadata(t, store, "source/child", "file", value)
				namespaceMetadata(t, store, "destination/old-child", "file", value)
			}
			beforeEntries, err := store.ListAll(ctx)
			if err != nil {
				t.Fatal(err)
			}
			beforeBindings := namespaceBindings(t, store)
			if _, err := store.db.ExecContext(ctx, "CREATE TRIGGER reject_namespace_move BEFORE INSERT ON metadata_bindings WHEN NEW.path='destination' BEGIN SELECT RAISE(ABORT, 'injected metadata move failure'); END"); err != nil {
				t.Fatal(err)
			}
			if variant == "tree" {
				err = store.RenameTree(ctx, "source", "destination", nil, nil)
			} else {
				err = namespaceRename(store, ctx, variant, "source", "destination")
			}
			if err == nil {
				t.Fatal("expected metadata insertion failure after byte namespace changes")
			}
			afterEntries, err := store.ListAll(ctx)
			if err != nil || !reflect.DeepEqual(afterEntries, beforeEntries) {
				t.Fatalf("failed rename changed byte rows: before=%+v, after=%+v, err=%v", beforeEntries, afterEntries, err)
			}
			if after := namespaceBindings(t, store); !reflect.DeepEqual(after, beforeBindings) {
				t.Fatalf("failed rename changed metadata bindings: before=%v, after=%v", beforeBindings, after)
			}
			assertNamespaceAttribute(t, store, source.ID, value)
			assertNamespaceAttribute(t, store, destination.ID, value)
			if variant != "tree" {
				for _, entry := range beforeEntries {
					got, err := os.ReadFile(entry.BackingPath)
					want := []byte{0, 0xff, entry.Path[0]}
					if err != nil || !bytes.Equal(got, want) {
						t.Fatalf("failed rename changed %q backing: got=%x, err=%v", entry.Path, got, err)
					}
				}
			}
		})
	}
}

func TestNamespaceMetadataCreationFailureRollsBackExistingObject(t *testing.T) {
	for _, kind := range []string{"file", "opened-file", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := testStore(t)
			ctx := context.Background()
			if err := store.Mkdir(ctx, "node", 0o755); err != nil {
				t.Fatal(err)
			}
			value := []byte{0xff, 0, 'f'}
			old := namespaceMetadata(t, store, "node", "dir", value)
			child := namespaceMetadata(t, store, "node/child", "file", value)
			beforeEntries, err := store.ListAll(ctx)
			if err != nil {
				t.Fatal(err)
			}
			beforeBindings := namespaceBindings(t, store)
			beforeFiles, err := os.ReadDir(store.upperDir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, "CREATE TRIGGER reject_namespace_creation BEFORE INSERT ON metadata_bindings WHEN NEW.path='node' BEGIN SELECT RAISE(ABORT, 'injected metadata creation failure'); END"); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file":
				_, err = store.CreateFile(ctx, "node", 0o644)
			case "opened-file":
				var file *os.File
				_, file, err = store.CreateFileOpened(ctx, "node", 0o644)
				if file != nil {
					_ = file.Close()
				}
			case "symlink":
				_, err = store.CreateSymlink(ctx, "node", "../target")
			case "directory":
				err = store.CreateDirectory(ctx, "node", 0o755)
			}
			if err == nil {
				t.Fatal("expected metadata insertion failure after byte upsert")
			}
			afterEntries, err := store.ListAll(ctx)
			if err != nil || !reflect.DeepEqual(afterEntries, beforeEntries) {
				t.Fatalf("failed creation changed byte rows: before=%+v, after=%+v, err=%v", beforeEntries, afterEntries, err)
			}
			if after := namespaceBindings(t, store); !reflect.DeepEqual(after, beforeBindings) {
				t.Fatalf("failed creation changed metadata bindings: before=%v, after=%v", beforeBindings, after)
			}
			afterFiles, err := os.ReadDir(store.upperDir)
			if err != nil || len(afterFiles) != len(beforeFiles) {
				t.Fatalf("failed creation leaked backing files: before=%d, after=%d, err=%v", len(beforeFiles), len(afterFiles), err)
			}
			assertNamespaceAttribute(t, store, old.ID, value)
			assertNamespaceAttribute(t, store, child.ID, value)
		})
	}
}

func TestNamespaceMetadataRemoveFailureRollsBackBytesAndBindings(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	if err := store.Mkdir(ctx, "node", 0o755); err != nil {
		t.Fatal(err)
	}
	value := []byte{0, 0xff, 'd'}
	old := namespaceMetadata(t, store, "node", "dir", value)
	child := namespaceMetadata(t, store, "node/child", "file", value)
	beforeEntries, err := store.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeBindings := namespaceBindings(t, store)
	if _, err := store.db.ExecContext(ctx, "CREATE TRIGGER reject_namespace_removal BEFORE DELETE ON metadata_bindings WHEN OLD.path='node/child' BEGIN SELECT RAISE(ABORT, 'injected metadata deletion failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(ctx, "node"); err == nil {
		t.Fatal("expected metadata deletion failure after byte whiteout")
	}
	afterEntries, err := store.ListAll(ctx)
	if err != nil || !reflect.DeepEqual(afterEntries, beforeEntries) {
		t.Fatalf("failed removal changed byte rows: before=%+v, after=%+v, err=%v", beforeEntries, afterEntries, err)
	}
	if after := namespaceBindings(t, store); !reflect.DeepEqual(after, beforeBindings) {
		t.Fatalf("failed removal changed metadata bindings: before=%v, after=%v", beforeBindings, after)
	}
	assertNamespaceAttribute(t, store, old.ID, value)
	assertNamespaceAttribute(t, store, child.ID, value)
	if _, err := os.Stat(beforeEntries[0].BackingPath); err != nil {
		t.Fatalf("failed removal deleted live backing: %v", err)
	}
}
