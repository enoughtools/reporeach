//go:build !windows

// Package fsbridge exposes the existing filesystem operations to RepoReach's
// native FSKit extension. File contents use bounded binary HTTP messages; JSON
// carries only filesystem metadata. The server listens exclusively on a private
// Unix socket and requires a fresh capability for each mount session.
package fsbridge

const (
	Version           = 1
	MaxChunkSize      = 1 << 20
	MaxMetadataSize   = 64 << 10
	DirectoryPageSize = 64 << 10
)

type Descriptor struct {
	Version int    `json:"version"`
	Socket  string `json:"socket"`
	Token   string `json:"token"`
}

type Request struct {
	Version    int            `json:"version"`
	Op         string         `json:"op"`
	Inode      uint64         `json:"inode,omitempty"`
	Parent     uint64         `json:"parent,omitempty"`
	Name       string         `json:"name,omitempty"`
	Handle     uint64         `json:"handle,omitempty"`
	Offset     uint64         `json:"offset,omitempty"`
	Size       int64          `json:"size,omitempty"`
	Mode       uint32         `json:"mode,omitempty"`
	Access     uint32         `json:"access,omitempty"` // 1 read, 2 write, 3 both; zero defaults to both
	OldParent  uint64         `json:"old_parent,omitempty"`
	OldName    string         `json:"old_name,omitempty"`
	NewParent  uint64         `json:"new_parent,omitempty"`
	NewName    string         `json:"new_name,omitempty"`
	Target     string         `json:"target,omitempty"`
	N          uint64         `json:"n,omitempty"`
	Attributes *SetAttributes `json:"attributes,omitempty"`
}

// SetAttributes includes only fields represented by the wire protocol. The
// current engine supports size, permission bits and mtime; unsupported fields
// fail before applying any supported changes.
type SetAttributes struct {
	Size    *uint64 `json:"size,omitempty"`
	Mode    *uint32 `json:"mode,omitempty"`
	UID     *uint32 `json:"uid,omitempty"`
	GID     *uint32 `json:"gid,omitempty"`
	AtimeNS *int64  `json:"atime_ns,omitempty"`
	MtimeNS *int64  `json:"mtime_ns,omitempty"`
}

type Attributes struct {
	Size        uint64 `json:"size"`
	Nlink       uint32 `json:"nlink"`
	Mode        uint32 `json:"mode"` // POSIX st_mode, including S_IFMT
	Type        string `json:"type"`
	UID         uint32 `json:"uid"`
	GID         uint32 `json:"gid"`
	AtimeNS     int64  `json:"atime_ns"`
	MtimeNS     int64  `json:"mtime_ns"`
	CtimeNS     int64  `json:"ctime_ns"`
	BirthtimeNS int64  `json:"birthtime_ns"` // zero when unavailable
}

type Node struct {
	Inode      uint64     `json:"inode"`
	Generation uint64     `json:"generation"`
	Attributes Attributes `json:"attributes"`
}

// Each returned directory entry grants one lookup reference, like lookup.
// Clients balance these references using forget independently of releasedir.
type DirectoryEntry struct {
	Name   string `json:"name"`
	Offset uint64 `json:"offset"`
	Node   Node   `json:"node"`
}

type Statistics struct {
	BlockSize       uint32 `json:"block_size"`
	Blocks          uint64 `json:"blocks"`
	BlocksFree      uint64 `json:"blocks_free"`
	BlocksAvailable uint64 `json:"blocks_available"`
	IOSize          uint32 `json:"io_size"`
	Inodes          uint64 `json:"inodes"`
	InodesFree      uint64 `json:"inodes_free"`
}

type Response struct {
	Version    int              `json:"version"`
	Errno      int              `json:"errno"`
	Node       *Node            `json:"node,omitempty"`
	Handle     uint64           `json:"handle,omitempty"`
	Entries    []DirectoryEntry `json:"entries"`
	NextOffset uint64           `json:"next_offset"`
	EOF        bool             `json:"eof"`
	Target     string           `json:"target,omitempty"`
	Written    int              `json:"written"`
	Stat       *Statistics      `json:"stat,omitempty"`
}
