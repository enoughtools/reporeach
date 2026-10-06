//go:build !windows

package catalogfs

import (
	"sync"
	"syscall"
)

// FreezeRepositoryWrites drains mutations already admitted for a repository
// and rejects subsequent mutations until release is called. Reads and mutations
// in other repositories remain available. The exclusive gate is released before
// returning, so rejected writes cannot block filesystem teardown.
//
// The claim belongs to the retained repository identity, including open handles
// after its path is hidden or renamed. Only one claim may exist at a time. The
// returned release is safe to call repeatedly or concurrently.
func (fs *FileSystem) FreezeRepositoryWrites(id string) (release func(), err error) {
	fs.mu.Lock()
	if fs.destroyed {
		fs.mu.Unlock()
		return nil, syscall.ESTALE
	}
	repo := fs.repositories[id]
	fs.mu.Unlock()
	if repo == nil {
		return nil, syscall.ENOENT
	}

	repo.mutationMu.Lock()
	defer repo.mutationMu.Unlock()
	// Teardown may have happened while existing mutations drained. Do not
	// publish a claim for a detached filesystem or a replaced repository.
	fs.mu.Lock()
	available := !fs.destroyed && fs.repositories[id] == repo
	fs.mu.Unlock()
	if !available {
		return nil, syscall.ESTALE
	}
	if repo.writesFrozen {
		return nil, syscall.EBUSY
	}
	repo.writesFrozen = true
	var once sync.Once
	return func() {
		once.Do(func() {
			repo.mutationMu.Lock()
			repo.writesFrozen = false
			repo.mutationMu.Unlock()
		})
	}, nil
}

// beginMutation holds shared admission for the full backend mutation. A freeze
// waits for these admissions, then records its claim without retaining a lock
// across unmount or publication. Synthetic catalogue metadata has no repository
// owner and remains writable while an individual repository is frozen.
func (fs *FileSystem) beginMutation(repo *repository) (release func(), err error) {
	fs.mu.Lock()
	destroyed := fs.destroyed
	fs.mu.Unlock()
	if destroyed {
		return nil, syscall.ESTALE
	}
	if repo == nil {
		return func() {}, nil
	}
	repo.mutationMu.RLock()
	// An admission can have waited behind the short freeze transition while
	// the filesystem detached. Its captured repository pointer is still valid,
	// but its backend must no longer receive a mutation.
	fs.mu.Lock()
	destroyed = fs.destroyed
	fs.mu.Unlock()
	if destroyed {
		repo.mutationMu.RUnlock()
		return nil, syscall.ESTALE
	}
	if repo.writesFrozen {
		repo.mutationMu.RUnlock()
		return nil, syscall.EBUSY
	}
	return repo.mutationMu.RUnlock, nil
}
