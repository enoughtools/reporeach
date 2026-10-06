//go:build darwin

package desktop

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const uuidAttributeReplySize = 4 + 5*4 + 16

const (
	aclAttributeHeaderSize = 4 + 4 + 4 + 16 + 4 + 4 + 8 + 8
	kauthFilesecHeaderSize = 4 + 16 + 16 + 4 + 4
	kauthACLEntrySize      = 16 + 4 + 4
	kauthACLMaxEntries     = 128
	kauthFilesecMagic      = 0x012cc16d
	kauthFilesecNoACL      = ^uint32(0)
	aclAttributeReplySize  = aclAttributeHeaderSize + kauthFilesecHeaderSize + kauthACLMaxEntries*kauthACLEntrySize
)

type nativeACLMetadata struct {
	Dev        int32
	Ino        uint64
	Birthtime  unix.Timespec
	UID        uint32
	Mode       uint32
	ObjectType uint32
	HasACL     bool
}

func (m nativeACLMetadata) matchesStat(stat *unix.Stat_t) bool {
	modeType, ok := nativeACLObjectMode(m.ObjectType)
	return stat != nil && ok &&
		m.Dev == stat.Dev && m.Ino == stat.Ino && m.Birthtime == stat.Btim &&
		m.UID == stat.Uid && m.Mode&07777 == uint32(stat.Mode)&07777 &&
		m.Mode&unix.S_IFMT == modeType && uint32(stat.Mode)&unix.S_IFMT == modeType
}

func nativeACLObjectMode(objectType uint32) (uint32, bool) {
	// Darwin's enum vtype uses VREG=1, VDIR=2 and VLNK=5. Refuse vnode
	// types outside the regular-file, directory and symlink cases we attest.
	switch objectType {
	case 1:
		return unix.S_IFREG, true
	case 2:
		return unix.S_IFDIR, true
	case 5:
		return unix.S_IFLNK, true
	default:
		return 0, false
	}
}

// nativeBackingVolumeUUID identifies the filesystem containing fd. The caller
// owns fd and must already have verified that it refers to a healthy, unmounted
// backing directory. A failure makes a persistent native-root exception
// unavailable; it must not prevent mounting an otherwise empty directory.
func nativeBackingVolumeUUID(fd int) (string, error) {
	var backingStat unix.Stat_t
	if err := unix.Fstat(fd, &backingStat); err != nil {
		return "", fmt.Errorf("inspect backing directory: %w", err)
	}
	if backingStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", errors.New("backing descriptor is not a directory")
	}
	var backingFS unix.Statfs_t
	if err := unix.Fstatfs(fd, &backingFS); err != nil {
		return "", fmt.Errorf("inspect backing filesystem: %w", err)
	}
	if backingFS.Fsid.Val == [2]int32{} {
		return "", errors.New("backing filesystem identity is unavailable")
	}
	rootPath, err := nativeVolumeMountPath(backingFS.Mntonname[:])
	if err != nil {
		return "", err
	}

	// Volume attributes require the volume root, which can differ from fd's
	// directory. Resolve the mount path without following its final component,
	// then verify the opened root belongs to the same filesystem before querying.
	rootFD, err := unix.Open(rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open backing volume root: %w", err)
	}
	defer unix.Close(rootFD)
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil {
		return "", fmt.Errorf("inspect backing volume root: %w", err)
	}
	var rootFS unix.Statfs_t
	if err := unix.Fstatfs(rootFD, &rootFS); err != nil {
		return "", fmt.Errorf("inspect opened backing filesystem: %w", err)
	}
	if rootStat.Mode&unix.S_IFMT != unix.S_IFDIR || rootStat.Dev != backingStat.Dev || rootFS.Fsid != backingFS.Fsid {
		return "", errors.New("backing volume root identity changed")
	}

	attr := unix.Attrlist{
		Bitmapcount: 5,
		Commonattr:  unix.ATTR_CMN_RETURNED_ATTRS,
		Volattr:     unix.ATTR_VOL_INFO | unix.ATTR_VOL_UUID,
	}
	var reply [uuidAttributeReplySize]byte
	_, _, errno := unix.Syscall6(
		unix.SYS_FGETATTRLIST,
		uintptr(rootFD),
		uintptr(unsafe.Pointer(&attr)),
		uintptr(unsafe.Pointer(&reply[0])),
		uintptr(len(reply)),
		unix.FSOPT_REPORT_FULLSIZE,
		0,
	)
	// Keep the request and output alive across the raw syscall. Pointer-to-
	// uintptr conversions stay in the call expression so the compiler can pin
	// their storage for the syscall on both supported Darwin architectures.
	runtime.KeepAlive(&attr)
	runtime.KeepAlive(&reply)
	if errno != 0 {
		return "", fmt.Errorf("read backing volume UUID: %w", errno)
	}
	uuid, err := validateUUIDAttributeReply(reply[:])
	if err != nil {
		return "", err
	}
	var backingAfter, rootAfter unix.Statfs_t
	if err := unix.Fstatfs(fd, &backingAfter); err != nil {
		return "", fmt.Errorf("recheck backing filesystem: %w", err)
	}
	if err := unix.Fstatfs(rootFD, &rootAfter); err != nil {
		return "", fmt.Errorf("recheck backing volume root: %w", err)
	}
	if backingAfter.Fsid != backingFS.Fsid || rootAfter.Fsid != backingFS.Fsid {
		return "", errors.New("backing filesystem identity changed during UUID query")
	}
	return uuid, nil
}

func nativeVolumeMountPath(value []byte) (string, error) {
	end := bytes.IndexByte(value, 0)
	if end <= 0 || value[0] != '/' {
		return "", errors.New("backing volume mount path is unavailable")
	}
	return string(value[:end]), nil
}

func validateUUIDAttributeReply(reply []byte) (string, error) {
	if len(reply) != uuidAttributeReplySize || binary.NativeEndian.Uint32(reply[:4]) != uuidAttributeReplySize {
		return "", errors.New("unexpected backing volume UUID reply size")
	}
	common := binary.NativeEndian.Uint32(reply[4:8])
	volume := binary.NativeEndian.Uint32(reply[8:12])
	if common != unix.ATTR_CMN_RETURNED_ATTRS || volume&unix.ATTR_VOL_UUID == 0 || volume&^(uint32(unix.ATTR_VOL_INFO)|uint32(unix.ATTR_VOL_UUID)) != 0 {
		return "", errors.New("unexpected backing volume UUID attributes")
	}
	for offset := 12; offset < 24; offset += 4 {
		if binary.NativeEndian.Uint32(reply[offset:offset+4]) != 0 {
			return "", errors.New("unexpected backing volume UUID attribute group")
		}
	}
	var zeroUUID [16]byte
	if bytes.Equal(reply[24:], zeroUUID[:]) {
		return "", errors.New("backing volume UUID is unavailable")
	}
	return hex.EncodeToString(reply[24:]), nil
}

// nativePathACLMetadata reads identity and security metadata from the same
// no-follow vnode lookup without opening or enumerating path. The caller must
// match the returned identity to its separately retained object descriptor or
// Fstatat result. Any error leaves ACL absence unproven.
func nativePathACLMetadata(path string) (nativeACLMetadata, error) {
	if path == "" || path[0] != '/' {
		return nativeACLMetadata{}, errors.New("ACL metadata path must be absolute")
	}
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return nativeACLMetadata{}, fmt.Errorf("prepare ACL metadata path: %w", err)
	}
	// Request this attribute as mandatory. With RETURNED_ATTRS, XNU omits the
	// bit for both unsupported ACLs and supported directories without an ACL.
	// Without it, unsupported attributes fail and a zero-length reference is
	// the native representation of supported ACL absence.
	attr := unix.Attrlist{
		Bitmapcount: 5,
		Commonattr: unix.ATTR_CMN_DEVID | unix.ATTR_CMN_OBJTYPE | unix.ATTR_CMN_CRTIME |
			unix.ATTR_CMN_OWNERID | unix.ATTR_CMN_ACCESSMASK | unix.ATTR_CMN_EXTENDED_SECURITY | unix.ATTR_CMN_FILEID,
	}
	var reply [aclAttributeReplySize]byte
	_, _, errno := unix.Syscall6(
		unix.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&attr)),
		uintptr(unsafe.Pointer(&reply[0])),
		uintptr(len(reply)),
		unix.FSOPT_NOFOLLOW|unix.FSOPT_REPORT_FULLSIZE,
		0,
	)
	runtime.KeepAlive(pathPtr)
	runtime.KeepAlive(&attr)
	runtime.KeepAlive(&reply)
	if errno != 0 {
		return nativeACLMetadata{}, fmt.Errorf("read path ACL metadata: %w", errno)
	}
	return validateACLAttributeReply(reply[:])
}

func validateACLAttributeReply(reply []byte) (nativeACLMetadata, error) {
	if len(reply) < aclAttributeHeaderSize || len(reply) > aclAttributeReplySize {
		return nativeACLMetadata{}, errors.New("unexpected path ACL reply size")
	}
	reported := binary.NativeEndian.Uint32(reply[:4])
	if reported < aclAttributeHeaderSize || uint64(reported) > uint64(len(reply)) {
		return nativeACLMetadata{}, errors.New("truncated path ACL reply")
	}
	metadata := nativeACLMetadata{
		Dev:        int32(binary.NativeEndian.Uint32(reply[4:8])),
		ObjectType: binary.NativeEndian.Uint32(reply[8:12]),
		Birthtime: unix.Timespec{
			Sec:  int64(binary.NativeEndian.Uint64(reply[12:20])),
			Nsec: int64(binary.NativeEndian.Uint64(reply[20:28])),
		},
		UID:  binary.NativeEndian.Uint32(reply[28:32]),
		Mode: binary.NativeEndian.Uint32(reply[32:36]),
		Ino:  binary.NativeEndian.Uint64(reply[44:52]),
	}
	modeType, supportedType := nativeACLObjectMode(metadata.ObjectType)
	if !supportedType || metadata.Birthtime.Nsec < 0 || metadata.Birthtime.Nsec >= 1_000_000_000 ||
		metadata.Mode&^(uint32(unix.S_IFMT)|07777) != 0 ||
		metadata.Mode&unix.S_IFMT != modeType {
		return nativeACLMetadata{}, errors.New("invalid path ACL object identity")
	}
	// attr_dataoffset is signed and relative to the reference at byte 36.
	// FILEID follows it, so the ACL payload starts 16 bytes after the reference.
	offset := int32(binary.NativeEndian.Uint32(reply[36:40]))
	length := binary.NativeEndian.Uint32(reply[40:44])
	if offset != 16 || uint64(aclAttributeHeaderSize)+uint64(length) != uint64(reported) {
		return nativeACLMetadata{}, errors.New("invalid path ACL attribute reference")
	}
	if length == 0 {
		return metadata, nil
	}
	if length < kauthFilesecHeaderSize {
		return nativeACLMetadata{}, errors.New("truncated path ACL security record")
	}
	payload := reply[aclAttributeHeaderSize:reported]
	if binary.NativeEndian.Uint32(payload[:4]) != kauthFilesecMagic {
		return nativeACLMetadata{}, errors.New("invalid path ACL security record")
	}
	count := binary.NativeEndian.Uint32(payload[36:40])
	flags := binary.NativeEndian.Uint32(payload[40:44])
	if count == kauthFilesecNoACL {
		if length != kauthFilesecHeaderSize || flags != 0 {
			return nativeACLMetadata{}, errors.New("invalid absent path ACL security record")
		}
		return metadata, nil
	}
	if count > kauthACLMaxEntries || uint64(kauthFilesecHeaderSize)+uint64(count)*kauthACLEntrySize != uint64(length) {
		return nativeACLMetadata{}, errors.New("invalid path ACL entry count")
	}
	// Even an ACL with zero entries is present. The exception policy rejects
	// all ACLs, so this helper need not interpret individual access entries.
	metadata.HasACL = true
	return metadata, nil
}
