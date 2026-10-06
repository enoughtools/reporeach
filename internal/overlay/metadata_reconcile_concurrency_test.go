package overlay

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestMetadataOnlyReconcileSerializesAcrossStores(t *testing.T) {
	s, cfg := metadataTestStore(t)
	other := openSecondMetadataStore(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	object := bindTestMetadata(t, s, "vanishing", "file")
	setTestMetadata(t, s, object.ID, "retained", []byte{1, 0})
	updated := []byte{0xff, 0, 0xfe}
	writer, err := beginMetadataTransaction(ctx, other.db)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `UPDATE metadata_xattrs SET value=? WHERE object_id=? AND name=?`, updated, object.ID, "retained"); err != nil {
		t.Fatal(err)
	}
	if err := updateMetadataCtimeTx(ctx, writer, object.ID); err != nil {
		t.Fatal(err)
	}

	// A deferred WAL transaction can read while another Store holds a writer.
	// Commit that writer after Reconcile has read its metadata-only binding, so
	// attempting to detach from the stale snapshot fails before the fix. With
	// writer intent, Reconcile waits before reading and sees the committed data.
	readReached := make(chan struct{})
	writerReleased := make(chan struct{})
	defer func() {
		select {
		case <-writerReleased:
		default:
			close(writerReleased)
		}
	}()
	reconciled := make(chan error, 1)
	go func() {
		reconciled <- s.ReconcileChecked(ctx, func(path string) (model.BaseNode, bool, error) {
			if path == "vanishing" {
				close(readReached)
				select {
				case <-writerReleased:
				case <-ctx.Done():
					return model.BaseNode{}, false, ctx.Err()
				}
			}
			return model.BaseNode{}, false, nil
		})
	}()
	reservation := time.NewTimer(100 * time.Millisecond)
	defer reservation.Stop()
	readBeforeCommit := false
	select {
	case <-readReached:
		readBeforeCommit = true
	case <-reservation.C:
	case err := <-reconciled:
		t.Fatalf("reconciliation exited while another Store held the writer: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	close(writerReleased)
	select {
	case err = <-reconciled:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Detaching a namespace binding must never delete the identity or its
	// attributes, including when a stale transaction is rolled back.
	if _, found, objectErr := s.MetadataObject(ctx, object.ID); objectErr != nil || !found {
		t.Fatalf("reconciliation lost the retained metadata identity: found=%v, err=%v", found, objectErr)
	}
	if actual := getTestMetadata(t, s, object.ID, "retained"); !bytes.Equal(actual, updated) {
		t.Fatalf("reconciliation changed another Store's committed binary attribute: %x", actual)
	}
	boundID, bound := metadataBindingID(t, s, "vanishing")
	if err != nil {
		if !bound || boundID != object.ID {
			t.Fatal("failed reconciliation partially detached its namespace binding")
		}
		t.Fatalf("metadata-only reconciliation did not serialize before reading (read before writer commit=%v): %v", readBeforeCommit, err)
	}
	if bound {
		t.Fatal("successful reconciliation retained the vanished namespace binding")
	}
}
