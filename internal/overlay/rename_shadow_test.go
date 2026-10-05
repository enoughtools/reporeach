package overlay

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestRenameWithSourceWhiteoutRollsBackFailedDeletion(t *testing.T) {
	ctx := context.Background()
	store, _ := testStore(t)
	sourceData := []byte{0, 0xff, 's', 'r', 'c'}
	destinationData := []byte{0xff, 0, 'd', 's', 't'}
	for path, data := range map[string][]byte{"source": sourceData, "destination": destinationData} {
		if _, err := store.CreateFile(ctx, path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.WriteFile(ctx, path, 0, data); err != nil {
			t.Fatal(err)
		}
	}
	source, _, err := store.Lookup(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	destination, _, err := store.Lookup(ctx, "destination")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_source_deletion BEFORE INSERT ON overlay_entries
		WHEN NEW.path='source' AND NEW.kind='delete'
		BEGIN SELECT RAISE(ABORT, 'test rejects source deletion'); END`); err != nil {
		t.Fatal(err)
	}
	destinationBase := model.BaseNode{Path: "destination", Type: "file", ObjectOID: "destination-base", Mode: 0o100755}
	if err := store.RenameWithSourceWhiteout(ctx, "source", "destination", &destinationBase); err == nil {
		t.Fatal("expected failure after destination insertion and before source deletion")
	}
	for path, before := range map[string]model.OverlayEntry{"source": source, "destination": destination} {
		after, exists, err := store.Lookup(ctx, path)
		if err != nil || !exists || after != before {
			t.Fatalf("failed rename changed %q: before=%+v, after=%+v, exists=%v, err=%v", path, before, after, exists, err)
		}
		got, err := os.ReadFile(before.BackingPath)
		want := sourceData
		if path == "destination" {
			want = destinationData
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("failed rename changed %q backing: got=%x, err=%v", path, got, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER reject_source_deletion"); err != nil {
		t.Fatal(err)
	}
	if err := store.RenameWithSourceWhiteout(ctx, "source", "destination", &destinationBase); err != nil {
		t.Fatalf("retry rename: %v", err)
	}
	moved, exists, err := store.Lookup(ctx, "destination")
	if err != nil || !exists || moved.BackingPath != source.BackingPath || moved.SourceOID != destinationBase.ObjectOID || moved.SourceMode != destinationBase.Mode {
		t.Fatalf("retry lost source backing or destination metadata: %+v, exists=%v, err=%v", moved, exists, err)
	}
	deleted, exists, err := store.Lookup(ctx, "source")
	if err != nil || !exists || !deleted.IsDeleted() || deleted.TargetPath != "" {
		t.Fatalf("retry lost source deletion: %+v, exists=%v, err=%v", deleted, exists, err)
	}
	if _, err := os.Stat(destination.BackingPath); !os.IsNotExist(err) {
		t.Fatalf("successful rename retained replaced backing: %v", err)
	}
}
