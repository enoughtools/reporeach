package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

func TestSettingsRejectsOverlappingMountRootsBeforeFilesystemChanges(t *testing.T) {
	for _, destination := range []string{"nested", "ancestor", "symlinked nested"} {
		t.Run(destination, func(t *testing.T) {
			s := newDesktopTestService(t, "exit 4")
			s.dependencyReady = func() bool { return true }
			mounted := newDesktopFakeMount()
			s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) { return mounted, nil }
			if err := s.Mount(context.Background()); err != nil {
				t.Fatal(err)
			}
			oldRoot := s.Status().MountRoot
			newRoot := filepath.Join(oldRoot, "nested")
			switch destination {
			case "ancestor":
				newRoot = filepath.Dir(oldRoot)
			case "symlinked nested":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(oldRoot, alias); err != nil {
					t.Fatal(err)
				}
				newRoot = filepath.Join(alias, "nested")
			}
			if err := s.Settings(context.Background(), newRoot); err == nil || !strings.Contains(err.Error(), "outside the current") {
				t.Fatalf("overlapping destination error = %v", err)
			}
			if status := s.Status(); !status.Mounted || status.MountRoot != oldRoot {
				t.Fatalf("rejected settings altered mount: %+v", status)
			}
			mounted.mu.Lock()
			unmountCalls := mounted.unmountCalls
			mounted.mu.Unlock()
			if unmountCalls != 0 {
				t.Fatal("overlapping settings detached the active catalogue")
			}
			if _, err := os.Lstat(filepath.Join(oldRoot, "nested")); !os.IsNotExist(err) {
				t.Fatal("overlapping settings created a folder inside the active catalogue")
			}
		})
	}
}

func TestSettingsCanonicalExistingRootIsNoOp(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	root := s.Status().MountRoot
	if err := s.Settings(context.Background(), root+string(os.PathSeparator)+"."); err != nil {
		t.Fatal(err)
	}
	if got := s.Status().MountRoot; got != root {
		t.Fatalf("equivalent root was rewritten to %s", got)
	}
}
