//go:build !windows

package catalogfs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

// ErrPreviewUnavailable selects the authoritative writable view, for example
// when a managed checkout already has local changes. Other acquisition errors
// are returned to the caller; they must never masquerade as an empty directory.
var ErrPreviewUnavailable = errors.New("repository browsing preview unavailable")

// PreviewDirectory is a complete list of immediate children of one directory
// at an immutable Git commit. Entries use canonical repository-relative paths.
// Unknown sizes remain unknown; zero is not inferred from absent metadata.
type PreviewDirectory struct {
	Revision string
	Entries  []model.BaseNode
	// GitFileSize declares the synthesized root .git pointer's exact size.
	// The pointer is created by ArtifactFS, independently of committed tree
	// entries. Its contents remain unavailable until the runtime is ready.
	GitFileSize uint64
}

type Preview func(context.Context, Entry, string) (PreviewDirectory, error)

type previewInodeKey struct {
	repo *repository
	path string
}

func (fs *FileSystem) previewDirectory(ctx context.Context, repo *repository, path string) (PreviewDirectory, error) {
	if fs.preview == nil {
		return PreviewDirectory{}, ErrPreviewUnavailable
	}
	path = model.CleanPath(path)
	for {
		repo.mu.Lock()
		if repo.backend != nil || repo.preparing != nil || repo.previewUnavailable {
			repo.mu.Unlock()
			return PreviewDirectory{}, ErrPreviewUnavailable
		}
		if directory, ok := repo.previewDirs[path]; ok {
			repo.mu.Unlock()
			return directory, nil
		}
		if pending := repo.previewPending[path]; pending != nil {
			repo.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return PreviewDirectory{}, ctx.Err()
			}
		}
		if repo.previewPending == nil {
			repo.previewPending = make(map[string]chan struct{})
		}
		pending := make(chan struct{})
		repo.previewPending[path] = pending
		entry := repo.entry
		repo.mu.Unlock()
		directory, err := fs.preview(ctx, entry, path)
		if err == nil {
			err = validatePreview(path, &directory)
		}
		repo.mu.Lock()
		if err == nil && (repo.backend != nil || repo.preparing != nil) {
			err = ErrPreviewUnavailable
		}
		if err == nil && repo.previewRevision != "" && repo.previewRevision != directory.Revision {
			err = syscall.ESTALE
		}
		if err == nil {
			repo.previewRevision = directory.Revision
			if repo.previewDirs == nil {
				repo.previewDirs = make(map[string]PreviewDirectory)
			}
			repo.previewDirs[path] = directory
		} else if errors.Is(err, ErrPreviewUnavailable) {
			repo.previewUnavailable = true
		}
		delete(repo.previewPending, path)
		close(pending)
		repo.mu.Unlock()
		return directory, err
	}
}

func validatePreview(path string, directory *PreviewDirectory) error {
	if directory.Revision == "" {
		return fmt.Errorf("browsing metadata has no immutable revision")
	}
	if directory.GitFileSize > math.MaxInt64 || directory.GitFileSize != 0 && path != "." {
		return fmt.Errorf("invalid browsing Git pointer size")
	}
	entries := append([]model.BaseNode(nil), directory.Entries...)
	prefix := ""
	if path != "." {
		prefix = path + "/"
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		name, child := strings.CutPrefix(entry.Path, prefix)
		if !child || !validChild(name) || entry.Path != model.CleanPath(entry.Path) || seen[name] || name == ".git" || entry.SizeBytes < 0 {
			return fmt.Errorf("invalid immediate browsing entry %q", entry.Path)
		}
		if entry.Type != "file" && entry.Type != "dir" && entry.Type != "symlink" {
			return fmt.Errorf("invalid browsing entry type %q", entry.Type)
		}
		if entry.SizeState != "known" && entry.SizeState != "unknown" {
			return fmt.Errorf("invalid browsing size state %q", entry.SizeState)
		}
		seen[name] = true
	}
	if directory.GitFileSize != 0 {
		entries = append(entries, model.BaseNode{Path: ".git", Type: "file", Mode: 0o644,
			SizeState: "known", SizeBytes: int64(directory.GitFileSize)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	directory.Entries = entries
	return nil
}

func (fs *FileSystem) previewAttributes(node model.BaseNode) (fuseops.InodeAttributes, bool) {
	mode := os.FileMode(node.Mode & 0o777)
	known := node.SizeState == "known"
	size := uint64(node.SizeBytes)
	switch node.Type {
	case "dir":
		mode |= os.ModeDir
		known, size = true, 4096
	case "symlink":
		mode |= os.ModeSymlink
	}
	if !known {
		size = 0
	}
	return fuseops.InodeAttributes{Size: size, Nlink: 1, Mode: mode, Atime: fs.created, Mtime: fs.created, Ctime: fs.created,
		Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}, known
}

func (fs *FileSystem) previewInode(repo *repository, node model.BaseNode, lookup bool) fuseops.InodeID {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	key := previewInodeKey{repo, node.Path}
	id := fs.previewInodes[key]
	if id == 0 {
		id = fs.nextInode
		fs.nextInode++
		copy := node
		fs.inodes[id] = &inode{repo: repo, repoPath: node.Path, preview: &copy}
		fs.previewInodes[key] = id
	}
	if lookup {
		fs.inodes[id].refs++
	}
	return id
}

// LookUpMetadata exposes native lookup without forcing unknown-size content to
// hydrate. Its bool describes size authority, independently of inode lifetime.
func (fs *FileSystem) LookUpMetadata(ctx context.Context, op *fuseops.LookUpInodeOp) (bool, error) {
	return fs.lookup(ctx, op, true)
}

func (fs *FileSystem) previewLookup(ctx context.Context, n *inode, name string, allowUnknown bool) (fuseops.ChildInodeEntry, bool, bool, error) {
	if n.repo == nil {
		return fuseops.ChildInodeEntry{}, true, false, nil
	}
	directory, err := fs.previewDirectory(ctx, n.repo, n.repoPath)
	if errors.Is(err, ErrPreviewUnavailable) {
		return fuseops.ChildInodeEntry{}, true, false, nil
	}
	if err != nil {
		return fuseops.ChildInodeEntry{}, true, true, err
	}
	if name == ".git" && n.repoPath == "." && directory.GitFileSize == 0 {
		// Older providers do not declare the runtime pointer's metadata.
		// Its existence and content must still use the actual writable view.
		return fuseops.ChildInodeEntry{}, true, false, nil
	}
	path := model.CleanPath(n.repoPath + "/" + name)
	for _, node := range directory.Entries {
		if node.Path != path {
			continue
		}
		attrs, known := fs.previewAttributes(node)
		if !known && !allowUnknown {
			return fuseops.ChildInodeEntry{}, true, false, nil
		}
		expiry := time.Now().Add(time.Second)
		return fuseops.ChildInodeEntry{Child: fs.previewInode(n.repo, node, true), Attributes: attrs,
			AttributesExpiration: expiry, EntryExpiration: expiry}, known, true, nil
	}
	return fuseops.ChildInodeEntry{}, true, true, syscall.ENOENT
}

// GetMetadataAttributes has the same native unknown-size contract as lookup.
// An existing writable backend always supersedes the immutable preview.
func (fs *FileSystem) GetMetadataAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) (bool, error) {
	n, err := fs.node(op.Inode)
	if err != nil {
		return true, err
	}
	if n.preview != nil {
		n.repo.mu.Lock()
		dormant := n.repo.backend == nil && n.repo.preparing == nil
		n.repo.mu.Unlock()
		if dormant {
			attrs, known := fs.previewAttributes(*n.preview)
			op.Attributes = attrs
			op.AttributesExpiration = time.Now().Add(time.Second)
			return known, nil
		}
	}
	if n.path != "" {
		return true, fs.GetInodeAttributes(ctx, op)
	}
	n, backend, err := fs.activateNode(ctx, op.Inode, n)
	if err != nil {
		return true, err
	}
	child := *op
	child.Inode = n.local
	known, err := backend.GetMetadataAttributes(ctx, &child)
	child.Inode = op.Inode
	*op = child
	return known, err
}

// resolveLocal binds a retained preview inode to one real backend lookup. The
// binding owns its own reference; old preview references never get replayed as
// backend forgets. This is also what preserves native FSItem identity on open.
func (fs *FileSystem) resolveLocal(ctx context.Context, id fuseops.InodeID, backend *fusefs.ArtifactFuse) (*inode, error) {
	for {
		fs.mu.Lock()
		n := fs.inodes[id]
		if n == nil {
			fs.mu.Unlock()
			return nil, syscall.ESTALE
		}
		if n.local != 0 {
			copy := *n
			fs.mu.Unlock()
			return &copy, nil
		}
		if pending := n.binding; pending != nil {
			fs.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		pending := make(chan struct{})
		n.binding = pending
		path := n.repoPath
		fs.mu.Unlock()
		local := fuseops.InodeID(fuseops.RootInodeID)
		var err error
		for _, name := range strings.Split(path, "/") {
			op := &fuseops.LookUpInodeOp{Parent: local, Name: name}
			_, err = backend.LookUpMetadata(ctx, op)
			if local != fuseops.RootInodeID {
				_ = backend.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: local, N: 1})
			}
			if err != nil {
				local = 0
				break
			}
			local = op.Entry.Child
		}
		fs.mu.Lock()
		current := fs.inodes[id]
		if current == n && err == nil {
			n.local = local
			n.backendRefs++
			fs.childInodes[inodeKey{n.repo, local}] = id
		}
		n.binding = nil
		close(pending)
		fs.mu.Unlock()
		if current != n && local != 0 {
			_ = backend.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: local, N: 1})
			return nil, syscall.ESTALE
		}
		if err != nil {
			return nil, err
		}
	}
}

func (fs *FileSystem) mapNamedInode(repo *repository, path string, local fuseops.InodeID, lookup bool, backendLookup bool) fuseops.InodeID {
	path = model.CleanPath(path)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if id := fs.previewInodes[previewInodeKey{repo, path}]; id != 0 {
		n := fs.inodes[id]
		// A replaced object at the same path must not inherit a retained inode.
		if n.local == 0 || n.local == local {
			n.local = local
			fs.childInodes[inodeKey{repo, local}] = id
			if lookup {
				n.refs++
			}
			if backendLookup {
				n.backendRefs++
			}
			return id
		}
	}
	id := fs.mapInodeLocked(repo, local, lookup)
	fs.inodes[id].repoPath = path
	return id
}

func (fs *FileSystem) openPreviewDirectory(ctx context.Context, n *inode, op *fuseops.OpenDirOp) (bool, error) {
	directory, err := fs.previewDirectory(ctx, n.repo, n.repoPath)
	if errors.Is(err, ErrPreviewUnavailable) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if n.preview != nil && n.preview.Type != "dir" {
		return true, syscall.ENOTDIR
	}
	h := &handle{directory: true, repo: n.repo, repoPath: n.repoPath, globalInode: op.Inode}
	for _, node := range directory.Entries {
		id := fs.previewInode(n.repo, node, false)
		copy := node
		parts := strings.Split(node.Path, "/")
		h.entries = append(h.entries, catalogEntry{name: parts[len(parts)-1], inode: id, preview: &copy})
		fs.retainDirectoryInode(h, id)
	}
	op.Handle = fs.addHandle(h)
	return true, nil
}

func (fs *FileSystem) retainDirectoryInode(h *handle, id fuseops.InodeID) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if h.direntInodes == nil {
		h.direntInodes = make(map[fuseops.InodeID]struct{})
	}
	if _, ok := h.direntInodes[id]; !ok {
		h.direntInodes[id] = struct{}{}
		fs.inodes[id].dirRefs++
	}
}

func previewDirentType(typ string) fuseutil.DirentType {
	switch typ {
	case "dir":
		return fuseutil.DT_Directory
	case "symlink":
		return fuseutil.DT_Link
	default:
		return fuseutil.DT_File
	}
}

func (fs *FileSystem) readBackendDirectory(ctx context.Context, h *handle, op *fuseops.ReadDirOp) error {
	remaining := len(op.Dst) - op.BytesRead
	if remaining < 32 {
		return nil
	}
	if remaining > 64<<10 {
		remaining = 64 << 10
	}
	entries, err := h.backend.ReadDirectoryEntries(ctx, h.local, op.Offset, remaining)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id := fs.mapNamedInode(h.repo, model.CleanPath(h.repoPath+"/"+entry.Name), entry.Entry.Child, false, true)
		fs.retainDirectoryInode(h, id)
		mode := entry.Entry.Attributes.Mode
		typ := fuseutil.DT_File
		if mode.IsDir() {
			typ = fuseutil.DT_Directory
		} else if mode&os.ModeSymlink != 0 {
			typ = fuseutil.DT_Link
		}
		width := fuseutil.WriteDirent(op.Dst[op.BytesRead:], fuseutil.Dirent{Offset: entry.Offset, Inode: id, Name: entry.Name, Type: typ})
		op.BytesRead += width
		// Ordinary catalogue nodes forward lookup counts directly. READDIR
		// acquired metadata here only to recover its path; the backend's
		// directory handle already retains their zero-lookup identity.
		fs.mu.Lock()
		preview := fs.inodes[id].preview != nil
		fs.mu.Unlock()
		if !preview {
			_ = h.backend.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: entry.Entry.Child, N: 1})
		}
	}
	return nil
}

func previewDormant(n *inode) bool {
	if n.preview == nil {
		return false
	}
	n.repo.mu.Lock()
	defer n.repo.mu.Unlock()
	return n.repo.backend == nil && n.repo.preparing == nil
}

func (fs *FileSystem) moveRepositoryPaths(repo *repository, oldPath, newPath string) {
	if oldPath == newPath {
		return
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var moving []*inode
	for _, n := range fs.inodes {
		if n.repo != repo || n.path != "" {
			continue
		}
		if n.repoPath == oldPath || strings.HasPrefix(n.repoPath, oldPath+"/") {
			moving = append(moving, n)
			if n.preview != nil {
				delete(fs.previewInodes, previewInodeKey{repo, n.repoPath})
			}
		} else if n.preview != nil && (n.repoPath == newPath || strings.HasPrefix(n.repoPath, newPath+"/")) {
			// A retained overwritten object remains stale in its backend;
			// it must not capture lookups for the replacement at this path.
			delete(fs.previewInodes, previewInodeKey{repo, n.repoPath})
		}
	}
	for _, n := range moving {
		n.repoPath = model.CleanPath(newPath + strings.TrimPrefix(n.repoPath, oldPath))
		if n.preview != nil {
			copy := *n.preview
			copy.Path = n.repoPath
			n.preview = &copy
			fs.previewInodes[previewInodeKey{repo, n.repoPath}] = fs.childInodes[inodeKey{repo, n.local}]
		}
	}
	for _, h := range fs.handles {
		if h.repo == repo && (h.repoPath == oldPath || strings.HasPrefix(h.repoPath, oldPath+"/")) {
			h.repoPath = model.CleanPath(newPath + strings.TrimPrefix(h.repoPath, oldPath))
		}
	}
}
