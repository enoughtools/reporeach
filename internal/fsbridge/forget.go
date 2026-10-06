//go:build !windows

package fsbridge

import (
	"context"
	"math"
	"syscall"

	"github.com/jacobsa/fuse/fuseops"
)

// batchForget validates the complete bounded batch before dropping any
// references, including duplicates and count overflow. Directory handles retain
// their identities independently of these lookup references.
func (h *Handler) batchForget(ctx context.Context, forgets []Forget) error {
	if len(forgets) == 0 || len(forgets) > MaxBatchForgets {
		return syscall.EINVAL
	}
	counts := make(map[uint64]uint64, len(forgets))
	var inodes []uint64
	for _, forget := range forgets {
		if forget.Inode == 0 || forget.N == 0 || forget.N > math.MaxUint64-counts[forget.Inode] {
			return syscall.EINVAL
		}
		if counts[forget.Inode] == 0 {
			inodes = append(inodes, forget.Inode)
		}
		counts[forget.Inode] += forget.N
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for inode, count := range counts {
		if count > h.lookups[inode] {
			return syscall.EINVAL
		}
	}
	for _, inode := range inodes {
		count := counts[inode]
		if err := h.filesystem.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: fuseops.InodeID(inode), N: count}); err != nil {
			// Keep failed and unprocessed references tracked for closeResources
			// to retry. A failed backend callback does not establish release.
			return err
		}
		h.lookups[inode] -= count
		if h.lookups[inode] == 0 {
			delete(h.lookups, inode)
		}
	}
	return nil
}
