//go:build !windows

package fsbridge

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

type openHandle struct {
	mu        sync.RWMutex
	inode     uint64
	directory bool
	closed    bool
}

type Handler struct {
	filesystem    fuseutil.FileSystem
	authorization string
	mu            sync.Mutex
	handles       map[uint64]*openHandle
	lookups       map[uint64]uint64
}

func New(filesystem fuseutil.FileSystem, token []byte) (*Handler, error) {
	if filesystem == nil || len(token) != 32 {
		return nil, errors.New("filesystem bridge requires a filesystem and a 32-byte capability")
	}
	return &Handler{filesystem: filesystem, authorization: "Bearer " + hex.EncodeToString(token), handles: make(map[uint64]*openHandle), lookups: make(map[uint64]uint64)}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(h.authorization)) != 1 {
		writeResponse(w, http.StatusUnauthorized, Response{Version: Version, Errno: int(syscall.EACCES)})
		return
	}
	switch {
	case r.URL.Path == "/v1/fs" && r.Method == http.MethodPost:
		h.metadata(w, r)
	case r.URL.Path == "/v1/fs/read" && r.Method == http.MethodGet:
		h.read(w, r)
	case r.URL.Path == "/v1/fs/write" && r.Method == http.MethodPut:
		h.write(w, r)
	case r.URL.Path == "/v1/fs/xattr" && r.Method == http.MethodGet:
		h.getXattr(w, r)
	case r.URL.Path == "/v1/fs/xattr" && r.Method == http.MethodPut:
		h.setXattr(w, r)
	default:
		writeResponse(w, http.StatusNotFound, Response{Version: Version, Errno: int(syscall.ENOSYS)})
	}
}

func writeResponse(w http.ResponseWriter, status int, response Response) {
	if response.Entries == nil {
		response.Entries = make([]DirectoryEntry, 0)
	}
	if response.XattrNames == nil {
		response.XattrNames = make([]string, 0)
	}
	data, err := json.Marshal(response)
	if err != nil {
		data = []byte(`{"version":1,"errno":5}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-RepoReach-Errno", strconv.Itoa(response.Errno))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func errno(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) {
		return int(syscall.EINTR)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return int(syscall.ETIMEDOUT)
	}
	var code syscall.Errno
	if errors.As(err, &code) {
		return int(code)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return int(syscall.ENOENT)
	case errors.Is(err, fs.ErrExist):
		return int(syscall.EEXIST)
	case errors.Is(err, fs.ErrPermission):
		return int(syscall.EACCES)
	case errors.Is(err, fs.ErrInvalid):
		return int(syscall.EINVAL)
	default:
		return int(syscall.EIO)
	}
}

func (h *Handler) metadata(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxMetadataSize)
	data, err := io.ReadAll(r.Body)
	if err != nil || !validMetadataEncoding(data) {
		writeResponse(w, http.StatusBadRequest, Response{Version: Version, Errno: int(syscall.EINVAL)})
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		writeResponse(w, http.StatusBadRequest, Response{Version: Version, Errno: int(syscall.EINVAL)})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || request.Version != Version {
		writeResponse(w, http.StatusBadRequest, Response{Version: Version, Errno: int(syscall.EINVAL)})
		return
	}
	response, err := h.dispatch(r.Context(), request)
	if err != nil {
		if request.Op == "listxattr" || request.Op == "removexattr" {
			response = xattrErrorResponse(err)
		} else {
			response = Response{Version: Version, Errno: errno(err)}
		}
	}
	writeResponse(w, http.StatusOK, response)
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && utf8.ValidString(name) && !strings.ContainsAny(name, "/\x00") && model.CleanPath(name) == name
}

func (h *Handler) remember(node Node) {
	h.mu.Lock()
	h.lookups[node.Inode]++
	h.mu.Unlock()
}

func (h *Handler) register(handle, inode uint64, directory bool) {
	h.mu.Lock()
	h.handles[handle] = &openHandle{inode: inode, directory: directory}
	h.mu.Unlock()
}

// lockHandle retains a handle until this operation completes. Release waits for
// all operations already using that handle, rather than racing their backend.
func (h *Handler) lockHandle(handle, inode uint64, directory bool) (*openHandle, error) {
	h.mu.Lock()
	state := h.handles[handle]
	h.mu.Unlock()
	if state == nil {
		return nil, syscall.EBADF
	}
	state.mu.RLock()
	if state.closed || state.inode != inode || state.directory != directory {
		state.mu.RUnlock()
		return nil, syscall.EBADF
	}
	return state, nil
}

func (h *Handler) release(ctx context.Context, handle uint64, directory bool) error {
	h.mu.Lock()
	state := h.handles[handle]
	h.mu.Unlock()
	if state == nil {
		return syscall.EBADF
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed || state.directory != directory {
		return syscall.EBADF
	}
	var err error
	if directory {
		err = h.filesystem.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: fuseops.HandleID(handle)})
	} else {
		err = h.filesystem.ReleaseFileHandle(ctx, &fuseops.ReleaseFileHandleOp{Handle: fuseops.HandleID(handle)})
	}
	if err != nil {
		return err
	}
	state.closed = true
	h.mu.Lock()
	delete(h.handles, handle)
	h.mu.Unlock()
	return nil
}

func (h *Handler) dispatch(ctx context.Context, r Request) (Response, error) {
	response := Response{Version: Version}
	inode, parent, handle := fuseops.InodeID(r.Inode), fuseops.InodeID(r.Parent), fuseops.HandleID(r.Handle)
	childOp := r.Op == "lookup" || r.Op == "create" || r.Op == "mkdir" || r.Op == "symlink" || r.Op == "unlink" || r.Op == "rmdir"
	if childOp && (r.Parent == 0 || !validName(r.Name)) {
		return response, syscall.EINVAL
	}
	if r.Op == "rename" && (r.OldParent == 0 || r.NewParent == 0 || !validName(r.OldName) || !validName(r.NewName)) {
		return response, syscall.EINVAL
	}
	if r.Op == "create" || r.Op == "mkdir" {
		if r.Mode & ^uint32(0o777) != 0 {
			return response, syscall.ENOTSUP
		}
	}
	if (r.Op == "create" || r.Op == "open") && r.Access > 3 {
		return response, syscall.EINVAL
	}
	if err := ctx.Err(); err != nil {
		return response, err
	}
	switch r.Op {
	case "lookup":
		op := &fuseops.LookUpInodeOp{Parent: parent, Name: r.Name}
		known := true
		var err error
		if fs, ok := h.filesystem.(*catalogfs.FileSystem); ok {
			known, err = fs.LookUpMetadata(ctx, op)
		} else if fs, ok := h.filesystem.(*fusefs.ArtifactFuse); ok {
			known, err = fs.LookUpMetadata(ctx, op)
		} else {
			err = h.filesystem.LookUpInode(ctx, op)
		}
		if err != nil {
			return response, err
		}
		node := nodeFromEntry(op.Entry)
		if !known {
			node.Attributes.SizeKnown = &known
		}
		h.remember(node)
		response.Node = &node
	case "getattr":
		if inode == 0 {
			return response, syscall.EINVAL
		}
		op := &fuseops.GetInodeAttributesOp{Inode: inode}
		known := true
		if r.Handle != 0 {
			state, err := h.lockHandle(r.Handle, r.Inode, false)
			if err != nil {
				return response, err
			}
			defer state.mu.RUnlock()
			switch fs := h.filesystem.(type) {
			case *catalogfs.FileSystem:
				if r.RequireSize {
					op.Attributes, err = fs.GetFileHandleAttributes(ctx, inode, handle)
				} else {
					op.Attributes, known, err = fs.GetFileHandleMetadataAttributes(ctx, inode, handle)
				}
			case *fusefs.ArtifactFuse:
				if r.RequireSize {
					op.Attributes, err = fs.GetFileHandleAttributes(ctx, inode, handle)
				} else {
					op.Attributes, known, err = fs.GetFileHandleMetadataAttributes(ctx, inode, handle)
				}
			default:
				err = syscall.ENOSYS
			}
			if err != nil {
				return response, err
			}
		} else {
			var err error
			if fs, ok := h.filesystem.(*catalogfs.FileSystem); ok && !r.RequireSize {
				known, err = fs.GetMetadataAttributes(ctx, op)
			} else if fs, ok := h.filesystem.(*fusefs.ArtifactFuse); ok && !r.RequireSize {
				known, err = fs.GetMetadataAttributes(ctx, op)
			} else {
				err = h.filesystem.GetInodeAttributes(ctx, op)
			}
			if err != nil {
				return response, err
			}
		}
		response.Node = &Node{Inode: r.Inode, Attributes: attributes(op.Attributes)}
		if !known {
			response.Node.Attributes.SizeKnown = &known
		}
	case "setattr":
		if inode == 0 || r.Attributes == nil {
			return response, syscall.EINVAL
		}
		a := r.Attributes
		if a.UID != nil || a.GID != nil || a.AtimeNS != nil || (a.Mode != nil && *a.Mode & ^uint32(0o777) != 0) {
			return response, syscall.ENOTSUP
		}
		if a.Size != nil && *a.Size > math.MaxInt64 {
			return response, syscall.EFBIG
		}
		op := &fuseops.SetInodeAttributesOp{Inode: inode, Size: a.Size}
		if r.Handle != 0 {
			state, err := h.lockHandle(r.Handle, r.Inode, false)
			if err != nil {
				return response, err
			}
			defer state.mu.RUnlock()
			op.Handle = &handle
		}
		if a.Mode != nil {
			mode := os.FileMode(*a.Mode)
			op.Mode = &mode
		}
		if a.MtimeNS != nil {
			mtime := time.Unix(0, *a.MtimeNS)
			op.Mtime = &mtime
		}
		var err error
		if r.Handle != 0 {
			switch fs := h.filesystem.(type) {
			case *catalogfs.FileSystem:
				err = fs.SetFileHandleAttributes(ctx, op)
			case *fusefs.ArtifactFuse:
				err = fs.SetFileHandleAttributes(ctx, op)
			default:
				err = syscall.ENOSYS
			}
		} else {
			err = h.filesystem.SetInodeAttributes(ctx, op)
		}
		if err != nil {
			return response, err
		}
		response.Node = &Node{Inode: r.Inode, Attributes: attributes(op.Attributes)}
	case "opendir":
		if inode == 0 {
			return response, syscall.EINVAL
		}
		op := &fuseops.OpenDirOp{Inode: inode}
		if err := h.filesystem.OpenDir(ctx, op); err != nil {
			return response, err
		}
		response.Handle = uint64(op.Handle)
		h.register(response.Handle, r.Inode, true)
	case "readdir":
		return h.readdir(ctx, r)
	case "releasedir":
		return response, h.release(ctx, r.Handle, true)
	case "create":
		op := &fuseops.CreateFileOp{Parent: parent, Name: r.Name, Mode: os.FileMode(r.Mode)}
		switch r.Access {
		case 1:
			op.OpenFlags = syscall.O_RDONLY
		case 2:
			op.OpenFlags = syscall.O_WRONLY
		default:
			op.OpenFlags = syscall.O_RDWR
		}
		if err := h.filesystem.CreateFile(ctx, op); err != nil {
			return response, err
		}
		node := nodeFromEntry(op.Entry)
		h.remember(node)
		response.Node = &node
		response.Handle = uint64(op.Handle)
		h.register(response.Handle, node.Inode, false)
	case "mkdir":
		op := &fuseops.MkDirOp{Parent: parent, Name: r.Name, Mode: os.FileMode(r.Mode)}
		if err := h.filesystem.MkDir(ctx, op); err != nil {
			return response, err
		}
		node := nodeFromEntry(op.Entry)
		h.remember(node)
		response.Node = &node
	case "symlink":
		if !utf8.ValidString(r.Target) || strings.ContainsRune(r.Target, 0) {
			return response, syscall.EINVAL
		}
		op := &fuseops.CreateSymlinkOp{Parent: parent, Name: r.Name, Target: r.Target}
		if err := h.filesystem.CreateSymlink(ctx, op); err != nil {
			return response, err
		}
		node := nodeFromEntry(op.Entry)
		h.remember(node)
		response.Node = &node
	case "rename":
		return response, h.filesystem.Rename(ctx, &fuseops.RenameOp{OldParent: fuseops.InodeID(r.OldParent), OldName: r.OldName, NewParent: fuseops.InodeID(r.NewParent), NewName: r.NewName})
	case "unlink":
		return response, h.filesystem.Unlink(ctx, &fuseops.UnlinkOp{Parent: parent, Name: r.Name})
	case "rmdir":
		return response, h.filesystem.RmDir(ctx, &fuseops.RmDirOp{Parent: parent, Name: r.Name})
	case "readlink":
		if inode == 0 {
			return response, syscall.EINVAL
		}
		op := &fuseops.ReadSymlinkOp{Inode: inode}
		if err := h.filesystem.ReadSymlink(ctx, op); err != nil {
			return response, err
		}
		if !utf8.ValidString(op.Target) || strings.ContainsRune(op.Target, 0) {
			return response, syscall.EILSEQ
		}
		response.Target = op.Target
	case "listxattr":
		if inode == 0 {
			return response, syscall.EINVAL
		}
		return h.listXattr(ctx, inode)
	case "removexattr":
		if inode == 0 || !validXattrName(r.Name) {
			return response, syscall.EINVAL
		}
		return response, h.filesystem.RemoveXattr(ctx, &fuseops.RemoveXattrOp{Inode: inode, Name: r.Name})
	case "open":
		if inode == 0 {
			return response, syscall.EINVAL
		}
		op := &fuseops.OpenFileOp{Inode: inode}
		switch r.Access {
		case 1:
			op.OpenFlags = syscall.O_RDONLY
		case 2:
			op.OpenFlags = syscall.O_WRONLY
		default:
			op.OpenFlags = syscall.O_RDWR
		}
		if err := h.filesystem.OpenFile(ctx, op); err != nil {
			return response, err
		}
		response.Handle = uint64(op.Handle)
		h.register(response.Handle, r.Inode, false)
	case "release":
		return response, h.release(ctx, r.Handle, false)
	case "fsync", "flush":
		state, err := h.lockHandle(r.Handle, r.Inode, false)
		if err != nil {
			return response, err
		}
		defer state.mu.RUnlock()
		if r.Op == "fsync" {
			return response, h.filesystem.SyncFile(ctx, &fuseops.SyncFileOp{Inode: inode, Handle: handle})
		}
		return response, h.filesystem.FlushFile(ctx, &fuseops.FlushFileOp{Inode: inode, Handle: handle})
	case "forget":
		if inode == 0 || r.N == 0 {
			return response, syscall.EINVAL
		}
		h.mu.Lock()
		count := h.lookups[r.Inode]
		if r.N > count {
			h.mu.Unlock()
			return response, syscall.EINVAL
		}
		h.lookups[r.Inode] -= r.N
		if h.lookups[r.Inode] == 0 {
			delete(h.lookups, r.Inode)
		}
		h.mu.Unlock()
		return response, h.filesystem.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: inode, N: r.N})
	case "batchforget":
		return response, h.batchForget(ctx, r.Forgets)
	case "statfs":
		op := &fuseops.StatFSOp{}
		if err := h.filesystem.StatFS(ctx, op); err != nil {
			return response, err
		}
		response.Stat = &Statistics{op.BlockSize, op.Blocks, op.BlocksFree, op.BlocksAvailable, op.IoSize, op.Inodes, op.InodesFree}
	default:
		return response, syscall.ENOSYS
	}
	return response, nil
}

func nodeFromEntry(entry fuseops.ChildInodeEntry) Node {
	return Node{Inode: uint64(entry.Child), Generation: uint64(entry.Generation), Attributes: attributes(entry.Attributes)}
}

func attributes(a fuseops.InodeAttributes) Attributes {
	mode, typ := uint32(a.Mode.Perm()), "file"
	switch {
	case a.Mode.IsDir():
		mode |= syscall.S_IFDIR
		typ = "dir"
	case a.Mode&os.ModeSymlink != 0:
		mode |= syscall.S_IFLNK
		typ = "symlink"
	default:
		mode |= syscall.S_IFREG
	}
	if a.Mode&os.ModeSetuid != 0 {
		mode |= syscall.S_ISUID
	}
	if a.Mode&os.ModeSetgid != 0 {
		mode |= syscall.S_ISGID
	}
	if a.Mode&os.ModeSticky != 0 {
		mode |= syscall.S_ISVTX
	}
	return Attributes{Size: a.Size, Nlink: a.Nlink, Mode: mode, Type: typ, UID: a.Uid, GID: a.Gid,
		AtimeNS: timestamp(a.Atime), MtimeNS: timestamp(a.Mtime), CtimeNS: timestamp(a.Ctime), BirthtimeNS: timestamp(a.Crtime)}
}

func timestamp(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func (h *Handler) readdir(ctx context.Context, r Request) (Response, error) {
	response := Response{Version: Version, NextOffset: r.Offset, Entries: make([]DirectoryEntry, 0)}
	if r.Offset > math.MaxInt64 {
		return response, syscall.EINVAL
	}
	state, err := h.lockHandle(r.Handle, r.Inode, true)
	if err != nil {
		return response, err
	}
	defer state.mu.RUnlock()
	// The concrete adapters expose captured directory metadata and inode
	// lifetime rules without ordinary lookup's unknown-size blob hydration.
	var entries []fusefs.DirectoryEntry
	switch fs := h.filesystem.(type) {
	case *catalogfs.FileSystem:
		entries, err = fs.ReadDirectoryEntries(ctx, fuseops.HandleID(r.Handle), fuseops.DirOffset(r.Offset), DirectoryPageSize)
	case *fusefs.ArtifactFuse:
		entries, err = fs.ReadDirectoryEntries(ctx, fuseops.HandleID(r.Handle), fuseops.DirOffset(r.Offset), DirectoryPageSize)
	default:
		return h.readdirByLookup(ctx, r)
	}
	if err != nil {
		return response, err
	}
	failed := true
	defer func() {
		if failed {
			for _, entry := range entries {
				_ = h.filesystem.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: entry.Entry.Child, N: 1})
			}
		}
	}()
	response.EOF = len(entries) == 0
	for _, entry := range entries {
		if !validName(entry.Name) || entry.Entry.Child == 0 || uint64(entry.Offset) <= response.NextOffset || uint64(entry.Offset) > math.MaxInt64 {
			return response, syscall.EILSEQ
		}
		node := nodeFromEntry(entry.Entry)
		if !entry.SizeKnown {
			known := false
			node.Attributes.SizeKnown = &known
		}
		response.Entries = append(response.Entries, DirectoryEntry{Name: entry.Name, Offset: uint64(entry.Offset), Node: node})
		response.NextOffset = uint64(entry.Offset)
	}
	for _, entry := range response.Entries {
		h.remember(entry.Node)
	}
	failed = false
	return response, nil
}

// Keep the general fuseutil.FileSystem protocol adapter for other filesystems;
// RepoReach's concrete catalogue and repository adapters use typed enumeration.
func (h *Handler) readdirByLookup(ctx context.Context, r Request) (Response, error) {
	response := Response{Version: Version, NextOffset: r.Offset, Entries: make([]DirectoryEntry, 0)}
	op := &fuseops.ReadDirOp{Inode: fuseops.InodeID(r.Inode), Handle: fuseops.HandleID(r.Handle), Offset: fuseops.DirOffset(r.Offset), Dst: make([]byte, DirectoryPageSize)}
	if err := h.filesystem.ReadDir(ctx, op); err != nil {
		return response, err
	}
	if op.BytesRead < 0 || op.BytesRead > len(op.Dst) {
		return response, syscall.EIO
	}
	data := op.Dst[:op.BytesRead]
	// The existing adapter emits fuse_dirent in native byte order on both Darwin
	// and Linux. Decode only the public fixed header and aligned filename.
	var acquired []Node
	failed := true
	defer func() {
		if failed {
			for _, node := range acquired {
				_ = h.filesystem.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: fuseops.InodeID(node.Inode), N: 1})
			}
		}
	}()
	for len(data) != 0 {
		if len(data) < 24 {
			return response, syscall.EIO
		}
		offset := binary.NativeEndian.Uint64(data[8:16])
		length := uint64(binary.NativeEndian.Uint32(data[16:20]))
		width := uint64(24) + ((length + 7) & ^uint64(7))
		if length == 0 || width > uint64(len(data)) || offset <= response.NextOffset || offset > math.MaxInt64 {
			return response, syscall.EIO
		}
		name := string(data[24 : 24+length])
		if !validName(name) {
			return response, syscall.EILSEQ
		}
		response.NextOffset = offset
		lookup := &fuseops.LookUpInodeOp{Parent: fuseops.InodeID(r.Inode), Name: name}
		if err := h.filesystem.LookUpInode(ctx, lookup); err != nil {
			if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, fs.ErrNotExist) {
				return response, err
			}
		} else {
			node := nodeFromEntry(lookup.Entry)
			acquired = append(acquired, node)
			response.Entries = append(response.Entries, DirectoryEntry{Name: name, Offset: offset, Node: node})
		}
		data = data[width:]
	}
	response.EOF = op.BytesRead == 0
	for _, node := range acquired {
		h.remember(node)
	}
	failed = false
	return response, nil
}

func parseFileRequest(r *http.Request, read bool) (inode, handle uint64, offset int64, size int64, err error) {
	q := r.URL.Query()
	allowed := map[string]bool{"inode": true, "handle": true, "offset": true}
	if read {
		allowed["size"] = true
	}
	for key, values := range q {
		if !allowed[key] || len(values) != 1 {
			return 0, 0, 0, 0, syscall.EINVAL
		}
	}
	inode, err = strconv.ParseUint(q.Get("inode"), 10, 64)
	if err != nil || inode == 0 {
		return 0, 0, 0, 0, syscall.EINVAL
	}
	handle, err = strconv.ParseUint(q.Get("handle"), 10, 64)
	if err != nil || handle == 0 {
		return 0, 0, 0, 0, syscall.EINVAL
	}
	offset, err = strconv.ParseInt(q.Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		return 0, 0, 0, 0, syscall.EINVAL
	}
	if read {
		size, err = strconv.ParseInt(q.Get("size"), 10, 64)
		if err != nil || size < 0 || size > MaxChunkSize {
			return 0, 0, 0, 0, syscall.EINVAL
		}
	}
	if offset > math.MaxInt64-size {
		return 0, 0, 0, 0, syscall.EINVAL
	}
	return
}

func binaryError(w http.ResponseWriter, err error) {
	writeResponse(w, http.StatusConflict, Response{Version: Version, Errno: errno(err)})
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	inode, handle, offset, size, err := parseFileRequest(r, true)
	if err != nil {
		binaryError(w, err)
		return
	}
	state, err := h.lockHandle(handle, inode, false)
	if err != nil {
		binaryError(w, err)
		return
	}
	defer state.mu.RUnlock()
	op := &fuseops.ReadFileOp{Inode: fuseops.InodeID(inode), Handle: fuseops.HandleID(handle), Offset: offset, Size: size, Dst: make([]byte, int(size))}
	if err := h.filesystem.ReadFile(r.Context(), op); err != nil {
		if op.Callback != nil {
			op.Callback()
		}
		binaryError(w, err)
		return
	}
	if op.Callback != nil {
		defer op.Callback()
	}
	if op.BytesRead < 0 || int64(op.BytesRead) > size {
		binaryError(w, syscall.EIO)
		return
	}
	if op.Data != nil {
		var total int64
		for _, data := range op.Data {
			total += int64(len(data))
			if total > size {
				binaryError(w, syscall.EIO)
				return
			}
		}
		if total != int64(op.BytesRead) {
			binaryError(w, syscall.EIO)
			return
		}
	} else if op.BytesRead > len(op.Dst) {
		binaryError(w, syscall.EIO)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(op.BytesRead))
	w.Header().Set("X-RepoReach-Errno", "0")
	w.WriteHeader(http.StatusOK)
	if op.Data != nil {
		for _, data := range op.Data {
			if _, err := w.Write(data); err != nil {
				return
			}
		}
	} else {
		_, _ = w.Write(op.Dst[:op.BytesRead])
	}
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request) {
	inode, handle, offset, _, err := parseFileRequest(r, false)
	if err != nil {
		binaryError(w, err)
		return
	}
	if r.ContentLength > MaxChunkSize {
		binaryError(w, syscall.EFBIG)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxChunkSize))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			binaryError(w, syscall.EFBIG)
		} else if r.Context().Err() != nil {
			binaryError(w, r.Context().Err())
		} else {
			binaryError(w, syscall.EINVAL)
		}
		return
	}
	if offset > math.MaxInt64-int64(len(data)) {
		binaryError(w, syscall.EINVAL)
		return
	}
	state, err := h.lockHandle(handle, inode, false)
	if err != nil {
		binaryError(w, err)
		return
	}
	defer state.mu.RUnlock()
	op := &fuseops.WriteFileOp{Inode: fuseops.InodeID(inode), Handle: fuseops.HandleID(handle), Offset: offset, Data: data}
	if err := h.filesystem.WriteFile(r.Context(), op); err != nil {
		if op.Callback != nil {
			op.Callback()
		}
		binaryError(w, err)
		return
	}
	if op.Callback != nil {
		defer op.Callback()
	}
	writeResponse(w, http.StatusOK, Response{Version: Version, Written: len(data)})
}

// closeResources runs only after the HTTP server has drained its operations.
func (h *Handler) closeResources(ctx context.Context) error {
	h.mu.Lock()
	handles := make(map[uint64]bool, len(h.handles))
	for id, state := range h.handles {
		handles[id] = state.directory
	}
	h.mu.Unlock()
	var result error
	for id, directory := range handles {
		result = errors.Join(result, h.release(ctx, id, directory))
	}
	h.mu.Lock()
	lookups := make(map[uint64]uint64, len(h.lookups))
	for inode, count := range h.lookups {
		lookups[inode] = count
	}
	h.mu.Unlock()
	for inode, count := range lookups {
		if err := h.filesystem.ForgetInode(ctx, &fuseops.ForgetInodeOp{Inode: fuseops.InodeID(inode), N: count}); err != nil {
			result = errors.Join(result, err)
		} else {
			h.mu.Lock()
			delete(h.lookups, inode)
			h.mu.Unlock()
		}
	}
	return result
}
