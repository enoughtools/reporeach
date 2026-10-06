package desktop

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/model"
)

const previewEvictionJournalName = "preview-content-eviction.json"

type previewContentOwnership struct {
	Version      int                  `json:"version"`
	RepositoryID string               `json:"repositoryID"`
	Root         string               `json:"root"`
	Identity     localHandoffIdentity `json:"identity"`
}

func canonicalPreviewGitRoot(stateDir, id string) string {
	return filepath.Join(stateDir, "engine", "repos", engineName(id), "preview-git")
}

func previewContentOwnershipPath(stateDir, id string) string {
	return filepath.Join(stateDir, "preview-content-owners", engineName(id)+".json")
}

// Establish ownership before the first acquisition. A missing former cache can
// be recreated; an existing foreign/nonempty cache is never silently adopted.
func ensurePreviewContentOwnership(stateDir, id, gitRoot string) error {
	if gitRoot != canonicalPreviewGitRoot(stateDir, id) {
		return errors.New("preview Git storage must occupy its canonical private cache folder")
	}
	owner, name, ok := strings.Cut(id, "/")
	if !ok || validateComponent(owner) != nil || validateComponent(name) != nil {
		return errors.New("invalid preview repository identity")
	}
	if err := handoffPrivateDirectory(stateDir, false); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(stateDir, "engine"), filepath.Join(stateDir, "engine", "repos"), filepath.Dir(gitRoot)} {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(path, 0700); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := verifyHandoffDirectory(path, localHandoffIdentity{}, false); err != nil {
			return err
		}
	}
	ownerPath := previewContentOwnershipPath(stateDir, id)
	if err := handoffPrivateDirectory(filepath.Dir(ownerPath), true); err != nil {
		return err
	}
	var saved previewContentOwnership
	readErr := handoffReadJSON(ownerPath, &saved)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if readErr == nil && (saved.Version != 1 || saved.RepositoryID != id || saved.Root != gitRoot || !saved.Identity.valid()) {
		return errors.New("preview cache ownership record is invalid")
	}
	info, err := os.Lstat(gitRoot)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("preview cache folder was replaced")
		}
		if readErr == nil {
			return verifyHandoffDirectory(gitRoot, saved.Identity, true)
		}
		entries, err := os.ReadDir(gitRoot)
		if err != nil || len(entries) != 0 {
			return errors.New("existing preview cache has no ownership record; its files were retained")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(gitRoot, 0700); err != nil {
			return err
		}
	} else {
		return err
	}
	identity, err := handoffReadIdentity(gitRoot)
	if err != nil {
		return err
	}
	if err := verifyHandoffDirectory(gitRoot, identity, true); err != nil {
		return err
	}
	record := previewContentOwnership{Version: 1, RepositoryID: id, Root: gitRoot, Identity: identity}
	if err := handoffSyncDirectory(gitRoot); err != nil {
		return err
	}
	if err := handoffSyncDirectory(filepath.Dir(gitRoot)); err != nil {
		return err
	}
	return handoffWriteJSON(ownerPath, record, false)
}

// Content bytes share the canonical immutable ArtifactFS blob cache, so a
// successful promotion neither duplicates nor loses a downloaded preview blob.
func cachedPreviewBlobBytes(stateDir, id string) (int64, error) {
	_, total, err := cachedPreviewBlobInventory(stateDir, id)
	return total, err
}

func cachedPreviewBlobInventory(stateDir, id string) (map[string]int64, int64, error) {
	root := filepath.Join(stateDir, "engine", "cache", "blobs", engineName(id))
	sizes := make(map[string]int64)
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return sizes, 0, nil
	} else if err != nil {
		return nil, 0, err
	}
	if err := verifyHandoffDirectory(root, localHandoffIdentity{}, false); err != nil {
		return nil, 0, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return sizes, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var total int64
	for _, entry := range entries {
		if !validPreviewOID(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, 0, err
		}
		if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
			return nil, 0, errors.New("preview blob cache contains an unsafe file")
		}
		sizes[entry.Name()] = info.Size()
		total += info.Size()
	}
	return sizes, total, nil
}

type previewMetadataAccounting struct {
	Identity localHandoffIdentity
	Bytes    int64
}

func (s *Service) notePreviewCachedBytes(id string) {
	sizes, bytes, err := cachedPreviewBlobInventory(s.opts.StateDir, id)
	if err != nil {
		return
	}
	s.mu.Lock()
	repo, exists := s.repositoryLocked(id)
	s.mu.Unlock()
	if !exists || repo.LocalPath != "" {
		return
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	metadata, err := retainedPreviewMetadataBlobBytes(ctx, s.opts.StateDir, repo)
	if err != nil {
		return
	}
	identity, _ := handoffReadIdentity(filepath.Join(s.opts.StateDir, "previews", previewKey(repo), "git"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if index := s.repositoryIndexLocked(id); index >= 0 && s.state.Repositories[index].LocalPath == "" {
		if s.previewBlobSizes == nil {
			s.previewBlobSizes = make(map[string]map[string]int64)
			s.previewMetaCounts = make(map[string]previewMetadataAccounting)
		}
		// Newly discovered repositories may finish their first independent
		// content reads together. Merge inventories so a slower initializer
		// cannot overwrite an OID already counted by the other callback.
		for oid, size := range s.previewBlobSizes[id] {
			if _, exists := sizes[oid]; !exists {
				sizes[oid] = size
				bytes += size
			}
		}
		s.previewBlobSizes[id] = sizes
		s.previewMetaCounts[id] = previewMetadataAccounting{Identity: identity, Bytes: metadata}
		s.state.Repositories[index].DownloadedBytes = bytes + metadata
	}
}

// The successful content reader already verified this blob. Count each OID
// once, directly from its descriptor, without scanning the cache on reads or
// Status polls. Startup initializes the inventory once per repository.
func (s *Service) notePreviewCachedFile(id string, file *os.File) {
	root := filepath.Join(s.opts.StateDir, "engine", "cache", "blobs", engineName(id))
	oid := filepath.Base(file.Name())
	if file.Name() != filepath.Join(root, oid) || !validPreviewOID(oid) {
		return
	}
	info, err := file.Stat()
	if err != nil {
		return
	}
	s.mu.Lock()
	repo, exists := s.repositoryLocked(id)
	_, initialized := s.previewBlobSizes[id]
	metadata := s.previewMetaCounts[id]
	s.mu.Unlock()
	if !exists || repo.LocalPath != "" {
		return
	}
	if !initialized {
		s.notePreviewCachedBytes(id)
		return
	}
	identity, _ := handoffReadIdentity(filepath.Join(s.opts.StateDir, "previews", previewKey(repo), "git"))
	if identity != metadata.Identity {
		ctx := s.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		bytes, err := retainedPreviewMetadataBlobBytes(ctx, s.opts.StateDir, repo)
		if err != nil {
			return
		}
		metadata = previewMetadataAccounting{Identity: identity, Bytes: bytes}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if index := s.repositoryIndexLocked(id); index >= 0 && s.state.Repositories[index].LocalPath == "" {
		sizes := s.previewBlobSizes[id]
		if sizes == nil {
			return
		}
		prior := sizes[oid]
		sizes[oid] = info.Size()
		s.state.Repositories[index].DownloadedBytes += info.Size() - prior + metadata.Bytes - s.previewMetaCounts[id].Bytes
		s.previewMetaCounts[id] = metadata
	}
}

// Caller has normally detached the catalogue before pausing: waiting for a
// callback from within a mount drain would deadlock. Pooled Git readers are
// closed only after every owned producer has joined.
func (s *Service) pausePreviewContent(ctx context.Context, id string) (func(), error) {
	if s.preview == nil {
		return func() {}, nil
	}
	release, directories, err := s.preview.PauseContent(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.previewGit != nil {
		for _, directory := range directories {
			s.previewGit.CloseRepository(directory)
		}
	}
	return release, nil
}

type previewEvictionJournal struct {
	Version      int                     `json:"version"`
	RepositoryID string                  `json:"repositoryID"`
	Root         string                  `json:"root"`
	Identity     localHandoffIdentity    `json:"identity"`
	Objects      []previewEvictionObject `json:"objects"`
}

type previewEvictionObject struct {
	Original string                    `json:"original"`
	Cleanup  localHandoffCleanupObject `json:"cleanup"`
}

// evictPreviewContent runs under detached lifecycle ownership with content
// acquisition paused. Its cache-only journal is committed before any rename:
// these immutable copies have no user-visible Git/index/overlay write path.
// Metadata snapshots and their offline selected commit remain untouched.
func (s *Service) evictPreviewContent(ctx context.Context, id string, includeBlobs bool) error {
	if _, err := os.Lstat(filepath.Join(s.opts.StateDir, previewEvictionJournalName)); err == nil {
		return errors.New("an interrupted cache cleanup needs recovery before cached data can be reclaimed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.verifyPreviewMetadataReclaimable(ctx, id); err != nil {
		return err
	}
	if err := s.verifyPreviewContentOwnership(ctx, id, includeBlobs); err != nil {
		return err
	}
	gitRoot := canonicalPreviewGitRoot(s.opts.StateDir, id)
	paths := []string{gitRoot}
	if includeBlobs {
		paths = append(paths, filepath.Join(s.opts.StateDir, "engine", "cache", "blobs", engineName(id)))
	}
	var objects []previewEvictionObject
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		object, err := capturePreviewCleanupObject(ctx, path)
		if err != nil {
			return err
		}
		if err := handoffDirectoryNotInUse(ctx, path); err != nil {
			return err
		}
		objects = append(objects, previewEvictionObject{Original: path, Cleanup: object})
	}
	if len(objects) == 0 {
		return nil
	}
	// Capturing a current inventory must not adopt changes that arrived after
	// the producer proof was checked. Verify the recorded generations again
	// before publishing this immutable cleanup plan.
	if err := s.verifyPreviewContentOwnership(ctx, id, includeBlobs); err != nil {
		return err
	}
	parent := filepath.Join(s.opts.StateDir, "preview-content-evictions")
	if err := handoffPrivateDirectory(parent, true); err != nil {
		return err
	}
	root := filepath.Join(parent, "eviction-"+rand.Text())
	if err := os.Mkdir(root, 0700); err != nil {
		return err
	}
	identity, err := handoffReadIdentity(root)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = handoffRemoveOwnedEmptyDirectory(root, identity)
		}
	}()
	for index := range objects {
		objects[index].Cleanup.Root = filepath.Join(root, fmt.Sprint(index))
	}
	journal := previewEvictionJournal{Version: 1, RepositoryID: id, Root: root, Identity: identity, Objects: objects}
	if err := s.validatePreviewEviction(journal); err != nil {
		return err
	}
	if err := handoffSyncDirectory(root); err != nil {
		return err
	}
	if err := handoffSyncDirectory(parent); err != nil {
		return err
	}
	if err := handoffWriteJSON(filepath.Join(s.opts.StateDir, previewEvictionJournalName), journal, true); err != nil {
		return err
	}
	published = true
	if err := s.finishPreviewEviction(ctx, journal); err != nil {
		return s.markLocalHandoffRecovery(err)
	}
	return s.removePreviewEvictionJournal()
}

// Native metadata acquisition can receive blobs from servers that ignore Git's
// blob filter. The offline snapshot is retained; claim no completed Free while
// that preserved metadata clone still holds content outside the six classes.
func (s *Service) verifyPreviewMetadataReclaimable(ctx context.Context, id string) error {
	s.mu.Lock()
	repo, exists := s.repositoryLocked(id)
	s.mu.Unlock()
	if !exists {
		return errors.New("repository is not in the catalogue")
	}
	bytes, err := retainedPreviewMetadataBlobBytes(ctx, s.opts.StateDir, repo)
	if err != nil {
		return err
	}
	if bytes > 0 {
		s.mu.Lock()
		if index := s.repositoryIndexLocked(id); index >= 0 && s.state.Repositories[index].DownloadedBytes < bytes {
			s.state.Repositories[index].DownloadedBytes = bytes
		}
		s.mu.Unlock()
		return fmt.Errorf("the source did not support blob filtering; its preserved browsing metadata contains %d bytes of content, so cached data was retained", bytes)
	}
	return nil
}

func retainedPreviewMetadataBlobBytes(ctx context.Context, stateDir string, repo Repository) (int64, error) {
	keys := map[string]bool{previewKey(repo): true}
	if sources, err := os.ReadDir(canonicalPreviewGitRoot(stateDir, repo.ID)); err == nil {
		for _, source := range sources {
			if validHandoffDigest(source.Name()) {
				keys[source.Name()] = true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	var total int64
	for key := range keys {
		gitDir := filepath.Join(stateDir, "previews", key, "git")
		if _, err := os.Lstat(gitDir); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := verifyHandoffDirectory(gitDir, localHandoffIdentity{}, false); err != nil {
			return 0, err
		}
		objects, err := localCheckoutGit(ctx, gitDir, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype) %(objectsize)")
		if err != nil {
			return 0, errors.New("preserved browsing metadata could not be checked for retained content")
		}
		for line := range strings.SplitSeq(objects, "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || fields[0] != "blob" {
				continue
			}
			bytes, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || bytes < 0 || total+bytes < total {
				return 0, errors.New("preserved browsing metadata returned invalid content accounting")
			}
			total += bytes
		}
	}
	return total, nil
}

// Keep checks this before capturing its six storage classes, otherwise a new
// user file in preview-git would become part of a later disposable backup.
func (s *Service) verifyPreviewContentOwnership(ctx context.Context, id string, includeBlobs bool) error {
	if includeBlobs {
		if err := verifyPreviewOnlyStorage(s.opts.StateDir, id); err != nil {
			return err
		}
	}
	gitRoot := canonicalPreviewGitRoot(s.opts.StateDir, id)
	var ownership previewContentOwnership
	if _, err := os.Lstat(gitRoot); err == nil {
		if err := handoffReadJSON(previewContentOwnershipPath(s.opts.StateDir, id), &ownership); err != nil {
			return errors.New("preview cache ownership could not be verified; its files were retained")
		}
		if ownership.Version != 1 || ownership.Root != gitRoot || ownership.RepositoryID != id {
			return errors.New("preview cache ownership does not match this repository")
		}
		if err := verifyHandoffDirectory(gitRoot, ownership.Identity, true); err != nil {
			return err
		}
		if err := validatePreviewGitCache(ctx, s.opts.StateDir, id, gitRoot); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if includeBlobs {
		return verifyPreviewBlobGeneration(ctx, s.opts.StateDir, id, filepath.Join(s.opts.StateDir, "engine", "cache", "blobs", engineName(id)))
	}
	return nil
}

func verifyPreviewOnlyStorage(stateDir, id string) error {
	name := engineName(id)
	parent := filepath.Join(stateDir, "engine", "repos", name)
	entries, err := os.ReadDir(parent)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "preview-git" {
			return errors.New("unregistered repository storage contains other local data; it was retained")
		}
	}
	for _, path := range []string{filepath.Join(stateDir, "engine", "overlays", name), filepath.Join(stateDir, "engine", "meta", name+".sqlite"), filepath.Join(stateDir, "engine", "meta", name+".sqlite-wal"), filepath.Join(stateDir, "engine", "meta", name+".sqlite-shm")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.New("unregistered repository storage contains other local data; it was retained")
		}
	}
	return nil
}

func capturePreviewCleanupObject(ctx context.Context, root string) (localHandoffCleanupObject, error) {
	identity, err := handoffReadIdentity(root)
	if err != nil {
		return localHandoffCleanupObject{}, err
	}
	object := localHandoffCleanupObject{Root: root, Identity: identity}
	manifest, err := checkoutTreeManifest(ctx, root, false)
	if err != nil {
		return object, err
	}
	for relative, record := range manifest {
		identity, err := handoffReadCleanupIdentity(filepath.Join(root, relative))
		if err != nil {
			return object, err
		}
		object.Entries = append(object.Entries, localHandoffCleanupEntry{PathRaw: base64.StdEncoding.EncodeToString([]byte(relative)), Identity: identity, Proof: handoffEntryProof(record)})
	}
	sort.Slice(object.Entries, func(i, j int) bool { return object.Entries[i].PathRaw < object.Entries[j].PathRaw })
	return object, verifyHandoffCleanupSubset(ctx, object)
}

func validatePreviewBlobCache(ctx context.Context, root string) error {
	if err := verifyHandoffDirectory(root, localHandoffIdentity{}, false); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !validPreviewOID(entry.Name()) {
			return errors.New("blob cache contains unrecognized files; all cached data was retained")
		}
		file, err := openVerifiedPreviewBlob(ctx, filepath.Join(root, entry.Name()), model.BaseNode{ObjectOID: entry.Name(), SizeState: "unknown"})
		if err != nil {
			return errors.New("cached blob bytes do not match their immutable identity; they were retained")
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func validatePreviewGitCache(ctx context.Context, stateDir, id, root string) error {
	keys, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, source := range keys {
		if len(source.Name()) != 64 || !validHandoffDigest(source.Name()) || !source.IsDir() {
			return errors.New("preview Git cache contains unknown sources; its files were retained")
		}
		commits, err := os.ReadDir(filepath.Join(root, source.Name()))
		if err != nil {
			return err
		}
		for _, commit := range commits {
			if !validPreviewOID(commit.Name()) || !commit.IsDir() {
				return errors.New("preview Git cache contains unknown revisions; its files were retained")
			}
			path := filepath.Join(root, source.Name(), commit.Name())
			children, err := os.ReadDir(path)
			if err != nil || len(children) != 1 || children[0].Name() != "git" || !children[0].IsDir() {
				return errors.New("preview acquisition contains unknown data; its files were retained")
			}
			gitDir := filepath.Join(path, "git")
			if err := beginPreviewGitGeneration(ctx, stateDir, id, gitDir); err != nil {
				return err
			}
			head, err := localCheckoutGit(ctx, gitDir, "rev-parse", "--verify", "HEAD^{commit}")
			if err != nil || head != commit.Name() {
				return errors.New("preview Git source changed from its immutable revision; it was retained")
			}
			changed, err := localCheckoutGit(ctx, gitDir, "diff-index", "--cached", "--name-only", "-z", "HEAD", "--")
			if err != nil || changed != "" {
				return errors.New("preview Git index contains local changes; it was retained")
			}
		}
	}
	return nil
}

func (s *Service) validatePreviewEviction(journal previewEvictionJournal) error {
	owner, name, ok := strings.Cut(journal.RepositoryID, "/")
	if journal.Version != 1 || !ok || validateComponent(owner) != nil || validateComponent(name) != nil || !journal.Identity.valid() || filepath.Dir(journal.Root) != filepath.Join(s.opts.StateDir, "preview-content-evictions") || !strings.HasPrefix(filepath.Base(journal.Root), "eviction-") || validateComponent(filepath.Base(journal.Root)) != nil || len(journal.Objects) < 1 || len(journal.Objects) > 2 {
		return errors.New("invalid preview cache eviction journal")
	}
	known := map[string]bool{canonicalPreviewGitRoot(s.opts.StateDir, journal.RepositoryID): true, filepath.Join(s.opts.StateDir, "engine", "cache", "blobs", engineName(journal.RepositoryID)): true}
	for index, object := range journal.Objects {
		if !known[object.Original] || object.Cleanup.Root != filepath.Join(journal.Root, fmt.Sprint(index)) || !object.Cleanup.Identity.valid() {
			return errors.New("preview eviction contains unowned storage paths")
		}
		delete(known, object.Original)
		entries, err := handoffCleanupEntries(object.Cleanup)
		if err != nil {
			return err
		}
		if entry, exists := entries["."]; !exists || entry.Identity != object.Cleanup.Identity {
			return errors.New("preview eviction has no root identity proof")
		}
	}
	return nil
}

func (s *Service) finishPreviewEviction(ctx context.Context, journal previewEvictionJournal) error {
	if err := verifyHandoffDirectory(journal.Root, journal.Identity, true); err != nil {
		return err
	}
	for _, object := range journal.Objects {
		_, retiredErr := os.Lstat(object.Cleanup.Root)
		if errors.Is(retiredErr, os.ErrNotExist) {
			original := object.Cleanup
			original.Root = object.Original
			if _, err := os.Lstat(object.Original); errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err := verifyHandoffCleanupSubset(ctx, original); err != nil {
				return err
			}
			if err := handoffDirectoryNotInUse(ctx, object.Original); err != nil {
				return err
			}
			if err := handoffRenameOwnedObject(object.Original, object.Cleanup.Root, object.Cleanup.Identity); err != nil {
				return err
			}
		} else if retiredErr != nil {
			return retiredErr
		}
		if _, err := os.Lstat(object.Original); !errors.Is(err, os.ErrNotExist) {
			return errors.New("preview source storage reappeared during eviction; both copies were retained")
		}
		if err := handoffRemovePlannedObject(ctx, object.Cleanup, nil); err != nil {
			return err
		}
	}
	return handoffRemoveOwnedEmptyDirectory(journal.Root, journal.Identity)
}

func (s *Service) recoverPreviewContentEviction(ctx context.Context) error {
	var journal previewEvictionJournal
	err := handoffReadJSON(filepath.Join(s.opts.StateDir, previewEvictionJournalName), &journal)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.validatePreviewEviction(journal); err != nil {
		return err
	}
	if _, err := os.Lstat(journal.Root); !errors.Is(err, os.ErrNotExist) {
		if err := s.finishPreviewEviction(ctx, journal); err != nil {
			return err
		}
	} else {
		for _, object := range journal.Objects {
			if _, err := os.Lstat(object.Original); !errors.Is(err, os.ErrNotExist) {
				return errors.New("preview eviction lost its quarantine while source data remains; its files were retained")
			}
		}
	}
	return s.removePreviewEvictionJournal()
}

func (s *Service) removePreviewEvictionJournal() error {
	path := filepath.Join(s.opts.StateDir, previewEvictionJournalName)
	var journal previewEvictionJournal
	if err := handoffReadJSON(path, &journal); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := s.validatePreviewEviction(journal); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return handoffSyncDirectory(s.opts.StateDir)
}
