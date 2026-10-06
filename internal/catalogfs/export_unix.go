//go:build !windows

package catalogfs

import (
	"context"
	"syscall"
)

// ActivateRepositoryForExport binds a frozen repository's retained browsing
// identities and readers to its authoritative working-tree backend before an
// export inventories the namespace. Without this boundary an uncached nested
// directory can promote the repository halfway through the first inventory,
// changing preview attributes to working-tree attributes without a user edit.
//
// The lifecycle owner must prepare the existing backend first and retain its
// FreezeRepositoryWrites claim until export finishes. Activation uses the
// ordinary adapter binding; it does not check out files or reset Git state.
func (fs *FileSystem) ActivateRepositoryForExport(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fs.mu.Lock()
	repo := fs.repositories[id]
	destroyed := fs.destroyed
	fs.mu.Unlock()
	if destroyed {
		return syscall.ESTALE
	}
	if repo == nil {
		return syscall.ENOENT
	}
	// This shared admission does not admit a mutation. It keeps the exclusive
	// freeze claim alive if its owner concurrently attempts to release it.
	repo.mutationMu.RLock()
	defer repo.mutationMu.RUnlock()
	if !repo.writesFrozen {
		return syscall.EBUSY
	}
	if _, err := fs.activateRepo(ctx, repo); err != nil {
		return err
	}
	fs.mu.Lock()
	available := !fs.destroyed && fs.repositories[id] == repo
	fs.mu.Unlock()
	if !available {
		return syscall.ESTALE
	}
	return nil
}
