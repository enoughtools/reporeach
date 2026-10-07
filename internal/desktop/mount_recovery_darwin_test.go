//go:build darwin

package desktop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

func TestRecoverMountUnhealthyOwnerNeverFallsThroughToOwnerlessRecovery(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "owned detach refuses"
		if running {
			name = "repository operation blocks detach"
		}
		t.Run(name, func(t *testing.T) {
			s := newDesktopTestService(t, "exit 4")
			s.dependencyReady = func() bool { return true }
			bridge := &fakePlatformBridge{done: make(chan struct{})}
			close(bridge.done)
			inventories, recoveryCalls, mountCalls := 0, 0, 0
			// Closed fake bridge models an interrupted owner. Its injected
			// inventory fails before any mount path or OS command is accessed.
			mount := &nativeFSKitMount{bridge: bridge, ops: fsKitMountOperations{
				timeout: time.Second,
				mounts: func() ([]fsKitMountIdentity, error) {
					inventories++
					return nil, errors.New("fixture ownership cannot be established")
				},
			}}
			s.mu.Lock()
			s.mounted = mount
			if running {
				s.ops = []Operation{{ID: "download", RepositoryID: "octocat/repo", Action: "keep", Status: "running"}}
			}
			s.mu.Unlock()
			// This owner is purely a fixture; release its reference before the
			// shared service cleanup rather than simulating another detach.
			defer func() { s.mu.Lock(); s.mounted = nil; s.mu.Unlock() }()
			s.recoverMountCatalogue = func(context.Context) error { recoveryCalls++; return nil }
			s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
				mountCalls++
				return nil, errors.New("unexpected replacement mount")
			}
			err := s.RecoverMount(context.Background())
			wantInventories := 1
			if running {
				wantInventories = 0
				if err == nil || !strings.Contains(err.Error(), "finish repository operations") {
					t.Fatalf("running operation recovery error = %v", err)
				}
			} else if !errors.Is(err, errFSKitMountOwnership) {
				t.Fatalf("owned recovery error = %v", err)
			}
			s.mu.Lock()
			owner, maintenance := s.mounted, s.maintenance
			s.mu.Unlock()
			if owner != mount || maintenance || inventories != wantInventories || recoveryCalls != 0 || mountCalls != 0 || bridge.closeCount() != 0 {
				t.Fatalf("owned recovery lost isolation: sameOwner=%v maintenance=%v inventories=%d recovery=%d mount=%d bridgeCloses=%d", owner == mount, maintenance, inventories, recoveryCalls, mountCalls, bridge.closeCount())
			}
		})
	}
}
