package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/registry"
)

type storageTransaction struct {
	Version         int              `json:"version"`
	Original        model.RepoConfig `json:"original"`
	DisabledVersion string           `json:"disabledVersion"`
	dir             string
}

func repositoryStoragePaths(cfg model.RepoConfig) []string {
	return []string{filepath.Dir(cfg.GitDir), cfg.OverlayDir, cfg.BlobCacheDir,
		cfg.MetaDBPath, cfg.MetaDBPath + "-wal", cfg.MetaDBPath + "-shm"}
}

func (s *Service) newStorageTransaction(original, disabled model.RepoConfig) (*storageTransaction, error) {
	dir, err := os.MkdirTemp(s.root, ".free-space-"+string(original.ID)+"-")
	if err != nil {
		return nil, err
	}
	transaction := &storageTransaction{Version: 1, Original: original, DisabledVersion: disabled.ConfigVersion, dir: dir}
	data, err := json.Marshal(transaction)
	if err != nil {
		_ = os.Remove(dir)
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "transaction.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.Remove(dir)
		return nil, err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := syncStorageDirectory(dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := syncStorageDirectory(s.root); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return transaction, nil
}

func syncStorageDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func removeStorageTransaction(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return syncStorageDirectory(filepath.Dir(path))
}

// RecoverStorageTransactions must run at startup before mounts or catalogue
// activation. It rolls back interrupted eviction, or finishes deleting an
// eviction whose engine-registration removal committed. Unknown/colliding data
// is retained and reported rather than overwritten.
func (s *Service) RecoverStorageTransactions(ctx context.Context) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".free-space-") {
			continue
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected storage transaction entry; repository data was retained")
		}
		dir := filepath.Join(s.root, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "transaction.json"))
		if err != nil {
			// A kill before the manifest was written can leave an empty directory;
			// nothing was disabled or moved yet, so removing an empty dir is safe.
			if errors.Is(err, os.ErrNotExist) {
				if err := os.Remove(dir); err == nil {
					continue
				}
			}
			return fmt.Errorf("read retained storage transaction %s: %w", dir, err)
		}
		var transaction storageTransaction
		if err := json.Unmarshal(data, &transaction); err != nil {
			return fmt.Errorf("read retained storage transaction %s: %w", dir, err)
		}
		transaction.dir = dir
		if err := s.validateStorageTransaction(transaction); err != nil {
			return err
		}
		cfg := transaction.Original
		if err := s.withRepoPrepareLock(ctx, cfg.Name, func() error {
			return s.withRepoConfigLock(ctx, cfg.Name, func() error {
				latest, err := s.registry.GetRepo(ctx, cfg.Name)
				if errors.Is(err, registry.ErrRepoNotFound) {
					return removeStorageTransaction(dir) // Registration removal committed.
				}
				if err != nil {
					return err
				}
				if latest.ConfigVersion != transaction.DisabledVersion && latest.ConfigVersion != cfg.ConfigVersion {
					pending := false
					for i := range repositoryStoragePaths(cfg) {
						if _, err := os.Lstat(filepath.Join(dir, fmt.Sprintf("%d", i))); err == nil {
							pending = true
						} else if !errors.Is(err, os.ErrNotExist) {
							return err
						}
					}
					if !pending {
						return removeStorageTransaction(dir) // Rollback committed before journal cleanup.
					}
					return errors.New("storage transaction conflicts with a newer repository configuration; retained data needs recovery")
				}
				for i, destination := range repositoryStoragePaths(cfg) {
					source := filepath.Join(dir, fmt.Sprintf("%d", i))
					if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
						continue
					} else if err != nil {
						return err
					}
					if _, err := os.Lstat(destination); err == nil {
						return fmt.Errorf("storage recovery collision at %s; retained data remains at %s", destination, source)
					} else if !errors.Is(err, os.ErrNotExist) {
						return err
					}
					if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
						return err
					}
					if err := os.Rename(source, destination); err != nil {
						return err
					}
					if err := errors.Join(syncStorageDirectory(filepath.Dir(destination)), syncStorageDirectory(dir)); err != nil {
						return err
					}
				}
				// A previous recovery attempt may have restored a path and then
				// failed its directory sync. Flush all surviving parents before
				// making the registration usable or removing the journal.
				for _, path := range repositoryStoragePaths(cfg) {
					parent := filepath.Dir(path)
					if _, err := os.Stat(parent); errors.Is(err, os.ErrNotExist) {
						continue
					} else if err != nil {
						return err
					}
					if err := syncStorageDirectory(parent); err != nil {
						return err
					}
				}
				if err := syncStorageDirectory(dir); err != nil {
					return err
				}
				if latest.ConfigVersion == transaction.DisabledVersion {
					cfg.ConfigVersion = rand.Text()
					if err := s.registry.AddRepo(ctx, cfg); err != nil {
						return err
					}
				}
				return removeStorageTransaction(dir)
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) validateStorageTransaction(transaction storageTransaction) error {
	cfg := transaction.Original
	if transaction.Version != 1 || transaction.DisabledVersion == "" {
		return errors.New("unrecognized storage transaction; retained data needs recovery")
	}
	if err := model.ValidateRepoName(cfg.Name); err != nil {
		return err
	}
	if err := model.ValidateRepoName(string(cfg.ID)); err != nil {
		return err
	}
	expected := model.RepoConfig{ID: cfg.ID, Name: cfg.Name}
	s.fillPaths(&expected)
	if cfg.PreparedGitDir || cfg.GitDir != expected.GitDir || cfg.OverlayDir != expected.OverlayDir || cfg.BlobCacheDir != expected.BlobCacheDir || cfg.MetaDBPath != expected.MetaDBPath || cfg.OverlayDBPath != expected.OverlayDBPath {
		return errors.New("storage transaction contains unexpected paths; retained data needs recovery")
	}
	// Recovery must not follow a replaced storage parent outside the state root.
	for _, path := range repositoryStoragePaths(cfg) {
		for parent := filepath.Dir(path); parent != filepath.Clean(s.root); parent = filepath.Dir(parent) {
			if parent == filepath.Dir(parent) {
				return errors.New("storage transaction escapes its state root")
			}
			info, err := os.Lstat(parent)
			if err == nil && info.Mode()&os.ModeSymlink != 0 {
				return errors.New("storage transaction has a symbolic link parent; retained data needs recovery")
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	entries, err := os.ReadDir(transaction.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("storage transaction contains a symbolic link; retained data needs recovery")
		}
		if entry.Name() != "transaction.json" && entry.Name() != "0" && entry.Name() != "1" && entry.Name() != "2" && entry.Name() != "3" && entry.Name() != "4" && entry.Name() != "5" {
			return errors.New("storage transaction contains unknown files; retained data needs recovery")
		}
	}
	return nil
}
