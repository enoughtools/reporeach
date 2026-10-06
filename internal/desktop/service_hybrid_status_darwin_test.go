//go:build darwin

package desktop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHybridStatusCanonicalVirtualRootWithAncestorAlias(t *testing.T) {
	physicalParent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(t.TempDir(), "state-parent")
	if err := os.Symlink(physicalParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	lexicalStateDir := filepath.Join(aliasParent, "state")
	gh := fakeGitHub(t, "exit 99")
	s, err := New(context.Background(), Options{
		StateDir: lexicalStateDir, MountRoot: filepath.Join(t.TempDir(), "repositories"), GHPath: gh.path,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close service: %v", err)
		}
	})
	s.dependencyReady = func() bool { return true }
	want := filepath.Join(physicalParent, "state", "native-catalogue", "volume")
	if s.opts.StateDir != lexicalStateDir {
		t.Fatal("canonical Finder metadata rewrote the engine's existing storage paths")
	}
	privateRoot, err := s.hybridCatalogueMountRoot()
	if err != nil {
		t.Fatal(err)
	}
	canonicalMountRoot, err := filepath.EvalSymlinks(privateRoot)
	if err != nil || canonicalMountRoot != want || s.Status().VirtualRoot != canonicalMountRoot {
		t.Fatalf("Finder status does not match the canonical kernel mount location: got %q mount %q want %q, %v", s.Status().VirtualRoot, canonicalMountRoot, want, err)
	}
	// Status must keep working without walking the alias again. Moving only the
	// disposable alias leaves all real state and open stores at their location.
	movedAlias := aliasParent + "-moved"
	if err := os.Rename(aliasParent, movedAlias); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Rename(movedAlias, aliasParent); err != nil {
			t.Errorf("restore test alias: %v", err)
		}
	}()
	if got := s.Status().VirtualRoot; got != want {
		t.Fatalf("status re-resolved an unavailable state alias: got %q want %q", got, want)
	}
	s.mu.Lock()
	s.hybridCatalogue = false
	s.mu.Unlock()
	encoded, err := json.Marshal(s.Status())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["virtualRoot"]; present {
		t.Fatal("legacy transport status exposed a private virtual root")
	}
}
