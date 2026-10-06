//go:build !windows

package fusefs

import (
	"context"
	"math"
	"os"
	"syscall"
	"time"

	"github.com/jacobsa/fuse/fuseops"
)

// GetFileHandleAttributes addresses an open file independently of its lookup
// reference and directory entry. FSKit can request fstat after unlink or after
// reclaiming the corresponding item; the open descriptor still owns the bytes.
func (fs *ArtifactFuse) GetFileHandleAttributes(ctx context.Context, inode fuseops.InodeID, handle fuseops.HandleID) (fuseops.InodeAttributes, error) {
	attrs, _, err := fs.getFileHandleAttributes(ctx, inode, handle, true)
	return attrs, err
}

// GetFileHandleMetadataAttributes keeps open-descriptor authority after rename
// or unlink, without acquiring an unknown-size base blob for an attached file.
func (fs *ArtifactFuse) GetFileHandleMetadataAttributes(ctx context.Context, inode fuseops.InodeID, handle fuseops.HandleID) (fuseops.InodeAttributes, bool, error) {
	return fs.getFileHandleAttributes(ctx, inode, handle, false)
}

func (fs *ArtifactFuse) getFileHandleAttributes(ctx context.Context, inode fuseops.InodeID, handle fuseops.HandleID, requireSize bool) (fuseops.InodeAttributes, bool, error) {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	if err := ctx.Err(); err != nil {
		return fuseops.InodeAttributes{}, true, fuseOperationError("handle getattr", err)
	}
	fh, err := fs.fileHandle(handle)
	if err != nil {
		return fuseops.InodeAttributes{}, true, err
	}
	fh.mu.Lock()
	if fh.inode.ID != inode {
		fh.mu.Unlock()
		return fuseops.InodeAttributes{}, true, syscall.EBADF
	}
	if fh.detached {
		attrs, err := detachedHandleAttributes(fh)
		metadataID := fh.inode.MetadataID
		fh.mu.Unlock()
		if err == nil {
			err = fs.applyMetadataObjectCtime(ctx, metadataID, &attrs)
		}
		return attrs, true, err
	}
	path := fh.path
	metadataID := fh.inode.MetadataID
	fh.mu.Unlock()
	if path == ".git" {
		attrs := fs.gitFileAttrs()
		return attrs, true, fs.applyMetadataObjectCtime(ctx, metadataID, &attrs)
	}
	if !requireSize {
		attrs, known, _, _, err := fs.metadataAttributes(ctx, path)
		if err != nil {
			return fuseops.InodeAttributes{}, true, fuseOperationError("handle getattr metadata", err)
		}
		return attrs, known, fs.applyMetadataObjectCtime(ctx, metadataID, &attrs)
	}
	mode, size, typ, mtime, ctime, err := fs.resolveAttrs(ctx, path)
	if err != nil {
		return fuseops.InodeAttributes{}, true, fuseOperationError("handle getattr", err)
	}
	attrs := inodeAttrs(mode, uint64(size), typ, mtime, ctime)
	return attrs, true, fs.applyMetadataObjectCtime(ctx, metadataID, &attrs)
}

// SetFileHandleAttributes preserves the distinction between an attached
// worktree path and an unlinked open backing file. Truncating the latter must
// never create a new overlay entry or resurrect the deleted path.
func (fs *ArtifactFuse) SetFileHandleAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	if op.Handle == nil {
		return syscall.EBADF
	}
	if op.Size != nil && *op.Size > math.MaxInt64 {
		return syscall.EFBIG
	}
	if op.Uid != nil || op.Gid != nil || op.Atime != nil {
		return syscall.ENOTSUP
	}
	if op.Mode != nil && (*op.Mode & ^os.FileMode(0o777)) != 0 {
		return syscall.ENOTSUP
	}
	if err := ctx.Err(); err != nil {
		return fuseOperationError("handle setattr", err)
	}
	fh, err := fs.fileHandle(*op.Handle)
	if err != nil {
		return err
	}
	fh.mu.Lock()
	if fh.inode.ID != op.Inode {
		fh.mu.Unlock()
		return syscall.EBADF
	}
	if fh.detached {
		defer fh.mu.Unlock()
		if fh.cacheFile == nil {
			return syscall.EIO
		}
		if op.Size != nil {
			if fh.access != 0 && fh.access&2 == 0 {
				return syscall.EBADF
			}
			if err := fh.cacheFile.Truncate(int64(*op.Size)); err != nil {
				return fuseOperationError("truncate detached file", err)
			}
			fh.clearDetachedMtime()
		}
		if op.Mode != nil {
			if err := fh.cacheFile.Chmod(*op.Mode); err != nil {
				return fuseOperationError("chmod detached file", err)
			}
		}
		if op.Mtime != nil {
			if err := setDetachedMtime(fh, *op.Mtime); err != nil {
				return err
			}
		}
		op.Attributes, err = detachedHandleAttributes(fh)
		return err
	}
	path := fh.path
	file := fh.cacheFile
	retained := fh.cacheGeneration == -1 && file != nil
	fh.mu.Unlock()
	if path == ".git" {
		return syscall.EROFS
	}
	if op.Size != nil || op.Mode != nil || op.Mtime != nil {
		if err := fs.prepareOpenHandlesForOverlay(ctx, path); err != nil {
			return fuseOperationError("prepare handle attributes", err)
		}
		// Promotion may have rebound this descriptor from the immutable blob.
		fh.mu.Lock()
		file = fh.cacheFile
		retained = fh.cacheGeneration == -1 && file != nil
		fh.mu.Unlock()
	}
	if op.Size != nil {
		if fh.access != 0 && fh.access&2 == 0 {
			return syscall.EBADF
		}
		fs.closeCachedFilesForPath(path)
		var err error
		if retained {
			fs.resolver.transition.RLock()
			err = fs.engine.Overlay.TruncateFrom(ctx, path, int64(*op.Size), file)
			fs.resolver.transition.RUnlock()
		} else {
			err = fs.engine.Truncate(ctx, path, int64(*op.Size))
		}
		if err != nil {
			return fuseOperationError("handle truncate", err)
		}
		fs.closeCachedFilesForPath(path)
	}
	if op.Mode != nil {
		if err := fs.engine.SetMode(ctx, path, uint32(op.Mode.Perm())); err != nil {
			return fuseOperationError("handle chmod", err)
		}
	}
	if op.Mtime != nil {
		fs.closeCachedFilesForPath(path)
		if err := fs.engine.SetMtime(ctx, path, *op.Mtime); err != nil {
			return fuseOperationError("handle set mtime", err)
		}
		fs.closeCachedFilesForPath(path)
	}
	mode, size, typ, mtime, ctime, err := fs.resolver.Getattr(path)
	if err != nil {
		return fuseOperationError("handle getattr after update", err)
	}
	op.Attributes = inodeAttrs(mode, uint64(size), typ, mtime, ctime)
	return nil
}

func fileAccess(flags int) uint32 {
	switch flags & syscall.O_ACCMODE {
	case syscall.O_WRONLY:
		return 2
	case syscall.O_RDWR:
		return 3
	default:
		return 1
	}
}

func fileOpenFlags(access uint32) int {
	switch access {
	case 2:
		return os.O_WRONLY
	case 3:
		return os.O_RDWR
	default:
		return os.O_RDONLY
	}
}

func setDetachedMtime(handle *FileHandle, mtime time.Time) error {
	attrs, err := detachedHandleAttributes(handle)
	if err != nil {
		return err
	}
	times := []syscall.Timeval{syscall.NsecToTimeval(attrs.Atime.UnixNano()), syscall.NsecToTimeval(mtime.UnixNano())}
	if err := syscall.Futimes(int(handle.cacheFile.Fd()), times); err != nil {
		return fuseOperationError("set detached mtime", err)
	}
	if handle.detachedMetadata == nil {
		handle.detachedMetadata = &detachedMetadata{}
	}
	handle.detachedMetadata.mu.Lock()
	// futimes has microsecond precision; the virtual inode retains the exact
	// requested nanoseconds consistently across all its open descriptors.
	handle.detachedMetadata.mtime = &mtime
	handle.detachedMetadata.mu.Unlock()
	return nil
}

func detachedHandleAttributes(handle *FileHandle) (fuseops.InodeAttributes, error) {
	if handle.cacheFile == nil {
		return fuseops.InodeAttributes{}, syscall.EIO
	}
	info, err := handle.cacheFile.Stat()
	if err != nil {
		return fuseops.InodeAttributes{}, fuseOperationError("stat detached file", err)
	}
	attrs := fuseops.InodeAttributes{Size: uint64(info.Size()), Mode: info.Mode(), Nlink: 0}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		attrs.Uid, attrs.Gid = stat.Uid, stat.Gid
		attrs.Atime, attrs.Mtime, attrs.Ctime, attrs.Crtime = handleStatTimes(stat)
	} else {
		attrs.Mtime = info.ModTime()
	}
	if handle.detachedMetadata != nil {
		handle.detachedMetadata.mu.Lock()
		if handle.detachedMetadata.mtime != nil {
			attrs.Mtime = *handle.detachedMetadata.mtime
		}
		handle.detachedMetadata.mu.Unlock()
	}
	return attrs, nil
}

func (handle *FileHandle) clearDetachedMtime() {
	if handle.detachedMetadata == nil {
		return
	}
	handle.detachedMetadata.mu.Lock()
	handle.detachedMetadata.mtime = nil
	handle.detachedMetadata.mu.Unlock()
}

// Retire namespace identities after deletion while allowing live file handles
// to retain the old inode object and descriptor. A newly created path must get
// a new identity, and old path-based requests must not act on its replacement.
func (fs *ArtifactFuse) retireInodePath(path string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for candidate, id := range fs.pathToInode {
		if samePathOrDescendant(candidate, path) {
			if inode := fs.inodes[id]; inode != nil {
				inode.Stale = true
			}
			delete(fs.pathToInode, candidate)
		}
	}
}
