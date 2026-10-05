//go:build !windows

package catalogfs

import (
	"context"
	"syscall"

	"github.com/jacobsa/fuse/fuseops"
)

func (fs *FileSystem) MkDir(ctx context.Context, op *fuseops.MkDirOp) error {
	if !validChild(op.Name) {
		return syscall.EINVAL
	}
	n, backend, err := fs.backend(ctx, op.Parent)
	if err != nil {
		return err
	}
	child := *op
	child.Parent = n.local
	if err := backend.MkDir(ctx, &child); err != nil {
		return err
	}
	child.Parent = op.Parent
	child.Entry.Child = fs.mapInode(n.repo, child.Entry.Child, true)
	*op = child
	return nil
}

func (fs *FileSystem) CreateSymlink(ctx context.Context, op *fuseops.CreateSymlinkOp) error {
	if !validChild(op.Name) {
		return syscall.EINVAL
	}
	n, backend, err := fs.backend(ctx, op.Parent)
	if err != nil {
		return err
	}
	child := *op
	child.Parent = n.local
	if err := backend.CreateSymlink(ctx, &child); err != nil {
		return err
	}
	child.Parent = op.Parent
	child.Entry.Child = fs.mapInode(n.repo, child.Entry.Child, true)
	*op = child
	return nil
}

func (fs *FileSystem) CreateFile(ctx context.Context, op *fuseops.CreateFileOp) error {
	if !validChild(op.Name) {
		return syscall.EINVAL
	}
	n, backend, err := fs.backend(ctx, op.Parent)
	if err != nil {
		return err
	}
	child := *op
	child.Parent = n.local
	if err := backend.CreateFile(ctx, &child); err != nil {
		return err
	}
	localInode := child.Entry.Child
	child.Parent = op.Parent
	child.Entry.Child = fs.mapInode(n.repo, child.Entry.Child, true)
	child.Handle = fs.addHandle(&handle{repo: n.repo, backend: backend, local: child.Handle, inode: localInode, globalInode: child.Entry.Child})
	*op = child
	return nil
}

func (fs *FileSystem) RmDir(ctx context.Context, op *fuseops.RmDirOp) error {
	if !validChild(op.Name) {
		return syscall.EINVAL
	}
	n, backend, err := fs.backend(ctx, op.Parent)
	if err != nil {
		return err
	}
	child := *op
	child.Parent = n.local
	return backend.RmDir(ctx, &child)
}

func (fs *FileSystem) Unlink(ctx context.Context, op *fuseops.UnlinkOp) error {
	if !validChild(op.Name) {
		return syscall.EINVAL
	}
	n, backend, err := fs.backend(ctx, op.Parent)
	if err != nil {
		return err
	}
	child := *op
	child.Parent = n.local
	return backend.Unlink(ctx, &child)
}

func (fs *FileSystem) OpenFile(ctx context.Context, op *fuseops.OpenFileOp) error {
	n, err := fs.node(op.Inode)
	if err != nil {
		return err
	}
	if n.path != "" {
		return syscall.EISDIR
	}
	backend, err := fs.activateRepo(ctx, n.repo)
	if err != nil {
		return err
	}
	child := *op
	child.Inode = n.local
	if err := backend.OpenFile(ctx, &child); err != nil {
		return err
	}
	child.Inode = op.Inode
	child.Handle = fs.addHandle(&handle{repo: n.repo, backend: backend, local: child.Handle, inode: n.local, globalInode: op.Inode})
	*op = child
	return nil
}

func (fs *FileSystem) SetInodeAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp) error {
	n, err := fs.node(op.Inode)
	if err != nil {
		return err
	}
	if n.path != "" {
		return syscall.EROFS
	}
	backend, err := fs.activateRepo(ctx, n.repo)
	if err != nil {
		return err
	}
	child := *op
	child.Inode = n.local
	if op.Handle != nil {
		h, err := fs.getHandle(*op.Handle, false)
		if err != nil {
			return err
		}
		if h.repo != n.repo || h.inode != n.local {
			return syscall.EBADF
		}
		localHandle := h.local
		child.Handle = &localHandle
	}
	if err := backend.SetInodeAttributes(ctx, &child); err != nil {
		return err
	}
	child.Inode, child.Handle = op.Inode, op.Handle
	*op = child
	return nil
}

func (fs *FileSystem) ReadSymlink(ctx context.Context, op *fuseops.ReadSymlinkOp) error {
	n, backend, err := fs.backend(ctx, op.Inode)
	if err != nil {
		return err
	}
	child := *op
	child.Inode = n.local
	if err := backend.ReadSymlink(ctx, &child); err != nil {
		return err
	}
	child.Inode = op.Inode
	*op = child
	return nil
}

func (fs *FileSystem) Rename(ctx context.Context, op *fuseops.RenameOp) error {
	if !validChild(op.OldName) || !validChild(op.NewName) {
		return syscall.EINVAL
	}
	old, err := fs.node(op.OldParent)
	if err != nil {
		return err
	}
	new, err := fs.node(op.NewParent)
	if err != nil {
		return err
	}
	if old.repo == nil || new.repo == nil {
		return syscall.EROFS
	}
	if old.repo != new.repo {
		return syscall.EXDEV
	}
	backend, err := fs.activateRepo(ctx, old.repo)
	if err != nil {
		return err
	}
	child := *op
	child.OldParent, child.NewParent = old.local, new.local
	return backend.Rename(ctx, &child)
}

func (fs *FileSystem) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) error {
	h, err := fs.getHandle(op.Handle, false)
	if err != nil {
		return err
	}
	child := *op
	child.Inode, child.Handle = h.inode, h.local
	if err := h.backend.ReadFile(ctx, &child); err != nil {
		return err
	}
	child.Inode, child.Handle = op.Inode, op.Handle
	*op = child
	return nil
}

func (fs *FileSystem) WriteFile(ctx context.Context, op *fuseops.WriteFileOp) error {
	h, err := fs.getHandle(op.Handle, false)
	if err != nil {
		return err
	}
	child := *op
	child.Inode, child.Handle = h.inode, h.local
	if err := h.backend.WriteFile(ctx, &child); err != nil {
		return err
	}
	child.Inode, child.Handle = op.Inode, op.Handle
	*op = child
	return nil
}

func (fs *FileSystem) SyncFile(ctx context.Context, op *fuseops.SyncFileOp) error {
	h, err := fs.getHandle(op.Handle, false)
	if err != nil {
		return err
	}
	child := *op
	child.Inode, child.Handle = h.inode, h.local
	if err := h.backend.SyncFile(ctx, &child); err != nil {
		return err
	}
	child.Inode, child.Handle = op.Inode, op.Handle
	*op = child
	return nil
}

func (fs *FileSystem) FlushFile(ctx context.Context, op *fuseops.FlushFileOp) error {
	h, err := fs.getHandle(op.Handle, false)
	if err != nil {
		return err
	}
	child := *op
	child.Inode, child.Handle = h.inode, h.local
	if err := h.backend.FlushFile(ctx, &child); err != nil {
		return err
	}
	child.Inode, child.Handle = op.Inode, op.Handle
	*op = child
	return nil
}

func (fs *FileSystem) ReleaseFileHandle(ctx context.Context, op *fuseops.ReleaseFileHandleOp) error {
	h, err := fs.getHandle(op.Handle, false)
	if err != nil {
		return err
	}
	fs.mu.Lock()
	delete(fs.handles, op.Handle)
	fs.mu.Unlock()
	child := *op
	child.Handle = h.local
	return h.backend.ReleaseFileHandle(ctx, &child)
}

// ArtifactFS currently does not implement xattrs. Metadata probes on dormant
// catalogue folders must not activate a repository merely to return ENOSYS.
func (fs *FileSystem) GetXattr(context.Context, *fuseops.GetXattrOp) error   { return syscall.ENOSYS }
func (fs *FileSystem) ListXattr(context.Context, *fuseops.ListXattrOp) error { return syscall.ENOSYS }
func (fs *FileSystem) SetXattr(context.Context, *fuseops.SetXattrOp) error   { return syscall.ENOSYS }
func (fs *FileSystem) RemoveXattr(context.Context, *fuseops.RemoveXattrOp) error {
	return syscall.ENOSYS
}

// Destroy releases the userspace file descriptors after the mount stops. It
// does not close backing stores: those belong to the repository lifecycle owner.
func (fs *FileSystem) Destroy() {
	fs.mu.Lock()
	handles := fs.handles
	fs.handles = make(map[fuseops.HandleID]*handle)
	fs.mu.Unlock()
	for _, h := range handles {
		if h.backend == nil {
			continue
		}
		if h.directory {
			_ = h.backend.ReleaseDirHandle(context.Background(), &fuseops.ReleaseDirHandleOp{Handle: h.local})
		} else {
			_ = h.backend.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: h.local})
		}
	}
}
