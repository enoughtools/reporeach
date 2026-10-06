package overlay

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func metadataTestStore(t *testing.T) (*Store, model.RepoConfig) {
	t.Helper()
	s, cfg := testStore(t)
	if err := ensureMetadataSchema(context.Background(), s.db); err != nil {
		t.Fatal(err)
	}
	return s, cfg
}

func bindTestMetadata(t *testing.T, s *Store, path, nodeType string) model.MetadataObject {
	t.Helper()
	object, err := s.BindMetadata(context.Background(), path, nodeType)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func setTestMetadata(t *testing.T, s *Store, id model.MetadataObjectID, name string, value []byte) {
	t.Helper()
	if err := s.SetMetadataXattr(context.Background(), id, name, value, model.XattrAlwaysSet); err != nil {
		t.Fatal(err)
	}
}

func getTestMetadata(t *testing.T, s *Store, id model.MetadataObjectID, name string) []byte {
	t.Helper()
	value, found, err := s.GetMetadataXattr(context.Background(), id, name)
	if err != nil || !found {
		t.Fatalf("attribute lookup: found=%v, err=%v", found, err)
	}
	return value
}

func metadataTestTransaction(t *testing.T, s *Store, callback func(*sql.Tx) error, commit bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := callback(tx); err != nil {
		t.Fatal(err)
	}
	if commit {
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

func metadataBindingID(t *testing.T, s *Store, path string) (model.MetadataObjectID, bool) {
	t.Helper()
	var id model.MetadataObjectID
	err := s.db.QueryRowContext(context.Background(), `SELECT object_id FROM metadata_bindings WHERE path=?`, model.CleanPath(path)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return id, true
}

func TestMetadataBinaryAndEmptyAttributes(t *testing.T) {
	s, _ := metadataTestStore(t)
	ctx := context.Background()
	object := bindTestMetadata(t, s, "/docs/../hello", "file")
	if !validMetadataObjectID(object.ID) || object.Type != "file" || object.CtimeUnixNs != 0 {
		t.Fatalf("invalid persistent object: %+v", object)
	}
	if again := bindTestMetadata(t, s, "hello", "file"); again != object {
		t.Fatalf("binding changed without a namespace replacement: got %+v, want %+v", again, object)
	}
	if _, _, err := s.GetMetadataXattr(ctx, object.ID, "missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListMetadataXattrs(ctx, object.ID); err != nil {
		t.Fatal(err)
	}
	if after, found, err := s.MetadataObject(ctx, object.ID); err != nil || !found || after != object {
		t.Fatalf("read probes changed initial ctime: %+v, found=%v, err=%v", after, found, err)
	}
	binaryValue := []byte{0, 0xff, 0xc3, 0x28, '\n', 0, 0xfe}
	setTestMetadata(t, s, object.ID, "com.apple.FileSecurity", binaryValue)
	setTestMetadata(t, s, object.ID, "empty", nil)
	setTestMetadata(t, s, object.ID, "a", []byte{1})
	setTestMetadata(t, s, object.ID, "A", []byte{2})
	if got := getTestMetadata(t, s, object.ID, "com.apple.FileSecurity"); !bytes.Equal(got, binaryValue) {
		t.Fatalf("binary attribute changed: %x", got)
	}
	if got := getTestMetadata(t, s, object.ID, "empty"); got == nil || len(got) != 0 {
		t.Fatalf("empty attribute = %v, want a present empty value", got)
	}
	var valueType string
	if err := s.db.QueryRowContext(ctx, `SELECT typeof(value) FROM metadata_xattrs WHERE object_id=? AND name='empty'`, object.ID).Scan(&valueType); err != nil || valueType != "blob" {
		t.Fatalf("empty attribute storage type = %q, err=%v", valueType, err)
	}
	names, err := s.ListMetadataXattrs(ctx, object.ID)
	if err != nil || !slices.Equal(names, []string{"A", "a", "com.apple.FileSecurity", "empty"}) {
		t.Fatalf("case-sensitive sorted list = %v, err=%v", names, err)
	}
	if _, found, err := s.GetMetadataXattr(ctx, object.ID, "missing"); found || err != nil {
		t.Fatalf("missing attribute: found=%v, err=%v", found, err)
	}
	updated, found, err := s.MetadataObject(ctx, object.ID)
	if err != nil || !found || updated.CtimeUnixNs <= object.CtimeUnixNs {
		t.Fatalf("metadata ctime did not advance: %+v, found=%v, err=%v", updated, found, err)
	}
	if again := bindTestMetadata(t, s, "hello", "file"); again != updated {
		t.Fatal("binding probe changed existing attribute ctime")
	}
	getTestMetadata(t, s, object.ID, "empty")
	if _, err := s.ListMetadataXattrs(ctx, object.ID); err != nil {
		t.Fatal(err)
	}
	if after, found, err := s.MetadataObject(ctx, object.ID); err != nil || !found || after != updated {
		t.Fatalf("read probes changed existing attribute ctime: %+v, found=%v, err=%v", after, found, err)
	}
	if dirty, err := s.DirtyCount(ctx); err != nil || dirty != 0 {
		t.Fatalf("attribute-only write created a byte overlay: dirty=%d, err=%v", dirty, err)
	}
	if entries, err := s.ListByPrefix(ctx, "."); err != nil || len(entries) != 0 {
		t.Fatalf("attribute-only write created namespace entries: %v, err=%v", entries, err)
	}
}

func TestMetadataSetPoliciesAndRemoval(t *testing.T) {
	s, _ := metadataTestStore(t)
	ctx := context.Background()
	object := bindTestMetadata(t, s, "file", "file")
	if names, err := s.ListMetadataXattrs(ctx, object.ID); err != nil || names == nil || len(names) != 0 {
		t.Fatalf("empty list = %v, err=%v", names, err)
	}
	if has, err := s.HasMetadataXattrs(ctx); err != nil || has {
		t.Fatalf("empty store has attributes: %v, err=%v", has, err)
	}
	if err := s.SetMetadataXattr(ctx, object.ID, "key", []byte{1}, model.XattrMustReplace); !errors.Is(err, model.ErrXattrNotFound) {
		t.Fatalf("replace missing attribute = %v", err)
	}
	if err := s.SetMetadataXattr(ctx, object.ID, "key", []byte{1}, model.XattrMustCreate); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.MetadataObject(ctx, object.ID)
	if err := s.SetMetadataXattr(ctx, object.ID, "key", []byte{2}, model.XattrMustCreate); !errors.Is(err, model.ErrXattrExists) {
		t.Fatalf("create existing attribute = %v", err)
	}
	if after, _, _ := s.MetadataObject(ctx, object.ID); after != before {
		t.Fatal("failed create changed object metadata")
	}
	if got := getTestMetadata(t, s, object.ID, "key"); !bytes.Equal(got, []byte{1}) {
		t.Fatal("failed create changed attribute value")
	}
	if err := s.SetMetadataXattr(ctx, object.ID, "key", []byte{2}, model.XattrMustReplace); err != nil {
		t.Fatal(err)
	}
	if got := getTestMetadata(t, s, object.ID, "key"); !bytes.Equal(got, []byte{2}) {
		t.Fatal("replace did not persist new value")
	}
	before, _, _ = s.MetadataObject(ctx, object.ID)
	if err := s.RemoveMetadataXattr(ctx, object.ID, "missing"); !errors.Is(err, model.ErrXattrNotFound) {
		t.Fatalf("remove missing attribute = %v", err)
	}
	if after, _, _ := s.MetadataObject(ctx, object.ID); after != before {
		t.Fatal("failed removal changed object metadata")
	}
	if err := s.RemoveMetadataXattr(ctx, object.ID, "key"); err != nil {
		t.Fatal(err)
	}
	if after, _, _ := s.MetadataObject(ctx, object.ID); after.CtimeUnixNs <= before.CtimeUnixNs {
		t.Fatal("removal did not advance ctime")
	}
	if has, err := s.HasMetadataXattrs(ctx); err != nil || has {
		t.Fatalf("removed last attribute: has=%v, err=%v", has, err)
	}
}

func TestMetadataValidationAndMissingObjects(t *testing.T) {
	s, _ := metadataTestStore(t)
	ctx := context.Background()
	object := bindTestMetadata(t, s, "file", "file")
	for _, name := range []string{"", "zero\x00byte", strings.Repeat("n", model.MaxXattrNameBytes+1), strings.Repeat("é", 64), "invalid\xff"} {
		if err := s.SetMetadataXattr(ctx, object.ID, name, nil, model.XattrAlwaysSet); !errors.Is(err, model.ErrInvalidXattr) {
			t.Fatalf("invalid name accepted: err=%v", err)
		}
		if _, _, err := s.GetMetadataXattr(ctx, object.ID, name); !errors.Is(err, model.ErrInvalidXattr) {
			t.Fatalf("invalid lookup name accepted: err=%v", err)
		}
		if err := s.RemoveMetadataXattr(ctx, object.ID, name); !errors.Is(err, model.ErrInvalidXattr) {
			t.Fatalf("invalid removal name accepted: err=%v", err)
		}
	}
	for _, name := range []string{strings.Repeat("n", model.MaxXattrNameBytes), strings.Repeat("é", 63) + "n"} {
		setTestMetadata(t, s, object.ID, name, nil)
	}
	if err := s.SetMetadataXattr(ctx, object.ID, "key", nil, model.XattrSetPolicy(255)); !errors.Is(err, model.ErrInvalidXattr) {
		t.Fatalf("invalid policy = %v", err)
	}
	value := bytes.Repeat([]byte{0xff}, model.MaxXattrValueBytes)
	setTestMetadata(t, s, object.ID, "largest", value)
	before, _, _ := s.MetadataObject(ctx, object.ID)
	if err := s.SetMetadataXattr(ctx, object.ID, "largest", append(value, 0), model.XattrAlwaysSet); !errors.Is(err, model.ErrXattrTooLarge) {
		t.Fatalf("oversized value = %v", err)
	}
	if got := getTestMetadata(t, s, object.ID, "largest"); !bytes.Equal(got, value) {
		t.Fatal("oversized replacement changed the stored value")
	}
	if after, _, _ := s.MetadataObject(ctx, object.ID); after != before {
		t.Fatal("oversized replacement changed ctime")
	}
	missing := model.MetadataObjectID(strings.Repeat("0", 32))
	for _, id := range []model.MetadataObjectID{missing, "bad", model.MetadataObjectID(strings.Repeat("A", 32))} {
		if _, found, err := s.MetadataObject(ctx, id); found || err != nil {
			t.Fatalf("missing object lookup: found=%v, err=%v", found, err)
		}
		if _, _, err := s.GetMetadataXattr(ctx, id, "key"); !errors.Is(err, model.ErrMetadataObjectNotFound) {
			t.Fatalf("get missing object = %v", err)
		}
		if _, err := s.ListMetadataXattrs(ctx, id); !errors.Is(err, model.ErrMetadataObjectNotFound) {
			t.Fatalf("list missing object = %v", err)
		}
		if err := s.SetMetadataXattr(ctx, id, "key", nil, model.XattrAlwaysSet); !errors.Is(err, model.ErrMetadataObjectNotFound) {
			t.Fatalf("set missing object = %v", err)
		}
		if err := s.RemoveMetadataXattr(ctx, id, "key"); !errors.Is(err, model.ErrMetadataObjectNotFound) {
			t.Fatalf("remove missing object = %v", err)
		}
	}
}

func TestMetadataUsageLimits(t *testing.T) {
	tests := []struct {
		name               string
		usage              metadataXattrUsage
		exists             bool
		oldBytes, newBytes int64
		full               bool
	}{
		{"last attribute", metadataXattrUsage{count: model.MaxXattrsPerObject - 1}, false, 0, 0, false},
		{"attribute count", metadataXattrUsage{count: model.MaxXattrsPerObject}, false, 0, 0, true},
		{"replace full count", metadataXattrUsage{count: model.MaxXattrsPerObject}, true, 1, 1, false},
		{"exact object limit", metadataXattrUsage{objectBytes: model.MaxXattrObjectBytes - 1}, false, 0, 1, false},
		{"object overflow", metadataXattrUsage{objectBytes: model.MaxXattrObjectBytes}, false, 0, 1, true},
		{"object replacement", metadataXattrUsage{objectBytes: model.MaxXattrObjectBytes}, true, 1, 1, false},
		{"object shrinking replacement", metadataXattrUsage{objectBytes: model.MaxXattrObjectBytes}, true, 1, 0, false},
		{"exact store limit", metadataXattrUsage{storeBytes: model.MaxXattrStoreBytes - 1}, false, 0, 1, false},
		{"store overflow", metadataXattrUsage{storeBytes: model.MaxXattrStoreBytes}, false, 0, 1, true},
		{"store replacement", metadataXattrUsage{storeBytes: model.MaxXattrStoreBytes}, true, 1, 1, false},
		{"store shrinking replacement", metadataXattrUsage{storeBytes: model.MaxXattrStoreBytes}, true, 1, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkMetadataXattrUsage(tt.usage, tt.exists, tt.oldBytes, tt.newBytes)
			if errors.Is(err, model.ErrXattrStorageFull) != tt.full || (err != nil && !tt.full) {
				t.Fatalf("limit result = %v, want full=%v", err, tt.full)
			}
		})
	}
}

func TestMetadataAttributeCountIsAtomic(t *testing.T) {
	s, _ := metadataTestStore(t)
	ctx := context.Background()
	object := bindTestMetadata(t, s, "file", "file")
	for i := range model.MaxXattrsPerObject {
		setTestMetadata(t, s, object.ID, fmt.Sprintf("key-%03d", i), nil)
	}
	before, _, _ := s.MetadataObject(ctx, object.ID)
	if err := s.SetMetadataXattr(ctx, object.ID, "overflow", nil, model.XattrAlwaysSet); !errors.Is(err, model.ErrXattrStorageFull) {
		t.Fatalf("attribute count overflow = %v", err)
	}
	if after, _, _ := s.MetadataObject(ctx, object.ID); after != before {
		t.Fatal("quota failure changed object metadata")
	}
	setTestMetadata(t, s, object.ID, "key-000", []byte{1})
	if err := s.RemoveMetadataXattr(ctx, object.ID, "key-001"); err != nil {
		t.Fatal(err)
	}
	setTestMetadata(t, s, object.ID, "replacement", nil)
	names, err := s.ListMetadataXattrs(ctx, object.ID)
	if err != nil || len(names) != model.MaxXattrsPerObject {
		t.Fatalf("attribute count = %d, err=%v", len(names), err)
	}
}

func TestMetadataConcurrentCreateHasOneWinner(t *testing.T) {
	s, _ := metadataTestStore(t)
	object := bindTestMetadata(t, s, "file", "file")
	const writers = 16
	errorsByWriter := make([]error, writers)
	var wait sync.WaitGroup
	for i := range writers {
		wait.Go(func() {
			errorsByWriter[i] = s.SetMetadataXattr(context.Background(), object.ID, "key", []byte{byte(i)}, model.XattrMustCreate)
		})
	}
	wait.Wait()
	winners := 0
	for _, err := range errorsByWriter {
		if err == nil {
			winners++
		} else if !errors.Is(err, model.ErrXattrExists) {
			t.Fatalf("unexpected concurrent create error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("successful creates = %d, want 1", winners)
	}
}

func openSecondMetadataStore(t *testing.T, cfg model.RepoConfig) *Store {
	t.Helper()
	s, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := ensureMetadataSchema(context.Background(), s.db); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMetadataSeparateStoresMustCreate(t *testing.T) {
	s, cfg := metadataTestStore(t)
	other := openSecondMetadataStore(t, cfg)
	object := bindTestMetadata(t, s, "file", "file")
	const writers = 16
	results := make([]error, writers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range writers {
		wait.Go(func() {
			<-start
			store := s
			if i%2 != 0 {
				store = other
			}
			results[i] = store.SetMetadataXattr(context.Background(), object.ID, "key", []byte{byte(i)}, model.XattrMustCreate)
		})
	}
	close(start)
	wait.Wait()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, model.ErrXattrExists) {
			t.Fatalf("cross-store create returned a generic database error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("cross-store create winners = %d, want 1", winners)
	}
}

func TestMetadataSeparateStoresBindOneIdentity(t *testing.T) {
	s, cfg := metadataTestStore(t)
	other := openSecondMetadataStore(t, cfg)
	const callers = 16
	objects := make([]model.MetadataObject, callers)
	results := make([]error, callers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range callers {
		wait.Go(func() {
			<-start
			store := s
			if i%2 != 0 {
				store = other
			}
			objects[i], results[i] = store.BindMetadata(context.Background(), "file", "file")
		})
	}
	close(start)
	wait.Wait()
	for i := range callers {
		if results[i] != nil || objects[i] != objects[0] || objects[i].CtimeUnixNs != 0 {
			t.Fatalf("concurrent binding %d = %+v, err=%v, want %+v", i, objects[i], results[i], objects[0])
		}
	}
}

func TestMetadataSeparateStoresRespectAttributeQuota(t *testing.T) {
	s, cfg := metadataTestStore(t)
	other := openSecondMetadataStore(t, cfg)
	object := bindTestMetadata(t, s, "file", "file")
	const available = 4
	for i := range model.MaxXattrsPerObject - available {
		setTestMetadata(t, s, object.ID, fmt.Sprintf("existing-%03d", i), []byte{1})
	}
	const writers = 16
	results := make([]error, writers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range writers {
		wait.Go(func() {
			<-start
			store := s
			if i%2 != 0 {
				store = other
			}
			results[i] = store.SetMetadataXattr(context.Background(), object.ID, fmt.Sprintf("new-%03d", i), []byte{byte(i)}, model.XattrMustCreate)
		})
	}
	close(start)
	wait.Wait()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, model.ErrXattrStorageFull) {
			t.Fatalf("cross-store quota returned a generic database error: %v", err)
		}
	}
	if winners != available {
		t.Fatalf("cross-store quota winners = %d, want %d", winners, available)
	}
	names, err := s.ListMetadataXattrs(context.Background(), object.ID)
	if err != nil || len(names) != model.MaxXattrsPerObject {
		t.Fatalf("cross-store final count = %d, err=%v", len(names), err)
	}
}

func TestMetadataScalarRenameDetachesDestinationSubtree(t *testing.T) {
	s, _ := metadataTestStore(t)
	ctx := context.Background()
	source := bindTestMetadata(t, s, "source", "file")
	destination := bindTestMetadata(t, s, "destination", "dir")
	child := bindTestMetadata(t, s, "destination/child", "file")
	setTestMetadata(t, s, destination.ID, "retained", []byte{1})
	setTestMetadata(t, s, child.ID, "retained", []byte{2})
	metadataTestTransaction(t, s, func(tx *sql.Tx) error {
		return moveMetadataBindingsTx(ctx, tx, "source", "destination", false)
	}, true)
	if got, found := metadataBindingID(t, s, "destination"); !found || got != source.ID {
		t.Fatal("scalar rename did not preserve the source identity")
	}
	if _, found := metadataBindingID(t, s, "destination/child"); found {
		t.Fatal("scalar rename left a replaced directory child bound")
	}
	if !bytes.Equal(getTestMetadata(t, s, destination.ID, "retained"), []byte{1}) || !bytes.Equal(getTestMetadata(t, s, child.ID, "retained"), []byte{2}) {
		t.Fatal("scalar rename discarded replaced identities")
	}
}

func TestMetadataTypeReplacementDetachesDescendants(t *testing.T) {
	s, _ := metadataTestStore(t)
	ctx := context.Background()
	oldDir := bindTestMetadata(t, s, "node", "dir")
	child := bindTestMetadata(t, s, "node/child", "file")
	setTestMetadata(t, s, oldDir.ID, "old", []byte{1})
	setTestMetadata(t, s, child.ID, "child", []byte{2})
	replacement := bindTestMetadata(t, s, "node", "file")
	if replacement.ID == oldDir.ID || replacement.Type != "file" {
		t.Fatal("type replacement retained the old identity")
	}
	if _, bound := metadataBindingID(t, s, "node/child"); bound {
		t.Fatal("type replacement left a child binding visible")
	}
	if _, found, err := s.GetMetadataXattr(ctx, replacement.ID, "old"); found || err != nil {
		t.Fatalf("replacement inherited old attribute: found=%v, err=%v", found, err)
	}
	if !bytes.Equal(getTestMetadata(t, s, oldDir.ID, "old"), []byte{1}) || !bytes.Equal(getTestMetadata(t, s, child.ID, "child"), []byte{2}) {
		t.Fatal("detached identities lost attributes")
	}
	setTestMetadata(t, s, oldDir.ID, "old", []byte{3})
	if _, found, err := s.GetMetadataXattr(ctx, replacement.ID, "old"); found || err != nil {
		t.Fatalf("retained old identity write affected the replacement: found=%v, err=%v", found, err)
	}
}

func TestMetadataNamespaceHelpersPreserveRetainedObjects(t *testing.T) {
	s, _ := metadataTestStore(t)
	ctx := context.Background()
	source := bindTestMetadata(t, s, "docs", "dir")
	child := bindTestMetadata(t, s, "docs/deep/file", "file")
	neighbor := bindTestMetadata(t, s, "docs2/file", "file")
	destination := bindTestMetadata(t, s, "target", "dir")
	destinationChild := bindTestMetadata(t, s, "target/old", "file")
	for i, object := range []model.MetadataObject{source, child, neighbor, destination, destinationChild} {
		setTestMetadata(t, s, object.ID, "key", []byte{byte(i)})
	}
	metadataTestTransaction(t, s, func(tx *sql.Tx) error {
		return moveMetadataBindingsTx(ctx, tx, "docs", "target", true)
	}, true)
	for path, want := range map[string]model.MetadataObjectID{"target": source.ID, "target/deep/file": child.ID, "docs2/file": neighbor.ID} {
		if got, found := metadataBindingID(t, s, path); !found || got != want {
			t.Fatalf("moved binding: found=%v, got %q, want %q", found, got, want)
		}
	}
	for _, path := range []string{"docs", "docs/deep/file", "target/old"} {
		if _, found := metadataBindingID(t, s, path); found {
			t.Fatal("old namespace binding survived replacement")
		}
	}
	if got := getTestMetadata(t, s, destination.ID, "key"); !bytes.Equal(got, []byte{3}) {
		t.Fatal("overwritten destination lost retained attributes")
	}
	if got := getTestMetadata(t, s, destinationChild.ID, "key"); !bytes.Equal(got, []byte{4}) {
		t.Fatal("overwritten destination child lost retained attributes")
	}
	metadataTestTransaction(t, s, func(tx *sql.Tx) error {
		return moveMetadataBindingsTx(ctx, tx, "target", "rolled-back", true)
	}, false)
	if got, found := metadataBindingID(t, s, "target"); !found || got != source.ID {
		t.Fatal("rolled-back move changed the source binding")
	}
	if _, found := metadataBindingID(t, s, "rolled-back"); found {
		t.Fatal("rolled-back move published its destination")
	}
	metadataTestTransaction(t, s, func(tx *sql.Tx) error {
		return moveMetadataBindingsTx(ctx, tx, "target", "target", true)
	}, true)
	metadataTestTransaction(t, s, func(tx *sql.Tx) error {
		return detachMetadataBindingsTx(ctx, tx, "target", true)
	}, true)
	if _, found := metadataBindingID(t, s, "target/deep/file"); found {
		t.Fatal("removed tree retained a child binding")
	}
	if got := getTestMetadata(t, s, child.ID, "key"); !bytes.Equal(got, []byte{1}) {
		t.Fatal("unlink affected attributes on a retained identity")
	}
	setTestMetadata(t, s, child.ID, "key", []byte{9})
	newChild := bindTestMetadata(t, s, "target/deep/file", "file")
	if newChild.ID == child.ID {
		t.Fatal("same-path replacement reused the detached identity")
	}
	if _, found, err := s.GetMetadataXattr(ctx, newChild.ID, "key"); found || err != nil {
		t.Fatalf("retained unlinked identity write affected the new file: found=%v, err=%v", found, err)
	}
	metadataTestTransaction(t, s, func(tx *sql.Tx) error {
		_, err := createMetadataBindingTx(ctx, tx, "docs2/file", "file", true)
		return err
	}, true)
	if got, found := metadataBindingID(t, s, "docs2/file"); !found || got == neighbor.ID {
		t.Fatal("same-type replacement retained the old identity")
	}
	if got := getTestMetadata(t, s, neighbor.ID, "key"); !bytes.Equal(got, []byte{2}) {
		t.Fatal("same-type replacement changed retained attributes")
	}
}

func TestMetadataRestartAndExplicitDetachedCollection(t *testing.T) {
	s, cfg := metadataTestStore(t)
	ctx := context.Background()
	bound := bindTestMetadata(t, s, "live", "file")
	detached := bindTestMetadata(t, s, "removed", "file")
	setTestMetadata(t, s, bound.ID, "live", []byte{0, 0xff})
	setTestMetadata(t, s, detached.ID, "retained", []byte{0xfe, 0})
	metadataTestTransaction(t, s, func(tx *sql.Tx) error {
		return detachMetadataBindingsTx(ctx, tx, "removed", false)
	}, true)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if err := ensureMetadataSchema(ctx, reopened.db); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(getTestMetadata(t, reopened, bound.ID, "live"), []byte{0, 0xff}) || !bytes.Equal(getTestMetadata(t, reopened, detached.ID, "retained"), []byte{0xfe, 0}) {
		t.Fatal("restart lost bound or detached attributes")
	}
	if again := bindTestMetadata(t, reopened, "live", "file"); again.ID != bound.ID {
		t.Fatal("restart replaced a persistent metadata identity")
	}
	if has, err := reopened.HasMetadataXattrs(ctx); err != nil || !has {
		t.Fatalf("persistent attributes not reported: has=%v, err=%v", has, err)
	}
	// The test has no mounted/inode users; it can explicitly collect detached
	// identities without invalidating any live filesystem reference.
	if err := reopened.CollectDetachedMetadata(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := reopened.MetadataObject(ctx, detached.ID); found || err != nil {
		t.Fatalf("detached object survived explicit GC: found=%v, err=%v", found, err)
	}
	if _, _, err := reopened.GetMetadataXattr(ctx, detached.ID, "retained"); !errors.Is(err, model.ErrMetadataObjectNotFound) {
		t.Fatalf("collected object attribute = %v", err)
	}
	if !bytes.Equal(getTestMetadata(t, reopened, bound.ID, "live"), []byte{0, 0xff}) {
		t.Fatal("collection affected a bound identity")
	}
	var orphanAttrs int
	if err := reopened.db.QueryRowContext(ctx, `SELECT count(*) FROM metadata_xattrs WHERE object_id=?`, detached.ID).Scan(&orphanAttrs); err != nil || orphanAttrs != 0 {
		t.Fatalf("collected object left orphan attributes: count=%d, err=%v", orphanAttrs, err)
	}
	metadataTestTransaction(t, reopened, func(tx *sql.Tx) error {
		return detachMetadataBindingsTx(ctx, tx, ".", true)
	}, true)
	if has, err := reopened.HasMetadataXattrs(ctx); err != nil || !has {
		t.Fatalf("detached-only attributes not reported: has=%v, err=%v", has, err)
	}
	if err := reopened.CollectDetachedMetadata(ctx); err != nil {
		t.Fatal(err)
	}
	if has, err := reopened.HasMetadataXattrs(ctx); err != nil || has {
		t.Fatalf("collected last detached attribute: has=%v, err=%v", has, err)
	}
}
