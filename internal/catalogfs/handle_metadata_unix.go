//go:build !windows

package catalogfs

import (
	"context"
	"syscall"

	"github.com/jacobsa/fuse/fuseops"
)

// Handle metadata deliberately uses the retained backend and local identity,
// allowing fstat/ftruncate after hiding a repository or forgetting its inode.
func (fs *FileSystem) GetFileHandleAttributes(ctx context.Context, inode fuseops.InodeID, handleID fuseops.HandleID) (fuseops.InodeAttributes, error) {
	h, err := fs.getHandle(handleID, false)
	if err != nil {
		return fuseops.InodeAttributes{}, err
	}
	if inode != h.globalInode {
		return fuseops.InodeAttributes{}, syscall.EBADF
	}
	return h.backend.GetFileHandleAttributes(ctx, h.inode, h.local)
}

func (fs *FileSystem) SetFileHandleAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp) error {
	if op.Handle == nil {
		return syscall.EBADF
	}
	h, err := fs.getHandle(*op.Handle, false)
	if err != nil {
		return err
	}
	if op.Inode != h.globalInode {
		return syscall.EBADF
	}
	release, err := fs.beginMutation(h.repo)
	if err != nil {
		return err
	}
	defer release()
	child := *op
	child.Inode = h.inode
	localHandle := h.local
	child.Handle = &localHandle
	if err := h.backend.SetFileHandleAttributes(ctx, &child); err != nil {
		return err
	}
	child.Inode, child.Handle = op.Inode, op.Handle
	*op = child
	return nil
}
