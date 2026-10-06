//go:build !windows

package fusefs

import (
	"context"
	"errors"
	iofs "io/fs"
	"syscall"
	"time"

	"github.com/jacobsa/fuse/fuseops"
)

// LookUpMetadata grants the same inode reference as ordinary lookup while
// leaving unavailable base-object sizes unresolved. Native callers can omit
// the size attribute; ordinary POSIX lookup keeps its exact-size contract.
func (fs *ArtifactFuse) LookUpMetadata(ctx context.Context, op *fuseops.LookUpInodeOp) (bool, error) {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	parent, err := fs.requireLiveInode(ctx, op.Parent, syscall.ENOENT)
	if err != nil {
		return true, err
	}
	path := cleanChildPath(parent.Path, op.Name)
	var attrs fuseops.InodeAttributes
	known, typ, mode := true, "file", uint32(0o644)
	if parent.IsRoot && op.Name == ".git" {
		attrs = fs.gitFileAttrs()
	} else {
		attrs, known, typ, mode, err = fs.metadataAttributes(ctx, path)
		if err != nil {
			if errors.Is(err, iofs.ErrNotExist) {
				return true, syscall.ENOENT
			}
			return true, fuseOperationError("lookup metadata", err)
		}
		if err := fs.refreshMetadataPath(ctx, path, typ); err != nil {
			return true, err
		}
	}
	fs.mu.Lock()
	ref := fs.allocInode(path, typ, mode, fs.resolver.Generation())
	fs.mu.Unlock()
	op.Entry.Child = ref.ID
	op.Entry.Attributes = attrs
	setChildEntryExpiry(&op.Entry, time.Second)
	if err := fs.applyMetadataCtime(ctx, ref, &op.Entry.Attributes); err != nil {
		fs.dropInodeLookup(ref.ID)
		return true, err
	}
	return known, nil
}

// GetMetadataAttributes reads the authoritative merged working tree without
// fetching an unknown-size blob. Local overlay sizes and Git pointer sizes are
// already exact. The bool must accompany the result through native transport.
func (fs *ArtifactFuse) GetMetadataAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) (bool, error) {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	ref, err := fs.requireLiveInode(ctx, op.Inode, syscall.ESTALE)
	if err != nil {
		return true, err
	}
	if ref.Path == ".git" {
		op.Attributes = fs.gitFileAttrs()
		op.AttributesExpiration = attrExpiry(time.Minute)
		return true, fs.applyMetadataCtime(ctx, ref, &op.Attributes)
	}
	if ref.IsRoot && fs.resolver == nil {
		now := time.Now()
		op.Attributes = inodeAttrs(ref.Mode, 4096, "dir", now, now)
		op.AttributesExpiration = attrExpiry(time.Second)
		return true, fs.applyMetadataCtime(ctx, ref, &op.Attributes)
	}
	attrs, known, _, _, err := fs.metadataAttributes(ctx, ref.Path)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return true, syscall.ENOENT
		}
		return true, fuseOperationError("getattr metadata", err)
	}
	op.Attributes = attrs
	op.AttributesExpiration = attrExpiry(time.Second)
	return known, fs.applyMetadataCtime(ctx, ref, &op.Attributes)
}

func (fs *ArtifactFuse) metadataAttributes(ctx context.Context, path string) (fuseops.InodeAttributes, bool, string, uint32, error) {
	if err := ctx.Err(); err != nil {
		return fuseops.InodeAttributes{}, true, "", 0, err
	}
	n, generation, commitTime, err := fs.resolver.ResolvePathState(path)
	if err != nil {
		return fuseops.InodeAttributes{}, true, "", 0, err
	}
	if n.FromOverlay {
		typ := n.Overlay.NodeType()
		attrs := inodeAttrs(n.Overlay.Mode, uint64(n.Overlay.SizeBytes), typ,
			time.Unix(0, n.Overlay.MtimeUnixNs), time.Unix(0, n.Overlay.CtimeUnixNs))
		return attrs, true, typ, n.Overlay.Mode, nil
	}
	mode, typ := normalizeMode(n.Base.Mode, n.Base.Type), n.Base.Type
	known := n.Base.SizeState == "known" || typ == "dir"
	size := uint64(0)
	if known {
		size = uint64(n.Base.SizeBytes)
	}
	if commitTime == 0 {
		commitTime = generation
	}
	stamp := time.Unix(commitTime, 0)
	return inodeAttrs(mode, size, typ, stamp, stamp), known, typ, mode, nil
}
