package desktop

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

func (s *Service) organizationEnabledLocked(owner string) bool {
	for _, disabled := range s.state.DisabledOrganizations {
		if strings.EqualFold(disabled, owner) {
			return false
		}
	}
	return true
}

func (s *Service) repositoryEnabledLocked(repo Repository) bool {
	return !repo.Disabled && s.organizationEnabledLocked(repo.Owner)
}

func (s *Service) canonicalOwnerLocked(owner string) string {
	for _, repo := range s.state.Repositories {
		if strings.EqualFold(repo.Owner, owner) {
			return repo.Owner
		}
	}
	for _, saved := range s.state.DisabledOrganizations {
		if strings.EqualFold(saved, owner) {
			return saved
		}
	}
	return owner
}

func (s *Service) organizationsLocked() []Organization {
	owners := map[string]string{}
	for _, repo := range s.state.Repositories {
		key := strings.ToLower(repo.Owner)
		if _, exists := owners[key]; !exists {
			owners[key] = repo.Owner
		}
	}
	// Retain an absent owner's switch so access changes cannot strand its saved
	// policy or silently enable it when repositories appear again.
	for _, owner := range s.state.DisabledOrganizations {
		key := strings.ToLower(owner)
		if _, exists := owners[key]; !exists {
			owners[key] = owner
		}
	}
	groups := make([]Organization, 0, len(owners))
	for _, owner := range owners {
		groups = append(groups, Organization{Name: owner, Enabled: s.organizationEnabledLocked(owner)})
	}
	sort.Slice(groups, func(i, j int) bool { return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name) })
	return groups
}

func (s *Service) SetOrganizationEnabled(ctx context.Context, owner string, enabled bool) error {
	if err := validateComponent(owner); err != nil {
		return fmt.Errorf("invalid organization or owner group: %w", err)
	}
	return s.changeVisibility(ctx, owner, "", enabled)
}

func (s *Service) SetRepositoryEnabled(ctx context.Context, id string, enabled bool) error {
	return s.changeVisibility(ctx, "", id, enabled)
}

// changeVisibility serializes catalogue publication with discovery and root
// migration. It reserves affected repository locks without waiting, avoiding a
// lock-order deadlock with storage removal (repo lock, then lifecycle lock).
// Existing file handles stay valid; the switch controls new catalogue entries
// and background pin work, rather than revoking already-open files.
func (s *Service) changeVisibility(ctx context.Context, owner, id string, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	if owner != "" {
		owner = s.canonicalOwnerLocked(owner)
	}
	if s.closing || s.maintenance || s.recoveryRequired {
		s.mu.Unlock()
		return errors.New("wait for the repository service to finish its current change")
	}
	if owner == "" {
		repo, exists := s.repositoryLocked(id)
		if !exists {
			s.mu.Unlock()
			return errors.New("repository is not in the catalogue")
		}
		id = repo.ID
		if repo.Disabled == !enabled {
			s.mu.Unlock()
			return nil
		}
	} else if s.organizationEnabledLocked(owner) == enabled {
		s.mu.Unlock()
		return nil
	}
	var reserved []chan struct{}
	defer func() {
		for _, lock := range reserved {
			lock <- struct{}{}
		}
	}()
	for _, repo := range s.state.Repositories {
		if (owner != "" && !strings.EqualFold(repo.Owner, owner)) || (owner == "" && repo.ID != id) {
			continue
		}
		if _, busy := s.cancels[repo.ID]; busy {
			s.mu.Unlock()
			return errors.New("cancel or finish the affected repository operation before changing visibility")
		}
		key := strings.ToLower(repo.ID)
		lock := s.locks[key]
		if lock == nil {
			lock = make(chan struct{}, 1)
			lock <- struct{}{}
			s.locks[key] = lock
		}
		select {
		case <-lock:
			reserved = append(reserved, lock)
		default:
			s.mu.Unlock()
			return errors.New("a repository is being opened; try changing visibility again when it finishes")
		}
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	previous := s.state
	oldEntries := s.entriesLocked()
	s.state.Repositories = append([]Repository(nil), previous.Repositories...)
	s.state.DisabledOrganizations = append([]string(nil), previous.DisabledOrganizations...)
	if owner == "" {
		s.state.Repositories[s.repositoryIndexLocked(id)].Disabled = !enabled
	} else {
		filtered := s.state.DisabledOrganizations[:0]
		for _, disabled := range s.state.DisabledOrganizations {
			if !strings.EqualFold(owner, disabled) {
				filtered = append(filtered, disabled)
			}
		}
		if !enabled {
			filtered = append(filtered, owner)
		}
		s.state.DisabledOrganizations = filtered
	}
	catalog, entries := s.catalog, s.entriesLocked()
	err := s.persistLocked()
	if err != nil {
		s.state = previous
		// writeState can fail after rename. Restore the previous durable policy
		// as well as the in-memory snapshot before returning the error.
		err = errors.Join(err, s.persistLocked())
	}
	if err != nil {
		s.mu.Unlock()
		return err
	}
	// SetEntries never invokes activation and releases each repository's mutex
	// before any callback can acquire s.mu. Keep the state lock until publication
	// finishes, so rollback cannot overwrite unrelated operation completions.
	if catalog != nil {
		if err := catalog.SetEntries(entries); err != nil {
			s.state = previous
			persistErr := s.persistLocked()
			restoreErr := catalog.SetEntries(oldEntries)
			s.mu.Unlock()
			return errors.Join(err, persistErr, restoreErr)
		}
	}
	s.mu.Unlock()
	return nil
}
