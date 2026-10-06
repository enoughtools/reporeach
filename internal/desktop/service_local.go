package desktop

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func (s *Service) runLocalAction(ctx context.Context, op Operation, repo Repository) error {
	if _, err := validateAdoptionLocalSource(repo.LocalPath, s.opts.StateDir, s.opts.StateDir); err != nil {
		return errors.New("the local checkout location could not be safely verified")
	}
	switch op.Action {
	case "keep", "prepare":
		if _, err := InspectLocalCheckout(ctx, repo.LocalPath); err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if index := s.repositoryIndexLocked(repo.ID); index >= 0 {
			s.state.Repositories[index].State = "local"
			s.state.Repositories[index].Pinned = true
			s.state.Repositories[index].Error = ""
		}
		return s.persistLocked()
	case "refresh":
		// Native Git uses the checkout's own remotes and authentication, and
		// fetch does not replace its branch, index, or working files.
		_, err := localCheckoutGit(ctx, repo.LocalPath, "fetch", "--all")
		if err != nil {
			return errors.New("Git could not fetch the local checkout's remotes; its local work was retained")
		}
		return nil
	case "free":
		if repo.LocalKind == "adopted" {
			return errors.New("adopted checkouts stay in their original folder; RepoReach never deletes them")
		}
		s.lifecycle.Lock()
		defer s.lifecycle.Unlock()
		s.mu.Lock()
		wasMounted := s.mounted != nil
		s.maintenance = true
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.maintenance = false
			s.mu.Unlock()
		}()
		if err := s.detachLocked(); err != nil {
			return err
		}
		releasePreview, err := s.pausePreviewContent(ctx, repo.ID)
		if err != nil {
			return errors.Join(err, s.restoreCatalogueAfterChange(ctx, wasMounted))
		}
		defer releasePreview()
		err = s.freeMaterializedCheckout(ctx, repo.ID)
		releasePreview()
		return errors.Join(err, s.restoreCatalogueAfterChange(ctx, wasMounted))
	default:
		return errors.New("unsupported local checkout action")
	}
}

func (s *Service) materializeRepository(ctx context.Context, op Operation, downloadedBytes int64) (retErr error) {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	if s.closing || s.quitPrepared || s.recoveryRequired {
		s.mu.Unlock()
		return errors.New("repository service is closing or needs recovery")
	}
	repo, ok := s.repositoryLocked(op.RepositoryID)
	wasMounted := s.mounted != nil
	root := s.state.MountRoot
	s.maintenance = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.maintenance = false
		s.mu.Unlock()
	}()
	if !ok || repo.LocalPath != "" {
		return errors.New("the virtual repository changed before it could be kept locally")
	}
	if !wasMounted {
		if err := s.mountLocked(ctx); err != nil {
			return err
		}
	}
	defer func() {
		// A detached or transient mount is restored only when it was already
		// running. Physical local files remain accessible independently.
		if !wasMounted {
			retErr = errors.Join(retErr, s.detachLocked())
		}
		retErr = errors.Join(retErr, s.restoreCatalogueAfterChange(ctx, wasMounted))
	}()
	s.mu.Lock()
	catalog := s.catalog
	mounted := s.mounted
	s.mu.Unlock()
	if catalog == nil {
		return errors.New("the virtual working tree is unavailable")
	}
	release, err := catalog.FreezeRepositoryWrites(repo.ID)
	if err != nil {
		return err
	}
	defer release()
	configs, err := s.engine.ListRepos(ctx)
	if err != nil {
		return err
	}
	var config model.RepoConfig
	for _, cfg := range configs {
		if cfg.Name == engineName(repo.ID) {
			config = cfg
			break
		}
	}
	if config.Name == "" {
		return errors.New("the prepared repository storage is unavailable")
	}
	view, err := s.virtualRepositoryPath(repo)
	if err != nil {
		return err
	}
	stage, err := StageLocalCheckoutForMountedView(ctx, config, view, filepath.Join(root, repo.Owner, repo.Name), mounted)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, stage.Close()) }()
	if err := stage.VerifySource(ctx); err != nil {
		return err
	}
	if err := s.detachLocked(); err != nil {
		return err
	}
	releasePreview, err := s.pausePreviewContent(ctx, repo.ID)
	if err != nil {
		return err
	}
	defer releasePreview()
	if err := s.engine.StopCatalogRepositoryStorage(ctx, config.Name); err != nil {
		return err
	}
	if err := stage.VerifyPrivateGit(ctx); err != nil {
		return err
	}
	if err := s.releaseHybridCatalogueLink(root, repo.Owner, repo.Name); err != nil {
		return err
	}
	if err := s.commitMaterializedCheckout(ctx, repo.ID, config, stage); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if index := s.repositoryIndexLocked(repo.ID); index >= 0 {
		s.state.Repositories[index].DownloadedBytes = downloadedBytes
	}
	return s.persistLocked()
}
