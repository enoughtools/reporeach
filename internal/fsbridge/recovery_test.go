//go:build !windows

package fsbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recoveryServer leaves session objects intact when stopped, modeling the
// ownership information available after a bridge process has exited.
func recoveryServer(t *testing.T, split bool) (*Server, SessionIdentity) {
	t.Helper()
	source := bridgeSourceDirectory(t)
	sockets := source
	var server *Server
	var err error
	if split {
		sockets = bridgeSourceDirectory(t)
		server, err = StartWithSocketDirectory(context.Background(), source, sockets, &protocolFilesystem{})
	} else {
		server, err = Start(context.Background(), source, &protocolFilesystem{})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopRecoveryServer(t, server) })
	identity, err := server.SessionIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return server, identity
}

func stopRecoveryServer(t *testing.T, server *Server) {
	t.Helper()
	_ = server.http.Close()
	select {
	case <-server.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("test bridge did not stop")
	}
	server.closeMu.Lock()
	defer server.closeMu.Unlock()
	if server.lease != nil {
		if err := server.lease.Close(); err != nil {
			t.Fatal(err)
		}
		server.lease = nil
	}
}

func requireRecoveryLease(t *testing.T, identity SessionIdentity) *RecoveryLease {
	t.Helper()
	lease, err := AcquireRecoveryLease(identity.SourceDirectory, identity.SocketDirectory, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	return lease
}

func requireRecoveryRefusal(t *testing.T, identity SessionIdentity) {
	t.Helper()
	lease, err := AcquireRecoveryLease(identity.SourceDirectory, identity.SocketDirectory, identity)
	if lease != nil {
		_ = lease.Close()
	}
	if err == nil || lease != nil {
		t.Fatal("recovery accepted an active, changed or unsafe session")
	}
}

func TestSessionIdentityCanonicalNonsecretMetadata(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_directory", true: "split_directory"}[split], func(t *testing.T) {
			server, identity := recoveryServer(t, split)
			if err := identity.Validate(identity.SourceDirectory, identity.SocketDirectory); err != nil {
				t.Fatal(err)
			}
			for path, expected := range map[string]FileIdentity{
				identity.SourceDirectory: identity.Source, identity.SocketDirectory: identity.SocketDirectoryFile,
				identity.SocketPath: identity.Socket, identity.DescriptorPath: identity.Descriptor, identity.LeasePath: identity.Lease,
			} {
				info, err := os.Lstat(path)
				if err != nil || recoveryIdentityOnVolume(recoveryInfoIdentity(info), expected.VolumeUUID) != expected {
					t.Fatal("receipt lost an original filesystem identity")
				}
			}
			encoded, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(identity.DescriptorPath)
			if err != nil {
				t.Fatal(err)
			}
			var descriptor Descriptor
			if json.Unmarshal(data, &descriptor) != nil || descriptor.Token == "" {
				t.Fatal("test descriptor is invalid")
			}
			if bytes.Contains(encoded, []byte(descriptor.Token)) || strings.Contains(strings.ToLower(string(encoded)), "token") {
				t.Fatal("session identity exposed the filesystem capability")
			}
			var roundtrip SessionIdentity
			if json.Unmarshal(encoded, &roundtrip) != nil || roundtrip != identity {
				t.Fatal("session identity did not survive JSON persistence")
			}
			bridgeDrain(t, server)
			if got, err := server.SessionIdentity(); err == nil || got != (SessionIdentity{}) {
				t.Fatal("closed server generated a new receipt")
			}
		})
	}
}

func TestSessionIdentityCanonicalAncestorAlias(t *testing.T) {
	root := bridgeSourceDirectory(t)
	alias := root + "-alias"
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })
	for _, name := range []string{"source", "sockets"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	server, err := StartWithSocketDirectory(context.Background(), filepath.Join(alias, "source"), filepath.Join(alias, "sockets"), &protocolFilesystem{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopRecoveryServer(t, server) })
	identity, err := server.SessionIdentity()
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil || identity.SourceDirectory != filepath.Join(canonicalRoot, "source") || identity.SocketDirectory != filepath.Join(canonicalRoot, "sockets") {
		t.Fatal("receipt retained an ancestor alias")
	}
	stopRecoveryServer(t, server)
	lease, err := AcquireRecoveryLease(filepath.Join(alias, "source"), filepath.Join(alias, "sockets"), identity)
	if err != nil {
		t.Fatal("canonical ancestry did not match the original directory identities")
	}
	defer lease.Close()
	if err := lease.CleanupDetached(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionIdentityValidateRejectsUnboundPaths(t *testing.T) {
	_, identity := recoveryServer(t, true)
	for _, name := range []string{"version", "source", "sockets", "descriptor", "socket", "lease", "source_inode", "sockets_inode", "descriptor_inode", "socket_inode", "lease_inode"} {
		t.Run(name, func(t *testing.T) {
			changed := identity
			switch name {
			case "version":
				changed.Version++
			case "source":
				changed.SourceDirectory += "/."
			case "sockets":
				changed.SocketDirectory += "/."
			case "descriptor":
				changed.DescriptorPath = identity.LeasePath
			case "socket":
				changed.SocketPath = filepath.Join(identity.SocketDirectory, "other.sock")
			case "lease":
				changed.LeasePath = identity.DescriptorPath
			case "source_inode":
				changed.Source.Inode = 0
			case "sockets_inode":
				changed.SocketDirectoryFile.Inode = 0
			case "descriptor_inode":
				changed.Descriptor.Inode = 0
			case "socket_inode":
				changed.Socket.Inode = 0
			case "lease_inode":
				changed.Lease.Inode = 0
			}
			if err := changed.Validate(identity.SourceDirectory, identity.SocketDirectory); err == nil {
				t.Fatal("invalid receipt passed pure validation")
			}
		})
	}
	for _, paths := range [][2]string{{".", identity.SocketDirectory}, {identity.SourceDirectory + "/.", identity.SocketDirectory}, {identity.SourceDirectory, identity.SocketDirectory + "/."}, {identity.SourceDirectory + "\x00", identity.SocketDirectory}, {identity.SourceDirectory, identity.SocketDirectory + "\x00"}} {
		if err := identity.Validate(paths[0], paths[1]); err == nil {
			t.Fatal("noncanonical input passed pure validation")
		}
	}
}

func TestRecoveryLeaseContentionAndExactCleanup(t *testing.T) {
	server, identity := recoveryServer(t, true)
	sourceBefore := saveSplitBridgeDirectory(t, identity.SourceDirectory)
	socketsBefore := saveSplitBridgeDirectory(t, identity.SocketDirectory)
	requireRecoveryRefusal(t, identity)
	assertSplitBridgeDirectoryPreserved(t, identity.SourceDirectory, sourceBefore, false)
	assertSplitBridgeDirectoryPreserved(t, identity.SocketDirectory, socketsBefore, false)
	stopRecoveryServer(t, server)
	lease := requireRecoveryLease(t, identity)
	requireRecoveryRefusal(t, identity)
	if next, err := StartWithSocketDirectory(context.Background(), identity.SourceDirectory, bridgeSourceDirectory(t), &protocolFilesystem{}); err == nil || next != nil {
		if next != nil {
			stopRecoveryServer(t, next)
		}
		t.Fatal("recovery lease allowed a competing session to rotate its descriptor")
	}
	// No filesystem mutation happens until the caller chooses cleanup after
	// independently establishing kernel detachment.
	assertSplitBridgeDirectoryPreserved(t, identity.SourceDirectory, sourceBefore, false)
	assertSplitBridgeDirectoryPreserved(t, identity.SocketDirectory, socketsBefore, false)
	for range 2 {
		if err := lease.CleanupDetached(); err != nil {
			t.Fatal(err)
		}
	}
	assertSplitBridgeAbsent(t, identity.SourceDirectory, identity.SocketPath)
	lock, err := os.Lstat(identity.LeasePath)
	if err != nil || recoveryIdentityOnVolume(recoveryInfoIdentity(lock), identity.Lease.VolumeUUID) != identity.Lease {
		t.Fatal("recovery cleanup removed or replaced the persistent lease")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.CleanupDetached(); err == nil {
		t.Fatal("closed lease accepted cleanup")
	}
	next, err := StartWithSocketDirectory(context.Background(), identity.SourceDirectory, identity.SocketDirectory, &protocolFilesystem{})
	if err != nil {
		t.Fatal(err)
	}
	bridgeServerCleanup(t, next)
	requireRecoveryRefusal(t, identity)
	bridgeDrain(t, next)
}

func TestRecoveryLeaseAcceptsAlreadyAbsentSessionFiles(t *testing.T) {
	for _, missing := range []string{"socket", "descriptor", "both"} {
		t.Run(missing, func(t *testing.T) {
			server, identity := recoveryServer(t, true)
			stopRecoveryServer(t, server)
			for name, path := range map[string]string{"socket": identity.SocketPath, "descriptor": identity.DescriptorPath} {
				if missing == name || missing == "both" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			}
			lease := requireRecoveryLease(t, identity)
			if err := lease.CleanupDetached(); err != nil {
				t.Fatal(err)
			}
			assertSplitBridgeAbsent(t, identity.SourceDirectory, identity.SocketPath)
			if _, err := os.Lstat(identity.LeasePath); err != nil {
				t.Fatal("cleanup removed the lease")
			}
		})
	}
}

func replaceRecoveryPath(t *testing.T, path string) os.FileInfo {
	t.Helper()
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, []byte("unrelated replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestRecoveryLeaseRefusesChangedFilesWithoutCreatingOrRemoving(t *testing.T) {
	for _, name := range []string{"missing_lease", "lease_replaced", "descriptor_replaced", "socket_replaced", "lease_symlink", "descriptor_symlink", "socket_symlink", "lease_hardlink", "descriptor_hardlink", "lease_mode", "descriptor_mode", "socket_mode", "source_mode", "sockets_mode", "source_inode", "sockets_inode"} {
		t.Run(name, func(t *testing.T) {
			server, identity := recoveryServer(t, true)
			stopRecoveryServer(t, server)
			switch name {
			case "missing_lease":
				if err := os.Remove(identity.LeasePath); err != nil {
					t.Fatal(err)
				}
			case "lease_replaced":
				replaceRecoveryPath(t, identity.LeasePath)
			case "descriptor_replaced":
				replaceRecoveryPath(t, identity.DescriptorPath)
			case "socket_replaced":
				replaceRecoveryPath(t, identity.SocketPath)
			case "lease_symlink", "descriptor_symlink", "socket_symlink":
				path := map[string]string{"lease_symlink": identity.LeasePath, "descriptor_symlink": identity.DescriptorPath, "socket_symlink": identity.SocketPath}[name]
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("unrelated", path); err != nil {
					t.Fatal(err)
				}
			case "lease_hardlink", "descriptor_hardlink":
				path := identity.LeasePath
				if name == "descriptor_hardlink" {
					path = identity.DescriptorPath
				}
				if err := os.Link(path, path+".second-link"); err != nil {
					t.Fatal(err)
				}
			case "lease_mode", "descriptor_mode", "socket_mode", "source_mode", "sockets_mode":
				path := map[string]string{"lease_mode": identity.LeasePath, "descriptor_mode": identity.DescriptorPath, "socket_mode": identity.SocketPath, "source_mode": identity.SourceDirectory, "sockets_mode": identity.SocketDirectory}[name]
				mode := os.FileMode(0o644)
				if name == "source_mode" || name == "sockets_mode" {
					mode = 0o755
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				if name == "source_mode" || name == "sockets_mode" {
					t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
				}
			case "source_inode":
				identity.Source.Inode++
			case "sockets_inode":
				identity.SocketDirectoryFile.Inode++
			}
			sourceBefore := saveSplitBridgeDirectory(t, identity.SourceDirectory)
			socketsBefore := saveSplitBridgeDirectory(t, identity.SocketDirectory)
			requireRecoveryRefusal(t, identity)
			assertSplitBridgeDirectoryPreserved(t, identity.SourceDirectory, sourceBefore, false)
			assertSplitBridgeDirectoryPreserved(t, identity.SocketDirectory, socketsBefore, false)
		})
	}
}

func TestRecoveryLeaseRefusesDirectorySymlink(t *testing.T) {
	server, identity := recoveryServer(t, true)
	stopRecoveryServer(t, server)
	for _, sourceAlias := range []bool{true, false} {
		path := identity.SocketDirectory
		if sourceAlias {
			path = identity.SourceDirectory
		}
		alias := path + "-alias"
		if err := os.Symlink(path, alias); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(alias) })
		source, sockets := identity.SourceDirectory, identity.SocketDirectory
		if sourceAlias {
			source = alias
		} else {
			sockets = alias
		}
		lease, err := AcquireRecoveryLease(source, sockets, identity)
		if lease != nil {
			_ = lease.Close()
		}
		if err == nil || lease != nil {
			t.Fatal("recovery accepted a symlink as its private directory")
		}
	}
}

func TestSessionIdentityRefusesReplacedOriginalObjects(t *testing.T) {
	for _, name := range []string{"lease", "descriptor", "socket", "source", "sockets"} {
		t.Run(name, func(t *testing.T) {
			server, identity := recoveryServer(t, true)
			if name == "source" || name == "sockets" {
				path := identity.SourceDirectory
				if name == "sockets" {
					path = identity.SocketDirectory
				}
				old := path + ".original"
				if err := os.Rename(path, old); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(old) })
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				replaceRecoveryPath(t, map[string]string{"lease": identity.LeasePath, "descriptor": identity.DescriptorPath, "socket": identity.SocketPath}[name])
			}
			if got, err := server.SessionIdentity(); err == nil || got != (SessionIdentity{}) {
				t.Fatal("server captured a receipt after an original object was replaced")
			}
		})
	}
}

func TestRecoveryCleanupRefusesChangesAfterAcquisition(t *testing.T) {
	for _, name := range []string{"lease", "descriptor", "socket", "source", "sockets"} {
		t.Run(name, func(t *testing.T) {
			server, identity := recoveryServer(t, true)
			stopRecoveryServer(t, server)
			lease := requireRecoveryLease(t, identity)
			if name == "source" || name == "sockets" {
				path := identity.SourceDirectory
				if name == "sockets" {
					path = identity.SocketDirectory
				}
				original := path + ".original"
				if err := os.Rename(path, original); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(original) })
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "untouched"), []byte("replacement directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				savedOriginal := saveSplitBridgeDirectory(t, original)
				savedReplacement := saveSplitBridgeDirectory(t, path)
				if err := lease.CleanupDetached(); err == nil {
					t.Fatal("cleanup followed a replaced directory binding")
				}
				assertSplitBridgeDirectoryPreserved(t, original, savedOriginal, false)
				assertSplitBridgeDirectoryPreserved(t, path, savedReplacement, false)
				return
			}
			path := map[string]string{"lease": identity.LeasePath, "descriptor": identity.DescriptorPath, "socket": identity.SocketPath}[name]
			replaceRecoveryPath(t, path)
			sourceBefore := saveSplitBridgeDirectory(t, identity.SourceDirectory)
			socketsBefore := saveSplitBridgeDirectory(t, identity.SocketDirectory)
			if err := lease.CleanupDetached(); err == nil {
				t.Fatal("cleanup accepted a changed session object")
			}
			assertSplitBridgeDirectoryPreserved(t, identity.SourceDirectory, sourceBefore, false)
			assertSplitBridgeDirectoryPreserved(t, identity.SocketDirectory, socketsBefore, false)
		})
	}
}

func TestRecoveryLeaseNoNewSocketOrCapabilityBeforeCleanup(t *testing.T) {
	server, identity := recoveryServer(t, true)
	stopRecoveryServer(t, server)
	lease := requireRecoveryLease(t, identity)
	before, err := os.ReadFile(identity.DescriptorPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := StartWithSocketDirectory(context.Background(), identity.SourceDirectory, identity.SocketDirectory, &protocolFilesystem{})
	if next != nil {
		stopRecoveryServer(t, next)
	}
	if err == nil || next != nil {
		t.Fatal("ordinary startup replaced a retained socket before cleanup")
	}
	after, err := os.ReadFile(identity.DescriptorPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed startup rotated the original capability")
	}
	lease = requireRecoveryLease(t, identity)
	if err := lease.CleanupDetached(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	// A different socket object at the same name cannot inherit old authority.
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: identity.SocketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(identity.SocketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	requireRecoveryRefusal(t, identity)
	if _, err := os.Lstat(identity.SocketPath); errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal removed a new socket")
	}
}
