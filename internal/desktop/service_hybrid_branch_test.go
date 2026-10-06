package desktop

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func hybridBranchFixture(t *testing.T) (*Service, Repository, model.RepoConfig) {
	t.Helper()
	source, _ := adoptionSource(t)
	s := newDesktopTestService(t, "exit 99")
	s.hybridCatalogue = true
	if s.preview == nil {
		s.previewGit = gitstore.New(nil)
		var err error
		s.preview, err = NewPreviewCache(s.ctx, s.opts.StateDir, s.previewGit, s.github)
		if err != nil {
			t.Fatal(err)
		}
	}
	// A direct virtual remote fixture exercises first writable initialization;
	// adopting an ordinary checkout in hybrid mode deliberately stays local.
	repo := Repository{ID: "BranchFixture/project", Owner: "BranchFixture", Name: "project", CloneURL: source,
		DefaultBranch: "trunk", Source: "manual", State: "virtual"}
	seedDesktopCatalogue(t, s, repo)
	if _, err := s.ensureRepository(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 1 || configs[0].RequiredCommit == "" {
		t.Fatalf("fixture did not acquire a verified preview baseline: configs=%+v err=%v", configs, err)
	}
	return s, repo, configs[0]
}

func branchPrivateGit(t *testing.T, s *Service, cfg model.RepoConfig, worktree string, args ...string) []byte {
	t.Helper()
	options := []string{"--git-dir", cfg.GitDir}
	if worktree != "" {
		options = append(options, "--work-tree", worktree)
	}
	options = append(options, "-c", "user.name=Branch fixture", "-c", "user.email=fixture@example.invalid")
	return adoptionGit(t, s.opts.StateDir, append(options, args...)...)
}

func TestHybridVerifiedCheckoutStartsOnLocalBranchAndCommitsSurviveCheckout(t *testing.T) {
	s, _, cfg := hybridBranchFixture(t)
	if branch := strings.TrimSpace(string(branchPrivateGit(t, s, cfg, "", "symbolic-ref", "HEAD"))); branch != "refs/heads/trunk" {
		t.Fatalf("new verified writable checkout was detached: %q", branch)
	}
	if head := strings.TrimSpace(string(branchPrivateGit(t, s, cfg, "", "rev-parse", "HEAD"))); head != cfg.RequiredCommit {
		t.Fatal("attaching the local branch changed the verified initial commit")
	}
	worktree := t.TempDir()
	branchPrivateGit(t, s, cfg, worktree, "checkout", "-f", "trunk")
	committed, alternate := []byte("committed through managed branch\n"), []byte("alternate branch bytes\n")
	writeAndCommit := func(content []byte, message string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(worktree, "tracked.txt"), content, 0o644); err != nil {
			t.Fatal(err)
		}
		branchPrivateGit(t, s, cfg, worktree, "add", "tracked.txt")
		branchPrivateGit(t, s, cfg, worktree, "commit", "-m", message)
	}
	writeAndCommit(committed, "managed branch commit")
	branchPrivateGit(t, s, cfg, worktree, "checkout", "-b", "acceptance")
	writeAndCommit(alternate, "alternate commit")
	for _, check := range []struct {
		branch  string
		content []byte
	}{{"trunk", committed}, {"acceptance", alternate}} {
		branchPrivateGit(t, s, cfg, worktree, "checkout", check.branch)
		content, err := os.ReadFile(filepath.Join(worktree, "tracked.txt"))
		if err != nil || !bytes.Equal(content, check.content) {
			t.Fatalf("checkout %s returned initial source bytes instead of branch commit: content=%q err=%v", check.branch, content, err)
		}
	}
}

func TestHybridReopeningPreparedCheckoutPreservesDetachedHeadAndStagedIndex(t *testing.T) {
	s, repo, cfg := hybridBranchFixture(t)
	worktree := t.TempDir()
	branchPrivateGit(t, s, cfg, worktree, "checkout", "-f", "--detach", "HEAD")
	if err := os.WriteFile(filepath.Join(worktree, "tracked.txt"), []byte("retained staged local work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	branchPrivateGit(t, s, cfg, worktree, "add", "tracked.txt")
	before := make(map[string][]byte)
	for _, file := range []string{"HEAD", "index", "refs/heads/trunk"} {
		content, err := os.ReadFile(filepath.Join(cfg.GitDir, file))
		if err != nil {
			t.Fatal(err)
		}
		before[file] = content
	}
	assertPreserved := func() {
		t.Helper()
		for file, expected := range before {
			content, err := os.ReadFile(filepath.Join(cfg.GitDir, file))
			if err != nil || !bytes.Equal(content, expected) {
				t.Fatalf("opening prepared checkout reset %s: err=%v", file, err)
			}
		}
	}
	if _, err := s.ensureRepository(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	assertPreserved()
	if err := s.engine.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err := daemon.New(context.Background(), filepath.Join(s.opts.StateDir, "engine"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.engine = engine
	if err := engine.SetCatalogViewPolicy(daemon.CatalogViewPersistentWorkingTree); err != nil {
		t.Fatal(err)
	}
	engine.SetMountRoot(s.state.MountRoot)
	if _, err := s.ensureRepository(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	assertPreserved()
}
