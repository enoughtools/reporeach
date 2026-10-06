//go:build darwin

package desktop

import (
	"encoding/binary"
	"encoding/hex"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNativePathACLMetadataTemporaryDirectory(t *testing.T) {
	path := t.TempDir()
	metadata, err := nativePathACLMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.HasACL {
		t.Fatal("fresh temporary directory unexpectedly has an ACL")
	}
	var directoryStat unix.Stat_t
	if err := unix.Lstat(path, &directoryStat); err != nil {
		t.Fatal(err)
	}
	if !metadata.matchesStat(&directoryStat) {
		t.Fatal("ACL query identity differs from the temporary directory")
	}
	file := filepath.Join(path, "receipt")
	fd, err := unix.Open(file, unix.O_CREAT|unix.O_WRONLY|unix.O_EXCL|unix.O_CLOEXEC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}
	fileMetadata, err := nativePathACLMetadata(file)
	if err != nil {
		t.Fatal(err)
	}
	var fileStat unix.Stat_t
	if err := unix.Lstat(file, &fileStat); err != nil {
		t.Fatal(err)
	}
	if !fileMetadata.matchesStat(&fileStat) || fileMetadata.ObjectType != 1 || fileMetadata.HasACL {
		t.Fatal("ACL query lost the regular file's identity or ACL absence")
	}
	// The query must apply to the link itself rather than follow its target.
	link := filepath.Join(t.TempDir(), "link")
	if err := unix.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	linkMetadata, err := nativePathACLMetadata(link)
	if err != nil {
		t.Fatal(err)
	}
	var linkStat unix.Stat_t
	if err := unix.Lstat(link, &linkStat); err != nil {
		t.Fatal(err)
	}
	if !linkMetadata.matchesStat(&linkStat) || linkMetadata.matchesStat(&directoryStat) || linkMetadata.ObjectType != 5 {
		t.Fatal("ACL query followed the symlink or lost its identity")
	}
}

func TestNativePathACLMetadataRejectsInvalidPaths(t *testing.T) {
	for _, path := range []string{"", "relative", "/invalid\x00path", filepath.Join(t.TempDir(), "missing")} {
		if got, err := nativePathACLMetadata(path); err == nil || got != (nativeACLMetadata{}) {
			t.Fatalf("invalid metadata path returned a usable identity: error = %v", err)
		}
	}
}

func testACLAttributeReply(count uint32) []byte {
	length := kauthFilesecHeaderSize
	if count != kauthFilesecNoACL {
		length += int(count) * kauthACLEntrySize
	}
	reply := make([]byte, aclAttributeHeaderSize+length)
	binary.NativeEndian.PutUint32(reply[:4], uint32(len(reply)))
	binary.NativeEndian.PutUint32(reply[4:8], 3)
	binary.NativeEndian.PutUint32(reply[8:12], 2)
	binary.NativeEndian.PutUint64(reply[12:20], 1_700_000_000)
	binary.NativeEndian.PutUint64(reply[20:28], 123_456_789)
	binary.NativeEndian.PutUint32(reply[28:32], 501)
	binary.NativeEndian.PutUint32(reply[32:36], unix.S_IFDIR|0700)
	binary.NativeEndian.PutUint32(reply[36:40], 16)
	binary.NativeEndian.PutUint32(reply[40:44], uint32(length))
	binary.NativeEndian.PutUint64(reply[44:52], 42)
	binary.NativeEndian.PutUint32(reply[52:56], kauthFilesecMagic)
	binary.NativeEndian.PutUint32(reply[88:92], count)
	return reply
}

func TestValidateACLAttributeReply(t *testing.T) {
	for _, count := range []uint32{kauthFilesecNoACL, 0, 1, kauthACLMaxEntries} {
		got, err := validateACLAttributeReply(testACLAttributeReply(count))
		if err != nil || got.HasACL != (count != kauthFilesecNoACL) {
			t.Fatalf("entry count = %d, ACL = %v, error = %v", count, got.HasACL, err)
		}
	}
	noACL := testACLAttributeReply(kauthFilesecNoACL)[:aclAttributeHeaderSize]
	binary.NativeEndian.PutUint32(noACL[:4], aclAttributeHeaderSize)
	binary.NativeEndian.PutUint32(noACL[40:44], 0)
	if got, err := validateACLAttributeReply(noACL); err != nil || got.HasACL {
		t.Fatalf("absent ACL = %v, error = %v", got.HasACL, err)
	}
}

func TestNativeACLMetadataMatchesStat(t *testing.T) {
	metadata, err := validateACLAttributeReply(testACLAttributeReply(kauthFilesecNoACL))
	if err != nil {
		t.Fatal(err)
	}
	stat := unix.Stat_t{Dev: 3, Ino: 42, Uid: 501, Mode: unix.S_IFDIR | 0700, Btim: unix.Timespec{Sec: 1_700_000_000, Nsec: 123_456_789}}
	if !metadata.matchesStat(&stat) || metadata.matchesStat(nil) {
		t.Fatal("ACL identity did not match the corresponding stat")
	}
	for _, mutate := range []func(*unix.Stat_t){
		func(stat *unix.Stat_t) { stat.Dev++ },
		func(stat *unix.Stat_t) { stat.Ino++ },
		func(stat *unix.Stat_t) { stat.Uid++ },
		func(stat *unix.Stat_t) { stat.Mode |= 0020 },
		func(stat *unix.Stat_t) { stat.Mode |= unix.S_ISUID },
		func(stat *unix.Stat_t) { stat.Mode = unix.S_IFREG | 0700 },
		func(stat *unix.Stat_t) { stat.Btim.Sec++ },
		func(stat *unix.Stat_t) { stat.Btim.Nsec++ },
	} {
		changed := stat
		mutate(&changed)
		if metadata.matchesStat(&changed) {
			t.Fatal("ACL identity accepted different object metadata")
		}
	}
	for _, objectType := range []uint32{1, 5} {
		reply := testACLAttributeReply(kauthFilesecNoACL)
		binary.NativeEndian.PutUint32(reply[8:12], objectType)
		mode, _ := nativeACLObjectMode(objectType)
		binary.NativeEndian.PutUint32(reply[32:36], mode|0600)
		got, err := validateACLAttributeReply(reply)
		changed := stat
		changed.Mode = uint16(mode | 0600)
		if err != nil || !got.matchesStat(&changed) {
			t.Fatalf("object type %d identity failed: %v", objectType, err)
		}
	}
}

func TestValidateACLAttributeReplyRejectsMalformed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"nil", func([]byte) []byte { return nil }},
		{"short header", func(reply []byte) []byte { return reply[:51] }},
		{"oversized allocation", func(reply []byte) []byte { return make([]byte, aclAttributeReplySize+1) }},
		{"reported length omits reference", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[:4], 4); return reply }},
		{"reported full size exceeds buffer", func(reply []byte) []byte {
			binary.NativeEndian.PutUint32(reply[:4], uint32(len(reply)+4))
			return reply
		}},
		{"negative offset", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[36:40], ^uint32(0)); return reply }},
		{"unaligned offset", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[36:40], 17); return reply }},
		{"reference overlaps header", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[36:40], 4); return reply }},
		{"reference beyond buffer", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[36:40], 0x7fffffff); return reply }},
		{"reference length exceeds buffer", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[40:44], ^uint32(0)); return reply }},
		{"empty reference with extra payload", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[40:44], 0); return reply }},
		{"short security record", func(reply []byte) []byte {
			binary.NativeEndian.PutUint32(reply[:4], 56)
			binary.NativeEndian.PutUint32(reply[40:44], 4)
			return reply[:56]
		}},
		{"bad magic", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[52:56], 0); return reply }},
		{"too many entries", func(reply []byte) []byte {
			binary.NativeEndian.PutUint32(reply[88:92], kauthACLMaxEntries+1)
			return reply
		}},
		{"entry count exceeds payload", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[88:92], 2); return reply }},
		{"absent ACL with extra entries", func(reply []byte) []byte {
			binary.NativeEndian.PutUint32(reply[88:92], kauthFilesecNoACL)
			return reply
		}},
		{"absent ACL with flags", func([]byte) []byte {
			reply := testACLAttributeReply(kauthFilesecNoACL)
			binary.NativeEndian.PutUint32(reply[92:96], 1)
			return reply
		}},
		{"unsupported vnode type", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[8:12], 3); return reply }},
		{"mode type differs from object type", func(reply []byte) []byte {
			binary.NativeEndian.PutUint32(reply[32:36], unix.S_IFREG|0700)
			return reply
		}},
		{"mode type missing", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[32:36], 0700); return reply }},
		{"unknown mode bits", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[32:36], 1<<31); return reply }},
		{"invalid birth nanoseconds", func(reply []byte) []byte { binary.NativeEndian.PutUint64(reply[20:28], 1_000_000_000); return reply }},
		{"negative birth nanoseconds", func(reply []byte) []byte { binary.NativeEndian.PutUint64(reply[20:28], ^uint64(0)); return reply }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := validateACLAttributeReply(test.mutate(testACLAttributeReply(1)))
			if err == nil || got != (nativeACLMetadata{}) {
				t.Fatalf("accepted malformed ACL reply: ACL = %v, error = %v", got.HasACL, err)
			}
		})
	}
}

func TestNativeBackingVolumeUUIDTemporaryDirectory(t *testing.T) {
	fd, err := unix.Open(t.TempDir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	first, err := nativeBackingVolumeUUID(fd)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := hex.DecodeString(first); err != nil || len(decoded) != 16 {
		t.Fatalf("invalid UUID encoding: length = %d, error = %v", len(first), err)
	}
	second, err := nativeBackingVolumeUUID(fd)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("backing volume UUID changed between consecutive queries")
	}
}

func testUUIDAttributeReply() []byte {
	reply := make([]byte, uuidAttributeReplySize)
	binary.NativeEndian.PutUint32(reply[:4], uuidAttributeReplySize)
	binary.NativeEndian.PutUint32(reply[4:8], unix.ATTR_CMN_RETURNED_ATTRS)
	binary.NativeEndian.PutUint32(reply[8:12], unix.ATTR_VOL_UUID)
	copy(reply[24:], []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10})
	return reply
}

func TestValidateUUIDAttributeReply(t *testing.T) {
	for _, infoBit := range []uint32{0, unix.ATTR_VOL_INFO} {
		reply := testUUIDAttributeReply()
		binary.NativeEndian.PutUint32(reply[8:12], unix.ATTR_VOL_UUID|infoBit)
		got, err := validateUUIDAttributeReply(reply)
		if err != nil {
			t.Fatal(err)
		}
		if got != "0123456789abcdeffedcba9876543210" {
			t.Fatalf("UUID = %q", got)
		}
	}
}

func TestValidateUUIDAttributeReplyRejectsMalformed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"nil", func([]byte) []byte { return nil }},
		{"short length", func(reply []byte) []byte { return reply[:3] }},
		{"truncated UUID", func(reply []byte) []byte { return reply[:39] }},
		{"extra byte", func(reply []byte) []byte { return append(reply, 0) }},
		{"reported truncated size", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[:4], 39); return reply }},
		{"reported full size exceeds buffer", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[:4], 44); return reply }},
		{"returned attributes missing", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[4:8], 0); return reply }},
		{"unexpected common attribute", func(reply []byte) []byte {
			binary.NativeEndian.PutUint32(reply[4:8], unix.ATTR_CMN_RETURNED_ATTRS|unix.ATTR_CMN_NAME)
			return reply
		}},
		{"UUID attribute missing", func(reply []byte) []byte {
			binary.NativeEndian.PutUint32(reply[8:12], unix.ATTR_VOL_INFO)
			return reply
		}},
		{"unexpected volume attribute", func(reply []byte) []byte {
			binary.NativeEndian.PutUint32(reply[8:12], unix.ATTR_VOL_UUID|unix.ATTR_VOL_NAME)
			return reply
		}},
		{"directory attribute", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[12:16], 1); return reply }},
		{"file attribute", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[16:20], 1); return reply }},
		{"fork attribute", func(reply []byte) []byte { binary.NativeEndian.PutUint32(reply[20:24], 1); return reply }},
		{"all-zero UUID", func(reply []byte) []byte { clear(reply[24:]); return reply }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := validateUUIDAttributeReply(test.mutate(testUUIDAttributeReply()))
			if err == nil || got != "" {
				t.Fatalf("accepted malformed reply: UUID = %q, error = %v", got, err)
			}
		})
	}
}

func TestNativeVolumeMountPath(t *testing.T) {
	for _, value := range [][]byte{nil, {}, {'/'}, {'/', 't', 'm', 'p'}, {0}, {'t', 'm', 'p', 0}} {
		if got, err := nativeVolumeMountPath(value); err == nil || got != "" {
			t.Fatalf("accepted unavailable path: %q, %v", got, err)
		}
	}
	if got, err := nativeVolumeMountPath([]byte{'/', 'V', 'o', 'l', 'u', 'm', 'e', 's', '/', 'D', 'a', 't', 'a', 0, 'x'}); err != nil || got != "/Volumes/Data" {
		t.Fatalf("mount path = %q, error = %v", got, err)
	}
}
