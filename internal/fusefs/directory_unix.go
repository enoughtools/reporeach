//go:build !windows

package fusefs

import (
	"context"
	"math"
	"syscall"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

// DirectoryEntry carries a captured directory entry without fetching its blob.
// Each entry owns one lookup reference, independently of the directory handle.
// SizeKnown distinguishes unavailable blob metadata from an empty file; ordinary
// lookup/getattr still resolve the exact size when a caller needs it.
type DirectoryEntry struct {
	Name      string
	Offset    fuseops.DirOffset
	Entry     fuseops.ChildInodeEntry
	SizeKnown bool
}

// ReadDirectoryEntries exposes the same snapshot and inode identities used by
// READDIRPLUS without relying on its platform-specific FUSE wire encoder.
// maxBytes bounds the page by ordinary directory-record widths.
func (fs *ArtifactFuse) ReadDirectoryEntries(ctx context.Context, handle fuseops.HandleID, offset fuseops.DirOffset, maxBytes int) (entries []DirectoryEntry, err error) {
	if uint64(offset) > math.MaxInt || maxBytes < 32 || maxBytes > 64<<10 {
		return nil, syscall.EINVAL
	}
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	dh, err := fs.dirHandle(handle)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			for _, entry := range entries {
				fs.dropInodeLookup(entry.Entry.Child)
			}
			entries = nil
		}
	}()
	remaining := make([]byte, maxBytes)
	for i := int(offset); i < len(dh.entries); i++ {
		if err = ctx.Err(); err != nil {
			return entries, err
		}
		e := dh.entries[i]
		width := fuseutil.WriteDirent(remaining, fuseutil.Dirent{Name: e.Name})
		if width == 0 {
			break
		}
		entry, entryErr := fs.childEntryFromReaddir(ctx, dh, e)
		if entryErr != nil {
			return entries, entryErr
		}
		entries = append(entries, DirectoryEntry{Name: e.Name, Offset: fuseops.DirOffset(i + 1), Entry: entry,
			SizeKnown: e.FromOverlay || e.SizeState == "known" || e.Type == "dir" || e.Name == ".git" && dh.inode.IsRoot})
		remaining = remaining[width:]
	}
	return entries, nil
}
