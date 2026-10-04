package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

type retryDesktopUnmount struct {
	*desktopFakeMount
	mu       sync.Mutex
	attempts int
}

func (m *retryDesktopUnmount) Unmount() error {
	m.mu.Lock()
	m.attempts++
	first := m.attempts == 1
	m.mu.Unlock()
	if first {
		return errors.New("busy mount")
	}
	return m.desktopFakeMount.Unmount()
}

func TestServiceClosingDuringMountRetainsOwnershipUntilDetached(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return true }
	entered, release := make(chan struct{}), make(chan struct{})
	mounted := &retryDesktopUnmount{desktopFakeMount: newDesktopFakeMount()}
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		close(entered)
		<-release
		return mounted, nil
	}
	mountDone := make(chan error, 1)
	go func() { mountDone <- s.Mount(context.Background()) }()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	awaitDesktop(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closing })
	close(release)
	select {
	case err := <-mountDone:
		if err == nil || !strings.Contains(err.Error(), "busy mount") {
			t.Fatalf("mount error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight mount did not finish")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("closing service did not detach retained mount")
	}
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	if mounted.attempts != 2 {
		t.Fatalf("unmount attempts = %d", mounted.attempts)
	}
}

func TestMountPathCannotReachPrivateStateThroughSymlinkAncestor(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	link := filepath.Join(t.TempDir(), "linked-state")
	if err := os.Symlink(s.opts.StateDir, link); err != nil {
		t.Fatal(err)
	}
	if err := s.Settings(context.Background(), filepath.Join(link, "mount")); err == nil {
		t.Fatal("mount was allowed inside private state through a symlink")
	}
}

func TestGitCredentialHelperDisablesTelemetryForTerminalGit(t *testing.T) {
	helper := githubCredentialHelper("/Applications/RepoReach.app/Contents/Helpers/gh")
	if !strings.HasPrefix(helper, "!GH_TELEMETRY=false '") || !strings.HasSuffix(helper, "' auth git-credential") {
		t.Fatalf("unexpected managed credential helper: %q", helper)
	}
}
