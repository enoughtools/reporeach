//go:build darwin || linux

package desktop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sys/unix"
)

const localHandoffCleanupName = "local-handoff-cleanup.json"
const maxLocalHandoffCleanupBytes = 256 << 20

// A committed cleanup is allowed to leave an unchanged subset of this plan.
// Missing entries are completed deletions; new, changed or replaced entries are
// never inferred to be owned. The immutable plan is flushed before any unlink.
type localHandoffCleanupPlan struct {
	Version      int                         `json:"version"`
	RepositoryID string                      `json:"repositoryID"`
	Objects      []localHandoffCleanupObject `json:"objects"`
}

type localHandoffCleanupObject struct {
	Root     string                     `json:"root"`
	Identity localHandoffIdentity       `json:"identity"`
	Entries  []localHandoffCleanupEntry `json:"entries"`
}

type localHandoffCleanupEntry struct {
	PathRaw  string               `json:"pathRaw"`
	Identity localHandoffIdentity `json:"identity"`
	Proof    string               `json:"proof"`
}

func (s *Service) localHandoffCleanupPlan(ctx context.Context, journal localHandoffJournal) (localHandoffCleanupPlan, error) {
	path := filepath.Join(s.opts.StateDir, localHandoffCleanupName)
	var plan localHandoffCleanupPlan
	file, err := handoffOpenPrivateRegularLimit(path, maxLocalHandoffCleanupBytes)
	if err == nil {
		defer file.Close()
		decoder := json.NewDecoder(io.LimitReader(file, maxLocalHandoffCleanupBytes+1))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&plan); err != nil {
			return plan, err
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return plan, errors.New("cleanup plan has trailing data; retained files were preserved")
		}
		return plan, validateLocalHandoffCleanupPlan(plan, journal)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return plan, err
	}
	if err := verifyHandoffQuarantine(ctx, journal); err != nil {
		return plan, err
	}
	if err := s.verifyRetiredReceipt(ctx, *journal.Receipt); err != nil {
		return plan, err
	}
	plan = localHandoffCleanupPlan{Version: 1, RepositoryID: journal.RepositoryID}
	objects := []localHandoffObject{{Retired: journal.Temporary, Identity: journal.Identity, Digest: journal.Digest}}
	objects = append(objects, journal.Receipt.Retired...)
	for _, original := range objects {
		object := localHandoffCleanupObject{Root: original.Retired, Identity: original.Identity}
		manifest, err := checkoutTreeManifest(ctx, object.Root, false)
		if errors.Is(err, os.ErrNotExist) {
			plan.Objects = append(plan.Objects, object)
			continue
		}
		if err != nil || handoffManifestDigest(manifest) != original.Digest {
			return plan, errors.New("storage changed before cleanup planning; its files were retained")
		}
		for relative, record := range manifest {
			identity, err := handoffReadCleanupIdentity(filepath.Join(object.Root, relative))
			if err != nil {
				return plan, err
			}
			object.Entries = append(object.Entries, localHandoffCleanupEntry{PathRaw: base64.StdEncoding.EncodeToString([]byte(relative)), Identity: identity, Proof: handoffEntryProof(record)})
		}
		if err := verifyHandoffCleanupSubset(ctx, object); err != nil {
			return plan, err
		}
		plan.Objects = append(plan.Objects, object)
	}
	if err := validateLocalHandoffCleanupPlan(plan, journal); err != nil {
		return plan, err
	}
	if err := handoffWriteJSON(path, plan, true); err != nil {
		return plan, err
	}
	return plan, nil
}

func validateLocalHandoffCleanupPlan(plan localHandoffCleanupPlan, journal localHandoffJournal) error {
	if plan.Version != 1 || plan.RepositoryID != journal.RepositoryID || len(plan.Objects) != len(journal.Receipt.Retired)+1 {
		return errors.New("cleanup plan does not match the committed repository")
	}
	expected := map[string]localHandoffIdentity{journal.Temporary: journal.Identity}
	for _, object := range journal.Receipt.Retired {
		expected[object.Retired] = object.Identity
	}
	for _, object := range plan.Objects {
		identity, exists := expected[object.Root]
		if !exists || identity != object.Identity {
			return errors.New("cleanup plan contains an unowned storage root")
		}
		delete(expected, object.Root)
		entries, err := handoffCleanupEntries(object)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			root, exists := entries["."]
			if !exists || root.Identity != object.Identity {
				return errors.New("cleanup plan is missing its root identity")
			}
		}
	}
	return nil
}

func handoffCleanupEntries(object localHandoffCleanupObject) (map[string]localHandoffCleanupEntry, error) {
	entries := make(map[string]localHandoffCleanupEntry, len(object.Entries))
	for _, entry := range object.Entries {
		pathBytes, err := base64.StdEncoding.DecodeString(entry.PathRaw)
		path := string(pathBytes)
		if err != nil || model.CleanPath(path) != path || path == ".." || strings.HasPrefix(path, "../") || filepath.IsAbs(path) || strings.IndexByte(path, 0) >= 0 || !validHandoffDigest(entry.Proof) || entry.Identity.Inode == 0 || entry.Identity.UID != uint32(os.Geteuid()) {
			return nil, errors.New("cleanup plan contains invalid file ownership metadata")
		}
		if _, exists := entries[path]; exists {
			return nil, errors.New("cleanup plan contains duplicate file paths")
		}
		entries[path] = entry
	}
	return entries, nil
}

func handoffEntryProof(record checkoutFileRecord) string {
	return handoffManifestDigest(map[string]checkoutFileRecord{".": record})
}

func verifyHandoffCleanupSubset(ctx context.Context, object localHandoffCleanupObject) error {
	identity, err := handoffReadCleanupIdentity(object.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || identity != object.Identity {
		return errors.New("cleanup storage root was replaced; remaining files were retained")
	}
	planned, err := handoffCleanupEntries(object)
	if err != nil {
		return err
	}
	manifest, err := checkoutTreeManifest(ctx, object.Root, false)
	if err != nil {
		return err
	}
	for relative, record := range manifest {
		entry, exists := planned[relative]
		if !exists || handoffEntryProof(record) != entry.Proof {
			return errors.New("new or changed data appeared during cleanup; remaining files were retained")
		}
		current, err := handoffReadCleanupIdentity(filepath.Join(object.Root, relative))
		if err != nil || current != entry.Identity {
			return errors.New("a cleanup file was replaced; remaining files were retained")
		}
	}
	return nil
}

// afterRemove is nil in production. Tests inject a failure after the first
// successful unlink to exercise exactly the state left by an interrupted Free.
func handoffRemovePlannedObject(ctx context.Context, object localHandoffCleanupObject, afterRemove func() error) error {
	if _, err := os.Lstat(object.Root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := verifyHandoffCleanupSubset(ctx, object); err != nil {
		return err
	}
	if object.Identity.Mode == unix.S_IFDIR {
		if err := handoffDirectoryNotInUse(ctx, object.Root); err != nil {
			return err
		}
	}
	parent, err := handoffOpenDirectory(filepath.Dir(object.Root))
	if err != nil {
		return err
	}
	defer parent.Close()
	var stat unix.Stat_t
	name := filepath.Base(object.Root)
	if unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW) != nil || handoffIdentity(&stat) != object.Identity {
		return errors.New("cleanup root changed before deletion")
	}
	if object.Identity.Mode == unix.S_IFREG {
		entries, err := handoffCleanupEntries(object)
		if err != nil {
			return err
		}
		manifest, err := checkoutTreeManifest(ctx, object.Root, false)
		if err != nil || handoffEntryProof(manifest["."]) != entries["."].Proof {
			return errors.New("cleanup file changed immediately before removal")
		}
		if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
			return err
		}
		return parent.Sync()
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), object.Root)
	defer directory.Close()
	if unix.Fstat(fd, &stat) != nil || handoffIdentity(&stat) != object.Identity {
		return errors.New("cleanup root changed while opening it")
	}
	entries, err := handoffCleanupEntries(object)
	if err != nil {
		return err
	}
	if err := handoffRemovePlannedChildren(ctx, directory, object.Root, ".", entries, afterRemove); err != nil {
		return err
	}
	if unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW) != nil || handoffIdentity(&stat) != object.Identity {
		return errors.New("cleanup root was replaced; the replacement was retained")
	}
	manifest, err := checkoutTreeManifest(ctx, object.Root, false)
	if err != nil || handoffEntryProof(manifest["."]) != entries["."].Proof {
		return errors.New("cleanup directory metadata changed before removal; it was retained")
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parent.Sync()
}

func handoffRemovePlannedChildren(ctx context.Context, directory *os.File, root, relative string, planned map[string]localHandoffCleanupEntry, afterRemove func() error) error {
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := model.CleanPath(filepath.Join(relative, entry.Name()))
		expected, exists := planned[path]
		var before unix.Stat_t
		if !exists || unix.Fstatat(int(directory.Fd()), entry.Name(), &before, unix.AT_SYMLINK_NOFOLLOW) != nil || handoffIdentity(&before) != expected.Identity {
			return errors.New("a cleanup entry was replaced or added; it was retained")
		}
		if before.Mode&unix.S_IFMT == unix.S_IFDIR {
			fd, err := unix.Openat(int(directory.Fd()), entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			child := os.NewFile(uintptr(fd), entry.Name())
			var opened unix.Stat_t
			if unix.Fstat(fd, &opened) != nil || handoffIdentity(&opened) != expected.Identity {
				child.Close()
				return errors.New("cleanup child directory changed while opening it")
			}
			removeErr, closeErr := handoffRemovePlannedChildren(ctx, child, root, path, planned, afterRemove), child.Close()
			if err := errors.Join(removeErr, closeErr); err != nil {
				return err
			}
			if unix.Fstatat(int(directory.Fd()), entry.Name(), &opened, unix.AT_SYMLINK_NOFOLLOW) != nil || handoffIdentity(&opened) != expected.Identity {
				return errors.New("cleanup child directory was replaced; the replacement was retained")
			}
			manifest, err := checkoutTreeManifest(ctx, filepath.Join(root, path), false)
			if err != nil || handoffEntryProof(manifest["."]) != expected.Proof {
				return errors.New("cleanup directory metadata changed; it was retained")
			}
			if err := unix.Unlinkat(int(directory.Fd()), entry.Name(), unix.AT_REMOVEDIR); err != nil {
				return err
			}
		} else {
			manifest, err := checkoutTreeManifest(ctx, filepath.Join(root, path), false)
			if err != nil || handoffEntryProof(manifest["."]) != expected.Proof {
				return errors.New("a cleanup file changed before deletion; it was retained")
			}
			if unix.Fstatat(int(directory.Fd()), entry.Name(), &before, unix.AT_SYMLINK_NOFOLLOW) != nil || handoffIdentity(&before) != expected.Identity {
				return errors.New("a cleanup file was replaced before deletion; it was retained")
			}
			if err := unix.Unlinkat(int(directory.Fd()), entry.Name(), 0); err != nil {
				return err
			}
		}
		if err := directory.Sync(); err != nil {
			return err
		}
		if afterRemove != nil {
			if err := afterRemove(); err != nil {
				return err
			}
		}
	}
	return directory.Sync()
}

func handoffReadCleanupIdentity(path string) (localHandoffIdentity, error) {
	parent, err := handoffOpenDirectory(filepath.Dir(path))
	if err != nil {
		return localHandoffIdentity{}, err
	}
	defer parent.Close()
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), filepath.Base(path), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return localHandoffIdentity{}, err
	}
	identity := handoffIdentity(&stat)
	if identity.Inode == 0 || identity.UID != uint32(os.Geteuid()) || (identity.Mode != unix.S_IFDIR && identity.Mode != unix.S_IFREG && identity.Mode != unix.S_IFLNK) {
		return localHandoffIdentity{}, errors.New("cleanup encountered unsupported file ownership")
	}
	return identity, checkoutValidateNativeMetadata(path)
}

func (s *Service) removeLocalHandoffCleanupPlan(journal localHandoffJournal) error {
	path := filepath.Join(s.opts.StateDir, localHandoffCleanupName)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	// Validate its private file type and its transaction binding before unlink.
	plan, err := s.localHandoffCleanupPlan(context.Background(), journal)
	if err != nil {
		return err
	}
	if err := validateLocalHandoffCleanupPlan(plan, journal); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return handoffSyncDirectory(filepath.Dir(path))
}
