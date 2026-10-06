//go:build darwin

package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func hybridCatalogueFixture(t *testing.T) (*Service, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, root := filepath.Join(base, "state"), filepath.Join(base, "catalogue")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Service{opts: Options{StateDir: state}}, root
}

func virtualHybridEntry(name string) hybridCatalogueEntry {
	return hybridCatalogueEntry{ID: "owner/" + name, Owner: "owner", Name: name}
}

func assertHybridLink(t *testing.T, path, target string) {
	t.Helper()
	actual, err := os.Readlink(path)
	if err != nil || actual != target {
		t.Fatalf("link=%q err=%v; want %q", actual, err, target)
	}
}

func TestHybridCatalogueUsesOrdinaryDirectoriesAndPreservesUnrelatedFiles(t *testing.T) {
	s, root := hybridCatalogueFixture(t)
	notes := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(notes, []byte("host data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.checkHybridCatalogueDirectory(root); err != nil {
		t.Fatal(err)
	}
	if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{virtualHybridEntry("first"), virtualHybridEntry("second")}); err != nil {
		t.Fatal(err)
	}
	privateRoot, err := s.hybridCatalogueMountRoot()
	if err != nil {
		t.Fatal(err)
	}
	assertHybridLink(t, filepath.Join(root, "owner", "first"), filepath.Join(privateRoot, "owner", "first"))
	info, err := os.Lstat(filepath.Join(root, "owner"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("owner must be ordinary directory: info=%v err=%v", info, err)
	}
	// Reconciliation needs no access to the target. The virtual volume and both
	// repos are absent throughout this test, proving publication is catalogue-only.
	if _, err := os.Lstat(filepath.Join(privateRoot, "owner")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("virtual owner unexpectedly created: %v", err)
	}
	if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{virtualHybridEntry("second")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "owner", "first")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hidden virtual link remains: %v", err)
	}
	data, err := os.ReadFile(notes)
	if err != nil || string(data) != "host data" {
		t.Fatalf("unrelated data changed: %q %v", data, err)
	}
}

func TestHybridCatalogueAdoptedAndMaterializedCheckoutsRetainHostData(t *testing.T) {
	s, root := hybridCatalogueFixture(t)
	source := filepath.Join(filepath.Dir(root), "existing-checkout")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	dirty := filepath.Join(source, "untracked.bin")
	contents := []byte{0, 1, 255, 0, 2}
	if err := os.WriteFile(dirty, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	adopted := virtualHybridEntry("adopted")
	adopted.LocalPath = source
	physicalPath := filepath.Join(root, "owner", "physical")
	if err := os.MkdirAll(physicalPath, 0o755); err != nil {
		t.Fatal(err)
	}
	physical := virtualHybridEntry("physical")
	physical.LocalPath = physicalPath
	physicalFile := filepath.Join(physicalPath, "local.txt")
	if err := os.WriteFile(physicalFile, []byte("local edits"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{adopted, physical}); err != nil {
		t.Fatal(err)
	}
	assertHybridLink(t, filepath.Join(root, "owner", "adopted"), source)
	if err := s.syncHybridCatalogue(root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "owner", "adopted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed adoption link remains: %v", err)
	}
	data, err := os.ReadFile(dirty)
	if err != nil || string(data) != string(contents) {
		t.Fatalf("adopted dirty bytes changed: %v", err)
	}
	data, err = os.ReadFile(physicalFile)
	if err != nil || string(data) != "local edits" {
		t.Fatalf("materialized data changed: %v", err)
	}
}

func TestHybridCataloguePreservesForeignLeafAndReplacedOwnedLink(t *testing.T) {
	for _, kind := range []string{"physical directory", "foreign identical symlink", "replaced owned symlink", "foreign file"} {
		t.Run(kind, func(t *testing.T) {
			s, root := hybridCatalogueFixture(t)
			entry := virtualHybridEntry("repo")
			path := filepath.Join(root, "owner", "repo")
			privateRoot, err := s.hybridCatalogueMountRoot()
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(privateRoot, "owner", "repo")
			if kind == "replaced owned symlink" {
				if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{entry}); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "physical directory":
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			case "foreign file":
				if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{entry}); !errors.Is(err, errHybridCatalogueConflict) {
				t.Fatalf("foreign entry accepted: %v", err)
			}
			if kind == "replaced owned symlink" {
				if err := s.releaseHybridCatalogueLink(root, "owner", "repo"); !errors.Is(err, errHybridCatalogueConflict) {
					t.Fatalf("replaced entry removed: %v", err)
				}
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("foreign entry replaced: %v", err)
			}
		})
	}
}

func TestHybridCatalogueRejectsOwnerSymlinkWithoutFollowingIt(t *testing.T) {
	s, root := hybridCatalogueFixture(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "owner")); err != nil {
		t.Fatal(err)
	}
	if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{virtualHybridEntry("repo")}); !errors.Is(err, errHybridCatalogueConflict) {
		t.Fatalf("owner symlink accepted: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink destination mutated: %v %v", entries, err)
	}
}

func TestHybridCatalogueRejectsRootReplacementAndUnsafeManifest(t *testing.T) {
	for _, kind := range []string{"root replacement", "root symlink", "manifest symlink", "manifest wrong mode"} {
		t.Run(kind, func(t *testing.T) {
			s, root := hybridCatalogueFixture(t)
			if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{virtualHybridEntry("repo")}); err != nil {
				t.Fatal(err)
			}
			path := hybridManifestPath(s.opts.StateDir, root)
			switch kind {
			case "root replacement":
				if err := os.Rename(root, root+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0o755); err != nil {
					t.Fatal(err)
				}
			case "root symlink":
				if err := os.Rename(root, root+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root+"-old", root); err != nil {
					t.Fatal(err)
				}
			case "manifest symlink":
				if err := os.Rename(path, path+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-saved", path); err != nil {
					t.Fatal(err)
				}
			case "manifest wrong mode":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.syncHybridCatalogue(root, nil); err == nil {
				t.Fatal("unsafe ownership record accepted")
			}
		})
	}
}

func TestHybridCatalogueRecoversJournaledPublication(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "before publication", true: "after publication"}[published], func(t *testing.T) {
			s, rootPath := hybridCatalogueFixture(t)
			root, err := openHybridCatalogueRoot(rootPath, false)
			if err != nil {
				t.Fatal(err)
			}
			defer root.close()
			privateRoot, err := s.hybridCatalogueMountRoot()
			if err != nil {
				t.Fatal(err)
			}
			directory, err := root.ownerDirectory("owner", true)
			if err != nil {
				t.Fatal(err)
			}
			defer directory.Close()
			temporary := ".reporeach-link-journalfixture"
			target := filepath.Join(privateRoot, "owner", "repo")
			if err := unix.Symlinkat(target, int(directory.Fd()), temporary); err != nil {
				t.Fatal(err)
			}
			link, err := hybridLinkAt(directory, temporary, root)
			if err != nil {
				t.Fatal(err)
			}
			link.PendingName = temporary
			manifest, err := s.readHybridCatalogueManifest(root)
			if err != nil {
				t.Fatal(err)
			}
			manifest.Links["owner/repo"] = link
			if err := s.writeHybridCatalogueManifest(root, manifest); err != nil {
				t.Fatal(err)
			}
			if published {
				if err := unix.Linkat(int(directory.Fd()), temporary, int(directory.Fd()), "repo", 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.syncHybridCatalogue(rootPath, []hybridCatalogueEntry{virtualHybridEntry("repo")}); err != nil {
				t.Fatal(err)
			}
			assertHybridLink(t, filepath.Join(rootPath, "owner", "repo"), target)
			if _, err := os.Lstat(filepath.Join(rootPath, "owner", temporary)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary remains: %v", err)
			}
			saved, err := s.readHybridCatalogueManifest(root)
			if err != nil || saved.Links["owner/repo"].PendingName != "" {
				t.Fatalf("journal incomplete: %v", err)
			}
		})
	}
}

func TestHybridCatalogueReleaseOwnedLinkAllowsPhysicalMaterialization(t *testing.T) {
	s, root := hybridCatalogueFixture(t)
	entry := virtualHybridEntry("repo")
	if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{entry}); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseHybridCatalogueLink(root, "owner", "repo"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "owner", "repo")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	entry.LocalPath = path
	if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{entry}); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseHybridCatalogueLink(root, "owner", "repo"); !errors.Is(err, errHybridCatalogueConflict) {
		t.Fatalf("ordinary directory became removable: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("physical checkout removed: %v", err)
	}
}

func TestHybridCatalogueRejectsInvalidPlanBeforeLeafPublication(t *testing.T) {
	s, root := hybridCatalogueFixture(t)
	entry := virtualHybridEntry("../escape")
	if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{entry}); err == nil {
		t.Fatal("path traversal accepted")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid plan changed chosen root: %v %v", entries, err)
	}
	if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{virtualHybridEntry("repo"), virtualHybridEntry("repo")}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate accepted: %v", err)
	}
}

func TestHybridCatalogueMissingOwnerRemovesOnlyStaleOwnershipRecords(t *testing.T) {
	s, root := hybridCatalogueFixture(t)
	if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{virtualHybridEntry("repo")}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "owner", "repo")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "owner")); err != nil {
		t.Fatal(err)
	}
	if err := s.syncHybridCatalogue(root, nil); err != nil {
		t.Fatal(err)
	}
	opened, err := openHybridCatalogueRoot(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	manifest, err := s.readHybridCatalogueManifest(opened)
	if err != nil || len(manifest.Links) != 0 {
		t.Fatalf("missing owner leaves stale ownership: %v %v", manifest, err)
	}
}

func TestHybridCatalogueMissingPhysicalCheckoutIsNotReportedAsPublished(t *testing.T) {
	s, root := hybridCatalogueFixture(t)
	entry := virtualHybridEntry("physical")
	entry.LocalPath = filepath.Join(root, "owner", "physical")
	if err := s.syncHybridCatalogue(root, []hybridCatalogueEntry{entry}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing checkout accepted: %v", err)
	}
}

func TestHybridCataloguePrivateMountIsSeparateAndNotBrowsable(t *testing.T) {
	s, root := hybridCatalogueFixture(t)
	privateRoot, err := s.hybridCatalogueMountRoot()
	if err != nil {
		t.Fatal(err)
	}
	if pathsOverlap(root, privateRoot) || privateRoot != filepath.Join(s.opts.StateDir, "native-catalogue", "volume") {
		t.Fatalf("private volume does not have separate private location: %q", privateRoot)
	}
	f := newFakeFSKitMount(t)
	f.ops.hidden = true
	mount := f.mount(t)
	if len(f.commands) != 1 || len(f.commands[0]) != 8 || f.commands[0][4] != "-o" || f.commands[0][5] != "nobrowse" {
		t.Fatalf("private mount is not hidden from Finder: %v", f.commands)
	}
	if err := mount.Unmount(); err != nil {
		t.Fatal(err)
	}
}
