//go:build darwin

package desktop

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sys/unix"
)

const hybridManifestVersion = 1

var errHybridCatalogueConflict = errors.New("a repository folder is already occupied; existing files were preserved")

type hybridCatalogueLink struct {
	Target   string                   `json:"target"`
	Identity nativeRootObjectIdentity `json:"identity"`
	// The same-directory temporary symlink is recorded and flushed before the
	// leaf becomes visible. Linkat publishes it without overwriting any entry.
	PendingName string `json:"pendingName,omitempty"`
}

type hybridCatalogueManifest struct {
	Version  int                            `json:"version"`
	Root     string                         `json:"root"`
	Identity nativeRootObjectIdentity       `json:"identity"`
	Links    map[string]hybridCatalogueLink `json:"links"`
}

type hybridCatalogueRoot struct {
	file             *os.File
	path, volumeUUID string
	identity         nativeRootObjectIdentity
	device           int32
}

func (root *hybridCatalogueRoot) close()  { _ = root.file.Close() }
func (root *hybridCatalogueRoot) fd() int { return int(root.file.Fd()) }

func (s *Service) hybridCatalogueMountRoot() (string, error) {
	parent := filepath.Join(s.opts.StateDir, "native-catalogue")
	if err := nativePrivateReceiptDirectory(s.opts.StateDir, false); err != nil {
		return "", err
	}
	if err := nativePrivateReceiptDirectory(parent, true); err != nil {
		return "", err
	}
	root := filepath.Join(parent, "volume")
	// Never chmod a mounted root: the receipt check handles OS-created entries
	// only after a normal detach, without inspecting a live virtual volume.
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(root, 0o700); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	return root, nil
}

func openHybridCatalogueRoot(root string, create bool) (*hybridCatalogueRoot, error) {
	if err := validateMountRoot(root); err != nil {
		return nil, err
	}
	if create {
		if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(root, 0o755); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
	}
	var before unix.Stat_t
	if err := unix.Lstat(root, &before); err != nil {
		return nil, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, errors.New("the repository catalogue must be a real folder")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	mounts, err := cachedDarwinMounts()
	if err != nil {
		return nil, err
	}
	if _, mounted := mountAtRoot(mounts, canonical); mounted {
		return nil, errors.New("unmount the previous repository volume before opening the ordinary catalogue folder")
	}
	fd, err := unix.Open(canonical, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), canonical)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		f.Close()
		return nil, err
	}
	if stat.Ino != before.Ino || stat.Dev != before.Dev || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o022 != 0 || !nativeObjectACLFree(canonical, &stat) {
		f.Close()
		return nil, errors.New("the repository catalogue folder changed or is writable by another user")
	}
	uuid, err := nativeBackingVolumeUUID(fd)
	identity := nativeRootIdentity(&stat, uuid)
	if err != nil || !identity.valid() {
		f.Close()
		return nil, errors.New("the repository catalogue folder could not be safely identified")
	}
	return &hybridCatalogueRoot{file: f, path: canonical, volumeUUID: uuid, identity: identity, device: stat.Dev}, nil
}

func (s *Service) checkHybridCatalogueDirectory(root string) error {
	opened, err := openHybridCatalogueRoot(root, true)
	if err != nil {
		return err
	}
	defer opened.close()
	if pathsOverlap(opened.path, s.opts.StateDir) {
		return errors.New("repository catalogue and private state folders must be separate")
	}
	_, err = s.readHybridCatalogueManifest(opened)
	return err
}

func hybridManifestPath(stateDir, root string) string {
	digest := sha256.Sum256([]byte(root))
	return filepath.Join(stateDir, "catalogue-links", hex.EncodeToString(digest[:])+".json")
}

func (s *Service) readHybridCatalogueManifest(root *hybridCatalogueRoot) (*hybridCatalogueManifest, error) {
	manifest := &hybridCatalogueManifest{Version: hybridManifestVersion, Root: root.path, Identity: root.identity, Links: map[string]hybridCatalogueLink{}}
	path := hybridManifestPath(s.opts.StateDir, root.path)
	if err := nativePrivateReceiptDirectory(s.opts.StateDir, false); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return manifest, nil
	}
	if err := nativePrivateReceiptDirectory(filepath.Dir(path), false); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return manifest, nil
	}
	if err != nil {
		return nil, errors.New("the catalogue link ownership record is unsafe")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || stat.Size > 2<<20 || !nativeObjectACLFree(path, &stat) {
		return nil, errors.New("the catalogue link ownership record is unsafe")
	}
	decoder := json.NewDecoder(io.LimitReader(f, 2<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(manifest) != nil || decoder.Decode(new(any)) != io.EOF || manifest.Version != hybridManifestVersion || manifest.Root != root.path || manifest.Identity != root.identity || manifest.Links == nil {
		return nil, errors.New("the catalogue link ownership record does not match this folder")
	}
	for key, link := range manifest.Links {
		owner, name, ok := strings.Cut(key, "/")
		if !ok || validateComponent(owner) != nil || validateComponent(name) != nil || !link.Identity.valid() || link.Identity.VolumeUUID != root.volumeUUID || link.Identity.UID != uint32(os.Geteuid()) || !filepath.IsAbs(link.Target) || "/"+model.CleanPath(link.Target) != link.Target || (link.PendingName != "" && (!strings.HasPrefix(link.PendingName, ".reporeach-link-") || validateComponent(link.PendingName) != nil)) {
			return nil, errors.New("the catalogue link ownership record is invalid")
		}
	}
	return manifest, nil
}

func (s *Service) writeHybridCatalogueManifest(root *hybridCatalogueRoot, manifest *hybridCatalogueManifest) error {
	if manifest.Root != root.path || manifest.Identity != root.identity {
		return errors.New("the catalogue link ownership record changed")
	}
	// Recheck the root path's object before publishing a record for the pinned fd.
	var stat unix.Stat_t
	if err := unix.Lstat(root.path, &stat); err != nil || nativeRootIdentity(&stat, root.volumeUUID) != root.identity {
		return errNativeRootChanged
	}
	path := hybridManifestPath(s.opts.StateDir, root.path)
	if err := nativePrivateReceiptDirectory(filepath.Dir(path), true); err != nil {
		return err
	}
	if _, err := s.readHybridCatalogueManifest(root); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".catalogue-links-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(manifest); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (root *hybridCatalogueRoot) ownerDirectory(owner string, create bool) (*os.File, error) {
	if validateComponent(owner) != nil {
		return nil, errors.New("invalid catalogue owner")
	}
	if create {
		if err := unix.Mkdirat(root.fd(), owner, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, err
		}
	}
	fd, err := unix.Openat(root.fd(), owner, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, err
		}
		return nil, errHybridCatalogueConflict
	}
	f := os.NewFile(uintptr(fd), filepath.Join(root.path, owner))
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Dev != root.device || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o022 != 0 || !nativeObjectACLFree(f.Name(), &stat) {
		f.Close()
		return nil, errHybridCatalogueConflict
	}
	return f, nil
}

func hybridLinkAt(directory *os.File, name string, root *hybridCatalogueRoot) (hybridCatalogueLink, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return hybridCatalogueLink{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFLNK {
		return hybridCatalogueLink{}, errHybridCatalogueConflict
	}
	buffer := make([]byte, 4097)
	n, err := unix.Readlinkat(int(directory.Fd()), name, buffer)
	if err != nil || n >= len(buffer) {
		return hybridCatalogueLink{}, errHybridCatalogueConflict
	}
	return hybridCatalogueLink{Target: string(buffer[:n]), Identity: nativeRootIdentity(&stat, root.volumeUUID)}, nil
}

func sameHybridLink(actual, owned hybridCatalogueLink) bool {
	return actual.Target == owned.Target && actual.Identity == owned.Identity
}

func removeOwnedHybridLink(directory *os.File, name string, root *hybridCatalogueRoot, owned hybridCatalogueLink) error {
	actual, err := hybridLinkAt(directory, name, root)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil || !sameHybridLink(actual, owned) {
		return errHybridCatalogueConflict
	}
	return unix.Unlinkat(int(directory.Fd()), name, 0)
}

func (s *Service) finishHybridLink(root *hybridCatalogueRoot, manifest *hybridCatalogueManifest, key string, directory *os.File) error {
	link := manifest.Links[key]
	_, name, _ := strings.Cut(key, "/")
	actual, err := hybridLinkAt(directory, name, root)
	if errors.Is(err, unix.ENOENT) && link.PendingName != "" {
		temporary, temporaryErr := hybridLinkAt(directory, link.PendingName, root)
		if errors.Is(temporaryErr, unix.ENOENT) {
			delete(manifest.Links, key)
			return s.writeHybridCatalogueManifest(root, manifest)
		}
		if temporaryErr != nil || !sameHybridLink(temporary, link) {
			return errHybridCatalogueConflict
		}
		if err := unix.Linkat(int(directory.Fd()), link.PendingName, int(directory.Fd()), name, 0); err != nil {
			return errHybridCatalogueConflict
		}
		actual, err = hybridLinkAt(directory, name, root)
	}
	if errors.Is(err, unix.ENOENT) && link.PendingName == "" {
		delete(manifest.Links, key)
		return s.writeHybridCatalogueManifest(root, manifest)
	}
	if err != nil || !sameHybridLink(actual, link) {
		return errHybridCatalogueConflict
	}
	if link.PendingName != "" {
		if err := removeOwnedHybridLink(directory, link.PendingName, root, link); err != nil {
			return err
		}
		if err := directory.Sync(); err != nil {
			return err
		}
		link.PendingName = ""
		manifest.Links[key] = link
		return s.writeHybridCatalogueManifest(root, manifest)
	}
	return nil
}

func (s *Service) publishHybridLink(root *hybridCatalogueRoot, manifest *hybridCatalogueManifest, key, target string, directory *os.File) error {
	_, name, _ := strings.Cut(key, "/")
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		return errHybridCatalogueConflict
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temporaryName := ".reporeach-link-" + hex.EncodeToString(random[:])
	if err := unix.Symlinkat(target, int(directory.Fd()), temporaryName); err != nil {
		return err
	}
	link, err := hybridLinkAt(directory, temporaryName, root)
	if err != nil {
		return err
	}
	link.PendingName = temporaryName
	if err := directory.Sync(); err != nil {
		return err
	}
	manifest.Links[key] = link
	if err := s.writeHybridCatalogueManifest(root, manifest); err != nil {
		return err
	}
	return s.finishHybridLink(root, manifest, key, directory)
}

// syncHybridCatalogue changes only manifest-owned symlinks. Ordinary checkout
// directories and unrelated files are neither hidden nor deleted. Callers hold
// the service lifecycle lock and normally detach the private volume first.
func (s *Service) syncHybridCatalogue(rootPath string, entries []hybridCatalogueEntry) error {
	root, err := openHybridCatalogueRoot(rootPath, true)
	if err != nil {
		return err
	}
	defer root.close()
	if pathsOverlap(root.path, s.opts.StateDir) {
		return errors.New("repository catalogue and private state folders must be separate")
	}
	privateRoot, err := s.hybridCatalogueMountRoot()
	if err != nil {
		return err
	}
	manifest, err := s.readHybridCatalogueManifest(root)
	if err != nil {
		return err
	}
	desired := make(map[string]string, len(entries))
	physical := make(map[string]bool)
	for _, entry := range entries {
		if validateComponent(entry.Owner) != nil || validateComponent(entry.Name) != nil || entry.ID != entry.Owner+"/"+entry.Name {
			return errors.New("invalid catalogue repository")
		}
		key := entry.Owner + "/" + entry.Name
		if _, exists := desired[key]; exists {
			return errors.New("duplicate catalogue repository")
		}
		target := filepath.Join(privateRoot, entry.Owner, entry.Name)
		if entry.LocalPath != "" {
			if !filepath.IsAbs(entry.LocalPath) || "/"+model.CleanPath(entry.LocalPath) != entry.LocalPath {
				return errors.New("local checkout path must be absolute")
			}
			target = entry.LocalPath
			physical[key] = target == filepath.Join(root.path, entry.Owner, entry.Name)
		}
		desired[key] = target
	}
	// Validate every desired leaf before changing existing links. This makes a
	// foreign checkout conflict fail before unrelated catalogue changes.
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		owner, name, _ := strings.Cut(key, "/")
		directory, err := root.ownerDirectory(owner, true)
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		err = unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if err == nil {
			owned, exists := manifest.Links[key]
			if physical[key] && stat.Mode&unix.S_IFMT == unix.S_IFDIR {
				directory.Close()
				continue
			}
			actual, linkErr := hybridLinkAt(directory, name, root)
			if !exists || linkErr != nil || !sameHybridLink(actual, owned) {
				directory.Close()
				return fmt.Errorf("%s: %w", key, errHybridCatalogueConflict)
			}
		} else if physical[key] {
			directory.Close()
			return fmt.Errorf("%s: local checkout is missing", key)
		} else if !errors.Is(err, unix.ENOENT) {
			directory.Close()
			return err
		}
		directory.Close()
	}
	ownedKeys := make([]string, 0, len(manifest.Links))
	for key := range manifest.Links {
		ownedKeys = append(ownedKeys, key)
	}
	sort.Strings(ownedKeys)
	for _, key := range ownedKeys {
		owned := manifest.Links[key]
		if target, keep := desired[key]; keep && target == owned.Target && !physical[key] {
			continue
		}
		owner, name, _ := strings.Cut(key, "/")
		directory, err := root.ownerDirectory(owner, false)
		if errors.Is(err, unix.ENOENT) {
			delete(manifest.Links, key)
			if err := s.writeHybridCatalogueManifest(root, manifest); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err = removeOwnedHybridLink(directory, name, root, owned); err == nil && owned.PendingName != "" {
			err = removeOwnedHybridLink(directory, owned.PendingName, root, owned)
		}
		if err == nil {
			err = directory.Sync()
		}
		directory.Close()
		if err != nil {
			return err
		}
		delete(manifest.Links, key)
		if err := s.writeHybridCatalogueManifest(root, manifest); err != nil {
			return err
		}
	}
	for _, key := range keys {
		if physical[key] {
			continue
		}
		owner, _, _ := strings.Cut(key, "/")
		directory, err := root.ownerDirectory(owner, true)
		if err != nil {
			return err
		}
		if _, owned := manifest.Links[key]; owned {
			err = s.finishHybridLink(root, manifest, key, directory)
		}
		if err == nil {
			if _, owned := manifest.Links[key]; !owned {
				err = s.publishHybridLink(root, manifest, key, desired[key], directory)
			}
		}
		directory.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// releaseHybridCatalogueLink reserves no right to remove a directory. It is
// used immediately before a prepared physical checkout is installed at a leaf.
func (s *Service) releaseHybridCatalogueLink(rootPath, owner, name string) error {
	if validateComponent(owner) != nil || validateComponent(name) != nil {
		return errors.New("invalid catalogue repository")
	}
	root, err := openHybridCatalogueRoot(rootPath, false)
	if err != nil {
		return err
	}
	defer root.close()
	manifest, err := s.readHybridCatalogueManifest(root)
	if err != nil {
		return err
	}
	key := owner + "/" + name
	owned, exists := manifest.Links[key]
	if !exists {
		return errHybridCatalogueConflict
	}
	directory, err := root.ownerDirectory(owner, false)
	if errors.Is(err, unix.ENOENT) {
		delete(manifest.Links, key)
		return s.writeHybridCatalogueManifest(root, manifest)
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := removeOwnedHybridLink(directory, name, root, owned); err != nil {
		return err
	}
	if owned.PendingName != "" {
		if err := removeOwnedHybridLink(directory, owned.PendingName, root, owned); err != nil {
			return err
		}
	}
	if err := directory.Sync(); err != nil {
		return err
	}
	delete(manifest.Links, key)
	return s.writeHybridCatalogueManifest(root, manifest)
}
