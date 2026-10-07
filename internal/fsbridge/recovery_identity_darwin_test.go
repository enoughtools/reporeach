//go:build darwin

package fsbridge

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRecoveryDurableIdentityPermitsDeviceRenumbering(t *testing.T) {
	server, identity := recoveryServer(t, true)
	stopRecoveryServer(t, server)
	// A new boot may renumber a retained APFS volume. Its persistent UUID,
	// inode, birth time, owner and mode must still identify each exact object.
	for _, file := range []*FileIdentity{&identity.Source, &identity.SocketDirectoryFile, &identity.Descriptor, &identity.Socket, &identity.Lease} {
		file.Device++
	}
	lease := requireRecoveryLease(t, identity)
	if err := lease.CleanupDetached(); err != nil {
		t.Fatal("a device renumbering prevented exact persistent object cleanup")
	}
	assertSplitBridgeAbsent(t, identity.SourceDirectory, identity.SocketPath)
	if _, err := os.Lstat(identity.LeasePath); err != nil {
		t.Fatal("durable cleanup removed the persistent lease")
	}
}

func TestRecoveryDurableIdentityRefusesBirthAndVolumeABA(t *testing.T) {
	server, original := recoveryServer(t, true)
	stopRecoveryServer(t, server)
	for _, name := range []string{"old_format", "source_birth", "sockets_birth", "descriptor_birth", "socket_birth", "lease_birth", "source_volume", "sockets_volume", "missing_birth", "missing_volume"} {
		t.Run(name, func(t *testing.T) {
			identity := original
			switch name {
			case "old_format":
				identity.Version = 1
			case "source_birth":
				identity.Source.BirthSec++
			case "sockets_birth":
				identity.SocketDirectoryFile.BirthSec++
			case "descriptor_birth":
				identity.Descriptor.BirthSec++
			case "socket_birth":
				identity.Socket.BirthSec++
			case "lease_birth":
				identity.Lease.BirthSec++
			case "source_volume":
				volume := strings.Repeat("1", 32)
				if volume == identity.Source.VolumeUUID {
					volume = strings.Repeat("2", 32)
				}
				identity.Source.VolumeUUID, identity.Descriptor.VolumeUUID, identity.Lease.VolumeUUID = volume, volume, volume
			case "sockets_volume":
				volume := strings.Repeat("1", 32)
				if volume == identity.SocketDirectoryFile.VolumeUUID {
					volume = strings.Repeat("2", 32)
				}
				identity.SocketDirectoryFile.VolumeUUID, identity.Socket.VolumeUUID = volume, volume
			case "missing_birth":
				identity.Lease.BirthSec, identity.Lease.BirthNSec = 0, 0
			case "missing_volume":
				identity.SocketDirectoryFile.VolumeUUID, identity.Socket.VolumeUUID = "", ""
			}
			sourceBefore := saveSplitBridgeDirectory(t, identity.SourceDirectory)
			socketsBefore := saveSplitBridgeDirectory(t, identity.SocketDirectory)
			requireRecoveryRefusal(t, identity)
			assertSplitBridgeDirectoryPreserved(t, identity.SourceDirectory, sourceBefore, false)
			assertSplitBridgeDirectoryPreserved(t, identity.SocketDirectory, socketsBefore, false)
		})
	}
}

func TestRecoveryDurableIdentityBindsPrivateDirectoryMode(t *testing.T) {
	server, identity := recoveryServer(t, true)
	stopRecoveryServer(t, server)
	for _, path := range []string{identity.SourceDirectory, identity.SocketDirectory} {
		if err := os.Chmod(path, 0o500); err != nil {
			t.Fatal(err)
		}
		requireRecoveryRefusal(t, identity)
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecoveryDarwinIdentityMatchingUsesPersistentBinding(t *testing.T) {
	identity := FileIdentity{Device: 12, Inode: 42, VolumeUUID: "0102030405060708090a0b0c0d0e0f10", BirthSec: 1000, BirthNSec: 1, UID: uint32(os.Getuid()), Mode: unix.S_IFREG | 0o600}
	if !recoveryFileIdentityValid(identity) {
		t.Fatal("valid durable identity was refused")
	}
	for _, name := range []string{"device", "inode", "volume", "birth_seconds", "birth_nanoseconds", "uid", "mode"} {
		t.Run(name, func(t *testing.T) {
			changed := identity
			switch name {
			case "device":
				changed.Device++
			case "inode":
				changed.Inode++
			case "volume":
				changed.VolumeUUID = "11111111111111111111111111111111"
			case "birth_seconds":
				changed.BirthSec++
			case "birth_nanoseconds":
				changed.BirthNSec++
			case "uid":
				changed.UID++
			case "mode":
				changed.Mode |= 0o100
			}
			if recoverySameIdentity(changed, identity) != (name == "device") {
				t.Fatal("durable identity matched the wrong object")
			}
		})
	}
}

func TestRecoveryIdentityKeepsSplitBackingVolumesIndependent(t *testing.T) {
	_, identity := recoveryServer(t, true)
	sourceVolume, socketVolume := strings.Repeat("1", 32), strings.Repeat("2", 32)
	identity.Source.VolumeUUID, identity.Descriptor.VolumeUUID, identity.Lease.VolumeUUID = sourceVolume, sourceVolume, sourceVolume
	identity.SocketDirectoryFile.VolumeUUID, identity.Socket.VolumeUUID = socketVolume, socketVolume
	if err := identity.Validate(identity.SourceDirectory, identity.SocketDirectory); err != nil {
		t.Fatal("valid independent source and socket backing volumes were refused")
	}
	for _, name := range []string{"descriptor", "lease", "socket"} {
		changed := identity
		switch name {
		case "descriptor":
			changed.Descriptor.VolumeUUID = socketVolume
		case "lease":
			changed.Lease.VolumeUUID = socketVolume
		case "socket":
			changed.Socket.VolumeUUID = sourceVolume
		}
		if err := changed.Validate(changed.SourceDirectory, changed.SocketDirectory); err == nil {
			t.Fatal("an endpoint inherited a different parent's backing volume")
		}
	}
}

func TestRecoveryUUIDReplyRequiresCompleteNonzeroVolumeUUID(t *testing.T) {
	valid := make([]byte, recoveryUUIDReplySize)
	binary.NativeEndian.PutUint32(valid[:4], recoveryUUIDReplySize)
	binary.NativeEndian.PutUint32(valid[4:8], unix.ATTR_CMN_RETURNED_ATTRS)
	binary.NativeEndian.PutUint32(valid[8:12], unix.ATTR_VOL_INFO|unix.ATTR_VOL_UUID)
	copy(valid[24:], bytes.Repeat([]byte{1}, 16))
	if value, err := recoveryUUIDAttributeReply(valid); err != nil || value != strings.Repeat("01", 16) {
		t.Fatal("complete volume UUID reply was refused")
	}
	for _, name := range []string{"short", "long", "reported_size", "common", "volume", "group", "zero"} {
		t.Run(name, func(t *testing.T) {
			reply := append([]byte(nil), valid...)
			switch name {
			case "short":
				reply = reply[:len(reply)-1]
			case "long":
				reply = append(reply, 0)
			case "reported_size":
				binary.NativeEndian.PutUint32(reply[:4], 1)
			case "common":
				binary.NativeEndian.PutUint32(reply[4:8], 0)
			case "volume":
				binary.NativeEndian.PutUint32(reply[8:12], unix.ATTR_VOL_INFO)
			case "group":
				binary.NativeEndian.PutUint32(reply[12:16], 1)
			case "zero":
				clear(reply[24:])
			}
			if value, err := recoveryUUIDAttributeReply(reply); err == nil || value != "" {
				t.Fatal("unproven volume UUID was accepted")
			}
		})
	}
}
