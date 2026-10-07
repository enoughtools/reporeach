//go:build darwin

package desktop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fsbridge"
	"golang.org/x/sys/unix"
)

func nativeSessionFixture(t *testing.T) (string, nativeMountSessionReceipt) {
	t.Helper()
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	// None of the recorded root/bridge paths need to exist. These tests must
	// exercise persistence without traversing or mounting the recorded root.
	source, sockets := "/private/enoughrepos-session-fixture/FSKit", "/private/enoughrepos-session-fixture/shared"
	digest := sha256.Sum256([]byte(source))
	object := func(inode uint64, mode uint32) fsbridge.FileIdentity {
		return fsbridge.FileIdentity{Device: 1, Inode: inode, VolumeUUID: "0102030405060708090a0b0c0d0e0f10",
			BirthSec: 1000, BirthNSec: 4, UID: uint32(os.Geteuid()), Mode: mode}
	}
	bridge := fsbridge.SessionIdentity{Version: fsbridge.SessionIdentityVersion,
		SourceDirectory: source, SocketDirectory: sockets, DescriptorPath: source + "/connection.json",
		SocketPath: sockets + "/b" + hex.EncodeToString(digest[:8]), LeasePath: source + "/.bridge.lock",
		Source: object(11, unix.S_IFDIR|0o700), SocketDirectoryFile: object(12, unix.S_IFDIR|0o700),
		Descriptor: object(13, unix.S_IFREG|0o600), Socket: object(14, unix.S_IFSOCK|0o600), Lease: object(15, unix.S_IFREG|0o600)}
	mount := fsKitMountIdentity{fsid: [2]int32{300, 24}, owner: uint32(os.Geteuid()), typeName: "reporeach",
		root: "/private/enoughrepos-session-fixture/volume", source: "file://" + source + "/"}
	root := nativeRootObjectIdentity{VolumeUUID: "0102030405060708090a0b0c0d0e0f10", Inode: 20, BirthSec: 1000, BirthNSec: 4, UID: uint32(os.Geteuid())}
	return state, newNativeMountSessionReceipt("11223344556677889900aabbccddeeff", mount, root, bridge)
}

func nativeSessionRaw(t *testing.T, state string, receipt nativeMountSessionReceipt, data []byte) string {
	t.Helper()
	path := nativeMountSessionPath(state, receipt.Mount.Root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeMountSessionRoundTripAndExactRemoval(t *testing.T) {
	state, receipt := nativeSessionFixture(t)
	if loaded, err := readNativeMountSession(state, receipt.Mount.Root); err != nil || loaded != nil {
		t.Fatalf("absent receipt: %+v %v", loaded, err)
	}
	if err := writeNativeMountSession(state, receipt); err != nil {
		t.Fatal(err)
	}
	loaded, err := readNativeMountSession(state, receipt.Mount.Root)
	if err != nil || loaded == nil || !loaded.sameSession(receipt) || loaded.identity() != receipt.identity() || loaded.fileIdentity.inode == 0 {
		t.Fatalf("receipt did not retain complete session: %+v %v", loaded, err)
	}
	digest := sha256.Sum256([]byte(receipt.Mount.Root))
	expectedPath := filepath.Join(state, "native-mount-sessions", hex.EncodeToString(digest[:])+".json")
	if nativeMountSessionPath(state, receipt.Mount.Root) != expectedPath {
		t.Fatal("receipt path is not bound to the root digest")
	}
	if err := removeNativeMountSession(state, receipt.Mount.Root, *loaded); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(expectedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact detached session was not removed: %v", err)
	}
	if err := removeNativeMountSession(state, receipt.Mount.Root, *loaded); err != nil {
		t.Fatalf("removing an already absent exact receipt: %v", err)
	}
}

func TestNativeMountSessionReplacementDoesNotAuthorizeStaleRemoval(t *testing.T) {
	state, first := nativeSessionFixture(t)
	if err := writeNativeMountSession(state, first); err != nil {
		t.Fatal(err)
	}
	old, err := readNativeMountSession(state, first.Mount.Root)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Mount.FSID[0]++
	second.Bridge.Socket.Inode++
	if err := writeNativeMountSession(state, second); err != nil {
		t.Fatal(err)
	}
	if err := removeNativeMountSession(state, first.Mount.Root, *old); !errors.Is(err, errNativeMountSessionChanged) {
		t.Fatalf("old session removed a newer session: %v", err)
	}
	loaded, err := readNativeMountSession(state, first.Mount.Root)
	if err != nil || loaded == nil || !loaded.sameSession(second) {
		t.Fatalf("new session was not preserved: %+v %v", loaded, err)
	}
	// Even identical JSON at a new inode must not inherit removal permission
	// from a receipt that was read before the atomic replacement.
	if err := writeNativeMountSession(state, second); err != nil {
		t.Fatal(err)
	}
	if err := removeNativeMountSession(state, second.Mount.Root, *loaded); !errors.Is(err, errNativeMountSessionChanged) {
		t.Fatalf("replaced receipt inode inherited stale removal permission: %v", err)
	}
}

func TestNativeMountSessionRemovalRequiresOriginalBirthAndVolumeBinding(t *testing.T) {
	for _, field := range []string{"volume", "birth seconds", "birth nanoseconds"} {
		t.Run(field, func(t *testing.T) {
			state, receipt := nativeSessionFixture(t)
			if err := writeNativeMountSession(state, receipt); err != nil {
				t.Fatal(err)
			}
			loaded, err := readNativeMountSession(state, receipt.Mount.Root)
			if err != nil || loaded == nil {
				t.Fatalf("loading original receipt: %+v %v", loaded, err)
			}
			expected := *loaded
			switch field {
			case "volume":
				expected.fileIdentity.volumeUUID = "aabbccddeeff00112233445566778899"
			case "birth seconds":
				expected.fileIdentity.birthSec++
			case "birth nanoseconds":
				expected.fileIdentity.birthNSec = (expected.fileIdentity.birthNSec + 1) % 1e9
			}
			if err := removeNativeMountSession(state, receipt.Mount.Root, expected); !errors.Is(err, errNativeMountSessionChanged) {
				t.Fatalf("same device/inode with changed %s authorized removal: %v", field, err)
			}
			preserved, err := readNativeMountSession(state, receipt.Mount.Root)
			if err != nil || preserved == nil || preserved.fileIdentity != loaded.fileIdentity {
				t.Fatalf("original receipt changed: %+v %v", preserved, err)
			}
		})
	}
}

func TestNativeMountSessionRejectsInvalidIdentity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*nativeMountSessionReceipt)
	}{
		{"version", func(r *nativeMountSessionReceipt) { r.Version++ }},
		{"boot uppercase", func(r *nativeMountSessionReceipt) { r.BootUUID = strings.ToUpper(r.BootUUID) }},
		{"boot missing", func(r *nativeMountSessionReceipt) { r.BootUUID = "" }},
		{"boot zero", func(r *nativeMountSessionReceipt) { r.BootUUID = strings.Repeat("0", 32) }},
		{"root relative", func(r *nativeMountSessionReceipt) { r.Mount.Root = "relative" }},
		{"root not canonical", func(r *nativeMountSessionReceipt) { r.Mount.Root += "/../volume" }},
		{"root NUL", func(r *nativeMountSessionReceipt) { r.Mount.Root += "\x00" }},
		{"zero fsid", func(r *nativeMountSessionReceipt) { r.Mount.FSID = nativeMountSessionFSID{} }},
		{"different mount owner", func(r *nativeMountSessionReceipt) { r.Mount.Owner++ }},
		{"different filesystem", func(r *nativeMountSessionReceipt) { r.Mount.TypeName = "apfs" }},
		{"different source", func(r *nativeMountSessionReceipt) { r.Mount.Source += "other" }},
		{"different root owner", func(r *nativeMountSessionReceipt) { r.RootIdentity.UID++ }},
		{"missing root inode", func(r *nativeMountSessionReceipt) { r.RootIdentity.Inode = 0 }},
		{"unknown root UUID", func(r *nativeMountSessionReceipt) { r.RootIdentity.VolumeUUID = strings.Repeat("0", 32) }},
		{"bridge descriptor", func(r *nativeMountSessionReceipt) { r.Bridge.DescriptorPath += ".other" }},
		{"bridge source relative", func(r *nativeMountSessionReceipt) { r.Bridge.SourceDirectory = "relative" }},
		{"bridge source NUL", func(r *nativeMountSessionReceipt) { r.Bridge.SourceDirectory += "\x00" }},
		{"bridge inode absent", func(r *nativeMountSessionReceipt) { r.Bridge.Lease.Inode = 0 }},
		{"bridge old schema", func(r *nativeMountSessionReceipt) { r.Bridge.Version = 1 }},
		{"bridge volume absent", func(r *nativeMountSessionReceipt) { r.Bridge.Source.VolumeUUID = "" }},
		{"bridge birth absent", func(r *nativeMountSessionReceipt) { r.Bridge.Descriptor.BirthSec, r.Bridge.Descriptor.BirthNSec = 0, 0 }},
		{"bridge owner changed", func(r *nativeMountSessionReceipt) { r.Bridge.Socket.UID++ }},
		{"bridge lease type changed", func(r *nativeMountSessionReceipt) { r.Bridge.Lease.Mode = unix.S_IFDIR | 0o700 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, receipt := nativeSessionFixture(t)
			tc.mutate(&receipt)
			if err := writeNativeMountSession(state, receipt); !errors.Is(err, errNativeMountSessionInvalid) {
				t.Fatalf("invalid identity was writable: %v", err)
			}
			data, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			nativeSessionRaw(t, state, receipt, data)
			if _, err := readNativeMountSession(state, receipt.Mount.Root); err == nil {
				t.Fatal("invalid stored identity was readable")
			}
		})
	}
}

func TestNativeMountSessionStrictDecodePreservesUnknownReceipts(t *testing.T) {
	for _, name := range []string{"malformed", "unknown field", "trailing object", "duplicate field", "duplicate case variant", "duplicate Unicode fold", "nested duplicate", "short fsid", "long fsid", "null fsid word", "oversized", "different root"} {
		t.Run(name, func(t *testing.T) {
			state, receipt := nativeSessionFixture(t)
			data, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "malformed":
				data = []byte("{broken")
			case "unknown field":
				data = append([]byte(`{"unknown":true,`), data[1:]...)
			case "trailing object":
				data = append(data, []byte(" {}")...)
			case "duplicate field":
				data = append([]byte(`{"version":1,`), data[1:]...)
			case "duplicate case variant":
				data = append([]byte(`{"VERSION":1,`), data[1:]...)
			case "duplicate Unicode fold":
				data = append([]byte(`{"verſion":1,`), data[1:]...)
			case "nested duplicate":
				data = bytes.Replace(data, []byte(`"owner":`), []byte(`"owner":1,"owner":`), 1)
			case "short fsid":
				data = bytes.Replace(data, []byte(`[300,24]`), []byte(`[300]`), 1)
			case "long fsid":
				data = bytes.Replace(data, []byte(`[300,24]`), []byte(`[300,24,99]`), 1)
			case "null fsid word":
				data = bytes.Replace(data, []byte(`[300,24]`), []byte(`[null,24]`), 1)
			case "oversized":
				data = append(data, bytes.Repeat([]byte(" "), nativeMountSessionLimit)...)
			case "different root":
				data = bytes.Replace(data, []byte(receipt.Mount.Root), []byte(receipt.Mount.Root+"-other"), 1)
			}
			path := nativeSessionRaw(t, state, receipt, data)
			if _, err := readNativeMountSession(state, receipt.Mount.Root); err == nil {
				t.Fatal("malformed or unknown receipt accepted")
			}
			if err := writeNativeMountSession(state, receipt); err == nil {
				t.Fatal("unknown receipt overwritten")
			}
			if err := removeNativeMountSession(state, receipt.Mount.Root, receipt); err == nil {
				t.Fatal("unknown receipt removed")
			}
			preserved, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(preserved, data) {
				t.Fatalf("unknown receipt bytes changed: %v", err)
			}
		})
	}
}

func TestNativeMountSessionRejectsUnsafeObjectsWithoutBlocking(t *testing.T) {
	for _, name := range []string{"FIFO", "symlink", "hardlink", "directory", "permissions", "special mode", "parent permissions", "state permissions", "parent symlink"} {
		t.Run(name, func(t *testing.T) {
			state, receipt := nativeSessionFixture(t)
			if err := writeNativeMountSession(state, receipt); err != nil {
				t.Fatal(err)
			}
			path := nativeMountSessionPath(state, receipt.Mount.Root)
			switch name {
			case "FIFO", "symlink", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				var err error
				switch name {
				case "FIFO":
					err = unix.Mkfifo(path, 0o600)
				case "symlink":
					err = os.Symlink(filepath.Join(state, "missing-target"), path)
				case "directory":
					err = os.Mkdir(path, 0o700)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(state, "linked-receipt")); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "special mode":
				if err := unix.Chmod(path, 0o4600); err != nil {
					t.Fatal(err)
				}
			case "parent permissions":
				if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
			case "state permissions":
				if err := os.Chmod(state, 0o755); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				parent := filepath.Dir(path)
				if err := os.Rename(parent, parent+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+"-original", parent); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			if _, err := readNativeMountSession(state, receipt.Mount.Root); err == nil {
				t.Fatal("unsafe receipt accepted")
			}
			if err := writeNativeMountSession(state, receipt); err == nil {
				t.Fatal("unsafe receipt replaced")
			}
			if err := removeNativeMountSession(state, receipt.Mount.Root, receipt); err == nil {
				t.Fatal("unsafe receipt removed")
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("unsafe object read blocked")
			}
		})
	}
}

func TestNativeMountSessionRejectsACLGrants(t *testing.T) {
	for _, object := range []string{"receipt", "parent", "state"} {
		t.Run(object, func(t *testing.T) {
			state, receipt := nativeSessionFixture(t)
			if err := writeNativeMountSession(state, receipt); err != nil {
				t.Fatal(err)
			}
			path := nativeMountSessionPath(state, receipt.Mount.Root)
			if object == "parent" {
				path = filepath.Dir(path)
			} else if object == "state" {
				path = state
			}
			if data, err := exec.Command("/bin/chmod", "+a", "everyone allow read", path).CombinedOutput(); err != nil {
				t.Fatalf("could not add disposable test ACL: %v %s", err, data)
			}
			if _, err := readNativeMountSession(state, receipt.Mount.Root); err == nil {
				t.Fatal("ACL-bearing session accepted")
			}
			if err := writeNativeMountSession(state, receipt); err == nil {
				t.Fatal("ACL-bearing session overwritten")
			}
			if err := removeNativeMountSession(state, receipt.Mount.Root, receipt); err == nil {
				t.Fatal("ACL-bearing session removed")
			}
		})
	}
}

func TestNativeBootUUIDCanonicalization(t *testing.T) {
	for _, input := range []string{"11223344-5566-7788-9900-AABBCCDDEEFF", "11223344556677889900aabbccddeeff"} {
		actual, err := canonicalNativeBootUUID(input)
		if err != nil || actual != "11223344556677889900aabbccddeeff" {
			t.Fatalf("boot UUID normalization: %q %v", actual, err)
		}
	}
	for _, input := range []string{"", strings.Repeat("0", 32), "11223344_5566-7788-9900-AABBCCDDEEFF", strings.Repeat("g", 32)} {
		if _, err := canonicalNativeBootUUID(input); err == nil {
			t.Fatalf("invalid boot UUID accepted: %q", input)
		}
	}
}
