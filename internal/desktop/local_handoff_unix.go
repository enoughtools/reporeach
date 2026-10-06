//go:build darwin || linux

package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type localHandoffIdentity struct {
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
	UID       uint32 `json:"uid"`
	Mode      uint32 `json:"mode"`
	BirthSec  int64  `json:"birthSec"`
	BirthNsec int64  `json:"birthNsec"`
}

func (i localHandoffIdentity) valid() bool {
	return i.Inode != 0 && i.UID == uint32(os.Geteuid()) && (i.Mode == unix.S_IFDIR || i.Mode == unix.S_IFREG)
}

func handoffIdentity(stat *unix.Stat_t) localHandoffIdentity {
	sec, nsec := handoffBirthTime(stat)
	return localHandoffIdentity{Device: uint64(stat.Dev), Inode: stat.Ino, UID: stat.Uid,
		Mode: uint32(stat.Mode) & unix.S_IFMT, BirthSec: sec, BirthNsec: nsec}
}

// Pin each directory component without following symlinks. Canonical aliases
// such as macOS /var -> /private/var are resolved once; the pinned object's
// identity is then compared to the caller's current path before any mutation.
func handoffOpenDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || hasAdoptionControl(path) {
		return nil, errors.New("storage handoff requires absolute directory paths")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(canonical, "/"), "/") {
		if component == "" {
			continue
		}
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	file := os.NewFile(uintptr(fd), canonical)
	var current, pinned unix.Stat_t
	if unix.Stat(path, &current) != nil || unix.Fstat(fd, &pinned) != nil || handoffIdentity(&current) != handoffIdentity(&pinned) {
		file.Close()
		return nil, errors.New("the storage handoff directory changed")
	}
	return file, nil
}

func handoffReadIdentity(path string) (localHandoffIdentity, error) {
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
	if !identity.valid() {
		return localHandoffIdentity{}, errors.New("storage handoff requires an owned real directory or regular file")
	}
	if err := checkoutValidateNativeMetadata(path); err != nil {
		return localHandoffIdentity{}, err
	}
	return identity, nil
}

func verifyHandoffDirectory(path string, expected localHandoffIdentity, private bool) error {
	identity, err := handoffReadIdentity(path)
	if err != nil {
		return err
	}
	if identity.Mode != unix.S_IFDIR || expected.valid() && identity != expected {
		return errors.New("the local checkout folder was replaced; its contents were retained")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	mask := os.FileMode(0o022)
	if private {
		mask = 0o077
	}
	if info.Mode().Perm()&mask != 0 {
		return errors.New("the storage handoff folder is writable by another user")
	}
	return nil
}

func handoffPrivateDirectory(path string, create bool) error {
	if create {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(path, 0o700); err != nil {
				return err
			}
			if err := handoffSyncDirectory(filepath.Dir(path)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return verifyHandoffDirectory(path, localHandoffIdentity{}, true)
}

func handoffOpenPrivateRegular(path string) (*os.File, error) {
	return handoffOpenPrivateRegularLimit(path, 1<<20)
}

func handoffOpenPrivateRegularLimit(path string, maximum int64) (*os.File, error) {
	if err := handoffPrivateDirectory(filepath.Dir(path), false); err != nil {
		return nil, err
	}
	parent, err := handoffOpenDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o600 || stat.Nlink != 1 || stat.Size > maximum || checkoutValidateNativeMetadata(path) != nil {
		file.Close()
		return nil, errors.New("the storage handoff ownership record is unsafe")
	}
	return file, nil
}

func handoffRenameOwnedDirectory(source, destination string, parentIdentity, identity localHandoffIdentity) error {
	if filepath.Dir(source) != filepath.Dir(destination) || identity.Mode != unix.S_IFDIR {
		return errors.New("a local checkout must be renamed within its original parent")
	}
	if err := verifyHandoffDirectory(filepath.Dir(source), parentIdentity, false); err != nil {
		return err
	}
	return handoffRenameOwnedObject(source, destination, identity)
}

func handoffRenameOwnedObject(source, destination string, identity localHandoffIdentity) error {
	parent, err := handoffOpenDirectory(filepath.Dir(source))
	if err != nil {
		return err
	}
	defer parent.Close()
	target, err := handoffOpenDirectory(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer target.Close()
	var before unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), filepath.Base(source), &before, unix.AT_SYMLINK_NOFOLLOW); err != nil || handoffIdentity(&before) != identity {
		return errors.New("the storage source was replaced before its handoff; data was retained")
	}
	if err := handoffRenameExclusive(parent, filepath.Base(source), target, filepath.Base(destination)); err != nil {
		return err
	}
	var after unix.Stat_t
	if err := unix.Fstatat(int(target.Fd()), filepath.Base(destination), &after, unix.AT_SYMLINK_NOFOLLOW); err != nil || handoffIdentity(&after) != identity {
		return errors.New("the renamed storage identity changed; data was retained")
	}
	return errors.Join(parent.Sync(), target.Sync())
}

// Restore either an already-restored directory or its exact quarantined inode.
// Never overwrite a directory that appeared at the original location.
func handoffRestoreOwnedDirectory(source, destination string, parentIdentity, identity localHandoffIdentity) error {
	sourceIdentity, sourceErr := handoffReadIdentity(source)
	destinationIdentity, destinationErr := handoffReadIdentity(destination)
	if errors.Is(sourceErr, os.ErrNotExist) && destinationErr == nil && destinationIdentity == identity {
		return verifyHandoffDirectory(filepath.Dir(destination), parentIdentity, false)
	}
	if sourceErr != nil || sourceIdentity != identity || !errors.Is(destinationErr, os.ErrNotExist) {
		return errors.New("storage recovery encountered a replaced or occupied folder; all surviving data was retained")
	}
	return handoffRenameOwnedDirectory(source, destination, parentIdentity, identity)
}

func handoffRemoveOwnedTree(ctx context.Context, path string, identity localHandoffIdentity, digest string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	actual, err := handoffReadIdentity(path)
	if err != nil || actual != identity {
		return errors.New("retained storage was replaced; it was not deleted")
	}
	if current, err := handoffTreeDigest(ctx, path); err != nil || current != digest {
		return errors.New("retained storage changed after verification; it was not deleted")
	}
	if identity.Mode == unix.S_IFDIR {
		if err := handoffDirectoryNotInUse(ctx, path); err != nil {
			return err
		}
	}
	// Pin the parent for removal and compare the exact object once again after
	// the content and busy checks. RemoveAll never follows interior symlinks.
	parent, err := handoffOpenDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), filepath.Base(path), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil || handoffIdentity(&stat) != identity {
		return errors.New("retained storage changed immediately before deletion")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if identity.Mode == unix.S_IFREG {
		if err := unix.Unlinkat(int(parent.Fd()), filepath.Base(path), 0); err != nil {
			return err
		}
	} else {
		fd, err := unix.Openat(int(parent.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		directory := os.NewFile(uintptr(fd), path)
		if unix.Fstat(fd, &stat) != nil || handoffIdentity(&stat) != identity {
			directory.Close()
			return errors.New("retained checkout changed while opening it for deletion")
		}
		removeErr := handoffRemoveChildren(directory)
		closeErr := directory.Close()
		if err := errors.Join(removeErr, closeErr); err != nil {
			return err
		}
		if unix.Fstatat(int(parent.Fd()), filepath.Base(path), &stat, unix.AT_SYMLINK_NOFOLLOW) != nil || handoffIdentity(&stat) != identity {
			return errors.New("retained storage root was replaced during cleanup; the replacement was retained")
		}
		if err := unix.Unlinkat(int(parent.Fd()), filepath.Base(path), unix.AT_REMOVEDIR); err != nil {
			return err
		}
	}
	return parent.Sync()
}

// Every descent is relative to the verified root inode. A replaced pathname
// cannot redirect recursive deletion into a foreign folder or symlink target.
func handoffRemoveChildren(directory *os.File) error {
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		var before unix.Stat_t
		if err := unix.Fstatat(int(directory.Fd()), entry.Name(), &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if before.Mode&unix.S_IFMT == unix.S_IFDIR {
			fd, err := unix.Openat(int(directory.Fd()), entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			child := os.NewFile(uintptr(fd), entry.Name())
			var opened unix.Stat_t
			if unix.Fstat(fd, &opened) != nil || handoffIdentity(&opened) != handoffIdentity(&before) {
				child.Close()
				return errors.New("retained child directory changed during cleanup")
			}
			removeErr, closeErr := handoffRemoveChildren(child), child.Close()
			if err := errors.Join(removeErr, closeErr); err != nil {
				return err
			}
			var current unix.Stat_t
			if unix.Fstatat(int(directory.Fd()), entry.Name(), &current, unix.AT_SYMLINK_NOFOLLOW) != nil || handoffIdentity(&current) != handoffIdentity(&before) {
				return errors.New("retained child directory was replaced during cleanup")
			}
			if err := unix.Unlinkat(int(directory.Fd()), entry.Name(), unix.AT_REMOVEDIR); err != nil {
				return err
			}
		} else if err := unix.Unlinkat(int(directory.Fd()), entry.Name(), 0); err != nil {
			return err
		}
	}
	return directory.Sync()
}

func handoffRemoveOwnedEmptyDirectory(path string, identity localHandoffIdentity) error {
	actual, err := handoffReadIdentity(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || actual != identity {
		return errors.New("retired storage container was replaced; it was retained")
	}
	parent, err := handoffOpenDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	var current unix.Stat_t
	if unix.Fstatat(int(parent.Fd()), filepath.Base(path), &current, unix.AT_SYMLINK_NOFOLLOW) != nil || handoffIdentity(&current) != identity {
		return errors.New("retired storage container changed before cleanup")
	}
	if err := unix.Unlinkat(int(parent.Fd()), filepath.Base(path), unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parent.Sync()
}
