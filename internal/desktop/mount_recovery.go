package desktop

import (
	"context"
	"errors"
)

// RecoverMount is an explicit request to replace a proven interrupted virtual
// session. A live owner never falls through to ownerless recovery on failure.
func (s *Service) RecoverMount(ctx context.Context) (retErr error) {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	if s.closing || s.quitPrepared || s.maintenance || s.recoveryRequired {
		s.mu.Unlock()
		return errors.New("wait for the repository service to finish its current change")
	}
	if s.mounted != nil && !s.platformOwnedMountNeedsRecovery(s.mounted) {
		// A previous request may already have succeeded. Stale UI state must
		// never turn its retry into detaching a healthy new session.
		s.mu.Unlock()
		return nil
	}
	for _, operation := range s.ops {
		if operation.Status == "running" {
			s.mu.Unlock()
			return errors.New("finish repository operations before recovering virtual folders")
		}
	}
	s.maintenance = true
	owned := s.mounted != nil
	s.mu.Unlock()
	defer func() {
		available := false
		if retErr != nil {
			available = s.platformMountRecoveryAvailable()
		}
		s.mu.Lock()
		s.maintenance = false
		s.mountRecoveryAvailable = available
		if retErr != nil {
			s.message = safeError(retErr)
		}
		s.mu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if owned {
		if err := s.detachLocked(); err != nil {
			return err
		}
	} else {
		if err := s.recoverMountCatalogue(ctx); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.mountLocked(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.MountDesired = true
	s.message = ""
	return s.persistLocked()
}
