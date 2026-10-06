//go:build darwin

package desktop

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sys/unix"
)

func TestStageLocalCheckoutPreservesMacResourceForkAndFinderMetadata(t *testing.T) {
	path, _ := adoptionSource(t)
	resource := []byte{0, 255, 128, 'r', 0, 254, 127}
	finder := make([]byte, 32)
	copy(finder[:4], []byte("TEXT"))
	attributes := map[string][]byte{"com.apple.ResourceFork": resource, "com.apple.FinderInfo": finder}
	if err := checkoutWriteXattrs(filepath.Join(path, "tracked.txt"), attributes); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "kept")
	stage, err := StageLocalCheckout(context.Background(), model.RepoConfig{GitDir: filepath.Join(path, ".git")}, path, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if err := stage.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stage.VerifyPublished(context.Background()); err != nil {
		t.Fatal(err)
	}
	kept, err := checkoutReadXattrs(filepath.Join(destination, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range attributes {
		if !bytes.Equal(kept[name], expected) {
			t.Fatalf("lost %s", name)
		}
	}
	if info, err := os.Stat(filepath.Join(destination, "tracked.txt")); err != nil || !info.Mode().IsRegular() {
		t.Fatal("resource-fork file ceased to be an ordinary local file")
	}
}

func TestCheckoutNativeMetadataAllowsOnlyOwnedFSKitUnsupportedACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordinary-host-file")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		t.Fatal(err)
	}
	metadata, err := nativePathACLMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		err    error
		owned  bool
		modify func(*nativeACLMetadata, *unix.Stat_t)
		allow  bool
	}{
		{name: "ordinary-ACL-free-host", allow: true},
		{name: "APFS-unsupported-is-unproven", err: unix.ENOTSUP},
		{name: "owned-unsupported", err: unix.ENOTSUP, owned: true, allow: true},
		{name: "owned-wrapped-unsupported", err: fmt.Errorf("ACL query: %w", unix.ENOTSUP), owned: true, allow: true},
		{name: "owned-access-denied", err: unix.EACCES, owned: true},
		{name: "APFS-invalid-request-still-refused", err: unix.EINVAL},
		{name: "owned-unsupported-invalid-request", err: unix.EINVAL, owned: true, allow: true},
		{name: "owned-query-unavailable", err: unix.ENOSYS, owned: true},
		{name: "actual-ACL-still-refused", owned: true, modify: func(m *nativeACLMetadata, _ *unix.Stat_t) { m.HasACL = true }},
		{name: "vnode-change-still-refused", owned: true, modify: func(m *nativeACLMetadata, _ *unix.Stat_t) { m.Ino++ }},
		{name: "native-flags-still-refused", err: unix.ENOTSUP, owned: true, modify: func(_ *nativeACLMetadata, s *unix.Stat_t) { s.Flags = unix.UF_IMMUTABLE }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, object := metadata, stat
			if test.modify != nil {
				test.modify(&value, &object)
			}
			err := checkoutValidateMetadataReply(value, test.err, &object, test.owned)
			if (err == nil) != test.allow {
				t.Fatalf("allow=%v, error=%v", test.allow, err)
			}
			if test.err != nil && !test.allow && test.modify == nil && !errors.Is(err, test.err) {
				t.Fatal("metadata diagnostic lost the OS error")
			}
		})
	}
}

func TestCheckoutVolumeACLCapabilityRequiresKnownDisabledInterface(t *testing.T) {
	const extendedSecurity = uint32(0x00000400)
	for _, name := range []string{"disabled", "supported", "unknown", "truncated", "oversized", "invalid-length"} {
		t.Run(name, func(t *testing.T) {
			reply := make([]byte, 36)
			binary.NativeEndian.PutUint32(reply[:4], uint32(len(reply)))
			binary.NativeEndian.PutUint32(reply[24:28], extendedSecurity)
			switch name {
			case "supported":
				binary.NativeEndian.PutUint32(reply[8:12], extendedSecurity)
			case "unknown":
				binary.NativeEndian.PutUint32(reply[24:28], 0)
			case "truncated":
				reply = reply[:35]
			case "oversized":
				reply = append(reply, 0)
			case "invalid-length":
				binary.NativeEndian.PutUint32(reply[:4], 32)
			}
			disabled, err := checkoutParseVolumeACLUnsupported(reply)
			if name == "disabled" {
				if err != nil || !disabled {
					t.Fatalf("known disabled ACL capability: %v, %v", disabled, err)
				}
			} else if name == "supported" {
				if err != nil || disabled {
					t.Fatalf("supported ACL capability treated as absent: %v, %v", disabled, err)
				}
			} else if err == nil || disabled {
				t.Fatalf("unproven capability was accepted: %v, %v", disabled, err)
			}
		})
	}
	path := t.TempDir()
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		t.Fatal(err)
	}
	if unix.ByteSliceToString(fs.Fstypename[:]) != "apfs" {
		t.Skip("the native APFS ACL capability proof needs APFS")
	}
	disabled, err := checkoutVolumeACLUnsupported(path)
	if err != nil || disabled {
		t.Fatalf("ordinary APFS reported unsupported ACLs: disabled=%v, error=%v", disabled, err)
	}
}

func TestStageLocalCheckoutPreservesSymlinkModeWithoutTouchingTarget(t *testing.T) {
	path, _ := adoptionSource(t)
	external := filepath.Join(t.TempDir(), "external-target")
	if err := os.WriteFile(external, []byte("external bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(path, "link")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	if err := checkoutCopySymlinkMode(link, 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(external)
	if err != nil || before.Mode().Perm() != 0600 {
		t.Fatalf("setting source link permissions followed its target: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "kept")
	stage, err := StageLocalCheckout(context.Background(), model.RepoConfig{GitDir: filepath.Join(path, ".git")}, path, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if err := stage.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stage.VerifyPublished(context.Background()); err != nil {
		t.Fatal(err)
	}
	copyInfo, err := os.Lstat(filepath.Join(destination, "link"))
	if err != nil || copyInfo.Mode()&os.ModeSymlink == 0 || copyInfo.Mode().Perm() != 0644 {
		t.Fatalf("native link permissions were not preserved: %v", err)
	}
	after, err := os.Stat(external)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || before.ModTime() != after.ModTime() {
		t.Fatalf("copying the link modified its external target: %v", err)
	}
	content, err := os.ReadFile(external)
	if err != nil || !bytes.Equal(content, []byte("external bytes")) {
		t.Fatal("copying link permissions modified external data")
	}
}

func TestCheckoutNativeMetadataRejectsChangedOrUnownedMounts(t *testing.T) {
	identity := fsKitMountIdentity{fsid: [2]int32{123, 24}, owner: uint32(os.Geteuid()), typeName: "reporeach", root: "/private/owned-volume", source: "file:///private/owned-source"}
	path := filepath.Join(identity.root, "owner", "repo", "tracked.txt")
	for _, name := range []string{"matching", "absent", "new-fsid", "different-type", "different-source", "different-owner", "moved", "duplicate", "nested-volume", "outside-view"} {
		t.Run(name, func(t *testing.T) {
			mount := identity
			mounts := []fsKitMountIdentity{mount}
			sourcePath := path
			switch name {
			case "absent":
				mounts = nil
			case "new-fsid":
				mounts[0].fsid[0]++
			case "different-type":
				mounts[0].typeName = "apfs"
			case "different-source":
				mounts[0].source = "file:///private/replacement-source"
			case "different-owner":
				mounts[0].owner++
			case "moved":
				mounts[0].root = "/private/moved-volume"
			case "duplicate":
				mounts = append(mounts, identity)
			case "nested-volume":
				mounts = append(mounts, fsKitMountIdentity{fsid: [2]int32{999, 26}, typeName: "apfs", root: filepath.Dir(path)})
			case "outside-view":
				sourcePath = "/private/ordinary-host-file"
			}
			err := checkoutValidateMountInventory(identity, sourcePath, mounts)
			if (err == nil) != (name == "matching") {
				t.Fatalf("%s mount inventory: %v", name, err)
			}
		})
	}
	for _, kind := range []string{"apfs", "lifs", "devicefs", ""} {
		other := identity
		other.typeName = kind
		if err := checkoutValidateMountInventory(other, path, []fsKitMountIdentity{other}); err == nil {
			t.Fatalf("unowned filesystem type %q was accepted", kind)
		}
	}
}
