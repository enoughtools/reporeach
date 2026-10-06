//go:build !windows

package catalogfs

import (
	"context"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"syscall"

	"github.com/jacobsa/fuse/fuseops"
)

func (fs *FileSystem) mutationBackend(ctx context.Context, id fuseops.InodeID) (*inode, *fusefs.ArtifactFuse, func(), error) {
	n, err := fs.node(id)
	if err != nil {
		return nil, nil, nil, err
	}
	if n.repo == nil {
		return nil, nil, nil, syscall.EROFS
	}
	release, err := fs.beginMutation(n.repo)
	if err != nil {
		return nil, nil, nil, err
	}
	n, backend, err := fs.activateNode(ctx, id, n)
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	return n, backend, release, nil
}

func (fs *FileSystem) MkDir(ctx context.Context, op *fuseops.MkDirOp) error {
	if !validChild(op.Name) {
		return syscall.EINVAL
	}
	n, backend, release, err := fs.mutationBackend(ctx, op.Parent)
	if err != nil {
		return err
	}
	defer release()
	child := *op
	child.Parent = n.local
	if err := backend.MkDir(ctx, &child); err != nil {
		return err
	}
	child.Parent = op.Parent
	child.Entry.Child = fs.mapNamedInode(n.repo, model.CleanPath(n.repoPath+"/"+op.Name), child.Entry.Child, true, true)
	*op = child
	return nil
}

func (fs *FileSystem) CreateSymlink(ctx context.Context, op *fuseops.CreateSymlinkOp) error {
	if !validChild(op.Name) {
		return syscall.EINVAL
	}
	n, backend, release, err := fs.mutationBackend(ctx, op.Parent)
	if err != nil {
		return err
	}
	defer release()
	child := *op
	child.Parent = n.local
	if err := backend.CreateSymlink(ctx, &child); err != nil {
		return err
	}
	child.Parent = op.Parent
	child.Entry.Child = fs.mapNamedInode(n.repo, model.CleanPath(n.repoPath+"/"+op.Name), child.Entry.Child, true, true)
	*op = child
	return nil
}

func (fs *FileSystem) CreateFile(ctx context.Context, op *fuseops.CreateFileOp) error {
	if !validChild(op.Name) {
		return syscall.EINVAL
	}
	n, backend, release, err := fs.mutationBackend(ctx, op.Parent)
	if err != nil {
		return err
	}
	defer release()
	child := *op
	child.Parent = n.local
	if err := backend.CreateFile(ctx, &child); err != nil {
		return err
	}
	localInode := child.Entry.Child
	child.Parent = op.Parent
	child.Entry.Child = fs.mapNamedInode(n.repo, model.CleanPath(n.repoPath+"/"+op.Name), child.Entry.Child, true, true)
	child.Handle = fs.addHandle(&handle{repo: n.repo, backend: backend, local: child.Handle, inode: localInode, globalInode: child.Entry.Child})
	*op = child
	return nil
}

func (fs *FileSystem) RmDir(ctx context.Context, op *fuseops.RmDirOp) error {
	if !validChild(op.Name) {
		return syscall.EINVAL
	}
	n, backend, release, err := fs.mutationBackend(ctx, op.Parent)
	if err != nil {
		return err
	}
	defer release()
	child := *op
	child.Parent = n.local
	return backend.RmDir(ctx, &child)
}

func (fs *FileSystem) Unlink(ctx context.Context, op *fuseops.UnlinkOp) error {
	if !validChild(op.Name) {
		return syscall.EINVAL
	}
	n, backend, release, err := fs.mutationBackend(ctx, op.Parent)
	if err != nil {
		return err
	}
	defer release()
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
	if op.OpenFlags&syscall.O_TRUNC != 0 || op.OpenFlags&syscall.O_ACCMODE != syscall.O_RDONLY {
		release, err := fs.beginMutation(n.repo)
		if err != nil {
			return err
		}
		defer release()
	}
	backend, err := fs.activateRepo(ctx, n.repo)
	if err != nil {
		return err
	}
	n, err = fs.resolveLocal(ctx, op.Inode, backend)
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
	release, err := fs.beginMutation(n.repo)
	if err != nil {
		return err
	}
	defer release()
	backend, err := fs.activateRepo(ctx, n.repo)
	if err != nil {
		return err
	}
	n, err = fs.resolveLocal(ctx, op.Inode, backend)
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
	release, err := fs.beginMutation(old.repo)
	if err != nil {
		return err
	}
	defer release()
	backend, err := fs.activateRepo(ctx, old.repo)
	if err != nil {
		return err
	}
	old, err = fs.resolveLocal(ctx, op.OldParent, backend)
	if err != nil {
		return err
	}
	new, err = fs.resolveLocal(ctx, op.NewParent, backend)
	if err != nil {
		return err
	}
	child := *op
	child.OldParent, child.NewParent = old.local, new.local
	if err := backend.Rename(ctx, &child); err != nil {
		return err
	}
	oldPath := model.CleanPath(old.repoPath + "/" + op.OldName)
	newPath := model.CleanPath(new.repoPath + "/" + op.NewName)
	fs.moveRepositoryPaths(old.repo, oldPath, newPath)
	return nil
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
	release, err := fs.beginMutation(h.repo)
	if err != nil {
		return err
	}
	defer release()
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

// Destroy releases the userspace file descriptors after the mount stops. It
// does not close backing stores: those belong to the repository lifecycle owner.
func (fs *FileSystem) Destroy() {
	fs.mu.Lock()
	fs.destroyed = true
	handles := fs.handles
	fs.handles = make(map[fuseops.HandleID]*handle)
	type retainedLookup struct {
		repo  *repository
		local fuseops.InodeID
		count uint64
	}
	var retained []retainedLookup
	for _, n := range fs.inodes {
		if n.preview != nil && n.backendRefs > 0 {
			retained = append(retained, retainedLookup{n.repo, n.local, n.backendRefs})
			n.backendRefs = 0
		}
	}
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
	for _, lookup := range retained {
		lookup.repo.mu.Lock()
		backend := lookup.repo.backend
		lookup.repo.mu.Unlock()
		if backend != nil {
			_ = backend.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: lookup.local, N: lookup.count})
		}
	}
}
