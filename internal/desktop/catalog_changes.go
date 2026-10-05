package desktop

import (
	"context"
	"errors"
	"time"
)

// quiesceCatalogueChange is called with lifecycle held and mu released, after
// rejecting any pre-existing maintenance. FSKit 26 cannot invalidate catalogue
// entries out of band, so publish membership changes only between sessions.
// FUSE keeps its existing live-publication and open-file contract.
func (s *Service) quiesceCatalogueChange(ctx context.Context) (bool, error) {
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		if s.quiescentCatalogue {
			s.maintenance = false
		}
		s.mu.Unlock()
		return false, err
	}
	if s.closing || s.quitPrepared || s.recoveryRequired {
		if s.quiescentCatalogue {
			s.maintenance = false
		}
		s.mu.Unlock()
		return false, errors.New("repository service is closing or needs recovery")
	}
	if !s.quiescentCatalogue {
		s.mu.Unlock()
		return false, nil
	}
	// Keep this claim through publication even if the previous session detached
	// externally. Visibility may already have released its reservations, and
	// new actions must not enter between this handoff and the state update.
	s.maintenance = true
	if s.mounted == nil {
		s.mu.Unlock()
		return false, nil
	}
	// Storage removal takes a repository lock before lifecycle. Draining a
	// session while such an operation waits here could deadlock an activation
	// that is waiting for its repository lock. Refuse before any unmount; new
	// operations cannot enter this cycle after maintenance is claimed below.
	for _, operation := range s.ops {
		if operation.Action == "free" && operation.Status == "running" {
			s.maintenance = false
			s.mu.Unlock()
			return false, errors.New("finish the storage removal before changing the repository catalogue")
		}
	}
	s.mu.Unlock()
	if err := s.detachLocked(); err != nil {
		s.mu.Lock()
		s.maintenance = false
		s.mu.Unlock()
		return false, err
	}
	return true, nil
}

// restoreCatalogueAfterChange is also called after a rolled-back publication.
// Canceling one API request must not strand the previously desired mount. Keep
// the service's own cancellation and a deadline so shutdown remains bounded.
// lifecycle is held and mu must be released during every mount callback.
func (s *Service) restoreCatalogueAfterChange(ctx context.Context, remount bool) error {
	if !remount {
		if s.quiescentCatalogue {
			s.mu.Lock()
			s.maintenance = false
			s.mu.Unlock()
		}
		return nil
	}
	remountCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	err := s.mountLocked(remountCtx)
	s.mu.Lock()
	s.maintenance = false
	if err != nil {
		s.message = "Could not restore the repository folder after changing the catalogue. If it is still shown as mounted, use Unmount first, then choose Mount to retry: " + safeError(err)
	} else {
		s.message = ""
	}
	s.mu.Unlock()
	return err
}
