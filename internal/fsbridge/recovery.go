//go:build !windows

package fsbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// SessionIdentityVersion identifies the nonsecret recovery receipt format.
const SessionIdentityVersion = 2

// FileIdentity binds an object to its backing volume, inode and birth time on
// Darwin. Device is observational on Darwin because it may change after boot;
// current descriptor/path device agreement is checked independently. Other
// platforms retain the same-boot device/inode contract.
type FileIdentity struct {
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
	VolumeUUID string `json:"volume_uuid"`
	BirthSec   int64  `json:"birth_sec"`
	BirthNSec  int64  `json:"birth_nsec"`
	UID        uint32 `json:"uid"`
	Mode       uint32 `json:"mode"`
}

// SessionIdentity contains only paths and filesystem identities, never the RPC
// capability or descriptor contents. The caller must bind it to the original
// boot and independently observed kernel mount before persisting it. Normal
// detachment requires that same boot. Post-reboot stale cleanup additionally
// requires independent kernel absence and Darwin's durable object identities.
type SessionIdentity struct {
	Version             int          `json:"version"`
	SourceDirectory     string       `json:"source_directory"`
	SocketDirectory     string       `json:"socket_directory"`
	DescriptorPath      string       `json:"descriptor_path"`
	SocketPath          string       `json:"socket_path"`
	LeasePath           string       `json:"lease_path"`
	Source              FileIdentity `json:"source"`
	SocketDirectoryFile FileIdentity `json:"socket_directory_file"`
	Descriptor          FileIdentity `json:"descriptor"`
	Socket              FileIdentity `json:"socket"`
	Lease               FileIdentity `json:"lease"`
}

// Validate checks the receipt's format and exact path relationships without
// accessing the filesystem. The caller supplies canonical absolute directory
// paths; AcquireRecoveryLease additionally proves their physical identities.
func (identity SessionIdentity) Validate(sourceDir, socketDir string) error {
	return validateRecoveryIdentity(identity, sourceDir, socketDir)
}

// SessionIdentity verifies that this server still owns its original private
// directories, descriptor, socket and lease. A closed or replaced session must
// not generate a recovery receipt for a different session at the same paths.
func (s *Server) SessionIdentity() (SessionIdentity, error) {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed || s.lease == nil {
		return SessionIdentity{}, errors.New("filesystem bridge session is closed")
	}
	source, sourceFile, err := openRecoveryDirectory(s.directory)
	if err != nil {
		return SessionIdentity{}, err
	}
	defer sourceFile.Close()
	sockets, socketFile, err := openRecoveryDirectory(filepath.Dir(s.socket))
	if err != nil {
		return SessionIdentity{}, err
	}
	defer socketFile.Close()
	sourceInfo, err := sourceFile.Stat()
	if err != nil || recoveryInfoIdentity(sourceInfo) != recoveryInfoIdentity(s.sourceInfo) {
		return SessionIdentity{}, errors.New("filesystem bridge source directory was replaced")
	}
	socketInfo, err := socketFile.Stat()
	if err != nil || recoveryInfoIdentity(socketInfo) != recoveryInfoIdentity(s.socketDirInfo) {
		return SessionIdentity{}, errors.New("filesystem bridge socket directory was replaced")
	}
	leaseInfo, err := s.lease.Stat()
	if err != nil {
		return SessionIdentity{}, errors.New("filesystem bridge session lease could not be checked")
	}
	sourceVolume, err := recoveryDirectoryVolumeUUID(sourceFile)
	if err != nil {
		return SessionIdentity{}, err
	}
	socketVolume := sourceVolume
	if !os.SameFile(sourceInfo, socketInfo) {
		socketVolume, err = recoveryDirectoryVolumeUUID(socketFile)
		if err != nil {
			return SessionIdentity{}, err
		}
	}
	identity := SessionIdentity{
		Version: SessionIdentityVersion, SourceDirectory: source, SocketDirectory: sockets,
		DescriptorPath: filepath.Join(source, "connection.json"), SocketPath: filepath.Join(sockets, filepath.Base(s.socket)),
		LeasePath: filepath.Join(source, ".bridge.lock"), Source: recoveryIdentityOnVolume(recoveryInfoIdentity(sourceInfo), sourceVolume),
		SocketDirectoryFile: recoveryIdentityOnVolume(recoveryInfoIdentity(socketInfo), socketVolume), Descriptor: recoveryIdentityOnVolume(recoveryInfoIdentity(s.descriptorInfo), sourceVolume),
		Socket: recoveryIdentityOnVolume(recoveryInfoIdentity(s.socketInfo), socketVolume), Lease: recoveryIdentityOnVolume(recoveryInfoIdentity(leaseInfo), sourceVolume),
	}
	if err := validateRecoveryIdentity(identity, source, sockets); err != nil {
		return SessionIdentity{}, err
	}
	if err := checkRecoveryBinding(sourceFile, socketFile, s.lease, identity, false); err != nil {
		return SessionIdentity{}, err
	}
	return identity, nil
}

// RecoveryLease prevents a cooperating server from rotating a recorded
// session while its surviving kernel mount is detached. It owns only existing
// objects; acquiring it never creates, removes or rewrites a session path.
type RecoveryLease struct {
	mu       sync.Mutex
	source   *os.File
	sockets  *os.File
	lease    *os.File
	expected SessionIdentity
}

// AcquireRecoveryLease accepts only the original session identity. Darwin
// identities survive host reboot through their volume UUID and birth time;
// other platforms require a caller-validated same-boot receipt. Keep the lease
// until independent kernel inventory proves the original mount absent.
// Existing descriptor/socket objects must match exactly; absence is allowed
// because a previous server may already have cleaned them before exiting.
func AcquireRecoveryLease(sourceDir, socketDir string, expected SessionIdentity) (_ *RecoveryLease, result error) {
	source, sourceFile, err := openRecoveryDirectory(sourceDir)
	if err != nil {
		return nil, err
	}
	sockets, socketFile, err := openRecoveryDirectory(socketDir)
	if err != nil {
		_ = sourceFile.Close()
		return nil, err
	}
	recovery := &RecoveryLease{source: sourceFile, sockets: socketFile, expected: expected}
	defer func() {
		if result != nil {
			_ = recovery.Close()
		}
	}()
	if err := validateRecoveryIdentity(expected, source, sockets); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(sourceFile.Fd()), ".bridge.lock", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("filesystem bridge original recovery lease is unavailable or unsafe")
	}
	recovery.lease = os.NewFile(uintptr(fd), expected.LeasePath)
	if err := checkRecoveryBinding(sourceFile, socketFile, recovery.lease, expected, true); err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.New("filesystem bridge recovery lease is held or unavailable")
	}
	// Opening and locking can race with a previous owner releasing the lease.
	// Recheck the pathname and every directory/file binding under the lock.
	if err := checkRecoveryBinding(sourceFile, socketFile, recovery.lease, expected, true); err != nil {
		return nil, err
	}
	return recovery, nil
}

// CleanupDetached removes only the recorded socket and descriptor in their
// original directories. The caller MUST first independently prove that the
// captured kernel session is absent. This method cannot establish that proof.
// It never removes the persistent lease or source/socket directories. The
// exclusive lease and private directories serialize cooperating writers; do
// not use this API to race another process that deliberately ignores the lease.
func (r *RecoveryLease) CleanupDetached() (retErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lease == nil {
		return errors.New("filesystem bridge recovery lease is closed")
	}
	if err := checkRecoveryBinding(r.source, r.sockets, r.lease, r.expected, true); err != nil {
		return err
	}
	// Sync on every return after validation, including a partial cleanup that
	// discovers a changed second path. A failure preserves the caller's receipt.
	defer func() {
		syncErr := r.source.Sync()
		if r.expected.SourceDirectory != r.expected.SocketDirectory {
			syncErr = errors.Join(syncErr, r.sockets.Sync())
		}
		if syncErr != nil {
			retErr = errors.Join(retErr, errors.New("filesystem bridge detached session cleanup could not be synced"))
		}
	}()
	for _, object := range []struct {
		directory *os.File
		name      string
		identity  FileIdentity
		kind      uint32
	}{
		{r.sockets, filepath.Base(r.expected.SocketPath), r.expected.Socket, unix.S_IFSOCK},
		{r.source, "connection.json", r.expected.Descriptor, unix.S_IFREG},
	} {
		// Revalidate all bindings before each unlink, including after an
		// earlier object was removed. Directory FDs cannot follow a rename
		// into a replacement directory.
		if err := checkRecoveryBinding(r.source, r.sockets, r.lease, r.expected, true); err != nil {
			return err
		}
		present, err := checkRecoveryObject(object.directory, object.name, object.identity, object.kind, true)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if err := unix.Unlinkat(int(object.directory.Fd()), object.name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return errors.New("filesystem bridge recorded session path could not be removed")
		}
	}
	return checkRecoveryBinding(r.source, r.sockets, r.lease, r.expected, true)
}

// Close releases the recovery lease without changing any filesystem path.
func (r *RecoveryLease) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result error
	for _, file := range []*os.File{r.lease, r.sockets, r.source} {
		if file != nil {
			result = errors.Join(result, file.Close())
		}
	}
	r.lease, r.sockets, r.source = nil, nil, nil
	return result
}

func recoveryInfoIdentity(info os.FileInfo) FileIdentity {
	if info == nil {
		return FileIdentity{}
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}
	}
	birthSec, birthNSec := recoveryInfoBirthTime(info)
	return FileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), BirthSec: birthSec, BirthNSec: birthNSec, UID: stat.Uid, Mode: uint32(stat.Mode)}
}

func recoveryStatIdentity(stat *unix.Stat_t) FileIdentity {
	birthSec, birthNSec := recoveryStatBirthTime(stat)
	return FileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), BirthSec: birthSec, BirthNSec: birthNSec, UID: stat.Uid, Mode: uint32(stat.Mode)}
}

func recoveryIdentityOnVolume(identity FileIdentity, volume string) FileIdentity {
	identity.VolumeUUID = volume
	return identity
}

func openRecoveryDirectory(path string) (string, *os.File, error) {
	before, err := bridgeDirectory(path)
	if err != nil {
		return "", nil, errors.New("filesystem bridge recovery directory is unavailable or unsafe")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, errors.New("filesystem bridge recovery directory could not be resolved")
	}
	fd, err := unix.Open(canonical, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", nil, errors.New("filesystem bridge recovery directory could not be opened safely")
	}
	file := os.NewFile(uintptr(fd), canonical)
	info, err := file.Stat()
	if err != nil || !os.SameFile(before, info) {
		_ = file.Close()
		return "", nil, errors.New("filesystem bridge recovery directory changed while opening")
	}
	current, err := bridgeDirectory(canonical)
	resolved, resolveErr := filepath.EvalSymlinks(canonical)
	if err != nil || resolveErr != nil || resolved != canonical || recoveryInfoIdentity(current) != recoveryInfoIdentity(info) {
		_ = file.Close()
		return "", nil, errors.New("filesystem bridge recovery directory binding changed")
	}
	return canonical, file, nil
}

func validateRecoveryIdentity(expected SessionIdentity, source, sockets string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(sockets) || strings.ContainsRune(source, 0) || strings.ContainsRune(sockets, 0) ||
		"/"+model.CleanPath(source) != source || "/"+model.CleanPath(sockets) != sockets ||
		expected.Version != SessionIdentityVersion || expected.SourceDirectory != source || expected.SocketDirectory != sockets ||
		expected.DescriptorPath != filepath.Join(source, "connection.json") || expected.LeasePath != filepath.Join(source, ".bridge.lock") {
		return errors.New("filesystem bridge recovery identity has invalid paths or version")
	}
	digest := sha256.Sum256([]byte(source))
	canonicalSocket := filepath.Join(sockets, "b"+hex.EncodeToString(digest[:8]))
	legacySocket := filepath.Join(source, "bridge.sock")
	if expected.SocketPath != canonicalSocket && !(source == sockets && expected.SocketPath == legacySocket) {
		return errors.New("filesystem bridge recovery identity has an invalid socket path")
	}
	for _, object := range []struct {
		identity FileIdentity
		kind     uint32
	}{
		{expected.Source, unix.S_IFDIR}, {expected.SocketDirectoryFile, unix.S_IFDIR},
		{expected.Descriptor, unix.S_IFREG}, {expected.Socket, unix.S_IFSOCK}, {expected.Lease, unix.S_IFREG},
	} {
		identity := object.identity
		if !recoveryFileIdentityValid(identity) || identity.UID != uint32(os.Getuid()) || identity.Mode&unix.S_IFMT != object.kind ||
			(object.kind == unix.S_IFDIR && identity.Mode&0o077 != 0) || (object.kind != unix.S_IFDIR && identity.Mode&0o7777 != 0o600) {
			return errors.New("filesystem bridge recovery identity is incomplete")
		}
	}
	if expected.Descriptor.VolumeUUID != expected.Source.VolumeUUID || expected.Lease.VolumeUUID != expected.Source.VolumeUUID ||
		expected.Socket.VolumeUUID != expected.SocketDirectoryFile.VolumeUUID ||
		(source == sockets && !recoverySameIdentity(expected.Source, expected.SocketDirectoryFile)) {
		return errors.New("filesystem bridge recovery identity has inconsistent backing volumes")
	}
	return nil
}

func checkRecoveryDirectory(file *os.File, path string, expected FileIdentity) error {
	info, err := bridgeDirectory(path)
	if err != nil {
		return errors.New("filesystem bridge recovery directory binding changed")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return errors.New("filesystem bridge recovery directory is no longer canonical")
	}
	var opened unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &opened); err != nil || recoveryInfoIdentity(info) != recoveryStatIdentity(&opened) || opened.Mode&unix.S_IFMT != unix.S_IFDIR || opened.Mode&0o077 != 0 || int(opened.Uid) != os.Getuid() {
		return errors.New("filesystem bridge recovery directory handle changed")
	}
	volume, err := recoveryDirectoryVolumeUUID(file)
	if err != nil || !recoverySameIdentity(recoveryIdentityOnVolume(recoveryStatIdentity(&opened), volume), expected) {
		return errors.New("filesystem bridge recovery directory backing identity changed")
	}
	return nil
}

func checkRecoveryObject(directory *os.File, name string, expected FileIdentity, kind uint32, allowAbsent bool) (bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) && allowAbsent {
		return false, nil
	}
	if err != nil || uint32(stat.Mode)&unix.S_IFMT != kind || stat.Mode&0o7777 != 0o600 || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 {
		return false, errors.New("filesystem bridge recovery session path is absent, replaced or unsafe")
	}
	var parent unix.Stat_t
	volume, volumeErr := recoveryDirectoryVolumeUUID(directory)
	if unix.Fstat(int(directory.Fd()), &parent) != nil || stat.Dev != parent.Dev || volumeErr != nil ||
		!recoverySameIdentity(recoveryIdentityOnVolume(recoveryStatIdentity(&stat), volume), expected) {
		return false, errors.New("filesystem bridge recovery session backing identity changed")
	}
	return true, nil
}

func checkRecoveryBinding(source, sockets, lease *os.File, expected SessionIdentity, allowAbsent bool) error {
	if err := checkRecoveryDirectory(source, expected.SourceDirectory, expected.Source); err != nil {
		return err
	}
	if err := checkRecoveryDirectory(sockets, expected.SocketDirectory, expected.SocketDirectoryFile); err != nil {
		return err
	}
	var held unix.Stat_t
	var parent unix.Stat_t
	if err := unix.Fstat(int(lease.Fd()), &held); err != nil || unix.Fstat(int(source.Fd()), &parent) != nil || held.Dev != parent.Dev ||
		!recoverySameIdentity(recoveryIdentityOnVolume(recoveryStatIdentity(&held), expected.Source.VolumeUUID), expected.Lease) ||
		held.Mode&unix.S_IFMT != unix.S_IFREG || held.Mode&0o7777 != 0o600 || int(held.Uid) != os.Getuid() || held.Nlink != 1 {
		return errors.New("filesystem bridge original recovery lease changed")
	}
	if _, err := checkRecoveryObject(source, ".bridge.lock", expected.Lease, unix.S_IFREG, false); err != nil {
		return err
	}
	if _, err := checkRecoveryObject(source, "connection.json", expected.Descriptor, unix.S_IFREG, allowAbsent); err != nil {
		return err
	}
	if _, err := checkRecoveryObject(sockets, filepath.Base(expected.SocketPath), expected.Socket, unix.S_IFSOCK, allowAbsent); err != nil {
		return err
	}
	return nil
}
