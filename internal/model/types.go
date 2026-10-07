// Modified by Enough Tools for EnoughRepos.
// Based on Cloudflare ArtifactFS (Apache-2.0); see UPSTREAM.md.

package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrBlobTooLarge = errors.New("blob too large")

// MetadataObjectID identifies a filesystem object independently of its current
// path and of a mount's transient inode numbers. Detached objects remain usable
// by retained inode references until an explicitly quiescent collection.
type MetadataObjectID string

type MetadataObject struct {
	ID          MetadataObjectID
	Type        string
	CtimeUnixNs int64
}

type XattrSetPolicy uint8

const (
	XattrAlwaysSet XattrSetPolicy = iota
	XattrMustCreate
	XattrMustReplace
	MaxXattrNameBytes   = 127
	MaxXattrValueBytes  = 1 << 20
	MaxXattrsPerObject  = 128
	MaxXattrObjectBytes = 32 << 20
	MaxXattrStoreBytes  = 256 << 20
)

var (
	ErrMetadataObjectNotFound = errors.New("filesystem metadata object not found")
	ErrXattrNotFound          = errors.New("extended attribute not found")
	ErrXattrExists            = errors.New("extended attribute already exists")
	ErrXattrTooLarge          = errors.New("extended attribute exceeds the supported limit")
	ErrXattrStorageFull       = errors.New("extended attribute storage limit reached")
	ErrInvalidXattr           = errors.New("invalid extended attribute")
)

type RepoID string

// SourceRequirement describes the remote revision ArtifactFS must acquire.
type SourceRequirement struct {
	Ref            string
	RequiredCommit string
	Depth          int
}

// PreparedSource records the exact source selected during acquisition.
type PreparedSource struct {
	Ref      string
	Commit   string
	Verified bool
	Acquired bool // This call installed a new remote acquisition rather than reusing its receipt.
}

type RepoConfig struct {
	ID                RepoID
	Name              string
	MountRoot         string
	MountPath         string
	RemoteURL         string
	RemoteURLRedacted string
	// CredentialHelper is a trusted, secret-free helper command for acquisition.
	// It is not persisted in the registry; acquired clones retain scoped Git config.
	CredentialHelper      string `json:"-"`
	Branch                string
	RefreshInterval       time.Duration
	GitDir                string
	OverlayDir            string
	BlobCacheDir          string
	MetaDBPath            string
	OverlayDBPath         string
	Enabled               bool
	PreparedGitDir        bool
	FetchRef              string
	PrepareState          string
	PrepareError          string
	RequiredCommit        string
	HistoryDepth          int
	RemoteRefreshDisabled bool
	AcquiredRef           string
	AcquiredCommit        string
	AcquiredAt            time.Time
	ConfigVersion         string
}

type RepoRuntimeState struct {
	RepoID                RepoID
	CurrentHEADOID        string
	CurrentHEADRef        string
	SnapshotGeneration    int64
	LastFetchAt           time.Time
	LastFetchResult       string
	AheadCount            int
	BehindCount           int
	Diverged              bool
	HydratedBlobCount     int64
	HydratedBlobBytes     int64
	DirtyOverlay          bool
	State                 string
	PrepareError          string
	SourceRef             string
	RequiredCommit        string
	Acquisition           string
	RemoteRefreshDisabled bool
}

const (
	PrepareStatePreparing     = "preparing"
	PrepareStateSyncPreparing = "sync-preparing"
	PrepareStateReady         = "ready"
	PrepareStateFailed        = "failed"
)

// BaseNode represents a tracked entry from the git tree. Inode IDs are assigned
// at runtime by the FUSE layer (monotonic allocation, like tigrisfs).
type BaseNode struct {
	RepoID    RepoID
	Path      string
	Type      string // file, dir, symlink
	Mode      uint32
	ObjectOID string
	SizeState string // unknown, known
	SizeBytes int64
}

type OverlayKind string

// MaxSymlinkTargetBytes matches the largest target supported by the FUSE
// adapters and common Linux PATH_MAX behavior.
const MaxSymlinkTargetBytes = 4096

const (
	OverlayKindCreate  OverlayKind = "create"
	OverlayKindModify  OverlayKind = "modify"
	OverlayKindDelete  OverlayKind = "delete"
	OverlayKindRename  OverlayKind = "rename"
	OverlayKindMkdir   OverlayKind = "mkdir"
	OverlayKindSymlink OverlayKind = "symlink"
)

type OverlayEntry struct {
	RepoID      RepoID
	Path        string
	Kind        OverlayKind
	BackingPath string
	Mode        uint32
	SizeBytes   int64
	MtimeUnixNs int64
	CtimeUnixNs int64
	SourceOID   string
	SourceMode  uint32
	TargetPath  string
}

func (e OverlayEntry) IsDeleted() bool {
	return e.Kind == OverlayKindDelete
}

func (e OverlayEntry) NodeType() string {
	switch e.Kind {
	case OverlayKindMkdir:
		return "dir"
	case OverlayKindSymlink:
		return "symlink"
	default:
		return "file"
	}
}

type HydrationTask struct {
	RepoID     RepoID
	Path       string
	ObjectOID  string
	SizeState  string
	SizeBytes  int64
	Priority   int
	Reason     string
	EnqueuedAt time.Time
}

// CleanPath normalizes a filesystem path for use as a map key or DB lookup.
// It strips leading slashes, cleans dot-segments, and defaults empty/root to ".".
func CleanPath(path string) string {
	if path == "." || path == "/" || path == "" {
		return "."
	}
	path = filepath.Clean(path)
	if path == "/" {
		return "."
	}
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "."
	}
	return path
}

// ValidateRepoName checks that a repo name is safe for use in filesystem paths.
func ValidateRepoName(name string) error {
	if name == "" {
		return fmt.Errorf("repo name must not be empty")
	}
	if strings.ContainsAny(name, "/\\") || name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("invalid repo name %q: must not contain path separators or '..'", name)
	}
	return nil
}

type Registry interface {
	AddRepo(ctx context.Context, cfg RepoConfig) error
	RemoveRepo(ctx context.Context, name string) error
	GetRepo(ctx context.Context, name string) (RepoConfig, error)
	ListRepos(ctx context.Context) ([]RepoConfig, error)
}

type GitStore interface {
	CloneBlobless(ctx context.Context, cfg RepoConfig) error
	CloneBloblessNonInteractive(ctx context.Context, cfg RepoConfig) error
	PrepareSource(ctx context.Context, cfg RepoConfig, requirement SourceRequirement) (PreparedSource, error)
	Fetch(ctx context.Context, repo RepoConfig) error
	FetchRefNonInteractive(ctx context.Context, repo RepoConfig, ref string) error
	ResolveHEAD(ctx context.Context, repo RepoConfig) (oid string, ref string, err error)
	PinWorkingTreeBaseline(ctx context.Context, repo RepoConfig, oid string) error
	BuildTreeIndex(ctx context.Context, repo RepoConfig, headOID string) ([]BaseNode, error)
	PrefetchBlobs(ctx context.Context, repo RepoConfig, objectOIDs []string) error
	BlobToCache(ctx context.Context, repo RepoConfig, objectOID string, dstPath string) (size int64, err error)
	ReadBlob(ctx context.Context, repo RepoConfig, objectOID string, maxBytes int64) ([]byte, error)
	ComputeAheadBehind(ctx context.Context, repo RepoConfig) (ahead int, behind int, diverged bool, err error)
	CommitTimestamp(ctx context.Context, repo RepoConfig, oid string) (int64, error)
	ReadTreeHEAD(ctx context.Context, repo RepoConfig) error
	PrepareFetchedBranch(ctx context.Context, repo RepoConfig, ref string) error
	ValidatePreparedGitDir(ctx context.Context, repo RepoConfig) error
}

type SnapshotStore interface {
	PublishGeneration(ctx context.Context, headOID string, ref string, nodes []BaseNode) (generation int64, err error)
	GetNode(generation int64, path string) (BaseNode, bool)
	LookupNode(ctx context.Context, generation int64, path string) (BaseNode, bool, error)
	ListChildren(generation int64, parentPath string) ([]BaseNode, error)
}

type OverlayStore interface {
	// BindMetadata requires a live namespace object. It returns its persistent
	// metadata identity, creating one if necessary; a type replacement detaches
	// the previous identity without changing retained references to it.
	BindMetadata(ctx context.Context, path, nodeType string) (MetadataObject, error)
	MetadataObject(ctx context.Context, id MetadataObjectID) (MetadataObject, bool, error)
	GetMetadataXattr(ctx context.Context, id MetadataObjectID, name string) ([]byte, bool, error)
	ListMetadataXattrs(ctx context.Context, id MetadataObjectID) ([]string, error)
	SetMetadataXattr(ctx context.Context, id MetadataObjectID, name string, value []byte, policy XattrSetPolicy) error
	RemoveMetadataXattr(ctx context.Context, id MetadataObjectID, name string) error
	HasMetadataXattrs(ctx context.Context) (bool, error)
	// CollectDetachedMetadata may run only after all mount/inode references to
	// this store have drained. Merely reopening a store does not collect objects.
	CollectDetachedMetadata(ctx context.Context) error
	Get(path string) (OverlayEntry, bool)
	Lookup(ctx context.Context, path string) (OverlayEntry, bool, error)
	EnsureCopyOnWrite(ctx context.Context, repo RepoConfig, path string, base BaseNode) (OverlayEntry, error)
	EnsureCopyOnWriteFrom(ctx context.Context, repo RepoConfig, path string, base BaseNode, src *os.File) (OverlayEntry, error)
	CreateFile(ctx context.Context, path string, mode uint32) (OverlayEntry, error)
	// CreateFileOpened retains the writable creation descriptor opened before
	// final mode bits are applied. The caller owns the returned descriptor.
	CreateFileOpened(ctx context.Context, path string, mode uint32) (OverlayEntry, *os.File, error)
	CreateSymlink(ctx context.Context, path string, target string) (OverlayEntry, error)
	WriteFile(ctx context.Context, path string, off int64, data []byte) (int, error)
	// WriteFileFrom writes through a retained descriptor only while it identifies
	// the current backing file at path.
	WriteFileFrom(ctx context.Context, path string, off int64, data []byte, file *os.File) (int, error)
	SyncFile(ctx context.Context, path string) error
	// SyncFileFrom syncs a retained descriptor after checking namespace identity.
	SyncFileFrom(ctx context.Context, path string, file *os.File) error
	Truncate(ctx context.Context, path string, size int64) error
	// TruncateFrom applies the same namespace identity check as WriteFileFrom.
	TruncateFrom(ctx context.Context, path string, size int64, file *os.File) error
	Remove(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string) error
	// RenameWithSourceWhiteout moves a created file or symlink that shadows a
	// base source and leaves that source deleted in one transaction. destinationBase,
	// when non-nil, supplies the overwritten destination's independent metadata.
	RenameWithSourceWhiteout(ctx context.Context, oldPath, newPath string, destinationBase *BaseNode) error
	RenameTree(ctx context.Context, oldPath, newPath string, sourceBasePaths, destinationBasePaths []string) error
	RenameAndMarkModifiedFromBase(ctx context.Context, oldPath, newPath string, sourceOID string, sourceMode uint32) error
	Mkdir(ctx context.Context, path string, mode uint32) error
	// CreateDirectory creates a new namespace identity. Mkdir also serves base
	// directory promotion and therefore preserves an existing metadata identity.
	CreateDirectory(ctx context.Context, path string, mode uint32) error
	SetMode(ctx context.Context, path string, mode uint32) error
	SetMtime(ctx context.Context, path string, t time.Time) error
	Reconcile(ctx context.Context, baseLookup func(path string) (BaseNode, bool)) error
	ReconcileChecked(ctx context.Context, baseLookup func(path string) (BaseNode, bool, error)) error
	DirtyCount(ctx context.Context) (int64, error)
	ListByPrefix(ctx context.Context, prefix string) ([]OverlayEntry, error)
}

type Hydrator interface {
	Enqueue(task HydrationTask)
	EnqueueBatch(tasks []HydrationTask)
	EnsureHydrated(ctx context.Context, repo RepoConfig, node BaseNode) (cachePath string, size int64, err error)
	OpenHydrated(ctx context.Context, repo RepoConfig, node BaseNode) (*os.File, int64, error)
	ReadBlob(ctx context.Context, repo RepoConfig, node BaseNode, maxBytes int64) ([]byte, error)
	QueueDepth(repoID RepoID) int
}
