//go:build !windows

package fusefs

import (
	"context"
	"errors"
	iofs "io/fs"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"golang.org/x/sys/unix"
)

// Metadata operations use the same namespace gate as rename and unlink. The
// opaque identity survives path retirement and is independent of data COW.
func (fs *ArtifactFuse) GetXattr(ctx context.Context, op *fuseops.GetXattrOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	if err := validateXattrName(op.Name); err != nil {
		return err
	}
	id, err := fs.xattrObject(ctx, op.Inode)
	if err != nil {
		return err
	}
	value, found, err := fs.engine.Overlay.GetMetadataXattr(ctx, id, op.Name)
	if err != nil {
		return xattrError("get xattr", err)
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

func (fs *ArtifactFuse) ListXattr(ctx context.Context, op *fuseops.ListXattrOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	op.BytesRead = 0
	id, err := fs.xattrObject(ctx, op.Inode)
	if err != nil {
		return err
	}
	names, err := fs.engine.Overlay.ListMetadataXattrs(ctx, id)
	if err != nil {
		return xattrError("list xattrs", err)
	}
	sort.Strings(names)
	for _, name := range names {
		op.BytesRead += len(name) + 1
	}
	if len(op.Dst) == 0 {
		return nil
	}
	if len(op.Dst) < op.BytesRead {
		return syscall.ERANGE
	}
	off := 0
	for _, name := range names {
		off += copy(op.Dst[off:], name)
		op.Dst[off] = 0
		off++
	}
	return nil
}

// Namespace operations cannot use a retained old object's pathname after HEAD
// has replaced its durable binding. Xattr operations may still use its ID.
func (fs *ArtifactFuse) requireLiveInode(ctx context.Context, id fuseops.InodeID, missing error) (*InodeRef, error) {
	ref, err := fs.requireInode(id, missing)
	if err != nil {
		return nil, err
	}
	if fs.engine != nil && fs.engine.Overlay != nil {
		if _, err := fs.xattrObject(ctx, id); err != nil {
			return nil, err
		}
		current, err := fs.requireInode(id, missing)
		if err != nil && fs.resolver != nil && !ref.IsRoot && ref.Path != ".git" {
			_, lookupErr := fs.resolver.ResolvePath(ref.Path)
			if errors.Is(lookupErr, iofs.ErrNotExist) {
				return nil, syscall.ENOENT
			}
		}
		return current, err
	}
	return ref, nil
}

func (fs *ArtifactFuse) SetXattr(ctx context.Context, op *fuseops.SetXattrOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	if err := validateXattrName(op.Name); err != nil {
		return err
	}
	policy, err := XattrSetPolicyFromFlags(op.Flags)
	if err != nil {
		return err
	}
	if len(op.Value) > model.MaxXattrValueBytes {
		return syscall.E2BIG
	}
	id, err := fs.xattrObject(ctx, op.Inode)
	if err != nil {
		return err
	}
	return xattrError("set xattr", fs.engine.Overlay.SetMetadataXattr(ctx, id, op.Name, op.Value, policy))
}

// XattrSetPolicyFromFlags decodes actual host kernel flags, which differ from
// the persistent model's platform-independent policies. The item is already
// resolved, so Darwin's NOFOLLOW affects no further path traversal here.
func XattrSetPolicyFromFlags(flags uint32) (model.XattrSetPolicy, error) {
	switch xattrInodeFlags(flags) {
	case 0:
		return model.XattrAlwaysSet, nil
	case unix.XATTR_CREATE:
		return model.XattrMustCreate, nil
	case unix.XATTR_REPLACE:
		return model.XattrMustReplace, nil
	default:
		return 0, syscall.EINVAL
	}
}

func (fs *ArtifactFuse) RemoveXattr(ctx context.Context, op *fuseops.RemoveXattrOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	if err := validateXattrName(op.Name); err != nil {
		return err
	}
	id, err := fs.xattrObject(ctx, op.Inode)
	if err != nil {
		return err
	}
	return xattrError("remove xattr", fs.engine.Overlay.RemoveMetadataXattr(ctx, id, op.Name))
}

func validateXattrName(name string) error {
	if name == "" || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return syscall.EINVAL
	}
	if len(name) > model.MaxXattrNameBytes {
		return syscall.ENAMETOOLONG
	}
	return nil
}

func xattrError(operation string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, model.ErrXattrNotFound):
		return fuse.ENOATTR
	case errors.Is(err, model.ErrXattrExists):
		return syscall.EEXIST
	case errors.Is(err, model.ErrInvalidXattr):
		return syscall.EINVAL
	case errors.Is(err, model.ErrXattrTooLarge):
		return syscall.E2BIG
	case errors.Is(err, model.ErrXattrStorageFull):
		return syscall.ENOSPC
	case errors.Is(err, model.ErrMetadataObjectNotFound):
		return syscall.ESTALE
	default:
		return fuseOperationError(operation, err)
	}
}

// metadataRef also finds an open descriptor's retained lookup object after
// Forget. It never uses the descriptor's old path to address a replacement.
func (fs *ArtifactFuse) metadataRef(id fuseops.InodeID) (*InodeRef, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if ref := fs.inodes[id]; ref != nil {
		copy := *ref
		return &copy, nil
	}
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		if fh.inode != nil && fh.inode.ID == id {
			copy := *fh.inode
			if fh.detached {
				copy.Stale = true
			}
			fh.mu.Unlock()
			return &copy, nil
		}
		fh.mu.Unlock()
	}
	for _, dh := range fs.dirHandles {
		if dh.inode.ID == id {
			copy := *dh.inode
			return &copy, nil
		}
	}
	return nil, syscall.ESTALE
}

func (fs *ArtifactFuse) saveMetadataRef(ref *InodeRef, id model.MetadataObjectID, stale bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if actual := fs.inodes[ref.ID]; actual != nil {
		actual.MetadataID = id
		if stale {
			actual.Stale = true
			if fs.pathToInode[actual.Path] == actual.ID {
				delete(fs.pathToInode, actual.Path)
			}
		}
	}
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		if fh.inode != nil && fh.inode.ID == ref.ID {
			fh.inode.MetadataID = id
			if stale {
				fh.inode.Stale = true
			}
		}
		fh.mu.Unlock()
	}
	for _, dh := range fs.dirHandles {
		if dh.inode.ID == ref.ID {
			dh.inode.MetadataID = id
			if stale {
				dh.inode.Stale = true
			}
		}
	}
}

// The caller owns handleOps. ResolvePath metadata is sufficient: asking for an
// xattr must never download a blob or manufacture an overlay data entry.
func (fs *ArtifactFuse) xattrObject(ctx context.Context, inode fuseops.InodeID) (model.MetadataObjectID, error) {
	if err := ctx.Err(); err != nil {
		return "", xattrError("xattr identity", err)
	}
	ref, err := fs.metadataRef(inode)
	if err != nil {
		return "", err
	}
	if fs.engine == nil || fs.engine.Overlay == nil {
		return "", syscall.ENOTSUP
	}
	if ref.Stale {
		if ref.MetadataID == "" {
			return "", syscall.ESTALE
		}
		return ref.MetadataID, nil
	}
	if fs.resolver != nil {
		fs.resolver.transition.RLock()
		defer fs.resolver.transition.RUnlock()
	}
	typ := ref.Type
	if !ref.IsRoot && ref.Path != ".git" {
		if fs.resolver == nil {
			return "", syscall.ESTALE
		}
		n, err := fs.resolver.resolvePath(ref.Path)
		if err != nil {
			if errors.Is(err, iofs.ErrNotExist) {
				if ref.MetadataID != "" {
					fs.saveMetadataRef(ref, ref.MetadataID, true)
					return ref.MetadataID, nil
				}
				return "", syscall.ENOENT
			}
			return "", xattrError("resolve xattr identity", err)
		}
		typ = resolvedNodeType(n)
		if typ != ref.Type {
			if ref.MetadataID == "" {
				return "", syscall.ESTALE
			}
			fs.saveMetadataRef(ref, ref.MetadataID, true)
			return ref.MetadataID, nil
		}
	}
	object, err := fs.engine.Overlay.BindMetadata(ctx, ref.Path, typ)
	if err != nil {
		return "", xattrError("bind xattr identity", err)
	}
	if ref.MetadataID != "" && ref.MetadataID != object.ID {
		fs.saveMetadataRef(ref, ref.MetadataID, true)
		return ref.MetadataID, nil
	}
	fs.saveMetadataRef(ref, object.ID, false)
	return object.ID, nil
}

// Capture existing lookup identities before the store's namespace transaction
// detaches or overwrites their path bindings. Open handles carry the same IDs.
func (fs *ArtifactFuse) captureMetadataForPaths(ctx context.Context, paths ...string) error {
	if fs.engine == nil || fs.engine.Overlay == nil {
		return nil
	}
	fs.mu.RLock()
	ids := make(map[fuseops.InodeID]struct{})
	for id, ref := range fs.inodes {
		for _, path := range paths {
			if !ref.Stale && samePathOrDescendant(ref.Path, path) {
				ids[id] = struct{}{}
			}
		}
	}
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		if fh.inode != nil && !fh.detached {
			for _, path := range paths {
				if samePathOrDescendant(fh.path, path) {
					ids[fh.inode.ID] = struct{}{}
				}
			}
		}
		fh.mu.Unlock()
	}
	fs.mu.RUnlock()
	for id := range ids {
		if _, err := fs.xattrObject(ctx, id); err != nil {
			if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ESTALE) {
				continue
			}
			return err
		}
	}
	return nil
}

// Open directory entries own the identity they listed. A historical directory
// handle may return an old object after HEAD or rename replaces its path; that
// object must not displace the current namespace's inode or metadata binding.
func (fs *ArtifactFuse) directoryEntryInode(ctx context.Context, dh *DirHandle, e ReaddirEntry, lookup bool) (*InodeRef, error) {
	path := cleanChildPath(dh.inode.Path, e.Name)
	live := true
	if e.MetadataID != "" && fs.resolver != nil {
		fs.resolver.transition.RLock()
		defer fs.resolver.transition.RUnlock()
		if path != ".git" {
			n, err := fs.resolver.resolvePath(path)
			if err != nil && !errors.Is(err, iofs.ErrNotExist) {
				return nil, xattrError("resolve directory metadata", err)
			}
			live = err == nil && resolvedNodeType(n) == e.Type
		}
		if live {
			object, err := fs.engine.Overlay.BindMetadata(ctx, path, e.Type)
			if err != nil {
				return nil, xattrError("resolve directory metadata identity", err)
			}
			live = object.ID == e.MetadataID
		}
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if dh.entryInodes == nil {
		dh.entryInodes = make(map[string]fuseops.InodeID)
	}
	if ref := fs.inodes[dh.entryInodes[e.Name]]; ref != nil {
		if lookup {
			ref.Refcnt++
		}
		return ref, nil
	}
	var ref *InodeRef
	if live {
		if existing := fs.inodes[fs.pathToInode[path]]; existing != nil && existing.MetadataID != "" && e.MetadataID != "" && existing.MetadataID != e.MetadataID {
			existing.Stale = true
			delete(fs.pathToInode, path)
		}
		ref = fs.allocInode(path, e.Type, e.Mode, dh.gen)
	} else {
		id := fs.nextInodeID
		fs.nextInodeID++
		ref = &InodeRef{ID: id, Path: path, Type: e.Type, Mode: e.Mode, Gen: dh.gen, Refcnt: 1, Stale: true}
		fs.inodes[id] = ref
	}
	if e.MetadataID != "" {
		ref.MetadataID = e.MetadataID
	}
	if !lookup {
		ref.Refcnt--
	}
	if dh.direntInodes == nil {
		dh.direntInodes = make(map[fuseops.InodeID]struct{})
	}
	dh.direntInodes[ref.ID] = struct{}{}
	ref.DirRefs++
	dh.entryInodes[e.Name] = ref.ID
	return ref, nil
}

// HEAD may remove and later recreate a same-typed path without a kernel unlink.
// A cached identity whose durable binding changed must not alias that new node.
func (fs *ArtifactFuse) refreshMetadataPath(ctx context.Context, path, typ string) error {
	if fs.engine == nil || fs.engine.Overlay == nil {
		return nil
	}
	fs.mu.RLock()
	var ref InodeRef
	if existing := fs.inodes[fs.pathToInode[path]]; existing != nil {
		ref = *existing
	}
	fs.mu.RUnlock()
	if ref.MetadataID == "" || ref.Stale {
		return nil
	}
	if fs.resolver != nil {
		fs.resolver.transition.RLock()
		defer fs.resolver.transition.RUnlock()
		if path != "." && path != ".git" {
			n, err := fs.resolver.resolvePath(path)
			if errors.Is(err, iofs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return xattrError("resolve refreshed metadata", err)
			}
			if resolvedNodeType(n) != typ {
				return nil
			}
		}
	}
	object, err := fs.engine.Overlay.BindMetadata(ctx, path, typ)
	if err != nil {
		return xattrError("refresh metadata identity", err)
	}
	if object.ID != ref.MetadataID {
		fs.saveMetadataRef(&ref, ref.MetadataID, true)
	}
	return nil
}

func (fs *ArtifactFuse) applyMetadataCtime(ctx context.Context, ref *InodeRef, attrs *fuseops.InodeAttributes) error {
	if fs.engine == nil || fs.engine.Overlay == nil {
		return nil
	}
	id, err := fs.xattrObject(ctx, ref.ID)
	if err != nil {
		return err
	}
	current, err := fs.metadataRef(ref.ID)
	if err != nil {
		return err
	}
	if current.Stale {
		return syscall.ESTALE
	}
	return fs.applyMetadataObjectCtime(ctx, id, attrs)
}

func (fs *ArtifactFuse) applyMetadataObjectCtime(ctx context.Context, id model.MetadataObjectID, attrs *fuseops.InodeAttributes) error {
	if id == "" || fs.engine == nil || fs.engine.Overlay == nil {
		return nil
	}
	object, found, err := fs.engine.Overlay.MetadataObject(ctx, id)
	if err != nil {
		return xattrError("xattr ctime", err)
	}
	if found && object.CtimeUnixNs > attrs.Ctime.UnixNano() {
		attrs.Ctime = time.Unix(0, object.CtimeUnixNs)
	}
	return nil
}
