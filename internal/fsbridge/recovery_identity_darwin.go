//go:build darwin

package fsbridge

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func recoveryInfoBirthTime(info os.FileInfo) (int64, int64) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return stat.Birthtimespec.Sec, stat.Birthtimespec.Nsec
}

func recoveryStatBirthTime(stat *unix.Stat_t) (int64, int64) { return stat.Btim.Sec, stat.Btim.Nsec }

func recoveryFileIdentityValid(identity FileIdentity) bool {
	if identity.Inode == 0 || (identity.BirthSec == 0 && identity.BirthNSec == 0) || identity.BirthNSec < 0 || identity.BirthNSec >= 1e9 ||
		len(identity.VolumeUUID) != 32 || strings.ToLower(identity.VolumeUUID) != identity.VolumeUUID {
		return false
	}
	uuid, err := hex.DecodeString(identity.VolumeUUID)
	return err == nil && !bytes.Equal(uuid, make([]byte, 16))
}

func recoverySameIdentity(actual, expected FileIdentity) bool {
	// Darwin can renumber the same backing volume's device after restarting.
	// Current directory/endpoint device agreement is checked separately.
	actual.Device = expected.Device
	return actual == expected
}

const recoveryUUIDReplySize = 4 + 5*4 + 16

// recoveryDirectoryVolumeUUID queries only the pinned ordinary bridge parent
// and its backing volume root. It never opens the mounted catalogue. Each
// query binds the public volume UUID to the current directory and root FSIDs.
func recoveryDirectoryVolumeUUID(directory *os.File) (string, error) {
	var stat unix.Stat_t
	var filesystem unix.Statfs_t
	if unix.Fstat(int(directory.Fd()), &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		unix.Fstatfs(int(directory.Fd()), &filesystem) != nil || filesystem.Fsid.Val == [2]int32{} {
		return "", errors.New("filesystem bridge backing directory identity is unavailable")
	}
	kind := filesystem.Fstypename[:]
	if end := bytes.IndexByte(kind, 0); end >= 0 {
		kind = kind[:end]
	}
	if string(kind) == "reporeach" {
		return "", errors.New("filesystem bridge source cannot belong to its virtual filesystem")
	}
	name := filesystem.Mntonname[:]
	end := bytes.IndexByte(name, 0)
	if end <= 0 || name[0] != '/' {
		return "", errors.New("filesystem bridge backing volume root is unavailable")
	}
	fd, err := unix.Open(string(name[:end]), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("filesystem bridge backing volume root could not be opened")
	}
	defer unix.Close(fd)
	var root unix.Stat_t
	var rootFS unix.Statfs_t
	if unix.Fstat(fd, &root) != nil || unix.Fstatfs(fd, &rootFS) != nil || root.Mode&unix.S_IFMT != unix.S_IFDIR ||
		root.Dev != stat.Dev || rootFS.Fsid != filesystem.Fsid {
		return "", errors.New("filesystem bridge backing volume root identity changed")
	}
	attr := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_RETURNED_ATTRS, Volattr: unix.ATTR_VOL_INFO | unix.ATTR_VOL_UUID}
	var reply [recoveryUUIDReplySize]byte
	_, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&attr)),
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), unix.FSOPT_REPORT_FULLSIZE, 0)
	runtime.KeepAlive(&attr)
	runtime.KeepAlive(&reply)
	if errno != 0 {
		return "", errors.New("filesystem bridge backing volume UUID could not be read")
	}
	uuid, err := recoveryUUIDAttributeReply(reply[:])
	if err != nil {
		return "", err
	}
	var after, rootAfter unix.Statfs_t
	if unix.Fstatfs(int(directory.Fd()), &after) != nil || unix.Fstatfs(fd, &rootAfter) != nil ||
		after.Fsid != filesystem.Fsid || rootAfter.Fsid != filesystem.Fsid {
		return "", errors.New("filesystem bridge backing volume changed during UUID query")
	}
	return uuid, nil
}

func recoveryUUIDAttributeReply(reply []byte) (string, error) {
	if len(reply) != recoveryUUIDReplySize || binary.NativeEndian.Uint32(reply[:4]) != recoveryUUIDReplySize ||
		binary.NativeEndian.Uint32(reply[4:8]) != unix.ATTR_CMN_RETURNED_ATTRS {
		return "", errors.New("filesystem bridge backing UUID reply is invalid")
	}
	volume := binary.NativeEndian.Uint32(reply[8:12])
	if volume&unix.ATTR_VOL_UUID == 0 || volume&^(uint32(unix.ATTR_VOL_INFO)|uint32(unix.ATTR_VOL_UUID)) != 0 {
		return "", errors.New("filesystem bridge backing UUID attributes are invalid")
	}
	for offset := 12; offset < 24; offset += 4 {
		if binary.NativeEndian.Uint32(reply[offset:offset+4]) != 0 {
			return "", errors.New("filesystem bridge backing UUID group is invalid")
		}
	}
	if bytes.Equal(reply[24:], make([]byte, 16)) {
		return "", errors.New("filesystem bridge backing volume UUID is unavailable")
	}
	return hex.EncodeToString(reply[24:]), nil
}
