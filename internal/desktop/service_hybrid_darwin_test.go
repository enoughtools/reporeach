//go:build darwin

package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHybridServiceAdoptsDirtyCheckoutInPlaceAndRetainsItWhenHidden(t *testing.T) {
	source, _ := adoptionSource(t)
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adoptionGit(t, source, "add", "tracked.txt")
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("unstaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "untracked.txt"), []byte{0, 255, 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	before := sourceState(t, source)
	s := newDesktopTestService(t, "exit 99")
	s.hybridCatalogue = true
	s.quiescentCatalogue = true
	status, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source, Owner: "work", Name: "existing"})
	if err != nil {
		t.Fatal(err)
	}
	repo := status.Repositories[0]
	physical, _ := filepath.EvalSymlinks(source)
	if repo.LocalPath != physical || repo.LocalKind != "adopted" || repo.State != "local" {
		t.Fatalf("adoption did not register the ordinary checkout: %+v", repo)
	}
	if configs, err := s.engine.ListRepos(context.Background()); err != nil || len(configs) != 0 {
		t.Fatalf("adoption prepared a separate managed clone: %d %v", len(configs), err)
	}
	leaf := filepath.Join(status.MountRoot, repo.Owner, repo.Name)
	assertHybridLink(t, leaf, physical)
	if err := s.SetOrganizationEnabled(context.Background(), "work", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(leaf); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hidden catalogue link retained: %v", err)
	}
	if !reflect.DeepEqual(before, sourceState(t, source)) {
		t.Fatal("adoption or hiding changed the source's index, edits, configuration or untracked files")
	}
	if err := s.SetRepositoryEnabled(context.Background(), repo.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOrganizationEnabled(context.Background(), "work", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(leaf); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("enabling organization lost the individual hidden setting")
	}
	if err := s.SetRepositoryEnabled(context.Background(), repo.ID, true); err != nil {
		t.Fatal(err)
	}
	assertHybridLink(t, leaf, physical)
	if _, err := s.Action(repo.ID, "keep"); err != nil {
		t.Fatal(err)
	}
	awaitDesktop(t, func() bool { return s.Status().Operations[0].Status != "running" })
	if operation := s.Status().Operations[0]; operation.Status != "complete" {
		t.Fatalf("keeping adopted checkout failed: %+v", operation)
	}
	if _, err := s.Action(repo.ID, "free"); err != nil {
		t.Fatal(err)
	}
	awaitDesktop(t, func() bool { return s.Status().Operations[1].Status != "running" })
	if operation := s.Status().Operations[1]; operation.Status != "failed" {
		t.Fatalf("free accepted an adopted original: %+v", operation)
	}
	if !reflect.DeepEqual(before, sourceState(t, source)) {
		t.Fatal("local Keep/Free changed an adopted original")
	}
}

func TestHybridServiceAdoptsCheckoutAlreadyInChosenFolderWithoutCoveringIt(t *testing.T) {
	source, _ := adoptionSource(t)
	s := newDesktopTestService(t, "exit 99")
	s.hybridCatalogue = true
	s.quiescentCatalogue = true
	root, _ := filepath.EvalSymlinks(filepath.Dir(source))
	s.state.MountRoot = root
	status, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source, Owner: "local", Name: "existing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Repositories) != 1 || status.Repositories[0].LocalKind != "adopted" {
		t.Fatalf("ordinary root checkout not adopted: %+v", status)
	}
	if _, err := os.Stat(filepath.Join(source, "tracked.txt")); err != nil {
		t.Fatalf("existing ordinary checkout was hidden: %v", err)
	}
}

func TestHybridServiceDetachedAdoptionRejectsBranchSwitchAndSurvivesRestart(t *testing.T) {
	source, _ := adoptionSource(t)
	adoptionGit(t, source, "checkout", "--detach", "HEAD")
	before := sourceState(t, source)
	s := newDesktopTestService(t, "exit 99")
	s.hybridCatalogue = true
	s.quiescentCatalogue = true
	if _, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source, Branch: "trunk"}); err == nil {
		t.Fatal("adoption accepted a branch switch")
	}
	if _, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(context.Background(), s.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	repo := reopened.Status().Repositories[0]
	if repo.LocalKind != "adopted" || repo.DefaultBranch != "" || repo.State != "local" {
		t.Fatalf("restart changed detached checkout metadata: %+v", repo)
	}
	if !reflect.DeepEqual(before, sourceState(t, source)) {
		t.Fatal("detached adoption changed source Git state")
	}
}
