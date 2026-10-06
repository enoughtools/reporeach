package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// Filesystem callbacks cannot wait behind an action that may drain the same
// mount. Existing runtimes bypass this path; new preparation retries when the
// explicit action or catalogue change has completed.
func (s *Service) lockRepoForActivation(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.quitPrepared || s.recoveryRequired || s.maintenance {
		return nil, syscall.EBUSY
	}
	key := strings.ToLower(id)
	lock := s.locks[key]
	if lock == nil {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		s.locks[key] = lock
	}
	select {
	case <-lock:
		return func() { lock <- struct{}{} }, nil
	default:
		return nil, syscall.EBUSY
	}
}

func (s *Service) refreshPreview(ctx context.Context, repo Repository) (retErr error) {
	if s.preview == nil {
		return ErrPreviewUnavailable
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	if s.closing || s.quitPrepared || s.recoveryRequired || s.maintenance {
		s.mu.Unlock()
		return errors.New("the repository catalogue is busy or needs recovery")
	}
	wasMounted := s.mounted != nil
	s.maintenance = true
	s.mu.Unlock()
	defer func() {
		retErr = errors.Join(retErr, s.restoreCatalogueAfterChange(ctx, wasMounted))
	}()
	if err := s.detachLocked(); err != nil {
		return err
	}
	preview, err := s.preview.Refresh(ctx, repo)
	if err != nil {
		return err
	}
	_, _, err = preview.Directory(ctx, ".")
	return err
}

func (s *Service) checkCatalogueDirectory(root string) error {
	if s.hybridCatalogue {
		return s.checkHybridCatalogueDirectory(root)
	}
	return s.checkMountDirectory(root)
}

// Lifecycle must be held while publishing the ordinary catalogue. Publication
// changes only our exact owned links, never the contents of a local checkout.
func (s *Service) publishHybridCatalogue() error {
	if !s.hybridCatalogue {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publishHybridCatalogueLocked()
}

// The desired links are published before a membership/visibility save. If a
// save fails, callers restore state and their lifecycle defer restores links.
// No mount callbacks or Git access occur while the state lock is held here.
func (s *Service) publishHybridCatalogueLocked() error {
	if !s.hybridCatalogue {
		return nil
	}
	root := s.state.MountRoot
	entries := make([]hybridCatalogueEntry, 0, len(s.state.Repositories))
	for _, repo := range s.state.Repositories {
		if s.repositoryEnabledLocked(repo) {
			entries = append(entries, hybridCatalogueEntry{ID: repo.ID, Owner: repo.Owner, Name: repo.Name, LocalPath: repo.LocalPath})
		}
	}
	return s.syncHybridCatalogue(root, entries)
}

func (s *Service) cataloguePreview(ctx context.Context, entry catalogfs.Entry, path string) (catalogfs.PreviewDirectory, error) {
	if !s.hybridCatalogue || s.preview == nil {
		return catalogfs.PreviewDirectory{}, catalogfs.ErrPreviewUnavailable
	}
	s.mu.Lock()
	repo, ok := s.repositoryLocked(entry.ID)
	root := s.state.MountRoot
	enabled := ok && s.repositoryEnabledLocked(repo)
	s.mu.Unlock()
	if !enabled {
		return catalogfs.PreviewDirectory{}, errors.New("the repository is hidden from the catalogue")
	}
	if repo.LocalPath != "" || repo.State != "virtual" {
		return catalogfs.PreviewDirectory{}, catalogfs.ErrPreviewUnavailable
	}
	if repo.Source == "manual" {
		if err := validateManualSourceLocation(repo, root, s.opts.StateDir); err != nil {
			return catalogfs.PreviewDirectory{}, err
		}
	}
	preview, err := s.preview.Acquire(ctx, repo)
	if err != nil {
		return catalogfs.PreviewDirectory{}, err
	}
	revision, nodes, err := preview.Directory(ctx, path)
	directory := catalogfs.PreviewDirectory{Revision: revision, Entries: nodes}
	if model.CleanPath(path) == "." {
		gitDir := filepath.Join(s.opts.StateDir, "engine", "repos", engineName(repo.ID), "git")
		directory.GitFileSize = uint64(len("gitdir: " + gitDir + "\n"))
	}
	return directory, err
}

func (s *Service) cataloguePreviewContent(ctx context.Context, entry catalogfs.Entry, path, revision string) (*os.File, error) {
	if !s.hybridCatalogue || s.preview == nil {
		return nil, catalogfs.ErrPreviewUnavailable
	}
	s.mu.Lock()
	repo, ok := s.repositoryLocked(entry.ID)
	root := s.state.MountRoot
	s.mu.Unlock()
	if !ok || repo.LocalPath != "" {
		return nil, catalogfs.ErrPreviewUnavailable
	}
	if repo.Source == "manual" {
		if err := validateManualSourceLocation(repo, root, s.opts.StateDir); err != nil {
			return nil, err
		}
	}
	// Existing file handles may outlive catalogue visibility. They retain the
	// selected source revision, independently of the current display state.
	preview, err := s.preview.Acquire(ctx, repo)
	if err != nil {
		return nil, err
	}
	if preview.Commit != revision {
		return nil, syscall.ESTALE
	}
	file, err := preview.OpenContent(ctx, path)
	if err == nil {
		s.notePreviewCachedFile(repo.ID, file)
	}
	return file, err
}

func (s *Service) startPreviewSeeding() {
	if !s.hybridCatalogue || s.preview == nil {
		return
	}
	s.mu.Lock()
	if s.closing || s.quitPrepared {
		s.mu.Unlock()
		return
	}
	var repos []Repository
	for _, repo := range s.state.Repositories {
		if repo.Source != "manual" && repo.LocalPath == "" && repo.State == "virtual" && s.repositoryEnabledLocked(repo) {
			repos = append(repos, repo)
		}
	}
	s.workers.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.workers.Done()
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
		defer cancel()
		if err := s.preview.SeedGitHubRoots(ctx, repos); err != nil && s.ctx.Err() == nil {
			s.logger.Warn("browsing metadata acquisition incomplete", "error", safeError(err))
		}
	}()
}

func (s *Service) virtualRepositoryPath(repo Repository) (string, error) {
	root, err := s.hybridCatalogueMountRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, repo.Owner, repo.Name), nil
}
