//go:build !darwin && !windows

package desktop

import (
	"context"
	"errors"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

func (s *Service) platformRecoverMountCatalogue(context.Context) error {
	return errors.New("virtual folder recovery is supported only by the native macOS filesystem")
}
func (s *Service) platformMountRecoveryAvailable() bool                  { return false }
func (s *Service) platformPreflightMountRecovery() error                 { return nil }
func (s *Service) platformOwnedMountNeedsRecovery(fusefs.MountedFS) bool { return false }
