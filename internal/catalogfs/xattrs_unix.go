//go:build !windows

package catalogfs

import (
	"context"
	"errors"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
)

func validateXattrName(name string) error {
	if name == "" || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return syscall.EINVAL
	}
	if len(name) > model.MaxXattrNameBytes {
		return syscall.ENAMETOOLONG
	}
	return nil
}

func metadataError(err error) error {
	switch {
	case errors.Is(err, model.ErrXattrNotFound):
		return fuse.ENOATTR
	case errors.Is(err, model.ErrXattrExists):
		return syscall.EEXIST
	case errors.Is(err, model.ErrXattrTooLarge):
		return syscall.E2BIG
	case errors.Is(err, model.ErrXattrStorageFull):
		return syscall.ENOSPC
	case errors.Is(err, model.ErrInvalidXattr):
		return syscall.EINVAL
	case errors.Is(err, model.ErrMetadataObjectNotFound):
		return syscall.ESTALE
	default:
		return err
	}
}

// syntheticMetadata never activates a repository. Repository roots keep their
// catalogue identity even after activation; only objects inside a repository
// are forwarded to its own filesystem's metadata store.
func (fs *FileSystem) syntheticMetadata(ctx context.Context, n *inode) (model.MetadataObject, error) {
	object, err := fs.metadata.BindMetadata(ctx, n.metadataPath, "dir")
	if err == nil {
		fs.mu.Lock()
		// A refresh may have retired this placeholder while the store access
		// was in flight. Cache only on the exact surviving namespace identity.
		if id, ok := fs.paths[n.path]; ok {
			current := fs.inodes[id]
			if current != nil && current.metadataPath == n.metadataPath {
				current.metadataID = object.ID
			}
		}
		fs.mu.Unlock()
	}
	return object, metadataError(err)
}

// xattrNode also recognizes an object retained by an open handle after the
// kernel forgets its lookup reference. A removed catalogue placeholder never
// takes this path, so it cannot attach metadata to a replacement repository.
func (fs *FileSystem) xattrNode(id fuseops.InodeID) (*inode, error) {
	n, err := fs.node(id)
	if err == nil {
		return n, nil
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.inodes[id] == nil {
		for _, h := range fs.handles {
			if h.globalInode == id && h.backend != nil && h.inode != fuseops.RootInodeID {
				return &inode{repo: h.repo, local: h.inode}, nil
			}
		}
	}
	return nil, err
}

func (fs *FileSystem) GetXattr(ctx context.Context, op *fuseops.GetXattrOp) error {
	if err := validateXattrName(op.Name); err != nil {
		return err
	}
	n, err := fs.xattrNode(op.Inode)
	if err != nil {
		return err
	}
	if n.path == "" {
		backend, err := fs.activateRepo(ctx, n.repo)
		if err != nil {
			return err
		}
		child := *op
		child.Inode = n.local
		err = backend.GetXattr(ctx, &child)
		child.Inode = op.Inode
		*op = child
		return err
	}
	if fs.metadata == nil {
		return fuse.ENOATTR
	}
	object, err := fs.syntheticMetadata(ctx, n)
	if err != nil {
		return err
	}
	value, found, err := fs.metadata.GetMetadataXattr(ctx, object.ID, op.Name)
	if err != nil {
		return metadataError(err)
	}
	if !found {
		return fuse.ENOATTR
	}
	op.BytesRead = len(value)
	if len(op.Dst) == 0 {
		return nil
	}
	if len(op.Dst) < len(value) {
		return syscall.ERANGE
	}
	copy(op.Dst, value)
	return nil
}

func (fs *FileSystem) ListXattr(ctx context.Context, op *fuseops.ListXattrOp) error {
	n, err := fs.xattrNode(op.Inode)
	if err != nil {
		return err
	}
	if n.path == "" {
		backend, err := fs.activateRepo(ctx, n.repo)
		if err != nil {
			return err
		}
		child := *op
		child.Inode = n.local
		err = backend.ListXattr(ctx, &child)
		child.Inode = op.Inode
		*op = child
		return err
	}
	if fs.metadata == nil {
		op.BytesRead = 0
		return nil
	}
	object, err := fs.syntheticMetadata(ctx, n)
	if err != nil {
		return err
	}
	names, err := fs.metadata.ListMetadataXattrs(ctx, object.ID)
	if err != nil {
		return metadataError(err)
	}
	sort.Strings(names)
	op.BytesRead = 0
	for _, name := range names {
		op.BytesRead += len(name) + 1
	}
	if len(op.Dst) == 0 {
		return nil
	}
	if len(op.Dst) < op.BytesRead {
		return syscall.ERANGE
	}
	offset := 0
	for _, name := range names {
		offset += copy(op.Dst[offset:], name)
		op.Dst[offset] = 0
		offset++
	}
	return nil
}

func (fs *FileSystem) SetXattr(ctx context.Context, op *fuseops.SetXattrOp) error {
	if err := validateXattrName(op.Name); err != nil {
		return err
	}
	policy, err := fusefs.XattrSetPolicyFromFlags(op.Flags)
	if err != nil {
		return err
	}
	if len(op.Value) > model.MaxXattrValueBytes {
		return syscall.E2BIG
	}
	n, err := fs.xattrNode(op.Inode)
	if err != nil {
		return err
	}
	if n.path == "" {
		backend, err := fs.activateRepo(ctx, n.repo)
		if err != nil {
			return err
		}
		child := *op
		child.Inode = n.local
		return backend.SetXattr(ctx, &child)
	}
	if fs.metadata == nil {
		return syscall.ENOTSUP
	}
	object, err := fs.syntheticMetadata(ctx, n)
	if err != nil {
		return err
	}
	return metadataError(fs.metadata.SetMetadataXattr(ctx, object.ID, op.Name, op.Value, policy))
}

func (fs *FileSystem) RemoveXattr(ctx context.Context, op *fuseops.RemoveXattrOp) error {
	if err := validateXattrName(op.Name); err != nil {
		return err
	}
	n, err := fs.xattrNode(op.Inode)
	if err != nil {
		return err
	}
	if n.path == "" {
		backend, err := fs.activateRepo(ctx, n.repo)
		if err != nil {
			return err
		}
		child := *op
		child.Inode = n.local
		return backend.RemoveXattr(ctx, &child)
	}
	if fs.metadata == nil {
		return syscall.ENOTSUP
	}
	object, err := fs.syntheticMetadata(ctx, n)
	if err != nil {
		return err
	}
	return metadataError(fs.metadata.RemoveMetadataXattr(ctx, object.ID, op.Name))
}
