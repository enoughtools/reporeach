//go:build !windows

package catalogfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
)

// PreviewContent opens one binary-safe immutable blob at the supplied revision.
// The caller owns the descriptor. The provider must neither create a writable
// runtime nor substitute a newer revision when the selected source has moved.
type PreviewContent func(context.Context, Entry, string, string) (*os.File, error)

type previewFile struct {
	entry    Entry
	node     model.BaseNode
	revision string
	file     *os.File
}

func (fs *FileSystem) openPreviewFile(n *inode, op *fuseops.OpenFileOp) (bool, error) {
	if fs.previewContent == nil || n.preview == nil || n.repoPath == ".git" ||
		op.OpenFlags&syscall.O_TRUNC != 0 || op.OpenFlags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return false, nil
	}
	// Match SetEntries' lock order while atomically excluding promotion.
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n.repo.mu.Lock()
	defer n.repo.mu.Unlock()
	if fs.destroyed || fs.inodes[op.Inode] == nil {
		return true, syscall.ESTALE
	}
	if n.repo.backend != nil || n.repo.preparing != nil || n.repo.previewUnavailable {
		return false, nil
	}
	if n.preview.Type == "dir" {
		return true, syscall.EISDIR
	}
	if n.preview.Type != "file" || n.repo.previewRevision == "" {
		return true, syscall.EINVAL
	}
	h := &handle{repo: n.repo, repoPath: n.repoPath, globalInode: op.Inode,
		previewFile: &previewFile{entry: n.repo.entry, node: *n.preview, revision: n.repo.previewRevision}}
	// A descriptor outlives the kernel's lookup reference. Keep this identity
	// until close so promotion can bind it before rename/unlink or a write.
	h.direntInodes = map[fuseops.InodeID]struct{}{op.Inode: {}}
	fs.inodes[op.Inode].dirRefs++
	op.Handle = fs.nextHandle
	fs.nextHandle++
	fs.handles[op.Handle] = h
	op.KeepPageCache = false
	return true, nil
}

// h.mu is held for each descriptor operation, including acquisition. Promotion
// waits for a current read before replacing its immutable descriptor.
func (fs *FileSystem) previewDescriptor(ctx context.Context, h *handle) (*os.File, error) {
	if h.closed || h.previewFile == nil {
		return nil, syscall.EBADF
	}
	if h.previewFile.file != nil {
		return h.previewFile.file, nil
	}
	file, err := fs.previewContent(ctx, h.previewFile.entry, h.repoPath, h.previewFile.revision)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, syscall.EIO
	}
	h.previewFile.file = file
	return file, nil
}

func (fs *FileSystem) readPreviewFile(ctx context.Context, h *handle, op *fuseops.ReadFileOp) error {
	if op.Offset < 0 || op.Size < 0 || uint64(op.Size) > uint64(^uint(0)>>1) {
		return syscall.EINVAL
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if op.Size == 0 {
		op.BytesRead = 0
		return nil
	}
	file, err := fs.previewDescriptor(ctx, h)
	if err != nil {
		return err
	}
	data := make([]byte, int(op.Size))
	n, err := file.ReadAt(data, op.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	op.Data, op.BytesRead = [][]byte{data[:n]}, n
	return nil
}

func (fs *FileSystem) promotePreviewFiles(ctx context.Context, repo *repository, backend *fusefs.ArtifactFuse) error {
	fs.mu.Lock()
	var handles []*handle
	for _, h := range fs.handles {
		if h.repo == repo && h.previewFile != nil {
			handles = append(handles, h)
		}
	}
	fs.mu.Unlock()
	for _, h := range handles {
		h.mu.Lock()
		err := fs.promotePreviewFile(ctx, h, backend)
		h.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (fs *FileSystem) promotePreviewFile(ctx context.Context, h *handle, backend *fusefs.ArtifactFuse) error {
	if h.closed || h.backend != nil {
		return nil
	}
	n, err := fs.resolveLocal(ctx, h.globalInode, backend)
	if err != nil {
		return err
	}
	op := &fuseops.OpenFileOp{Inode: n.local, OpenFlags: syscall.O_RDONLY}
	if err := backend.OpenFile(ctx, op); err != nil {
		return err
	}
	h.backend, h.local, h.inode = backend, op.Handle, n.local
	if h.previewFile.file != nil {
		_ = h.previewFile.file.Close()
		h.previewFile.file = nil
	}
	return nil
}

func (fs *FileSystem) readPreviewSymlink(ctx context.Context, n *inode, op *fuseops.ReadSymlinkOp) (bool, error) {
	if fs.previewContent == nil || n.preview == nil || n.preview.Type != "symlink" {
		return false, nil
	}
	n.repo.mu.Lock()
	if n.repo.backend != nil || n.repo.preparing != nil || n.repo.previewUnavailable {
		n.repo.mu.Unlock()
		return false, nil
	}
	entry, revision := n.repo.entry, n.repo.previewRevision
	n.repo.mu.Unlock()
	if n.preview.SizeState == "known" && n.preview.SizeBytes > model.MaxSymlinkTargetBytes {
		return true, syscall.ENAMETOOLONG
	}
	file, err := fs.previewContent(ctx, entry, n.repoPath, revision)
	if err != nil {
		return true, err
	}
	if file == nil {
		return true, syscall.EIO
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, model.MaxSymlinkTargetBytes+1))
	if err != nil {
		return true, err
	}
	if len(data) > model.MaxSymlinkTargetBytes {
		return true, syscall.ENAMETOOLONG
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return true, syscall.EIO
	}
	// The syscall's symlink target is a string; ordinary file bytes are always
	// streamed to a descriptor and never pass through text conversion.
	op.Target = string(data)
	return true, nil
}

func (fs *FileSystem) exactPreviewAttributes(ctx context.Context, repo *repository, node model.BaseNode, revision string) (fuseops.InodeAttributes, error) {
	repo.mu.Lock()
	entry := repo.entry
	repo.mu.Unlock()
	file, err := fs.previewContent(ctx, entry, node.Path, revision)
	if err != nil {
		return fuseops.InodeAttributes{}, err
	}
	if file == nil {
		return fuseops.InodeAttributes{}, syscall.EIO
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fuseops.InodeAttributes{}, err
	}
	attrs, _ := fs.previewAttributes(node)
	attrs.Size = uint64(info.Size())
	return attrs, nil
}
