//go:build !windows

// Modified by Enough Tools for EnoughRepos.
// Based on Cloudflare ArtifactFS (Apache-2.0); see UPSTREAM.md.

package fusefs

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

// MountedFS matches tigrisfs's interface for mount lifecycle.
type MountedFS interface {
	Join(ctx context.Context) error
	Unmount() error
}

// ArtifactFuse is the FUSE adapter following the tigrisfs GoofysFuse pattern:
// embed NotImplementedFileSystem + core state, thin operation wrappers.
type ArtifactFuse struct {
	fuseutil.NotImplementedFileSystem
	repo           model.RepoConfig
	resolver       *Resolver
	engine         *Engine
	gitfileContent []byte // synthesized .git gitfile, computed once

	mu           sync.RWMutex
	handleOps    sync.RWMutex
	inodes       map[fuseops.InodeID]*InodeRef
	pathToInode  map[string]fuseops.InodeID
	nextInodeID  fuseops.InodeID
	dirHandles   map[fuseops.HandleID]*DirHandle
	fileHandles  map[fuseops.HandleID]*FileHandle
	nextHandleID fuseops.HandleID
}

type InodeRef struct {
	ID         fuseops.InodeID
	Path       string
	Type       string // file, dir, symlink
	Mode       uint32
	Gen        int64
	Refcnt     int64
	DirRefs    int64 // directory handles retaining READDIR-only identities
	IsRoot     bool
	Overlay    bool
	Stale      bool
	MetadataID model.MetadataObjectID
}

type detachedMetadata struct {
	mu    sync.Mutex
	mtime *time.Time
}

type DirHandle struct {
	inode        *InodeRef
	gen          int64
	commitTime   int64
	entries      []ReaddirEntry
	direntInodes map[fuseops.InodeID]struct{}
	entryInodes  map[string]fuseops.InodeID
}

type FileHandle struct {
	mu               sync.Mutex
	inode            *InodeRef
	path             string
	cacheFile        *os.File
	cacheGeneration  int64
	invalidateSeq    uint64
	detached         bool
	detachedMetadata *detachedMetadata
	access           uint32 // accepted open capability: 1 read, 2 write, 3 both
}

// ReaddirEntry holds child metadata, avoiding per-child Getattr or snapshot lookups.
type ReaddirEntry struct {
	Name        string
	Type        string // file, dir, symlink
	Mode        uint32
	ObjectOID   string
	SizeState   string
	SizeBytes   int64
	FromOverlay bool
	MtimeUnixNs int64
	CtimeUnixNs int64
	MetadataID  model.MetadataObjectID
}

func (e ReaddirEntry) direntType() fuseutil.DirentType {
	switch e.Type {
	case "dir":
		return fuseutil.DT_Directory
	case "symlink":
		return fuseutil.DT_Link
	default:
		return fuseutil.DT_File
	}
}

func NewArtifactFuse(repo model.RepoConfig, resolver *Resolver, engine *Engine) *ArtifactFuse {
	fs := &ArtifactFuse{
		repo:           repo,
		resolver:       resolver,
		engine:         engine,
		gitfileContent: fmt.Appendf(nil, "gitdir: %s\n", repo.GitDir),
		inodes:         make(map[fuseops.InodeID]*InodeRef),
		pathToInode:    make(map[string]fuseops.InodeID),
		nextInodeID:    fuseops.RootInodeID + 1,
		dirHandles:     make(map[fuseops.HandleID]*DirHandle),
		fileHandles:    make(map[fuseops.HandleID]*FileHandle),
		nextHandleID:   1,
	}
	root := &InodeRef{ID: fuseops.RootInodeID, Path: ".", Type: "dir", Mode: 0o755, Refcnt: 1, IsRoot: true}
	fs.inodes[fuseops.RootInodeID] = root
	fs.pathToInode["."] = fuseops.RootInodeID
	return fs
}

func (fs *ArtifactFuse) allocInode(path, typ string, mode uint32, gen int64) *InodeRef {
	// Caller must hold fs.mu write lock.
	if id, ok := fs.pathToInode[path]; ok {
		if ref, ok := fs.inodes[id]; ok {
			if ref.Type == typ {
				ref.Refcnt++
				return ref
			}
			ref.Stale = true
			delete(fs.pathToInode, path)
		}
	}
	id := fs.nextInodeID
	fs.nextInodeID++
	ref := &InodeRef{ID: id, Path: path, Type: typ, Mode: mode, Gen: gen, Refcnt: 1}
	fs.inodes[id] = ref
	fs.pathToInode[path] = id
	return ref
}

func (fs *ArtifactFuse) requireInode(id fuseops.InodeID, missing error) (*InodeRef, error) {
	fs.mu.RLock()
	ref := fs.inodes[id]
	if ref == nil || ref.Stale {
		fs.mu.RUnlock()
		return nil, missing
	}
	snapshot := *ref
	fs.mu.RUnlock()
	return &snapshot, nil
}

func (fs *ArtifactFuse) dropInodeLookup(id fuseops.InodeID) {
	fs.mu.Lock()
	ref, ok := fs.inodes[id]
	if ok {
		ref.Refcnt--
		if ref.Refcnt <= 0 && ref.DirRefs == 0 && !ref.IsRoot {
			delete(fs.inodes, id)
			if fs.pathToInode[ref.Path] == id {
				delete(fs.pathToInode, ref.Path)
			}
		}
	}
	fs.mu.Unlock()
}

func (fs *ArtifactFuse) moveInodePath(oldPath, newPath string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	type inodeMove struct {
		id      fuseops.InodeID
		oldPath string
		newPath string
	}
	var moves []inodeMove
	movingIDs := map[fuseops.InodeID]bool{}
	for path, id := range fs.pathToInode {
		if samePathOrDescendant(path, oldPath) {
			moves = append(moves, inodeMove{
				id:      id,
				oldPath: path,
				newPath: newPath + strings.TrimPrefix(path, oldPath),
			})
			movingIDs[id] = true
		}
	}
	for path, id := range fs.pathToInode {
		if samePathOrDescendant(path, newPath) && !movingIDs[id] {
			if replaced := fs.inodes[id]; replaced != nil {
				replaced.Stale = true
			}
			delete(fs.pathToInode, path)
		}
	}
	for _, move := range moves {
		delete(fs.pathToInode, move.oldPath)
	}
	for _, move := range moves {
		if ref := fs.inodes[move.id]; ref != nil {
			ref.Path = move.newPath
			fs.pathToInode[move.newPath] = move.id
		}
	}
	for _, dh := range fs.dirHandles {
		if samePathOrDescendant(dh.inode.Path, oldPath) {
			dh.inode.Path = newPath + strings.TrimPrefix(dh.inode.Path, oldPath)
		}
	}
}

func samePathOrDescendant(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

func (fs *ArtifactFuse) childPath(ctx context.Context, parentID fuseops.InodeID, name string) (*InodeRef, string, error) {
	parent, err := fs.requireLiveInode(ctx, parentID, syscall.ENOENT)
	if err != nil {
		return nil, "", err
	}
	return parent, cleanChildPath(parent.Path, name), nil
}

func (fs *ArtifactFuse) dirHandle(handleID fuseops.HandleID) (*DirHandle, error) {
	fs.mu.RLock()
	dh := fs.dirHandles[handleID]
	fs.mu.RUnlock()
	if dh == nil {
		return nil, syscall.EBADF
	}
	return dh, nil
}

func (fs *ArtifactFuse) fileHandle(handleID fuseops.HandleID) (*FileHandle, error) {
	fs.mu.RLock()
	fh := fs.fileHandles[handleID]
	fs.mu.RUnlock()
	if fh == nil {
		return nil, syscall.EBADF
	}
	return fh, nil
}

func (fs *ArtifactFuse) closeCachedFilesForPath(path string) {
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := fh.path == path && !fh.detached
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.closeCachedFile()
	}
}

func (fs *ArtifactFuse) pinOpenHandles(path string) error {
	ov, ok, err := fs.engine.Overlay.Lookup(context.Background(), path)
	if err != nil || !ok || ov.IsDeleted() || ov.BackingPath == "" {
		return err
	}
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := fh.path == path && !fh.detached
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	if len(handles) == 0 {
		return nil
	}
	// All still-attached handles at this namespace path share the same backing
	// file, even when a lookup was forgotten and recreated while it was open.
	// Give this group one metadata object without retaining permanent tombstones.
	metadata := &detachedMetadata{}
	mtime := time.Unix(0, ov.MtimeUnixNs)
	metadata.mu.Lock()
	metadata.mtime = &mtime
	metadata.mu.Unlock()
	for _, fh := range handles {
		fh.mu.Lock()
		fh.detachedMetadata = metadata
		if fh.cacheFile == nil || fh.cacheGeneration != -1 {
			access := fh.access
			if access == 0 {
				access = 3 // synthetic legacy handles have both capabilities
			}
			f, openErr := os.OpenFile(ov.BackingPath, fileOpenFlags(access), 0)
			if openErr != nil {
				fh.mu.Unlock()
				return openErr
			}
			old := fh.cacheFile
			fh.cacheFile = f
			fh.cacheGeneration = -1
			if old != nil {
				_ = old.Close()
			}
		}
		fh.mu.Unlock()
	}
	return nil
}

// Promote and bind every open reader before the first mutation or permission
// change. Immutable blob descriptors cannot observe writes to a COW backing,
// and reopening them after chmod would lose their accepted read capability.
// The caller holds handleOps exclusively to keep promotion and rebinding atomic
// with respect to all reads, writes, opens, and namespace changes.
func (fs *ArtifactFuse) prepareOpenHandlesForOverlay(ctx context.Context, path string) error {
	fs.resolver.transition.RLock()
	defer fs.resolver.transition.RUnlock()
	if err := fs.engine.ensureOverlay(ctx, path); err != nil {
		return err
	}
	return fs.pinOpenHandles(path)
}

func (fs *ArtifactFuse) detachOpenHandles(path string) {
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := !fh.detached && samePathOrDescendant(fh.path, path)
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.mu.Lock()
		fh.detached = true
		fh.mu.Unlock()
	}
}

func (fs *ArtifactFuse) moveOpenHandles(oldPath, newPath string) {
	fs.mu.RLock()
	var handles []*FileHandle
	for _, fh := range fs.fileHandles {
		fh.mu.Lock()
		matches := !fh.detached && samePathOrDescendant(fh.path, oldPath)
		fh.mu.Unlock()
		if matches {
			handles = append(handles, fh)
		}
	}
	fs.mu.RUnlock()
	for _, fh := range handles {
		fh.mu.Lock()
		fh.path = newPath + strings.TrimPrefix(fh.path, oldPath)
		fh.mu.Unlock()
	}
}

func (fh *FileHandle) read(ctx context.Context, engine *Engine, off int64, size int) ([]byte, error) {
	fh.mu.Lock()
	path := fh.path
	if fh.detached && fh.cacheFile != nil {
		defer fh.mu.Unlock()
		return readFileChunkFrom(fh.cacheFile, off, size)
	}
	fh.mu.Unlock()
	currentGen := engine.Resolver.Generation()
	fh.mu.Lock()
	if fh.cacheFile != nil && (fh.cacheGeneration == -1 || fh.cacheGeneration == currentGen) {
		defer fh.mu.Unlock()
		return readFileChunkFrom(fh.cacheFile, off, size)
	}
	if fh.cacheFile != nil {
		f := fh.cacheFile
		fh.cacheFile = nil
		fh.cacheGeneration = 0
		fh.invalidateSeq++
		fh.mu.Unlock()
		_ = f.Close()
		fh.mu.Lock()
	}
	seq := fh.invalidateSeq
	fh.mu.Unlock()

	f, gen, ok, err := engine.BaseCacheFile(ctx, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return engine.Read(ctx, path, off, size)
	}
	fh.mu.Lock()
	if fh.invalidateSeq != seq || gen != engine.Resolver.Generation() {
		fh.mu.Unlock()
		_ = f.Close()
		return engine.Read(ctx, path, off, size)
	}
	if fh.cacheFile != nil && fh.cacheGeneration == gen {
		_ = f.Close()
		f = fh.cacheFile
	} else {
		if fh.cacheFile != nil {
			_ = fh.cacheFile.Close()
		}
		fh.cacheFile = f
		fh.cacheGeneration = gen
	}
	defer fh.mu.Unlock()
	return readFileChunkFrom(f, off, size)
}

func (fh *FileHandle) closeCachedFile() {
	fh.mu.Lock()
	if fh.detached || fh.cacheGeneration == -1 {
		fh.mu.Unlock()
		return
	}
	f := fh.cacheFile
	fh.cacheFile = nil
	fh.cacheGeneration = 0
	fh.invalidateSeq++
	fh.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

func (fh *FileHandle) release() {
	fh.mu.Lock()
	f := fh.cacheFile
	fh.cacheFile = nil
	fh.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// --- FUSE operations ---

func (fs *ArtifactFuse) StatFS(_ context.Context, op *fuseops.StatFSOp) error {
	const blockSize = 4096
	const totalSpace = 1 * 1024 * 1024 * 1024 * 1024 * 1024
	const totalBlocks = totalSpace / blockSize
	op.BlockSize = blockSize
	op.Blocks = totalBlocks
	op.BlocksFree = totalBlocks
	op.BlocksAvailable = totalBlocks
	op.IoSize = 1 * 1024 * 1024
	op.Inodes = 1_000_000_000
	op.InodesFree = 1_000_000_000
	return nil
}

func (fs *ArtifactFuse) LookUpInode(ctx context.Context, op *fuseops.LookUpInodeOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	parent, err := fs.requireLiveInode(ctx, op.Parent, syscall.ENOENT)
	if err != nil {
		return err
	}

	childPath := cleanChildPath(parent.Path, op.Name)

	// Synthesize .git gitfile in root
	if parent.IsRoot && op.Name == ".git" {
		fs.mu.Lock()
		ref := fs.allocInode(".git", "file", 0o644, fs.resolver.Generation())
		fs.mu.Unlock()
		op.Entry.Child = ref.ID
		op.Entry.Attributes = fs.gitFileAttrs()
		setChildEntryExpiry(&op.Entry, time.Minute)
		if err := fs.applyMetadataCtime(ctx, ref, &op.Entry.Attributes); err != nil {
			fs.dropInodeLookup(ref.ID)
			return err
		}
		return nil
	}

	mode, size, typ, mtime, ctime, err := fs.resolveAttrs(ctx, childPath)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return syscall.ENOENT
		}
		return fuseOperationError("lookup", err)
	}
	if err := fs.refreshMetadataPath(ctx, childPath, typ); err != nil {
		return err
	}

	fs.mu.Lock()
	ref := fs.allocInode(childPath, typ, mode, fs.resolver.Generation())
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	op.Entry.Attributes = inodeAttrs(mode, uint64(size), typ, mtime, ctime)
	setChildEntryExpiry(&op.Entry, time.Second)
	if err := fs.applyMetadataCtime(ctx, ref, &op.Entry.Attributes); err != nil {
		fs.dropInodeLookup(ref.ID)
		return err
	}
	return nil
}

func (fs *ArtifactFuse) GetInodeAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	ref, err := fs.requireLiveInode(ctx, op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}

	if ref.IsRoot {
		if fs.resolver != nil {
			if mode, size, typ, mtime, ctime, err := fs.resolver.Getattr(ref.Path); err == nil {
				op.Attributes = inodeAttrs(mode, uint64(size), typ, mtime, ctime)
				op.AttributesExpiration = attrExpiry(time.Second)
				return fs.applyMetadataCtime(ctx, ref, &op.Attributes)
			}
		}
		now := time.Now()
		op.Attributes = inodeAttrs(ref.Mode, 4096, "dir", now, now)
		op.AttributesExpiration = attrExpiry(time.Second)
		return fs.applyMetadataCtime(ctx, ref, &op.Attributes)
	}

	if ref.Path == ".git" {
		op.Attributes = fs.gitFileAttrs()
		op.AttributesExpiration = attrExpiry(time.Minute)
		return fs.applyMetadataCtime(ctx, ref, &op.Attributes)
	}

	mode, size, typ, mtime, ctime, err := fs.resolveAttrs(ctx, ref.Path)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return syscall.ENOENT
		}
		return fuseOperationError("getattr", err)
	}
	op.Attributes = inodeAttrs(mode, uint64(size), typ, mtime, ctime)
	op.AttributesExpiration = attrExpiry(time.Second)
	return fs.applyMetadataCtime(ctx, ref, &op.Attributes)
}

func (fs *ArtifactFuse) resolveAttrs(ctx context.Context, path string) (mode uint32, size int64, nodeType string, mtime time.Time, ctime time.Time, err error) {
	n, generation, commitTime, err := fs.resolver.ResolvePathState(path)
	if err != nil {
		return 0, 0, "", time.Time{}, time.Time{}, err
	}
	if n.FromOverlay {
		typ := n.Overlay.NodeType()
		mt := time.Unix(0, n.Overlay.MtimeUnixNs)
		ct := time.Unix(0, n.Overlay.CtimeUnixNs)
		return n.Overlay.Mode, n.Overlay.SizeBytes, typ, mt, ct, nil
	}

	mode = normalizeMode(n.Base.Mode, n.Base.Type)
	size = n.Base.SizeBytes
	if n.Base.Type == "file" && n.Base.SizeState != "known" && n.Base.ObjectOID != "" {
		_, hydratedSize, hErr := fs.engine.Hydrator.EnsureHydrated(ctx, fs.repo, n.Base)
		if hErr != nil {
			return 0, 0, "", time.Time{}, time.Time{}, hErr
		}
		size = hydratedSize
	} else if n.Base.Type == "symlink" && n.Base.SizeState != "known" && n.Base.ObjectOID != "" {
		target, readErr := fs.engine.Hydrator.ReadBlob(ctx, fs.repo, n.Base, model.MaxSymlinkTargetBytes)
		if readErr != nil {
			return 0, 0, "", time.Time{}, time.Time{}, readErr
		}
		size = int64(len(target))
	}

	// Base files use the HEAD commit timestamp for mtime so tools like
	// make see a stable, meaningful value.
	ct := commitTime
	if ct == 0 {
		ct = generation // fallback: commit time unavailable
	}
	mt := time.Unix(ct, 0)
	return mode, size, n.Base.Type, mt, mt, nil
}

func (fs *ArtifactFuse) SetInodeAttributes(ctx context.Context, op *fuseops.SetInodeAttributesOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	ref, err := fs.requireLiveInode(ctx, op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	if ref.Type == "file" && ref.Path != ".git" && (op.Size != nil || op.Mode != nil || op.Mtime != nil) {
		if err := fs.prepareOpenHandlesForOverlay(ctx, ref.Path); err != nil {
			return fuseOperationError("prepare file attributes", err)
		}
	}
	if op.Size != nil {
		fs.closeCachedFilesForPath(ref.Path)
		if err := fs.engine.Truncate(ctx, ref.Path, int64(*op.Size)); err != nil {
			return fuseOperationError("truncate", err)
		}
		fs.closeCachedFilesForPath(ref.Path)
	}
	if op.Mode != nil {
		if err := fs.engine.SetMode(ctx, ref.Path, uint32(op.Mode.Perm())); err != nil {
			if errors.Is(err, iofs.ErrInvalid) {
				return syscall.ENOTSUP
			}
			if errors.Is(err, iofs.ErrNotExist) {
				return syscall.ENOENT
			}
			return fuseOperationError("chmod", err)
		}
	}
	// Handle mtime updates (e.g., from touch)
	if op.Mtime != nil {
		fs.closeCachedFilesForPath(ref.Path)
		if err := fs.engine.SetMtime(ctx, ref.Path, *op.Mtime); err != nil {
			if errors.Is(err, iofs.ErrInvalid) {
				return syscall.ENOTSUP
			}
			return fuseOperationError("set mtime", err)
		}
		fs.closeCachedFilesForPath(ref.Path)
	}
	mode, size, typ, mtime, ctime, err := fs.resolver.Getattr(ref.Path)
	if err != nil {
		return fuseOperationError("getattr after update", err)
	}
	op.Attributes = inodeAttrs(mode, uint64(size), typ, mtime, ctime)
	op.AttributesExpiration = attrExpiry(time.Second)
	return nil
}

func (fs *ArtifactFuse) ForgetInode(_ context.Context, op *fuseops.ForgetInodeOp) error {
	fs.mu.Lock()
	ref, ok := fs.inodes[op.Inode]
	if ok {
		ref.Refcnt -= int64(op.N)
		if ref.Refcnt <= 0 && ref.DirRefs == 0 && !ref.IsRoot {
			delete(fs.inodes, op.Inode)
			if fs.pathToInode[ref.Path] == op.Inode {
				delete(fs.pathToInode, ref.Path)
			}
		}
	}
	fs.mu.Unlock()
	return nil
}

func (fs *ArtifactFuse) OpenDir(ctx context.Context, op *fuseops.OpenDirOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	ref, err := fs.requireLiveInode(ctx, op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	// Eagerly load children at open time to avoid races on concurrent ReadDir.
	fs.resolver.transition.RLock()
	gen, commitTime := fs.resolver.Generation(), fs.resolver.CommitTime()
	entries, err := fs.resolver.readdirTypedAt(ctx, ref.Path, gen)
	if err != nil {
		fs.resolver.transition.RUnlock()
		if errors.Is(err, iofs.ErrNotExist) {
			return syscall.ENOENT
		}
		return fuseOperationError("opendir", err)
	}
	if ref.IsRoot {
		entries = append([]ReaddirEntry{{Name: ".git", Type: "file", Mode: 0o644, SizeBytes: int64(len(fs.gitfileContent)), SizeState: "known"}}, entries...)
	}
	if fs.engine != nil && fs.engine.Overlay != nil {
		for i := range entries {
			object, err := fs.engine.Overlay.BindMetadata(ctx, cleanChildPath(ref.Path, entries[i].Name), entries[i].Type)
			if err != nil {
				fs.resolver.transition.RUnlock()
				return xattrError("bind directory entry metadata", err)
			}
			entries[i].MetadataID = object.ID
		}
	}
	fs.resolver.transition.RUnlock()

	dh := &DirHandle{inode: ref, gen: gen, commitTime: commitTime, entries: entries}
	fs.mu.Lock()
	handle := fs.nextHandleID
	fs.nextHandleID++
	fs.dirHandles[handle] = dh
	fs.mu.Unlock()
	op.Handle = handle
	return nil
}

func (fs *ArtifactFuse) ReadDir(ctx context.Context, op *fuseops.ReadDirOp) error {
	return fs.ReadDirWithInodeMapping(ctx, op, nil)
}

// ReadDirWithInodeMapping lets a containing filesystem translate the child
// filesystem's inode namespace. The mapper must not acquire lookup references:
// ordinary READDIR does not grant the kernel an inode lookup reference.
func (fs *ArtifactFuse) ReadDirWithInodeMapping(ctx context.Context, op *fuseops.ReadDirOp, mapInode func(fuseops.InodeID) fuseops.InodeID) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	dh, err := fs.dirHandle(op.Handle)
	if err != nil {
		return err
	}

	offset := int(op.Offset)
	for i := offset; i < len(dh.entries); i++ {
		e := dh.entries[i]
		dirent := fuseutil.Dirent{
			Offset: fuseops.DirOffset(i + 1),
			Inode:  fuseops.RootInodeID + 1, // placeholder; kernel re-looks-up via LookUpInode
			Name:   e.Name,
			Type:   e.direntType(),
		}
		n := fuseutil.WriteDirent(op.Dst[op.BytesRead:], dirent)
		if n == 0 {
			break
		}
		if mapInode != nil {
			// A catalogue needs genuine, nonzero child IDs: libc readdir skips
			// d_ino=0 entries, and a shared placeholder aliases all siblings.
			// READDIR grants no lookup references, so retain a zero-reference
			// identity until a later lookup/forget establishes its lifetime.
			ref, err := fs.directoryEntryInode(ctx, dh, e, false)
			if err != nil {
				return err
			}
			dirent.Inode = mapInode(ref.ID)
			n = fuseutil.WriteDirent(op.Dst[op.BytesRead:], dirent)
		}
		op.BytesRead += n
	}
	return nil
}

func (fs *ArtifactFuse) ReadDirPlus(ctx context.Context, op *fuseops.ReadDirPlusOp) error {
	return fs.ReadDirPlusWithInodeMapping(ctx, op, nil)
}

// ReadDirPlusWithInodeMapping translates only entries actually returned to the
// kernel. Each mapper call represents one lookup reference, just as LookUpInode.
func (fs *ArtifactFuse) ReadDirPlusWithInodeMapping(ctx context.Context, op *fuseops.ReadDirPlusOp, mapInode func(fuseops.InodeID) fuseops.InodeID) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	dh, err := fs.dirHandle(op.Handle)
	if err != nil {
		return err
	}

	offset := int(op.Offset)
	for i := offset; i < len(dh.entries); i++ {
		e := dh.entries[i]
		// Check the fixed wire record plus name before acquiring its lookup.
		preview := fuseutil.DirentPlus{Dirent: fuseutil.Dirent{Name: e.Name}, Entry: fuseops.ChildInodeEntry{Child: 1}}
		if fuseutil.WriteDirentPlus(op.Dst[op.BytesRead:], preview) == 0 {
			break
		}
		entry, err := fs.childEntryFromReaddir(ctx, dh, e)
		if err != nil {
			if errors.Is(err, iofs.ErrNotExist) {
				return syscall.ENOENT
			}
			return fuseOperationError("readdirplus", err)
		}
		dirent := fuseutil.DirentPlus{
			Dirent: fuseutil.Dirent{
				Offset: fuseops.DirOffset(i + 1),
				Inode:  entry.Child,
				Name:   e.Name,
				Type:   e.direntType(),
			},
			Entry: entry,
		}
		// Check capacity before mapping so an entry that does not fit does not
		// leak a lookup reference in the containing filesystem.
		n := fuseutil.WriteDirentPlus(op.Dst[op.BytesRead:], dirent)
		if n == 0 {
			fs.dropInodeLookup(entry.Child)
			break
		}
		if mapInode != nil {
			dirent.Entry.Child = mapInode(entry.Child)
			dirent.Dirent.Inode = dirent.Entry.Child
			n = fuseutil.WriteDirentPlus(op.Dst[op.BytesRead:], dirent)
		}
		op.BytesRead += n
	}
	return nil
}

func (fs *ArtifactFuse) childEntryFromReaddir(ctx context.Context, dh *DirHandle, e ReaddirEntry) (fuseops.ChildInodeEntry, error) {
	path := cleanChildPath(dh.inode.Path, e.Name)
	ref, err := fs.directoryEntryInode(ctx, dh, e, true)
	if err != nil {
		return fuseops.ChildInodeEntry{}, err
	}
	if path == ".git" {
		entry := fuseops.ChildInodeEntry{Child: ref.ID, Attributes: fs.gitFileAttrs()}
		setChildEntryExpiry(&entry, time.Minute)
		if err := fs.applyMetadataObjectCtime(ctx, e.MetadataID, &entry.Attributes); err != nil {
			fs.dropInodeLookup(ref.ID)
			return fuseops.ChildInodeEntry{}, err
		}
		return entry, nil
	}
	mode, size, typ, mtime, ctime := readdirAttrs(e, dh.gen, dh.commitTime)
	entry := fuseops.ChildInodeEntry{
		Child:      ref.ID,
		Attributes: inodeAttrs(mode, uint64(size), typ, mtime, ctime),
	}
	setChildEntryExpiry(&entry, time.Second)
	if err := fs.applyMetadataObjectCtime(ctx, e.MetadataID, &entry.Attributes); err != nil {
		fs.dropInodeLookup(ref.ID)
		return fuseops.ChildInodeEntry{}, err
	}
	return entry, nil
}

func readdirAttrs(e ReaddirEntry, gen, commitTime int64) (mode uint32, size int64, typ string, mtime time.Time, ctime time.Time) {
	typ = e.Type
	mode = normalizeMode(e.Mode, typ)
	size = e.SizeBytes
	if e.FromOverlay {
		return mode, size, typ, time.Unix(0, e.MtimeUnixNs), time.Unix(0, e.CtimeUnixNs)
	}
	ct := commitTime
	if ct == 0 {
		ct = gen
	}
	mt := time.Unix(ct, 0)
	return mode, size, typ, mt, mt
}

func (fs *ArtifactFuse) ReleaseDirHandle(_ context.Context, op *fuseops.ReleaseDirHandleOp) error {
	fs.mu.Lock()
	if dh := fs.dirHandles[op.Handle]; dh != nil {
		for id := range dh.direntInodes {
			ref := fs.inodes[id]
			if ref == nil {
				continue
			}
			ref.DirRefs--
			if ref.Refcnt <= 0 && ref.DirRefs == 0 && !ref.IsRoot {
				delete(fs.inodes, id)
				if fs.pathToInode[ref.Path] == id {
					delete(fs.pathToInode, ref.Path)
				}
			}
		}
	}
	delete(fs.dirHandles, op.Handle)
	fs.mu.Unlock()
	return nil
}

func (fs *ArtifactFuse) OpenFile(ctx context.Context, op *fuseops.OpenFileOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	ref, err := fs.requireLiveInode(ctx, op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	fh := &FileHandle{inode: ref, path: ref.Path, access: fileAccess(int(op.OpenFlags))}
	if ref.Path == ".git" && fh.access&2 != 0 {
		return syscall.EROFS
	}
	if ref.Path != ".git" {
		if fh.access&2 != 0 {
			if err := fs.prepareOpenHandlesForOverlay(ctx, ref.Path); err != nil {
				return fuseOperationError("prepare writable open", err)
			}
		}
		if ov, ok, err := fs.engine.Overlay.Lookup(ctx, ref.Path); err != nil {
			return fuseOperationError("open overlay metadata", err)
		} else if ok && ov.IsDeleted() {
			return syscall.ENOENT
		} else if ok {
			f, err := os.OpenFile(ov.BackingPath, fileOpenFlags(fh.access), 0)
			if err != nil {
				if os.IsPermission(err) {
					return syscall.EACCES
				}
				return fuseOperationError("open overlay data", err)
			}
			fh.cacheFile = f
			fh.cacheGeneration = -1
		} else {
			f, gen, base, err := fs.engine.BaseCacheFile(ctx, ref.Path)
			if err != nil {
				if errors.Is(err, iofs.ErrNotExist) {
					return syscall.ENOENT
				}
				return fuseOperationError("open base data", err)
			}
			if base {
				fh.cacheFile = f
				fh.cacheGeneration = gen
			}
		}
	}
	fs.mu.Lock()
	handle := fs.nextHandleID
	fs.nextHandleID++
	fs.fileHandles[handle] = fh
	fs.mu.Unlock()
	op.Handle = handle
	op.KeepPageCache = false
	return nil
}

func (fs *ArtifactFuse) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	fh, err := fs.fileHandle(op.Handle)
	if err != nil {
		return err
	}
	if fh.access != 0 && fh.access&1 == 0 {
		return syscall.EBADF
	}

	fh.mu.Lock()
	path := fh.path
	fh.mu.Unlock()
	if path == ".git" {
		start := int(op.Offset)
		if start >= len(fs.gitfileContent) {
			op.BytesRead = 0
			return nil
		}
		end := min(start+int(op.Size), len(fs.gitfileContent))
		op.Data = [][]byte{fs.gitfileContent[start:end]}
		op.BytesRead = end - start
		return nil
	}

	data, err := fh.read(ctx, fs.engine, op.Offset, int(op.Size))
	if err != nil {
		if os.IsNotExist(err) {
			return syscall.ENOENT
		}
		return fuseOperationError("read", err)
	}
	op.Data = [][]byte{data}
	op.BytesRead = len(data)
	return nil
}

func (fs *ArtifactFuse) WriteFile(ctx context.Context, op *fuseops.WriteFileOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	fh, err := fs.fileHandle(op.Handle)
	if err != nil {
		return err
	}
	if fh.access != 0 && fh.access&2 == 0 {
		return syscall.EBADF
	}
	fh.mu.Lock()
	if fh.detached {
		f := fh.cacheFile
		fh.mu.Unlock()
		if f == nil {
			return syscall.EIO
		}
		if _, err := f.WriteAt(op.Data, op.Offset); err != nil {
			return fuseOperationError("write detached data", err)
		}
		fh.clearDetachedMtime()
		return nil
	}
	path := fh.path
	file := fh.cacheFile
	retained := fh.cacheGeneration == -1 && file != nil
	fh.mu.Unlock()
	if retained {
		fs.resolver.transition.RLock()
		n, err := fs.engine.Overlay.WriteFileFrom(ctx, path, op.Offset, op.Data, file)
		fs.resolver.transition.RUnlock()
		if err != nil {
			if os.IsNotExist(err) {
				return syscall.ENOENT
			}
			return fuseOperationError("write retained data", err)
		}
		if n != len(op.Data) {
			return syscall.EIO
		}
		return nil
	}
	fs.closeCachedFilesForPath(path)
	n, err := fs.engine.Write(ctx, path, op.Offset, op.Data)
	if err != nil {
		return fuseOperationError("write", err)
	}
	if n != len(op.Data) {
		return syscall.EIO
	}
	fs.closeCachedFilesForPath(path)
	return nil
}

func (fs *ArtifactFuse) CreateFile(ctx context.Context, op *fuseops.CreateFileOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	_, childPath, err := fs.childPath(ctx, op.Parent, op.Name)
	if err != nil {
		return err
	}
	if childPath == ".git" {
		return syscall.EEXIST
	}
	fs.resolver.transition.RLock()
	defer fs.resolver.transition.RUnlock()
	if _, err := fs.resolver.resolvePath(childPath); err == nil {
		return syscall.EEXIST
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return fuseOperationError("check create name", err)
	}
	_, file, err := fs.engine.Overlay.CreateFileOpened(ctx, childPath, uint32(op.Mode))
	if err != nil {
		return fuseOperationError("create", err)
	}
	fs.retireInodePath(childPath)
	fs.mu.Lock()
	ref := fs.allocInode(childPath, "file", uint32(op.Mode), fs.resolver.Generation())
	handleRef := *ref
	fh := &FileHandle{inode: &handleRef, path: childPath, cacheFile: file, cacheGeneration: -1, access: fileAccess(int(op.OpenFlags))}
	handle := fs.nextHandleID
	fs.nextHandleID++
	fs.fileHandles[handle] = fh
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	now := time.Now()
	op.Entry.Attributes = inodeAttrs(uint32(op.Mode), 0, "file", now, now)
	setChildEntryExpiry(&op.Entry, time.Second)
	op.Handle = handle
	return nil
}

func (fs *ArtifactFuse) CreateSymlink(ctx context.Context, op *fuseops.CreateSymlinkOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	_, childPath, err := fs.childPath(ctx, op.Parent, op.Name)
	if err != nil {
		return err
	}
	if childPath == ".git" {
		return syscall.EEXIST
	}
	if len(op.Target) > model.MaxSymlinkTargetBytes {
		return syscall.ENAMETOOLONG
	}
	fs.resolver.transition.RLock()
	defer fs.resolver.transition.RUnlock()
	if _, err := fs.resolver.resolvePath(childPath); err == nil {
		return syscall.EEXIST
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return fuseOperationError("check symlink name", err)
	}
	if _, err := fs.engine.Overlay.CreateSymlink(ctx, childPath, op.Target); err != nil {
		return fuseOperationError("symlink", err)
	}
	fs.retireInodePath(childPath)
	fs.mu.Lock()
	ref := fs.allocInode(childPath, "symlink", 0o120000, fs.resolver.Generation())
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	now := time.Now()
	op.Entry.Attributes = inodeAttrs(0o120000, uint64(len(op.Target)), "symlink", now, now)
	setChildEntryExpiry(&op.Entry, time.Second)
	return nil
}

func (fs *ArtifactFuse) MkDir(ctx context.Context, op *fuseops.MkDirOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	_, childPath, err := fs.childPath(ctx, op.Parent, op.Name)
	if err != nil {
		return err
	}
	if childPath == ".git" {
		return syscall.EEXIST
	}
	fs.resolver.transition.RLock()
	defer fs.resolver.transition.RUnlock()
	if _, err := fs.resolver.resolvePath(childPath); err == nil {
		return syscall.EEXIST
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return fuseOperationError("check mkdir name", err)
	}
	if err := fs.engine.Overlay.CreateDirectory(ctx, childPath, uint32(op.Mode)); err != nil {
		return fuseOperationError("mkdir", err)
	}
	fs.retireInodePath(childPath)
	fs.mu.Lock()
	ref := fs.allocInode(childPath, "dir", uint32(op.Mode), fs.resolver.Generation())
	fs.mu.Unlock()

	op.Entry.Child = ref.ID
	now := time.Now()
	op.Entry.Attributes = inodeAttrs(uint32(op.Mode)|uint32(os.ModeDir), 4096, "dir", now, now)
	setChildEntryExpiry(&op.Entry, time.Second)
	return nil
}

func (fs *ArtifactFuse) RmDir(ctx context.Context, op *fuseops.RmDirOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	_, childPath, err := fs.childPath(ctx, op.Parent, op.Name)
	if err != nil {
		return err
	}
	if err := fs.captureMetadataForPaths(ctx, childPath); err != nil {
		return err
	}
	if err := fs.engine.Rmdir(ctx, childPath); err != nil {
		if os.IsExist(err) {
			return syscall.ENOTEMPTY
		}
		return fuseOperationError("rmdir", err)
	}
	fs.retireInodePath(childPath)
	return nil
}

func (fs *ArtifactFuse) Unlink(ctx context.Context, op *fuseops.UnlinkOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	_, childPath, err := fs.childPath(ctx, op.Parent, op.Name)
	if err != nil {
		return err
	}
	if err := fs.captureMetadataForPaths(ctx, childPath); err != nil {
		return err
	}
	if err := fs.engine.ensureOverlay(ctx, childPath); err != nil {
		return fuseOperationError("prepare unlink", err)
	}
	if err := fs.pinOpenHandles(childPath); err != nil {
		return fuseOperationError("pin unlink handles", err)
	}
	if err := fs.engine.Unlink(ctx, childPath); err != nil {
		return fuseOperationError("unlink", err)
	}
	fs.detachOpenHandles(childPath)
	fs.retireInodePath(childPath)
	return nil
}

func (fs *ArtifactFuse) Rename(ctx context.Context, op *fuseops.RenameOp) error {
	fs.handleOps.Lock()
	defer fs.handleOps.Unlock()
	oldParent, err := fs.requireLiveInode(ctx, op.OldParent, syscall.ENOENT)
	if err != nil {
		return err
	}
	newParent, err := fs.requireLiveInode(ctx, op.NewParent, syscall.ENOENT)
	if err != nil {
		return err
	}
	oldPath := cleanChildPath(oldParent.Path, op.OldName)
	newPath := cleanChildPath(newParent.Path, op.NewName)
	source, err := fs.resolver.ResolvePath(oldPath)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return syscall.ENOENT
		}
		return fuseOperationError("resolve rename source", err)
	}
	sourceType := resolvedNodeType(source)
	if oldPath == newPath {
		return nil
	}
	if sourceType == "dir" && strings.HasPrefix(newPath, oldPath+"/") {
		return syscall.EINVAL
	}
	if destination, err := fs.resolver.ResolvePath(newPath); err == nil {
		destinationType := resolvedNodeType(destination)
		if sourceType == "dir" && destinationType != "dir" {
			return syscall.ENOTDIR
		}
		if sourceType != "dir" && destinationType == "dir" {
			return syscall.EISDIR
		}
	}
	if err := fs.captureMetadataForPaths(ctx, oldPath, newPath); err != nil {
		return err
	}
	if sourceType != "dir" {
		if err := fs.engine.ensureOverlay(ctx, oldPath); err != nil {
			return fuseOperationError("prepare rename source", err)
		}
		if err := fs.pinOpenHandles(oldPath); err != nil {
			return fuseOperationError("pin rename source handles", err)
		}
		if _, err := fs.resolver.ResolvePath(newPath); err == nil {
			if err := fs.engine.ensureOverlay(ctx, newPath); err != nil {
				return fuseOperationError("prepare rename destination", err)
			}
			if err := fs.pinOpenHandles(newPath); err != nil {
				return fuseOperationError("pin rename destination handles", err)
			}
		}
	}
	if err := fs.engine.Rename(ctx, oldPath, newPath); err != nil {
		if errors.Is(err, iofs.ErrInvalid) {
			return syscall.EINVAL
		}
		if os.IsExist(err) {
			return syscall.ENOTEMPTY
		}
		if errors.Is(err, iofs.ErrNotExist) {
			return syscall.ENOENT
		}
		return fuseOperationError("rename", err)
	}
	fs.moveInodePath(oldPath, newPath)
	fs.detachOpenHandles(newPath)
	fs.moveOpenHandles(oldPath, newPath)
	return nil
}

func fuseOperationError(operation string, err error) error {
	// The kernel may interrupt an in-flight request, including for a caller's
	// signal. Report the interrupted syscall so clients can retry; EIO would
	// incorrectly describe a failed disk read or corrupted backing state.
	// https://docs.kernel.org/filesystems/fuse/fuse.html#interrupting-filesystem-operations
	if errors.Is(err, context.Canceled) {
		return syscall.EINTR
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return syscall.ETIMEDOUT
	}
	slog.Error("filesystem operation failed", "operation", operation, "error", auth.RedactString(err.Error()))
	return syscall.EIO
}

func (fs *ArtifactFuse) ReadSymlink(ctx context.Context, op *fuseops.ReadSymlinkOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	ref, err := fs.requireInode(op.Inode, syscall.ESTALE)
	if err != nil {
		return err
	}
	n, err := fs.resolver.ResolvePath(ref.Path)
	if err != nil {
		return syscall.ENOENT
	}
	if n.FromOverlay && n.Overlay.Kind == model.OverlayKindSymlink {
		if len(n.Overlay.TargetPath) > model.MaxSymlinkTargetBytes {
			return syscall.ENAMETOOLONG
		}
		op.Target = n.Overlay.TargetPath
		return nil
	}
	if n.Base.ObjectOID != "" {
		if err := validateKnownSymlinkTargetSize(n.Base); err != nil {
			return err
		}
		data, err := fs.engine.Hydrator.ReadBlob(ctx, fs.repo, n.Base, model.MaxSymlinkTargetBytes)
		if err != nil {
			if errors.Is(err, model.ErrBlobTooLarge) {
				return syscall.ENAMETOOLONG
			}
			return fuseOperationError("read symlink", err)
		}
		op.Target = string(data)
		return nil
	}
	return syscall.ENOENT
}

func validateKnownSymlinkTargetSize(node model.BaseNode) error {
	if node.SizeState != "known" {
		return nil
	}
	if node.SizeBytes < 0 {
		return syscall.EIO
	}
	if node.SizeBytes > model.MaxSymlinkTargetBytes {
		return syscall.ENAMETOOLONG
	}
	return nil
}

func (fs *ArtifactFuse) FlushFile(_ context.Context, _ *fuseops.FlushFileOp) error {
	return nil
}

func (fs *ArtifactFuse) SyncFile(ctx context.Context, op *fuseops.SyncFileOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	fh, err := fs.fileHandle(op.Handle)
	if err != nil {
		return err
	}
	fh.mu.Lock()
	if fh.detached {
		f := fh.cacheFile
		fh.mu.Unlock()
		if f == nil || f.Sync() != nil {
			return syscall.EIO
		}
		return nil
	}
	path := fh.path
	file := fh.cacheFile
	retained := file != nil && fh.cacheGeneration == -1
	if fh.cacheFile != nil && fh.cacheGeneration >= 0 {
		fh.mu.Unlock()
		return nil
	}
	fh.mu.Unlock()
	if retained {
		fs.resolver.transition.RLock()
		err := fs.engine.Overlay.SyncFileFrom(ctx, path, file)
		fs.resolver.transition.RUnlock()
		if err != nil {
			return fuseOperationError("sync retained data", err)
		}
		return nil
	}
	if err := fs.engine.Sync(ctx, path); err != nil {
		return fuseOperationError("sync", err)
	}
	return nil
}

func (fs *ArtifactFuse) ReleaseFileHandle(_ context.Context, op *fuseops.ReleaseFileHandleOp) error {
	fs.handleOps.RLock()
	defer fs.handleOps.RUnlock()
	fs.mu.Lock()
	fh := fs.fileHandles[op.Handle]
	delete(fs.fileHandles, op.Handle)
	fs.mu.Unlock()
	if fh != nil {
		fh.release()
	}
	return nil
}

// --- Mount lifecycle ---

type mountedFSWrapper struct {
	*fuse.MountedFileSystem
	mountPoint string
}

func (m *mountedFSWrapper) Unmount() error {
	return TryUnmount(m.mountPoint)
}

func MountRepo(repo model.RepoConfig, resolver *Resolver, engine *Engine) (MountedFS, error) {
	return MountRepoWithGate(repo, resolver, engine, nil)
}

func MountRepoWithGate(repo model.RepoConfig, resolver *Resolver, engine *Engine, gate *ReadyGate) (MountedFS, error) {
	fsint := NewArtifactFuse(repo, resolver, engine)
	server := fuseutil.NewFileSystemServer(NewGatedFileSystem(fsint, gate))

	mountCfg := &fuse.MountConfig{
		FSName:                  "artifact-fs:" + repo.Name,
		Subtype:                 "artifact-fs",
		DisableWritebackCaching: true,
		UseVectoredRead:         true,
	}
	// READDIRPLUS would cache unknown blob sizes as zero before lookup can hydrate them.
	platformMountConfig(mountCfg)

	mfs, err := fuse.Mount(repo.MountPath, server, mountCfg)
	if err != nil {
		return nil, fmt.Errorf("fuse mount %s: %w", repo.MountPath, err)
	}

	return &mountedFSWrapper{MountedFileSystem: mfs, mountPoint: repo.MountPath}, nil
}

func TryUnmount(mountPoint string) error {
	var err error
	for range 20 {
		err = fuse.Unmount(mountPoint)
		if err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return err
}

func inodeAttrs(mode uint32, size uint64, typ string, mtime time.Time, ctime time.Time) fuseops.InodeAttributes {
	m := os.FileMode(mode & 0o777)
	switch typ {
	case "dir":
		m |= os.ModeDir
		if size == 0 {
			size = 4096
		}
	case "symlink":
		// A raw Git symlink mode has no permission bits. An explicit local
		// chmod stores permissions alone, so mode 000 remains distinguishable.
		if mode == 0o120000 {
			m = 0o644
		}
		m |= os.ModeSymlink
	}
	return fuseops.InodeAttributes{
		Size:  size,
		Nlink: 1,
		Mode:  m,
		Uid:   uint32(os.Getuid()),
		Gid:   uint32(os.Getgid()),
		Atime: mtime,
		Mtime: mtime,
		Ctime: ctime,
	}
}

func cleanChildPath(parentPath string, name string) string {
	return model.CleanPath(filepath.Join(parentPath, name))
}

func attrExpiry(ttl time.Duration) time.Time {
	return time.Now().Add(ttl)
}

func setChildEntryExpiry(entry *fuseops.ChildInodeEntry, ttl time.Duration) {
	expiresAt := attrExpiry(ttl)
	entry.AttributesExpiration = expiresAt
	entry.EntryExpiration = expiresAt
}

func (fs *ArtifactFuse) gitFileAttrs() fuseops.InodeAttributes {
	now := time.Now()
	return fuseops.InodeAttributes{
		Size:  uint64(len(fs.gitfileContent)),
		Mode:  0o644,
		Nlink: 1,
		Uid:   uint32(os.Getuid()),
		Gid:   uint32(os.Getgid()),
		Atime: now,
		Mtime: now,
		Ctime: now,
	}
}
