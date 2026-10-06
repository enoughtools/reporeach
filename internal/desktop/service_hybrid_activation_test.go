package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/jacobsa/fuse/fuseops"
)

func TestHybridActivationRefusesOwnedRepositoryLockWithoutWaiting(t *testing.T) {
	s := &Service{locks: make(map[string]chan struct{})}
	unlock, err := s.lockRepo(context.Background(), "Fixture/Project")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { unlock() }()
	result := make(chan error, 1)
	go func() {
		release, err := s.lockRepoForActivation(context.Background(), "fixture/project")
		if release != nil {
			release()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, syscall.EBUSY) {
			t.Fatalf("activation behind an action = %v, want EBUSY", err)
		}
	case <-time.After(time.Second):
		t.Fatal("filesystem activation waited behind an action that may drain its mount")
	}
	// The refusal must not consume the lock's token or replace its identity.
	unlock()
	unlock = func() {}
	release, err := s.lockRepoForActivation(context.Background(), "fixture/project")
	if err != nil {
		t.Fatalf("activation after action completion: %v", err)
	}
	release()
	owned, err := s.lockRepo(context.Background(), "FIXTURE/PROJECT")
	if err != nil {
		t.Fatal(err)
	}
	owned()
}

func TestHybridActivationRefusesLifecycleGatesAndCanceledRequests(t *testing.T) {
	for _, gate := range []string{"maintenance", "recovery", "closing", "quit"} {
		t.Run(gate, func(t *testing.T) {
			s := &Service{locks: make(map[string]chan struct{})}
			switch gate {
			case "maintenance":
				s.maintenance = true
			case "recovery":
				s.recoveryRequired = true
			case "closing":
				s.closing = true
			case "quit":
				s.quitPrepared = true
			}
			if release, err := s.lockRepoForActivation(context.Background(), "fixture/project"); !errors.Is(err, syscall.EBUSY) || release != nil {
				t.Fatalf("activation during %s = %v, release present=%v", gate, err, release != nil)
			}
			if len(s.locks) != 0 {
				t.Fatal("refused activation created a repository lock")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if release, err := s.lockRepoForActivation(ctx, "fixture/project"); !errors.Is(err, context.Canceled) || release != nil {
				t.Fatalf("canceled activation = %v, release present=%v", err, release != nil)
			}
		})
	}
}

func hybridPreviewTestService(t *testing.T, responsePath string) (*Service, Repository) {
	t.Helper()
	t.Setenv("HYBRID_PREVIEW_RESPONSE", responsePath)
	s := newDesktopTestService(t, `cat "$HYBRID_PREVIEW_RESPONSE"`)
	s.hybridCatalogue = true
	s.quiescentCatalogue = true
	if s.preview == nil {
		s.preview = newPreviewFixtureCache(t, s.opts.StateDir, s.github)
	}
	repo := previewGitHubRepo("fixture", "project")
	repo.State = "virtual"
	seedDesktopCatalogue(t, s, repo)
	return s, repo
}

func assertHybridPreviewHasNoWritableStorage(t *testing.T, s *Service, repo Repository) {
	t.Helper()
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("metadata-only browsing prepared writable repositories: count=%d err=%v", len(configs), err)
	}
	if _, err := os.Lstat(filepath.Join(s.opts.StateDir, "engine", "repos", engineName(repo.ID), "git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata-only browsing created the writable Git directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.preview.root, previewKey(repo), "git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GitHub metadata-only browsing created a preview clone: %v", err)
	}
}

func TestHybridColdGitDiscoveryUsesExactManagedPointerMetadataWithoutPreparing(t *testing.T) {
	responsePath := filepath.Join(t.TempDir(), "response.json")
	if err := os.WriteFile(responsePath, []byte(previewRootJSON([]githubPreviewEntry{
		previewEntry("README.md", "blob", previewBlob, 0o100644, 19),
	}, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	s, repo := hybridPreviewTestService(t, responsePath)
	entry := catalogfs.Entry{ID: repo.ID, Owner: repo.Owner, Name: repo.Name}
	directory, err := s.cataloguePreview(context.Background(), entry, ".")
	if err != nil {
		t.Fatal(err)
	}
	gitFile := []byte("gitdir: " + filepath.Join(s.opts.StateDir, "engine", "repos", engineName(repo.ID), "git") + "\n")
	if directory.GitFileSize != uint64(len(gitFile)) || directory.Revision != previewCommit || len(directory.Entries) != 1 {
		t.Fatalf("cold root metadata = %+v, want exact .git length %d", directory, len(gitFile))
	}
	activations := 0
	fs, err := catalogfs.NewWithPreview([]catalogfs.Entry{entry}, func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		activations++
		return nil, errors.New("cold metadata must not activate a writable repository")
	}, nil, s.cataloguePreview)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Destroy()
	parent := fuseops.InodeID(fuseops.RootInodeID)
	for _, name := range []string{repo.Owner, repo.Name, ".git"} {
		op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
		known, err := fs.LookUpMetadata(context.Background(), op)
		if err != nil || !known {
			t.Fatalf("cold lookup %s: size known=%v err=%v", name, known, err)
		}
		parent = op.Entry.Child
	}
	attrs := &fuseops.GetInodeAttributesOp{Inode: parent}
	known, err := fs.GetMetadataAttributes(context.Background(), attrs)
	if err != nil || !known || attrs.Attributes.Size != uint64(len(gitFile)) || !attrs.Attributes.Mode.IsRegular() {
		t.Fatalf("cold .git attributes = %+v, size known=%v err=%v", attrs.Attributes, known, err)
	}
	if activations != 0 {
		t.Fatalf("cold Git discovery activated the engine %d times", activations)
	}
	assertHybridPreviewHasNoWritableStorage(t, s, repo)
}

func TestHybridPreviewRefreshPreservesBaselineWhenCanceledOrBusyThenRenewsWithoutPreparation(t *testing.T) {
	responsePath := filepath.Join(t.TempDir(), "response.json")
	oldRoot := previewRootJSON([]githubPreviewEntry{previewEntry("old.txt", "blob", previewBlob, 0o100644, 7)}, 1)
	if err := os.WriteFile(responsePath, []byte(oldRoot), 0o600); err != nil {
		t.Fatal(err)
	}
	s, repo := hybridPreviewTestService(t, responsePath)
	old, err := s.preview.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.refreshPreview(ctx, repo); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh = %v, want canceled", err)
	}
	if got := s.preview.SelectedRepositoryCommit(repo); got != previewCommit {
		t.Fatalf("canceled refresh replaced the selected baseline: %q", got)
	}
	old.mu.Lock()
	err = s.refreshPreview(context.Background(), repo)
	old.mu.Unlock()
	if !errors.Is(err, ErrPreviewInUse) {
		t.Fatalf("refresh with a metadata operation in flight = %v, want in use", err)
	}
	if got := s.preview.SelectedRepositoryCommit(repo); got != previewCommit {
		t.Fatalf("refused refresh replaced the selected baseline: %q", got)
	}
	s.mu.Lock()
	maintenance := s.maintenance
	s.mu.Unlock()
	if maintenance {
		t.Fatal("refused or canceled refresh stranded catalogue maintenance")
	}
	const newCommit = "6666666666666666666666666666666666666666"
	newRoot := previewRootJSON([]githubPreviewEntry{previewEntry("new.txt", "blob", previewBlob, 0o100644, 29)}, 1)
	newRoot = strings.ReplaceAll(newRoot, previewCommit, newCommit)
	newRoot = strings.ReplaceAll(newRoot, previewRootTree, "7777777777777777777777777777777777777777")
	if err := os.WriteFile(responsePath, []byte(newRoot), 0o600); err != nil {
		t.Fatal(err)
	}
	op, err := s.Action(repo.ID, "refresh")
	if err != nil {
		t.Fatal(err)
	}
	awaitDesktop(t, func() bool {
		for _, operation := range s.Status().Operations {
			if operation.ID == op.ID {
				return operation.Status != "running"
			}
		}
		return false
	})
	for _, operation := range s.Status().Operations {
		if operation.ID == op.ID && operation.Status != "complete" {
			t.Fatalf("metadata refresh action = %+v", operation)
		}
	}
	fresh, err := s.preview.Acquire(context.Background(), repo)
	if err != nil || fresh.Commit != newCommit || fresh == old {
		t.Fatalf("explicit metadata refresh did not select a new immutable preview: %+v err=%v", fresh, err)
	}
	oldRevision, oldEntries, err := old.Directory(context.Background(), ".")
	if err != nil || oldRevision != previewCommit || len(oldEntries) != 1 || oldEntries[0].Path != "old.txt" {
		t.Fatalf("refresh changed an existing immutable preview: %q %+v err=%v", oldRevision, oldEntries, err)
	}
	if state := s.Status().Repositories[0].State; state != "virtual" {
		t.Fatalf("metadata-only refresh changed virtual repository state to %q", state)
	}
	assertHybridPreviewHasNoWritableStorage(t, s, repo)
}
