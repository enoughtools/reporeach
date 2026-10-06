//go:build darwin

package desktop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sys/unix"
)

const nativeRootReceiptVersion = 1

var errMountRootNotEmpty = errors.New("choose an empty folder so RepoReach does not hide existing files")
var errNativeRootChanged = errors.New("the repository folder changed while its backing directory was checked")

// The volume UUID, inode and birth time identify an object across host boots.
// Device numbers are deliberately not persisted: Darwin can assign a different
// device number to the same APFS volume after restarting.
type nativeRootObjectIdentity struct {
	VolumeUUID string `json:"volumeUUID"`
	Inode      uint64 `json:"inode"`
	BirthSec   int64  `json:"birthSec"`
	BirthNSec  int64  `json:"birthNSec"`
	UID        uint32 `json:"uid"`
}

type nativeMountRootReceipt struct {
	Version             int                       `json:"version"`
	Root                string                    `json:"root"`
	Identity            nativeRootObjectIdentity  `json:"identity"`
	PreparedEmpty       bool                      `json:"preparedEmpty"`
	VerifiedNativeMount bool                      `json:"verifiedNativeMount"`
	SystemDirectory     *nativeRootObjectIdentity `json:"systemDirectory,omitempty"`
	wasEmpty            bool
}

func nativeRootIdentity(stat *unix.Stat_t, volumeUUID string) nativeRootObjectIdentity {
	return nativeRootObjectIdentity{VolumeUUID: volumeUUID, Inode: stat.Ino,
		BirthSec: stat.Btim.Sec, BirthNSec: stat.Btim.Nsec, UID: stat.Uid}
}

func (identity nativeRootObjectIdentity) valid() bool {
	if len(identity.VolumeUUID) != 32 || strings.ToLower(identity.VolumeUUID) != identity.VolumeUUID || identity.Inode == 0 ||
		(identity.BirthSec == 0 && identity.BirthNSec == 0) || identity.BirthNSec < 0 || identity.BirthNSec >= 1e9 {
		return false
	}
	uuid, err := hex.DecodeString(identity.VolumeUUID)
	if err != nil {
		return false
	}
	for _, b := range uuid {
		if b != 0 {
			return true
		}
	}
	return false
}

func nativeRootReceiptPath(stateDir, root string) string {
	digest := sha256.Sum256([]byte(root))
	return filepath.Join(stateDir, "native-mount-roots", hex.EncodeToString(digest[:])+".json")
}

func readNativeRootReceipt(path string) (*nativeMountRootReceipt, error) {
	if _, err := os.Lstat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err := nativePrivateReceiptDirectory(filepath.Dir(filepath.Dir(path)), false); err != nil {
		return nil, errors.New("the repository folder's private mount history is unsafe")
	}
	if err := nativePrivateReceiptDirectory(filepath.Dir(path), false); err != nil {
		return nil, errors.New("the repository folder's private mount history is unsafe")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("the repository folder's private mount history could not be read")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o600 || stat.Nlink != 1 || stat.Size > 8<<10 {
		return nil, errors.New("the repository folder's private mount history is unsafe")
	}
	if !nativeObjectACLFree(path, &stat) {
		return nil, errors.New("the repository folder's private mount history is unsafe")
	}
	decoder := json.NewDecoder(io.LimitReader(f, 8<<10))
	decoder.DisallowUnknownFields()
	var receipt nativeMountRootReceipt
	if err := decoder.Decode(&receipt); err != nil || decoder.Decode(new(any)) != io.EOF ||
		receipt.Version != nativeRootReceiptVersion || !filepath.IsAbs(receipt.Root) || "/"+model.CleanPath(receipt.Root) != receipt.Root ||
		!receipt.Identity.valid() || !receipt.PreparedEmpty || (receipt.SystemDirectory != nil && (!receipt.VerifiedNativeMount || !receipt.SystemDirectory.valid() || receipt.SystemDirectory.UID != 0 || receipt.SystemDirectory.VolumeUUID != receipt.Identity.VolumeUUID)) {
		return nil, errors.New("the repository folder's private mount history is invalid")
	}
	return &receipt, nil
}

func writeNativeRootReceipt(path string, receipt nativeMountRootReceipt) error {
	if err := nativePrivateReceiptDirectory(filepath.Dir(filepath.Dir(path)), false); err != nil {
		return errors.New("the repository folder's private mount history is unavailable")
	}
	if err := nativePrivateReceiptDirectory(filepath.Dir(path), true); err != nil {
		return errors.New("the repository folder's private mount history is unavailable")
	}
	// Validate an existing destination rather than replacing unknown private
	// files, symlinks or a record produced by a different root.
	previous, err := readNativeRootReceipt(path)
	if err != nil || (previous != nil && previous.Root != receipt.Root) {
		return errors.New("the repository folder's private mount history could not be safely updated")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".mount-root-*.tmp")
	if err != nil {
		return errors.New("the repository folder's private mount history could not be saved")
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	var temporaryStat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &temporaryStat); err != nil || !nativeObjectACLFree(name, &temporaryStat) {
		return errors.New("the repository folder's private mount history is unsafe")
	}
	if err := json.NewEncoder(f).Encode(receipt); err != nil {
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
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func nativeObjectACLFree(path string, stat *unix.Stat_t) bool {
	metadata, err := nativePathACLMetadata(path)
	return err == nil && !metadata.HasACL && metadata.matchesStat(stat)
}

func nativePrivateReceiptDirectory(path string, create bool) error {
	if err := privateDirectory(path, create); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil || !nativeObjectACLFree(path, &stat) {
		return errors.New("private mount history directory must have no additional access grants")
	}
	return nil
}

// checkMountDirectory leaves a genuinely empty directory usable even when its
// underlying filesystem cannot provide a persistent volume UUID. That missing
// identity can never authorize covering an existing entry.
func (s *Service) checkMountDirectory(root string) error {
	_, err := s.inspectNativeMountRoot(root, false)
	return err
}

func (s *Service) prepareNativeMountRoot(root string) (*nativeMountRootReceipt, error) {
	return s.inspectNativeMountRoot(root, true)
}

func (s *Service) inspectNativeMountRoot(root string, prepare bool) (*nativeMountRootReceipt, error) {
	// fseventsd can withdraw a newly created directory between enumeration and
	// Lstat. Restart the entire safety check; never authorize a missing object.
	for attempt := 0; attempt < 3; attempt++ {
		receipt, err := s.inspectNativeMountRootOnce(root, prepare)
		if !errors.Is(err, errNativeRootChanged) {
			return receipt, err
		}
	}
	return nil, errNativeRootChanged
}

func (s *Service) inspectNativeMountRootOnce(root string, prepare bool) (*nativeMountRootReceipt, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, err
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("mount folder must be a real directory")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	mounts, err := cachedDarwinMounts()
	if err != nil {
		return nil, errors.New("the repository folder's mount state could not be checked")
	}
	if _, mounted := mountAtRoot(mounts, canonical); mounted {
		return nil, errors.New("the repository folder is already a mounted filesystem")
	}
	fd, err := unix.Open(canonical, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), canonical)
	defer f.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 && !prepare {
		return nil, nil
	}
	if len(entries) != 0 && (len(entries) != 1 || entries[0].Name() != ".fseventsd") {
		return nil, errMountRootNotEmpty
	}
	volumeUUID, identityErr := nativeBackingVolumeUUID(fd)
	identity := nativeRootIdentity(&stat, volumeUUID)
	if identityErr != nil || !identity.valid() || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o022 != 0 ||
		!nativeObjectACLFree(canonical, &stat) || nativePrivateReceiptDirectory(s.opts.StateDir, false) != nil {
		if len(entries) == 0 {
			return nil, nil // Empty-root behavior does not depend on this feature.
		}
		return nil, errMountRootNotEmpty
	}
	path := nativeRootReceiptPath(s.opts.StateDir, canonical)
	receipt, err := readNativeRootReceipt(path)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		if receipt == nil || receipt.Root != canonical || receipt.Identity != identity {
			receipt = &nativeMountRootReceipt{Version: nativeRootReceiptVersion, Root: canonical, Identity: identity, PreparedEmpty: true}
		}
		if err := writeNativeRootReceipt(path, *receipt); err != nil {
			return nil, err
		}
		receipt.wasEmpty = true
		return receipt, nil
	}
	if receipt == nil || receipt.Root != canonical || receipt.Identity != identity || !receipt.PreparedEmpty || !receipt.VerifiedNativeMount {
		return nil, errMountRootNotEmpty
	}
	var systemStat unix.Stat_t
	if err := unix.Fstatat(fd, ".fseventsd", &systemStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, errNativeRootChanged
		}
		return nil, errMountRootNotEmpty
	}
	security, aclErr := nativePathACLMetadata(filepath.Join(canonical, ".fseventsd"))
	if aclErr == nil && !security.matchesStat(&systemStat) {
		return nil, errNativeRootChanged
	}
	if err := validateNativeSystemDirectory(receipt, &stat, &systemStat, security.HasACL, aclErr); err != nil {
		return nil, err
	}
	var after unix.Stat_t
	if err := unix.Fstatat(fd, ".fseventsd", &after, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, errNativeRootChanged
		}
		return nil, errMountRootNotEmpty
	}
	if nativeRootIdentity(&after, volumeUUID) != nativeRootIdentity(&systemStat, volumeUUID) || after.Mode != systemStat.Mode || after.Dev != systemStat.Dev {
		return nil, errNativeRootChanged
	}
	if receipt.SystemDirectory == nil {
		bound := nativeRootIdentity(&systemStat, volumeUUID)
		receipt.SystemDirectory = &bound
		if err := writeNativeRootReceipt(path, *receipt); err != nil {
			return nil, err
		}
	}
	return receipt, nil
}

func validateNativeSystemDirectory(receipt *nativeMountRootReceipt, root, system *unix.Stat_t, hasACL bool, aclErr error) error {
	identity := nativeRootIdentity(system, receipt.Identity.VolumeUUID)
	if receipt.Identity != nativeRootIdentity(root, receipt.Identity.VolumeUUID) || !receipt.PreparedEmpty || !receipt.VerifiedNativeMount || !identity.valid() || system.Mode&(unix.S_IFMT|0o7777) != unix.S_IFDIR|0o700 ||
		system.Uid != 0 || system.Dev != root.Dev || hasACL || aclErr != nil ||
		(receipt.SystemDirectory != nil && *receipt.SystemDirectory != identity) {
		return errMountRootNotEmpty
	}
	return nil
}

func (s *Service) recheckNativeMountRoot(root string, prepared *nativeMountRootReceipt) error {
	inspected, err := s.prepareNativeMountRoot(root)
	if err != nil {
		return err
	}
	if prepared == nil {
		return nil
	}
	if inspected == nil || inspected.Root != prepared.Root || inspected.Identity != prepared.Identity {
		return errNativeRootChanged
	}
	// A first system directory may legitimately appear during bridge startup.
	// Verify the exact freshly inspected binding, rather than the older snapshot.
	*prepared = *inspected
	return nil
}

func verifyNativeMountRoot(stateDir string, prepared *nativeMountRootReceipt) error {
	if prepared == nil {
		return nil // The empty backing filesystem did not support attestation.
	}
	path := nativeRootReceiptPath(stateDir, prepared.Root)
	receipt, err := readNativeRootReceipt(path)
	if err != nil || receipt == nil || receipt.Root != prepared.Root || receipt.Identity != prepared.Identity || !receipt.PreparedEmpty ||
		(receipt.SystemDirectory == nil) != (prepared.SystemDirectory == nil) ||
		(receipt.SystemDirectory != nil && *receipt.SystemDirectory != *prepared.SystemDirectory) {
		return errors.New("the repository folder's mount history changed during native setup")
	}
	receipt.VerifiedNativeMount = true
	if prepared.wasEmpty {
		// Only a successful mount after a final, independently empty backing-root
		// inspection retires the identity of an OS directory that disappeared.
		// Preparing or failing to mount that empty root never resets its binding.
		receipt.SystemDirectory = nil
	}
	return writeNativeRootReceipt(path, *receipt)
}
