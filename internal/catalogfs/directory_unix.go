//go:build !windows

package catalogfs

import (
	"context"
	"math"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

// ReadDirectoryEntries publishes typed READDIRPLUS metadata without hydrating
// unknown-size blobs or encoding Darwin's incompatible FUSE wire layout.
func (fs *FileSystem) ReadDirectoryEntries(ctx context.Context, handle fuseops.HandleID, offset fuseops.DirOffset, maxBytes int) ([]fusefs.DirectoryEntry, error) {
	if uint64(offset) > math.MaxInt || maxBytes < 32 || maxBytes > 64<<10 {
		return nil, syscall.EINVAL
	}
	h, err := fs.getHandle(handle, true)
	if err != nil {
		return nil, err
	}
	if h.backend != nil {
		entries, err := h.backend.ReadDirectoryEntries(ctx, h.local, offset, maxBytes)
		if err != nil {
			return nil, err
		}
		for i := range entries {
			local := entries[i].Entry.Child
			fs.mapDirectoryInode(h, local)
			entries[i].Entry.Child = fs.mapInode(h.repo, local, true)
		}
		return entries, nil
	}
	var entries []fusefs.DirectoryEntry
	remaining := make([]byte, maxBytes)
	for i := int(offset); i < len(h.entries); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e := h.entries[i]
		width := fuseutil.WriteDirent(remaining, fuseutil.Dirent{Name: e.name})
		if width == 0 {
			break
		}
		attrs, err := fs.syntheticDirectoryAttrs(ctx, &inode{metadataPath: e.metadataPath})
		if err != nil {
			return nil, err
		}
		expiry := time.Now().Add(time.Second)
		entries = append(entries, fusefs.DirectoryEntry{Name: e.name, Offset: fuseops.DirOffset(i + 1), SizeKnown: true,
			Entry: fuseops.ChildInodeEntry{Child: e.inode, Attributes: attrs, AttributesExpiration: expiry, EntryExpiration: expiry}})
		remaining = remaining[width:]
	}
	return entries, nil
}
