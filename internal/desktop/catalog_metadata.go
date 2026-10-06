package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
)

// Catalogue attributes belong to the desktop catalogue, independently of any
// repository's downloadable working tree or eviction transaction.
func openCatalogueMetadata(ctx context.Context, stateDir string) (*overlay.Store, error) {
	root := filepath.Join(stateDir, "catalog-metadata")
	upper := filepath.Join(root, "overlay")
	for _, dir := range []string{root, upper, filepath.Join(upper, "upper")} {
		if err := privateDirectory(dir, true); err != nil {
			return nil, err
		}
	}
	database := filepath.Join(root, "catalog-overlay.db")
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Lstat(database + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
			return nil, errors.New("catalogue metadata must use regular files owned by the current user")
		}
	}
	file, err := os.OpenFile(database, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	if statErr == nil && (!info.Mode().IsRegular() || !ownedByCurrentUser(info)) {
		statErr = errors.New("catalogue metadata database must be a regular file owned by the current user")
	}
	if statErr == nil {
		statErr = file.Chmod(0o600)
	}
	if err := errors.Join(statErr, file.Close()); err != nil {
		return nil, err
	}
	return overlay.New(ctx, model.RepoConfig{
		ID: "catalogue", OverlayDir: upper, OverlayDBPath: database,
	})
}

// closeCatalogueStoreLocked requires lifecycle. The caller has established
// that there is no mount using this session (or that mounting never began).
// A failed close retains ownership so shutdown or the next mount can retry.
func (s *Service) closeCatalogueStoreLocked() error {
	s.mu.Lock()
	metadata, catalogue, mounted := s.catalogMetadata, s.catalog, s.mounted
	s.mu.Unlock()
	if mounted != nil {
		return errors.New("catalogue metadata is still in use by a mounted filesystem")
	}
	if catalogue != nil {
		catalogue.Destroy()
	}
	if metadata != nil {
		if err := s.closeCatalogueMetadata(metadata); err != nil {
			return fmt.Errorf("close catalogue metadata: %w", err)
		}
	}
	s.mu.Lock()
	s.catalog, s.catalogMetadata = nil, nil
	s.mu.Unlock()
	return nil
}
