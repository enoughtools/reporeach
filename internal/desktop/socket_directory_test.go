package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceSocketDirectoryRequiresPrivateSeparatedFolder(t *testing.T) {
	for _, name := range []string{"valid", "relative", "missing", "symlink", "nonprivate", "below mount", "above mount", "physical overlap"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			group := filepath.Join(base, "group")
			if err := os.Mkdir(group, 0o700); err != nil {
				t.Fatal(err)
			}
			const sentinel = "other group member's data"
			file := filepath.Join(group, "sentinel")
			if err := os.WriteFile(file, []byte(sentinel), 0o600); err != nil {
				t.Fatal(err)
			}
			gh := fakeGitHub(t, "exit 4")
			opts := Options{StateDir: filepath.Join(base, "state"), MountRoot: filepath.Join(base, "mount"), GHPath: gh.path, FSKitSocketDir: group}
			switch name {
			case "relative":
				opts.FSKitSocketDir = "group"
			case "missing":
				opts.FSKitSocketDir = filepath.Join(base, "absent")
			case "symlink":
				alias := filepath.Join(base, "alias")
				if err := os.Symlink(group, alias); err != nil {
					t.Fatal(err)
				}
				opts.FSKitSocketDir = alias
			case "nonprivate":
				if err := os.Chmod(group, 0o755); err != nil {
					t.Fatal(err)
				}
			case "below mount":
				opts.StateDir = filepath.Join(t.TempDir(), "state")
				opts.MountRoot = base
			case "above mount":
				opts.MountRoot = filepath.Join(group, "mount")
			case "physical overlap":
				alias := filepath.Join(base, "alias")
				if err := os.Symlink(group, alias); err != nil {
					t.Fatal(err)
				}
				opts.MountRoot = filepath.Join(alias, "mount")
			}
			info, err := os.Lstat(group)
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(context.Background(), opts)
			if s != nil {
				defer func() {
					if err := s.Close(); err != nil {
						t.Error(err)
					}
				}()
			}
			if name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				canonical, err := filepath.EvalSymlinks(group)
				if err != nil || s.opts.FSKitSocketDir != canonical {
					t.Fatal("service did not retain the canonical shared socket directory")
				}
			} else if err == nil || s != nil {
				t.Fatal("unsafe shared socket folder was accepted")
			}
			after, statErr := os.Lstat(group)
			data, readErr := os.ReadFile(file)
			if statErr != nil || readErr != nil || !os.SameFile(info, after) || info.Mode() != after.Mode() || string(data) != sentinel {
				t.Fatal("socket directory validation changed caller-owned group contents or permissions")
			}
		})
	}
}

func TestSettingsCannotCoverSharedSocketDirectory(t *testing.T) {
	for _, name := range []string{"same", "nested", "ancestor", "physical alias"} {
		t.Run(name, func(t *testing.T) {
			s := newDesktopTestService(t, "exit 4")
			// Give the group its own ancestor, separate from the existing
			// mount's temporary tree on both Linux and Darwin.
			parent, err := os.MkdirTemp("/tmp", "rr-socket-boundary-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(parent) })
			group := filepath.Join(parent, "group")
			if err := os.Mkdir(group, 0o700); err != nil {
				t.Fatal(err)
			}
			s.opts.FSKitSocketDir = group
			root := group
			switch name {
			case "nested":
				root = filepath.Join(group, "mount")
			case "ancestor":
				root = filepath.Dir(group)
			case "physical alias":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(group, alias); err != nil {
					t.Fatal(err)
				}
				root = filepath.Join(alias, "mount")
			}
			before := s.Status().MountRoot
			if err := s.Settings(context.Background(), root); err == nil || !strings.Contains(err.Error(), "connection folder") {
				t.Fatalf("unsafe socket-directory destination error = %v", err)
			}
			if s.Status().MountRoot != before {
				t.Fatal("rejected destination changed the repository mount folder")
			}
			if _, err := os.Lstat(filepath.Join(group, "mount")); !os.IsNotExist(err) {
				t.Fatal("rejected destination created a folder in the shared socket directory")
			}
		})
	}
}
