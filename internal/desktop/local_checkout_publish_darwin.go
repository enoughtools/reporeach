//go:build darwin

package desktop

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"golang.org/x/sys/unix"
)

func checkoutValidateNativeMetadata(path string) error {
	metadata, err := nativePathACLMetadata(path)
	var stat unix.Stat_t
	if statErr := unix.Lstat(path, &stat); statErr != nil {
		return statErr
	}
	return checkoutValidateMetadataReply(metadata, err, &stat, false)
}

func checkoutValidateMetadataReply(metadata nativeACLMetadata, aclErr error, stat *unix.Stat_t, ownedUnsupportedACL bool) error {
	if aclErr != nil {
		if !ownedUnsupportedACL || !errors.Is(aclErr, unix.ENOTSUP) && !errors.Is(aclErr, unix.EINVAL) {
			return fmt.Errorf("the checkout's access-control metadata cannot be preserved: %w", aclErr)
		}
	} else if metadata.HasACL || !metadata.matchesStat(stat) {
		return errors.New("the checkout has unsupported or changed access-control metadata; retain it in place")
	}
	if stat == nil || stat.Flags != 0 {
		return errors.New("the checkout has unsupported native file flags; retain it in place")
	}
	return nil
}

func checkoutMountedViewMetadataValidator(mounted fusefs.MountedFS, root string) (func(string) error, error) {
	native, ok := mounted.(*nativeFSKitMount)
	if !ok {
		return checkoutValidateNativeMetadata, nil
	}
	identity, verified := native.verifiedIdentity()
	if !verified || identity.fsid == ([2]int32{}) || identity.owner != uint32(os.Geteuid()) || identity.typeName != "reporeach" ||
		identity.root != native.root || !mountSourceMatches(identity.source, native.source) || !checkoutPathWithin(identity.root, root) {
		return nil, errors.New("the native checkout source mount could not be safely identified")
	}
	if err := checkoutValidateMountSnapshot(identity, root, cachedDarwinMounts); err != nil {
		return nil, err
	}
	unsupportedACL, err := checkoutVolumeACLUnsupported(identity.root)
	if err != nil {
		return nil, fmt.Errorf("the owned native checkout volume cannot prove its ACL capability: %w", err)
	}
	if !unsupportedACL {
		return nil, errors.New("the owned native checkout volume reports ACL support; its unsupported ACL query cannot be bypassed")
	}
	if err := checkoutValidateMountSnapshot(identity, root, cachedDarwinMounts); err != nil {
		return nil, err
	}
	var rootStat, viewStat unix.Stat_t
	if err := unix.Lstat(identity.root, &rootStat); err != nil {
		return nil, err
	}
	if err := unix.Lstat(root, &viewStat); err != nil || viewStat.Mode&unix.S_IFMT != unix.S_IFDIR || viewStat.Dev != rootStat.Dev {
		return nil, errors.New("the native checkout source does not belong to its owned mount")
	}
	if err := checkoutValidateMountSnapshot(identity, root, cachedDarwinMounts); err != nil {
		return nil, err
	}
	return func(path string) error {
		currentIdentity, valid := native.verifiedIdentity()
		if !valid || currentIdentity != identity || !checkoutPathWithin(root, path) {
			return errors.New("the native checkout source mount changed")
		}
		if err := checkoutValidateMountSnapshot(identity, path, cachedDarwinMounts); err != nil {
			return err
		}
		metadata, aclErr := nativePathACLMetadata(path)
		var stat unix.Stat_t
		if err := unix.Lstat(path, &stat); err != nil || stat.Dev != rootStat.Dev || stat.Uid != uint32(os.Geteuid()) {
			return errors.New("a checkout vnode left its owned native mount")
		}
		if err := checkoutValidateMountSnapshot(identity, path, cachedDarwinMounts); err != nil {
			return err
		}
		// This RepoReach backend stores POSIX mode/owner and named xattrs, and
		// implements no ACL operation or ACL storage. Only its captured exact
		// mount, with disabled extended-security capability, may attest that
		// ENOTSUP or EINVAL means this mandatory ACL request is unsupported. Successful
		// replies still prove ACL absence and the same vnode; APFS never uses
		// this exception, including the copied Git directory and destination.
		return checkoutValidateMetadataReply(metadata, aclErr, &stat, true)
	}, nil
}

// Darwin's volume-capability interface distinguishes extended-security (ACL)
// support from ordinary POSIX permissions. Read only this capability from the
// already verified live native root; no user files or backing volumes are read.
func checkoutVolumeACLUnsupported(path string) (bool, error) {
	pathPointer, err := unix.BytePtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes := unix.Attrlist{Bitmapcount: 5, Volattr: unix.ATTR_VOL_INFO | unix.ATTR_VOL_CAPABILITIES}
	var reply [36]byte // uint32 length + capabilities[4] + valid[4].
	_, _, errno := unix.Syscall6(unix.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(pathPointer)), uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&reply[0])),
		uintptr(len(reply)), unix.FSOPT_NOFOLLOW|unix.FSOPT_REPORT_FULLSIZE, 0)
	runtime.KeepAlive(pathPointer)
	runtime.KeepAlive(&attributes)
	runtime.KeepAlive(&reply)
	if errno != 0 {
		return false, fmt.Errorf("read native volume ACL capability: %w", errno)
	}
	return checkoutParseVolumeACLUnsupported(reply[:])
}

func checkoutParseVolumeACLUnsupported(reply []byte) (bool, error) {
	if len(reply) != 36 || binary.NativeEndian.Uint32(reply[:4]) != uint32(len(reply)) {
		return false, errors.New("invalid native volume capability reply")
	}
	const extendedSecurity = uint32(0x00000400) // VOL_CAP_INT_EXTENDED_SECURITY, sys/attr.h.
	interfaces := binary.NativeEndian.Uint32(reply[8:12])
	validInterfaces := binary.NativeEndian.Uint32(reply[24:28])
	if validInterfaces&extendedSecurity == 0 {
		return false, errors.New("the native volume ACL capability is unavailable")
	}
	return interfaces&extendedSecurity == 0, nil
}

func checkoutValidateMountSnapshot(identity fsKitMountIdentity, path string, read func() ([]fsKitMountIdentity, error)) error {
	mounts, err := read()
	if err != nil {
		return errors.New("cannot verify the native checkout mount inventory")
	}
	return checkoutValidateMountInventory(identity, path, mounts)
}

func checkoutValidateMountInventory(identity fsKitMountIdentity, path string, mounts []fsKitMountIdentity) error {
	if identity.typeName != "reporeach" || identity.fsid == ([2]int32{}) || identity.owner != uint32(os.Geteuid()) || !checkoutPathWithin(identity.root, path) {
		return errors.New("the native checkout mount identity is invalid")
	}
	matching := 0
	for _, mount := range mounts {
		if mount.fsid == identity.fsid || mount.root == identity.root {
			if mount != identity {
				return errors.New("the native checkout mount was replaced or moved")
			}
			matching++
		}
		if mount.root != identity.root && checkoutPathWithin(identity.root, mount.root) && checkoutPathWithin(mount.root, path) {
			return errors.New("a nested filesystem overlaps the native checkout source")
		}
	}
	if matching != 1 {
		return errors.New("the exact native checkout mount is no longer present")
	}
	return nil
}

func checkoutPathWithin(root, path string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func publishCheckoutDirectory(parent *os.File, stage, destination string) error {
	return unix.RenameatxNp(int(parent.Fd()), stage, int(parent.Fd()), destination, unix.RENAME_EXCL)
}

func checkoutCopySymlinkMode(path string, mode os.FileMode) error {
	nativeMode := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		nativeMode |= unix.S_ISUID
	}
	if mode&os.ModeSetgid != 0 {
		nativeMode |= unix.S_ISGID
	}
	if mode&os.ModeSticky != 0 {
		nativeMode |= unix.S_ISVTX
	}
	// Darwin supports symlink permissions; Symlink creates them under the
	// process umask. Preserve the source link itself without touching its target.
	return unix.Fchmodat(unix.AT_FDCWD, path, nativeMode, unix.AT_SYMLINK_NOFOLLOW)
}

// APFS clones preserve protected native attributes that setxattr cannot copy.
// FSKit and cross-device sources fall back to the binary streaming path; their
// metadata still has to pass the same exact verification before publication.
func checkoutCloneRegular(source, destination string) (bool, error) {
	err := unix.Clonefile(source, destination, unix.CLONE_NOFOLLOW)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
			return false, errors.New("native file cloning left an unexpected destination; retained for inspection")
		}
		return false, nil
	}
	return false, err
}
