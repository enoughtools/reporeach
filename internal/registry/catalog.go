package registry

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// SetCatalogMountPaths changes the registered catalogue's mount locations in a
// single transaction. The supplied map may include dormant catalogue entries,
// but must contain a path for every registered repository. All other config
// fields, including staged preparation state and config versions, are retained.
// The caller must stop runtimes and serialize repository lifecycle changes.
func (s *Store) SetCatalogMountPaths(ctx context.Context, root string, paths map[string]string) error {
	if !filepath.IsAbs(root) || model.CleanPath(root) == "." || strings.IndexByte(root, 0) >= 0 {
		return errors.New("catalogue mount root must be an absolute directory")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT repo_id, name, config_version FROM repos ORDER BY name`)
	if err != nil {
		return err
	}
	type target struct {
		id, name, version, path string
	}
	var targets []target
	seen := map[string]bool{}
	for rows.Next() {
		var item target
		if err := rows.Scan(&item.id, &item.name, &item.version); err != nil {
			_ = rows.Close()
			return err
		}
		item.path = paths[item.name]
		if !filepath.IsAbs(item.path) || strings.IndexByte(item.path, 0) >= 0 {
			_ = rows.Close()
			return fmt.Errorf("catalogue is missing an absolute path for repository %s", item.name)
		}
		rel, err := filepath.Rel(root, item.path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			_ = rows.Close()
			return fmt.Errorf("repository %s is outside the catalogue mount root", item.name)
		}
		key := model.CleanPath(item.path)
		if seen[key] {
			_ = rows.Close()
			return errors.New("catalogue repositories have duplicate mount paths")
		}
		seen[key] = true
		targets = append(targets, item)
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	now := time.Now().UnixNano()
	for _, item := range targets {
		result, err := tx.ExecContext(ctx, `UPDATE repos SET mount_root=?, mount_path=?, updated_at_ns=? WHERE repo_id=? AND config_version=?`, root, item.path, now, item.id, item.version)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrRepoChanged
		}
	}
	return tx.Commit()
}
