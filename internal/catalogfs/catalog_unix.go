//go:build !windows

// Package catalogfs exposes an account's repositories under owner/repository
// directories in one writable ArtifactFS mount. Listing the catalogue is local;
// only entering a repository acquires its Git metadata and filesystem engine.
package catalogfs

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

// Entry identifies a repository independently from its display path. ID must be
// stable across catalogue refreshes; Owner and Name are single path components.
type Entry struct {
	ID    string
	Owner string
	Name  string
}

// Activate acquires a repository's metadata and returns its existing ArtifactFS
// adapter. Its snapshot, overlay and hydrator must outlive the catalogue mount.
// The callback must not mount a second filesystem at the repository directory.
type Activate func(context.Context, Entry) (*fusefs.ArtifactFuse, error)

type repository struct {
	entry     Entry
	mu        sync.Mutex
	backend   *fusefs.ArtifactFuse
	preparing chan struct{}
	err       error
}

type inode struct {
	path    string // set only for synthetic catalogue directories
	repo    *repository
	local   fuseops.InodeID
	refs    uint64
	dirRefs uint64
}

type inodeKey struct {
	repo  *repository
	local fuseops.InodeID
}

type handle struct {
	repo         *repository
	backend      *fusefs.ArtifactFuse
	local        fuseops.HandleID
	inode        fuseops.InodeID
	globalInode  fuseops.InodeID // retained even after the lookup identity is forgotten
	entries      []catalogEntry  // immutable snapshot for synthetic directory handles
	directory    bool
	direntInodes map[fuseops.InodeID]struct{}
}

type catalogEntry struct {
	name  string
	inode fuseops.InodeID
}

// FileSystem multiplexes independent ArtifactFS adapters. It translates every
// inode and handle so repositories cannot alias one another's kernel objects.
type FileSystem struct {
	fuseutil.NotImplementedFileSystem
	activate     Activate
	mu           sync.Mutex
	repositories map[string]*repository
	paths        map[string]fuseops.InodeID
	inodes       map[fuseops.InodeID]*inode
	childInodes  map[inodeKey]fuseops.InodeID
	handles      map[fuseops.HandleID]*handle
	nextInode    fuseops.InodeID
	nextHandle   fuseops.HandleID
	created      time.Time
}

var _ fuseutil.FileSystem = (*FileSystem)(nil)

// New validates the complete catalogue before exposing it. Root and repository
// placeholders are synthesized without calling Activate.
func New(entries []Entry, activate Activate) (*FileSystem, error) {
	if activate == nil {
		return nil, fmt.Errorf("catalogue activation callback is required")
	}
	fs := &FileSystem{
		activate: activate, repositories: make(map[string]*repository),
		paths:       map[string]fuseops.InodeID{".": fuseops.RootInodeID},
		inodes:      map[fuseops.InodeID]*inode{fuseops.RootInodeID: {path: "."}},
		childInodes: make(map[inodeKey]fuseops.InodeID), handles: make(map[fuseops.HandleID]*handle),
		nextInode: fuseops.RootInodeID + 1, nextHandle: 1, created: time.Now(),
	}
	if err := fs.SetEntries(entries); err != nil {
		return nil, err
	}
	return fs, nil
}

// SetEntries publishes an atomic metadata-only catalogue refresh. Existing
// activated repositories and open handles remain valid, including handles for a
// repository no longer returned by discovery. Removed paths stop being listed.
func (fs *FileSystem) SetEntries(entries []Entry) error {
	seenIDs, seenPaths := make(map[string]bool), make(map[string]bool)
	owners := make(map[string]string)
	for _, e := range entries {
		if e.ID == "" || !validComponent(e.Owner) || !validComponent(e.Name) {
			return fmt.Errorf("invalid catalogue entry %q", e.ID)
		}
		path := model.CleanPath(e.Owner + "/" + e.Name)
		folded := strings.ToLower(path)
		owner := strings.ToLower(e.Owner)
		if spelling, ok := owners[owner]; ok && spelling != e.Owner {
			return fmt.Errorf("owner name collision: %q and %q", spelling, e.Owner)
		}
		owners[owner] = e.Owner
		if seenIDs[e.ID] || seenPaths[folded] {
			return fmt.Errorf("duplicate catalogue entry %q", e.ID)
		}
		seenIDs[e.ID], seenPaths[folded] = true, true
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	paths := map[string]fuseops.InodeID{".": fuseops.RootInodeID}
	for _, e := range entries {
		repo := fs.repositories[e.ID]
		if repo == nil {
			repo = &repository{entry: e}
			fs.repositories[e.ID] = repo
		} else {
			// Activation sees a consistent discovery entry even during refresh.
			repo.mu.Lock()
			repo.entry = e
			repo.mu.Unlock()
		}
		ownerPath := model.CleanPath(e.Owner)
		if _, ok := paths[ownerPath]; !ok {
			paths[ownerPath] = fs.synthetic(ownerPath, nil)
		}
		repoPath := model.CleanPath(e.Owner + "/" + e.Name)
		paths[repoPath] = fs.synthetic(repoPath, repo)
	}
	fs.paths = paths
	// Retired placeholders are already stale and have no file descriptors of
	// their own. Reclaim them so repeated discovery/rename cycles stay bounded.
	for id, n := range fs.inodes {
		if n.path != "" && paths[n.path] != id {
			delete(fs.inodes, id)
		}
	}
	// Preserve repository identity tombstones until unmount. An operation may
	// already have routed to a dormant repository before a concurrent refresh
	// removes/re-adds its path; reusing the object keeps activation serialized.
	return nil
}

// validComponent validates without normalizing away traversal or separators.
func validComponent(name string) bool {
	if name == "" || name == "." || name == ".." || model.CleanPath(name) != name {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validChild(name string) bool {
	return name != "" && name != "." && name != ".." && model.CleanPath(name) == name && !strings.ContainsAny(name, "/\x00")
}

func (fs *FileSystem) synthetic(path string, repo *repository) fuseops.InodeID {
	if id, ok := fs.paths[path]; ok && fs.inodes[id].repo == repo {
		return id
	}
	id := fs.nextInode
	fs.nextInode++
	fs.inodes[id] = &inode{path: path, repo: repo, local: fuseops.RootInodeID}
	return id
}

func (fs *FileSystem) node(id fuseops.InodeID) (*inode, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := fs.inodes[id]
	if n == nil {
		return nil, syscall.ESTALE
	}
	if n.path != "" && fs.paths[n.path] != id {
		return nil, syscall.ESTALE
	}
	copy := *n
	return &copy, nil
}

func (fs *FileSystem) activateRepo(ctx context.Context, repo *repository) (*fusefs.ArtifactFuse, error) {
	repo.mu.Lock()
	if repo.backend != nil {
		backend := repo.backend
		repo.mu.Unlock()
		return backend, nil
	}
	if pending := repo.preparing; pending != nil {
		repo.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, syscall.EINTR
		case <-pending:
		}
		repo.mu.Lock()
		backend, err := repo.backend, repo.err
		repo.mu.Unlock()
		return backend, err
	}
	pending := make(chan struct{})
	repo.preparing = pending
	entry := repo.entry
	repo.mu.Unlock()
	backend, err := fs.activate(ctx, entry)
	if err == nil && backend == nil {
		err = fmt.Errorf("activation returned no filesystem for %s", entry.ID)
	}
	repo.mu.Lock()
	repo.backend = backend
	if err != nil {
		repo.backend = nil
		repo.err = syscall.EIO
	} else {
		repo.err = nil
	}
	repo.preparing = nil
	close(pending)
	repo.mu.Unlock()
	if err != nil {
		return nil, syscall.EIO
	}
	return backend, nil
}

func (fs *FileSystem) backend(ctx context.Context, id fuseops.InodeID) (*inode, *fusefs.ArtifactFuse, error) {
	n, err := fs.node(id)
	if err != nil {
		return nil, nil, err
	}
	if n.repo == nil {
		return nil, nil, syscall.EROFS
	}
	backend, err := fs.activateRepo(ctx, n.repo)
	return n, backend, err
}

func (fs *FileSystem) mapInode(repo *repository, local fuseops.InodeID, lookup bool) fuseops.InodeID {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.mapInodeLocked(repo, local, lookup)
}

func (fs *FileSystem) mapInodeLocked(repo *repository, local fuseops.InodeID, lookup bool) fuseops.InodeID {
	if local == fuseops.RootInodeID {
		for id, n := range fs.inodes {
			if n.repo == repo && n.path != "" && fs.paths[n.path] == id {
				return id
			}
		}
	}
	key := inodeKey{repo, local}
	id, ok := fs.childInodes[key]
	if !ok {
		id = fs.nextInode
		fs.nextInode++
		fs.childInodes[key] = id
		fs.inodes[id] = &inode{repo: repo, local: local}
	}
	if lookup {
		fs.inodes[id].refs++
	}
	return id
}

func (fs *FileSystem) mapDirectoryInode(h *handle, local fuseops.InodeID) fuseops.InodeID {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	id := fs.mapInodeLocked(h.repo, local, false)
	if h.direntInodes == nil {
		h.direntInodes = make(map[fuseops.InodeID]struct{})
	}
	if _, retained := h.direntInodes[id]; !retained {
		h.direntInodes[id] = struct{}{}
		fs.inodes[id].dirRefs++
	}
	return id
}

func (fs *FileSystem) addHandle(h *handle) fuseops.HandleID {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	id := fs.nextHandle
	fs.nextHandle++
	fs.handles[id] = h
	return id
}

func (fs *FileSystem) getHandle(id fuseops.HandleID, directory bool) (*handle, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	h := fs.handles[id]
	if h == nil || h.directory != directory {
		return nil, syscall.EBADF
	}
	return h, nil
}

func (fs *FileSystem) directoryAttrs() fuseops.InodeAttributes {
	return fuseops.InodeAttributes{Size: 4096, Nlink: 2, Mode: os.ModeDir | 0o755, Atime: fs.created, Mtime: fs.created, Ctime: fs.created, Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
}

func (fs *FileSystem) StatFS(ctx context.Context, op *fuseops.StatFSOp) error {
	// The same virtual capacity as an ordinary ArtifactFS working tree.
	return (&fusefs.ArtifactFuse{}).StatFS(ctx, op)
}

func (fs *FileSystem) LookUpInode(ctx context.Context, op *fuseops.LookUpInodeOp) error {
	if !validChild(op.Name) {
		return syscall.EINVAL
	}
	n, err := fs.node(op.Parent)
	if err != nil {
		return err
	}
	if n.repo == nil {
		path := model.CleanPath(n.path + "/" + op.Name)
		fs.mu.Lock()
		id, ok := fs.paths[path]
		fs.mu.Unlock()
		if !ok {
			return syscall.ENOENT
		}
		op.Entry = fuseops.ChildInodeEntry{Child: id, Attributes: fs.directoryAttrs(), AttributesExpiration: time.Now().Add(time.Second), EntryExpiration: time.Now().Add(time.Second)}
		return nil
	}
	backend, err := fs.activateRepo(ctx, n.repo)
	if err != nil {
		return err
	}
	child := *op
	child.Parent = n.local
	if err := backend.LookUpInode(ctx, &child); err != nil {
		return err
	}
	child.Parent = op.Parent
	child.Entry.Child = fs.mapInode(n.repo, child.Entry.Child, true)
	*op = child
	return nil
}

func (fs *FileSystem) GetInodeAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) error {
	n, err := fs.node(op.Inode)
	if err != nil {
		return err
	}
	if n.path != "" {
		op.Attributes = fs.directoryAttrs()
		op.AttributesExpiration = time.Now().Add(time.Second)
		return nil
	}
	backend, err := fs.activateRepo(ctx, n.repo)
	if err != nil {
		return err
	}
	child := *op
	child.Inode = n.local
	if err := backend.GetInodeAttributes(ctx, &child); err != nil {
		return err
	}
	child.Inode = op.Inode
	*op = child
	return nil
}

func (fs *FileSystem) ForgetInode(ctx context.Context, op *fuseops.ForgetInodeOp) error {
	fs.mu.Lock()
	n := fs.inodes[op.Inode]
	if n == nil || n.path != "" {
		fs.mu.Unlock()
		return nil
	}
	copy := *n
	if op.N >= n.refs {
		n.refs = 0
		if n.dirRefs == 0 {
			delete(fs.inodes, op.Inode)
			delete(fs.childInodes, inodeKey{n.repo, n.local})
		}
	} else {
		n.refs -= op.N
	}
	fs.mu.Unlock()
	copy.repo.mu.Lock()
	backend := copy.repo.backend
	copy.repo.mu.Unlock()
	if backend != nil {
		child := *op
		child.Inode = copy.local
		return backend.ForgetInode(ctx, &child)
	}
	return nil
}

func (fs *FileSystem) BatchForget(ctx context.Context, op *fuseops.BatchForgetOp) error {
	for _, entry := range op.Entries {
		if err := fs.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: entry.Inode, N: entry.N}); err != nil {
			return err
		}
	}
	return nil
}

func (fs *FileSystem) OpenDir(ctx context.Context, op *fuseops.OpenDirOp) error {
	n, err := fs.node(op.Inode)
	if err != nil {
		return err
	}
	if n.repo == nil {
		fs.mu.Lock()
		var entries []catalogEntry
		prefix := ""
		if n.path != "." {
			prefix = n.path + "/"
		}
		for path, id := range fs.paths {
			if path == "." {
				continue
			}
			name, ok := strings.CutPrefix(path, prefix)
			if ok && !strings.Contains(name, "/") {
				entries = append(entries, catalogEntry{name, id})
			}
		}
		fs.mu.Unlock()
		sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
		op.Handle = fs.addHandle(&handle{directory: true, entries: entries})
		return nil
	}
	backend, err := fs.activateRepo(ctx, n.repo)
	if err != nil {
		return err
	}
	child := *op
	child.Inode = n.local
	if err := backend.OpenDir(ctx, &child); err != nil {
		return err
	}
	child.Inode = op.Inode
	child.Handle = fs.addHandle(&handle{directory: true, repo: n.repo, backend: backend, local: child.Handle, inode: n.local})
	*op = child
	return nil
}

func (fs *FileSystem) ReadDir(ctx context.Context, op *fuseops.ReadDirOp) error {
	h, err := fs.getHandle(op.Handle, true)
	if err != nil {
		return err
	}
	if h.backend != nil {
		child := *op
		child.Handle = h.local
		child.Inode = h.inode
		// Ordinary READDIR retains an identity without granting a lookup
		// reference. Subsequent LOOKUP returns the same global child ID.
		if err := h.backend.ReadDirWithInodeMapping(ctx, &child, func(local fuseops.InodeID) fuseops.InodeID { return fs.mapDirectoryInode(h, local) }); err != nil {
			return err
		}
		child.Handle, child.Inode = op.Handle, op.Inode
		*op = child
		return nil
	}
	if uint64(op.Offset) > uint64(len(h.entries)) {
		return nil
	}
	for i := int(op.Offset); i < len(h.entries); i++ {
		e := h.entries[i]
		n := fuseutil.WriteDirent(op.Dst[op.BytesRead:], fuseutil.Dirent{Offset: fuseops.DirOffset(i + 1), Inode: e.inode, Name: e.name, Type: fuseutil.DT_Directory})
		if n == 0 {
			break
		}
		op.BytesRead += n
	}
	return nil
}

func (fs *FileSystem) ReadDirPlus(ctx context.Context, op *fuseops.ReadDirPlusOp) error {
	// The upstream encoder currently assumes Linux's fuse_attr wire layout.
	// Darwin's larger layout cannot be encoded safely; mounts use READDIR and
	// LOOKUP, just as ordinary ArtifactFS mounts do on macOS.
	if runtime.GOOS == "darwin" {
		return syscall.ENOSYS
	}
	h, err := fs.getHandle(op.Handle, true)
	if err != nil {
		return err
	}
	if h.backend != nil {
		child := *op
		child.Handle = h.local
		child.Inode = h.inode
		if err := h.backend.ReadDirPlusWithInodeMapping(ctx, &child, func(local fuseops.InodeID) fuseops.InodeID { return fs.mapInode(h.repo, local, true) }); err != nil {
			return err
		}
		child.Handle, child.Inode = op.Handle, op.Inode
		*op = child
		return nil
	}
	if uint64(op.Offset) > uint64(len(h.entries)) {
		return nil
	}
	for i := int(op.Offset); i < len(h.entries); i++ {
		e := h.entries[i]
		entry := fuseops.ChildInodeEntry{Child: e.inode, Attributes: fs.directoryAttrs(), AttributesExpiration: time.Now().Add(time.Second), EntryExpiration: time.Now().Add(time.Second)}
		n := fuseutil.WriteDirentPlus(op.Dst[op.BytesRead:], fuseutil.DirentPlus{Dirent: fuseutil.Dirent{Offset: fuseops.DirOffset(i + 1), Inode: e.inode, Name: e.name, Type: fuseutil.DT_Directory}, Entry: entry})
		if n == 0 {
			break
		}
		op.BytesRead += n
	}
	return nil
}

func (fs *FileSystem) ReleaseDirHandle(ctx context.Context, op *fuseops.ReleaseDirHandleOp) error {
	h, err := fs.getHandle(op.Handle, true)
	if err != nil {
		return err
	}
	fs.mu.Lock()
	delete(fs.handles, op.Handle)
	for id := range h.direntInodes {
		n := fs.inodes[id]
		if n == nil {
			continue
		}
		n.dirRefs--
		if n.refs == 0 && n.dirRefs == 0 {
			delete(fs.inodes, id)
			delete(fs.childInodes, inodeKey{n.repo, n.local})
		}
	}
	fs.mu.Unlock()
	if h.backend != nil {
		child := *op
		child.Handle = h.local
		return h.backend.ReleaseDirHandle(ctx, &child)
	}
	return nil
}

// Mount mounts the complete catalogue at a chosen existing, empty directory.
// It uses macFUSE on macOS and the standard FUSE backend on Linux.
func Mount(ctx context.Context, root string, fs *FileSystem) (fusefs.MountedFS, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg := &fuse.MountConfig{FSName: "RepoReach", Subtype: "reporeach", DisableWritebackCaching: true, UseVectoredRead: true}
	if runtime.GOOS == "darwin" {
		cfg.FuseImpl = fuse.FUSEImplMacFUSE
	}
	mounted, err := fuse.Mount(root, fuseutil.NewFileSystemServer(fs), cfg)
	if err != nil {
		return nil, fmt.Errorf("mount catalogue: %w", err)
	}
	return &mountedCatalog{MountedFileSystem: mounted, root: root}, nil
}

type mountedCatalog struct {
	*fuse.MountedFileSystem
	root string
}

func (m *mountedCatalog) Unmount() error { return fusefs.TryUnmount(m.root) }
