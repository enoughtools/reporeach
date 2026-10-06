package overlay

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// Metadata identities are independent of byte overlays. Reconciliation can
// remove a committed overlay without removing local attributes, and namespace
// removal detaches an identity without invalidating retained inode references.
var metadataMigrations = []string{
	`CREATE TABLE IF NOT EXISTS metadata_objects (
	  id TEXT PRIMARY KEY CHECK(length(id)=32 AND id NOT GLOB '*[^0-9a-f]*'),
	  node_type TEXT NOT NULL CHECK(node_type IN ('file','dir','symlink')),
	  ctime_unix_ns INTEGER NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS metadata_bindings (
	  path TEXT PRIMARY KEY,
	  object_id TEXT NOT NULL UNIQUE REFERENCES metadata_objects(id)
	);`,
	`CREATE TABLE IF NOT EXISTS metadata_xattrs (
	  object_id TEXT NOT NULL REFERENCES metadata_objects(id) ON DELETE CASCADE,
	  name TEXT COLLATE BINARY NOT NULL CHECK(length(CAST(name AS BLOB)) BETWEEN 1 AND 127 AND instr(name,char(0))=0),
	  value BLOB NOT NULL CHECK(typeof(value)='blob' AND length(value)<=1048576),
	  PRIMARY KEY(object_id,name)
	);`,
}

func ensureMetadataSchema(ctx context.Context, db *sql.DB) error {
	return meta.ExecMigrations(ctx, db, metadataMigrations)
}

// beginMetadataTransaction acquires SQLite's writer reservation before reading
// policy or quota state. Store.mu only serializes one Store instance; a deferred
// read transaction cannot safely upgrade after another instance commits in WAL
// mode. The zero-row UPDATE acquires write intent without changing any object.
func beginMetadataTransaction(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metadata_objects SET id=id WHERE 0`); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (s *Store) BindMetadata(ctx context.Context, path, nodeType string) (model.MetadataObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := beginMetadataTransaction(ctx, s.db)
	if err != nil {
		return model.MetadataObject{}, err
	}
	defer tx.Rollback()
	object, err := createMetadataBindingTx(ctx, tx, path, nodeType, false)
	if err != nil {
		return model.MetadataObject{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.MetadataObject{}, err
	}
	return object, nil
}

// createMetadataBindingTx runs in the namespace owner's transaction while it
// holds Store.mu. Only a caller that has established a live object may bind a
// path. replace creates a fresh identity even when the node type is unchanged.
func createMetadataBindingTx(ctx context.Context, tx *sql.Tx, path, nodeType string, replace bool) (model.MetadataObject, error) {
	path = model.CleanPath(path)
	if nodeType != "file" && nodeType != "dir" && nodeType != "symlink" {
		return model.MetadataObject{}, model.ErrInvalidXattr
	}
	var object model.MetadataObject
	err := tx.QueryRowContext(ctx, `SELECT o.id,o.node_type,o.ctime_unix_ns FROM metadata_bindings b JOIN metadata_objects o ON o.id=b.object_id WHERE b.path=?`, path).Scan(&object.ID, &object.Type, &object.CtimeUnixNs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return model.MetadataObject{}, err
	}
	if err == nil && object.Type == nodeType && !replace {
		return object, nil
	}
	if err == nil && object.Type != nodeType {
		if err := detachMetadataBindingsTx(ctx, tx, path, true); err != nil {
			return model.MetadataObject{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM metadata_bindings WHERE path=?`, path); err != nil {
			return model.MetadataObject{}, err
		}
	}
	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return model.MetadataObject{}, err
	}
	object = model.MetadataObject{
		ID:   model.MetadataObjectID(hex.EncodeToString(randomID[:])),
		Type: nodeType,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO metadata_objects(id,node_type,ctime_unix_ns) VALUES(?,?,?)`, object.ID, object.Type, object.CtimeUnixNs); err != nil {
		return model.MetadataObject{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO metadata_bindings(path,object_id) VALUES(?,?)`, path, object.ID); err != nil {
		return model.MetadataObject{}, err
	}
	return object, nil
}

// detachMetadataBindingsTx removes namespace bindings, never the objects that
// an existing inode may still reference. tree includes the path itself.
func detachMetadataBindingsTx(ctx context.Context, tx *sql.Tx, path string, tree bool) error {
	path = model.CleanPath(path)
	if !tree {
		_, err := tx.ExecContext(ctx, `DELETE FROM metadata_bindings WHERE path=?`, path)
		return err
	}
	if path == "." {
		_, err := tx.ExecContext(ctx, `DELETE FROM metadata_bindings`)
		return err
	}
	prefix := path + "/"
	_, err := tx.ExecContext(ctx, `DELETE FROM metadata_bindings WHERE path=? OR substr(path,1,length(?))=?`, path, prefix, prefix)
	return err
}

// moveMetadataBindingsTx moves all source identities and detaches replaced
// destination identities in the same namespace transaction. It also moves
// metadata-only descendants, which need not have an overlay_entries row.
func moveMetadataBindingsTx(ctx context.Context, tx *sql.Tx, oldPath, newPath string, tree bool) error {
	oldPath, newPath = model.CleanPath(oldPath), model.CleanPath(newPath)
	if oldPath == newPath {
		return nil
	}
	if tree && (oldPath == "." || newPath == "." || strings.HasPrefix(newPath, oldPath+"/") || strings.HasPrefix(oldPath, newPath+"/")) {
		return model.ErrInvalidXattr
	}
	query := `SELECT path,object_id FROM metadata_bindings WHERE path=?`
	args := []any{oldPath}
	if tree {
		prefix := oldPath + "/"
		query += ` OR substr(path,1,length(?))=?`
		args = append(args, prefix, prefix)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	type binding struct {
		path string
		id   model.MetadataObjectID
	}
	var bindings []binding
	for rows.Next() {
		var b binding
		if err := rows.Scan(&b.path, &b.id); err != nil {
			rows.Close()
			return err
		}
		bindings = append(bindings, b)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := detachMetadataBindingsTx(ctx, tx, newPath, true); err != nil {
		return err
	}
	if err := detachMetadataBindingsTx(ctx, tx, oldPath, tree); err != nil {
		return err
	}
	for _, b := range bindings {
		destination := model.CleanPath(newPath + strings.TrimPrefix(b.path, oldPath))
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata_bindings(path,object_id) VALUES(?,?)`, destination, b.id); err != nil {
			return err
		}
	}
	return nil
}

func validMetadataObjectID(id model.MetadataObjectID) bool {
	if len(id) != 32 {
		return false
	}
	for i := range len(id) {
		b := id[i]
		if b < '0' || (b > '9' && b < 'a') || b > 'f' {
			return false
		}
	}
	return true
}

func validateMetadataXattrName(name string) error {
	if name == "" || len(name) > model.MaxXattrNameBytes || !utf8.ValidString(name) || strings.IndexByte(name, 0) >= 0 {
		return model.ErrInvalidXattr
	}
	return nil
}

func (s *Store) MetadataObject(ctx context.Context, id model.MetadataObjectID) (model.MetadataObject, bool, error) {
	if !validMetadataObjectID(id) {
		return model.MetadataObject{}, false, nil
	}
	var object model.MetadataObject
	err := s.db.QueryRowContext(ctx, `SELECT id,node_type,ctime_unix_ns FROM metadata_objects WHERE id=?`, id).Scan(&object.ID, &object.Type, &object.CtimeUnixNs)
	if errors.Is(err, sql.ErrNoRows) {
		return model.MetadataObject{}, false, nil
	}
	return object, err == nil, err
}

func (s *Store) GetMetadataXattr(ctx context.Context, id model.MetadataObjectID, name string) ([]byte, bool, error) {
	if err := validateMetadataXattrName(name); err != nil {
		return nil, false, err
	}
	if !validMetadataObjectID(id) {
		return nil, false, model.ErrMetadataObjectNotFound
	}
	var value []byte
	var storedName sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT x.name,x.value FROM metadata_objects o LEFT JOIN metadata_xattrs x ON x.object_id=o.id AND x.name=? WHERE o.id=?`, name, id).Scan(&storedName, &value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, model.ErrMetadataObjectNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if !storedName.Valid {
		return nil, false, nil
	}
	if value == nil {
		value = []byte{}
	}
	return value, true, nil
}

func (s *Store) ListMetadataXattrs(ctx context.Context, id model.MetadataObjectID) ([]string, error) {
	if !validMetadataObjectID(id) {
		return nil, model.ErrMetadataObjectNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT x.name FROM metadata_objects o LEFT JOIN metadata_xattrs x ON x.object_id=o.id WHERE o.id=? ORDER BY x.name COLLATE BINARY`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	found := false
	for rows.Next() {
		found = true
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if name.Valid {
			names = append(names, name.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, model.ErrMetadataObjectNotFound
	}
	return names, nil
}

type metadataXattrUsage struct {
	count       int64
	objectBytes int64
	storeBytes  int64
}

// checkMetadataXattrUsage subtracts replaced bytes before checking limits, so a
// full object/store can still replace an attribute with a smaller value.
func checkMetadataXattrUsage(usage metadataXattrUsage, exists bool, oldBytes, newBytes int64) error {
	if !exists {
		usage.count++
	}
	delta := newBytes - oldBytes
	if usage.count > model.MaxXattrsPerObject || usage.objectBytes > model.MaxXattrObjectBytes-delta || usage.storeBytes > model.MaxXattrStoreBytes-delta {
		return model.ErrXattrStorageFull
	}
	return nil
}

func requireMetadataObjectTx(ctx context.Context, tx *sql.Tx, id model.MetadataObjectID) error {
	if !validMetadataObjectID(id) {
		return model.ErrMetadataObjectNotFound
	}
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM metadata_objects WHERE id=?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ErrMetadataObjectNotFound
	}
	return err
}

func updateMetadataCtimeTx(ctx context.Context, tx *sql.Tx, id model.MetadataObjectID) error {
	_, err := tx.ExecContext(ctx, `UPDATE metadata_objects SET ctime_unix_ns=max(ctime_unix_ns+1,?) WHERE id=?`, time.Now().UnixNano(), id)
	return err
}

func (s *Store) SetMetadataXattr(ctx context.Context, id model.MetadataObjectID, name string, value []byte, policy model.XattrSetPolicy) error {
	if err := validateMetadataXattrName(name); err != nil {
		return err
	}
	if len(value) > model.MaxXattrValueBytes {
		return model.ErrXattrTooLarge
	}
	if policy != model.XattrAlwaysSet && policy != model.XattrMustCreate && policy != model.XattrMustReplace {
		return model.ErrInvalidXattr
	}
	// database/sql binds a nil []byte as NULL. Empty attributes are real BLOBs.
	if value == nil {
		value = []byte{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := beginMetadataTransaction(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireMetadataObjectTx(ctx, tx, id); err != nil {
		return err
	}
	var oldBytes int64
	err = tx.QueryRowContext(ctx, `SELECT length(value) FROM metadata_xattrs WHERE object_id=? AND name=?`, id, name).Scan(&oldBytes)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if policy == model.XattrMustCreate && exists {
		return model.ErrXattrExists
	}
	if policy == model.XattrMustReplace && !exists {
		return model.ErrXattrNotFound
	}
	var usage metadataXattrUsage
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(length(value)),0),(SELECT coalesce(sum(length(value)),0) FROM metadata_xattrs) FROM metadata_xattrs WHERE object_id=?`, id).Scan(&usage.count, &usage.objectBytes, &usage.storeBytes); err != nil {
		return err
	}
	if err := checkMetadataXattrUsage(usage, exists, oldBytes, int64(len(value))); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO metadata_xattrs(object_id,name,value) VALUES(?,?,?) ON CONFLICT(object_id,name) DO UPDATE SET value=excluded.value`, id, name, value); err != nil {
		return err
	}
	if err := updateMetadataCtimeTx(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveMetadataXattr(ctx context.Context, id model.MetadataObjectID, name string) error {
	if err := validateMetadataXattrName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := beginMetadataTransaction(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireMetadataObjectTx(ctx, tx, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM metadata_xattrs WHERE object_id=? AND name=?`, id, name)
	if err != nil {
		return err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if removed == 0 {
		return model.ErrXattrNotFound
	}
	if err := updateMetadataCtimeTx(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// HasMetadataXattrs includes detached identities until quiescent collection.
// Free-space callers must not discard attributes retained by an old inode.
func (s *Store) HasMetadataXattrs(ctx context.Context) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata_xattrs)`).Scan(&found)
	return found, err
}

// CollectDetachedMetadata is safe only after every mount and retained inode
// reference to this store has drained. Ordinary open/reopen never calls it.
func (s *Store) CollectDetachedMetadata(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM metadata_objects WHERE NOT EXISTS(SELECT 1 FROM metadata_bindings b WHERE b.object_id=metadata_objects.id)`)
	return err
}
