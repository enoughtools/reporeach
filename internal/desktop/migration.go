package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

const rootMigrationFile = "rootmount-migration.json"

// A failed rollback leaves the catalogue detached. Startup recovery must finish
// before repository operations or mounts may resume.
var errRootMigrationRecoveryNeeded = errors.New("mount folder migration requires recovery; restart EnoughRepos")

type rootMigration struct {
	SchemaVersion int                          `json:"schemaVersion"`
	OldRoot       string                       `json:"oldRoot"`
	NewRoot       string                       `json:"newRoot"`
	Paths         map[string]rootMigrationPath `json:"paths"`
}

type rootMigrationPath struct {
	RepositoryID string  `json:"repositoryID"`
	OldPath      string  `json:"oldPath"`
	NewPath      string  `json:"newPath"`
	GitPresent   bool    `json:"gitPresent"`
	OldWorktree  *string `json:"oldWorktree"`
}

// migrateRoot runs with the manager lifecycle lock held, maintenance enabled,
// the catalogue detached, and every engine runtime stopped. mu is not held.
// The durable catalogue root is the commit record: a crash before its replacement
// rolls back; a crash after replacement finishes the new location on startup.
func (s *Service) migrateRoot(ctx context.Context, root string) error {
	s.mu.Lock()
	state := s.state
	state.Repositories = append([]Repository(nil), s.state.Repositories...)
	s.mu.Unlock()
	if root == state.MountRoot {
		return nil
	}
	journalPath := filepath.Join(s.opts.StateDir, rootMigrationFile)
	if _, err := os.Lstat(journalPath); err == nil {
		return errRootMigrationRecoveryNeeded
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	configs, err := s.engine.ListRepos(ctx)
	if err != nil {
		return err
	}
	byName := map[string]Repository{}
	for _, repo := range state.Repositories {
		byName[engineName(repo.ID)] = repo
	}
	journal := rootMigration{SchemaVersion: 1, OldRoot: state.MountRoot, NewRoot: root, Paths: map[string]rootMigrationPath{}}
	for _, cfg := range configs {
		repo, ok := byName[cfg.Name]
		if !ok {
			return errors.New("registered repository is absent from the catalogue")
		}
		entry := rootMigrationPath{
			RepositoryID: repo.ID,
			OldPath:      filepath.Join(state.MountRoot, repo.Owner, repo.Name),
			NewPath:      filepath.Join(root, repo.Owner, repo.Name),
		}
		if cfg.MountRoot != state.MountRoot || cfg.MountPath != entry.OldPath {
			return errors.New("registered repository has an unexpected mount location")
		}
		entry.GitPresent, err = s.migrationGitDirectory(cfg)
		if err != nil {
			return err
		}
		if entry.GitPresent {
			entry.OldWorktree, err = readMigrationWorktree(ctx, cfg.GitDir)
			if err != nil {
				return err
			}
		}
		journal.Paths[cfg.Name] = entry
	}
	if err := s.validateRootMigration(journal, configs, state); err != nil {
		return err
	}
	if err := writeRootMigration(journalPath, journal); err != nil {
		return err
	}
	if err := s.applyRootMigration(ctx, journal, true); err != nil {
		return s.rollbackRootMigration(journal, err)
	}
	s.mu.Lock()
	s.state.MountRoot = root
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return s.rollbackRootMigration(journal, err)
	}
	// Once the catalogue commits, keep the new root even if cleanup fails. A
	// retained journal will idempotently finish this direction on the next start.
	if err := removeRootMigration(journalPath); err != nil {
		return errors.Join(errRootMigrationRecoveryNeeded, err)
	}
	return nil
}

func (s *Service) rollbackRootMigration(journal rootMigration, cause error) error {
	// Cancellation of the settings request must not cancel the rollback.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.applyRootMigration(ctx, journal, false); err != nil {
		return errors.Join(cause, errRootMigrationRecoveryNeeded, fmt.Errorf("roll back repository paths: %w", err))
	}
	s.mu.Lock()
	s.state.MountRoot = journal.OldRoot
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return errors.Join(cause, errRootMigrationRecoveryNeeded, fmt.Errorf("roll back catalogue: %w", err))
	}
	if err := removeRootMigration(filepath.Join(s.opts.StateDir, rootMigrationFile)); err != nil {
		return errors.Join(cause, errRootMigrationRecoveryNeeded, err)
	}
	return cause
}

// recoverRootMigration must run immediately after opening the engine, before
// reconciling catalogue state or starting any mounts or repository operations.
func (s *Service) recoverRootMigration(ctx context.Context) error {
	path := filepath.Join(s.opts.StateDir, rootMigrationFile)
	journal, err := readRootMigration(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read mount folder migration: %w", err)
	}
	s.mu.Lock()
	state := s.state
	state.Repositories = append([]Repository(nil), s.state.Repositories...)
	s.mu.Unlock()
	if state.MountRoot != journal.OldRoot && state.MountRoot != journal.NewRoot {
		return errors.New("mount folder migration does not match the saved catalogue")
	}
	configs, err := s.engine.ListRepos(ctx)
	if err != nil {
		return err
	}
	if err := s.validateRootMigration(journal, configs, state); err != nil {
		return fmt.Errorf("invalid mount folder migration: %w", err)
	}
	if err := s.applyRootMigration(ctx, journal, state.MountRoot == journal.NewRoot); err != nil {
		return fmt.Errorf("recover mount folder migration: %w", err)
	}
	return removeRootMigration(path)
}

func (s *Service) validateRootMigration(journal rootMigration, configs []model.RepoConfig, state persistedState) error {
	if journal.SchemaVersion != 1 || journal.Paths == nil {
		return errors.New("unsupported migration schema")
	}
	for _, root := range []string{journal.OldRoot, journal.NewRoot} {
		if err := validateMountRoot(root); err != nil {
			return err
		}
		if pathsOverlap(root, s.opts.StateDir) {
			return errors.New("migration root overlaps private state")
		}
	}
	if model.CleanPath(journal.OldRoot) == model.CleanPath(journal.NewRoot) {
		return errors.New("migration roots are identical")
	}
	if len(journal.Paths) != len(configs) {
		return errors.New("migration does not match registered repositories")
	}
	byID := map[string]Repository{}
	for _, repo := range state.Repositories {
		if err := validateRepository(repo); err != nil {
			return err
		}
		byID[repo.ID] = repo
	}
	for _, cfg := range configs {
		entry, ok := journal.Paths[cfg.Name]
		repo, found := byID[entry.RepositoryID]
		if !ok || !found || cfg.Name != engineName(repo.ID) || cfg.ID != model.RepoID(cfg.Name) {
			return errors.New("migration contains an unknown repository")
		}
		if entry.OldPath != filepath.Join(journal.OldRoot, repo.Owner, repo.Name) || entry.NewPath != filepath.Join(journal.NewRoot, repo.Owner, repo.Name) {
			return errors.New("migration contains an unexpected repository path")
		}
		if !((cfg.MountRoot == journal.OldRoot && cfg.MountPath == entry.OldPath) || (cfg.MountRoot == journal.NewRoot && cfg.MountPath == entry.NewPath)) {
			return errors.New("registered repository is outside migration roots")
		}
		present, err := s.migrationGitDirectory(cfg)
		if err != nil {
			return err
		}
		if present != entry.GitPresent || (!present && entry.OldWorktree != nil) {
			return errors.New("repository storage changed during mount folder migration")
		}
		if entry.OldWorktree != nil && (!filepath.IsAbs(*entry.OldWorktree) || model.CleanPath(*entry.OldWorktree) != model.CleanPath(entry.OldPath)) {
			return errors.New("repository has an unexpected original Git worktree")
		}
	}
	return nil
}

func (s *Service) applyRootMigration(ctx context.Context, journal rootMigration, forward bool) error {
	configs, err := s.engine.ListRepos(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	state := s.state
	state.Repositories = append([]Repository(nil), s.state.Repositories...)
	s.mu.Unlock()
	if err := s.validateRootMigration(journal, configs, state); err != nil {
		return err
	}
	// Validate every existing Git worktree before making the first mutation.
	worktrees := make(map[string]*string, len(configs))
	for _, cfg := range configs {
		entry := journal.Paths[cfg.Name]
		if !entry.GitPresent {
			continue
		}
		current, err := readMigrationWorktree(ctx, cfg.GitDir)
		if err != nil {
			return err
		}
		original := (current == nil && entry.OldWorktree == nil) || (current != nil && entry.OldWorktree != nil && *current == *entry.OldWorktree)
		migrated := current != nil && *current == entry.NewPath
		if !original && !migrated {
			return errors.New("Git worktree changed during mount folder migration")
		}
		worktrees[cfg.Name] = current
	}
	paths := make(map[string]string, len(journal.Paths))
	root := journal.OldRoot
	if forward {
		root = journal.NewRoot
	}
	for _, cfg := range configs {
		entry := journal.Paths[cfg.Name]
		path := entry.OldPath
		if forward {
			path = entry.NewPath
		}
		paths[cfg.Name] = path
		if !entry.GitPresent {
			continue
		}
		worktree := entry.OldWorktree
		if forward {
			worktree = &path
		}
		current := worktrees[cfg.Name]
		if (current == nil && worktree == nil) || (current != nil && worktree != nil && *current == *worktree) {
			continue
		}
		if err := writeMigrationWorktree(ctx, cfg.GitDir, worktree); err != nil {
			return err
		}
	}
	return s.engine.UpdateCatalogMountRoot(ctx, root, paths)
}

// Only private clones belonging to this catalogue can have Git configuration
// rewritten. Never follow a symlink through any private storage component.
func (s *Service) migrationGitDirectory(cfg model.RepoConfig) (bool, error) {
	if cfg.ID != model.RepoID(cfg.Name) || cfg.PreparedGitDir || cfg.GitDir != filepath.Join(s.opts.StateDir, "engine", "repos", cfg.Name, "git") {
		return false, errors.New("migration requires an engine-owned Git directory")
	}
	for _, path := range []string{
		s.opts.StateDir,
		filepath.Join(s.opts.StateDir, "engine"),
		filepath.Join(s.opts.StateDir, "engine", "repos"),
		filepath.Join(s.opts.StateDir, "engine", "repos", cfg.Name),
		cfg.GitDir,
	} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
			return false, errors.New("repository storage must be a real directory owned by the current user")
		}
	}
	info, err := os.Lstat(filepath.Join(cfg.GitDir, "config"))
	if errors.Is(err, os.ErrNotExist) {
		// A failed initial clone can leave its directory without configuration.
		// There is no worktree value to rewrite; retain all partial clone data.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
		return false, errors.New("repository Git configuration must be an owned regular file")
	}
	return true, nil
}

func readMigrationWorktree(ctx context.Context, gitDir string) (*string, error) {
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "config", "--local", "--null", "--get-all", "core.worktree")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("read repository Git worktree: %w", err)
	}
	values := strings.Split(string(output), "\x00")
	if len(values) != 2 || values[1] != "" {
		return nil, errors.New("repository has multiple or malformed Git worktree values")
	}
	return &values[0], nil
}

func writeMigrationWorktree(ctx context.Context, gitDir string, path *string) error {
	if path != nil {
		if err := configureGitWorktree(ctx, gitDir, *path); err != nil {
			return err
		}
	} else {
		cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "config", "--local", "--unset-all", "core.worktree")
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 5 {
				return fmt.Errorf("restore repository Git worktree: %w", err)
			}
		}
	}
	f, err := os.Open(filepath.Join(gitDir, "config"))
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	return syncMigrationDirectory(gitDir)
}

func readRootMigration(path string) (rootMigration, error) {
	var journal rootMigration
	info, err := os.Lstat(path)
	if err != nil {
		return journal, err
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 {
		return journal, errors.New("migration journal must be a private owned regular file")
	}
	if info.Size() > 32<<20 {
		return journal, errors.New("migration journal is too large")
	}
	f, err := os.Open(path)
	if err != nil {
		return journal, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 32<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return journal, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return journal, errors.New("migration journal has trailing data")
	}
	return journal, nil
}

func writeRootMigration(path string, journal rootMigration) error {
	// Encoding map keys in lexical order keeps the journal reviewable. Marshal
	// does that deterministically; never serialize full repo configs or secrets.
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".rootmount-migration-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncMigrationDirectory(filepath.Dir(path))
}

func removeRootMigration(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncMigrationDirectory(filepath.Dir(path))
}

func syncMigrationDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
