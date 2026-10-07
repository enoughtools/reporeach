package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func adoptionGit(t *testing.T, directory string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = directory
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Git fixture %v: %v: %s", args, err, output)
	}
	return output
}

func adoptionSource(t *testing.T) (string, []byte) {
	t.Helper()
	// A real private fixture makes these tests independent of user credentials,
	// configuration, hooks, and remote services.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	directory := filepath.Join(t.TempDir(), "project.git")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	adoptionGit(t, directory, "init", "--initial-branch=trunk")
	adoptionGit(t, directory, "config", "user.name", "Fixture")
	adoptionGit(t, directory, "config", "user.email", "fixture@example.invalid")
	binary := []byte{0, 255, 128, 'A', '\n', 0, 1, 2, 254}
	for name, content := range map[string][]byte{"binary.dat": binary, "tracked.txt": []byte("committed\n"), ".gitignore": []byte("ignored.dat\n")} {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	adoptionGit(t, directory, "add", ".")
	adoptionGit(t, directory, "commit", "-m", "fixture")
	adoptionGit(t, directory, "branch", "feature/alternate")
	return directory, binary
}

func sourceState(t *testing.T, source string) map[string][]byte {
	t.Helper()
	state := map[string][]byte{"status": adoptionGit(t, source, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored")}
	for _, name := range []string{".git/HEAD", ".git/index", ".git/config", "tracked.txt", "binary.dat", "untracked.txt", "ignored.dat"} {
		content, err := os.ReadFile(filepath.Join(source, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		state[name] = content
	}
	return state
}

func TestAdoptNonbareSourceUsesCommittedTreeAndLeavesOriginalUntouched(t *testing.T) {
	source, binary := adoptionSource(t)
	for name, content := range map[string][]byte{"tracked.txt": []byte("staged\n"), "untracked.txt": []byte("untracked local work"), "ignored.dat": []byte("ignored local work")} {
		if err := os.WriteFile(filepath.Join(source, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	adoptionGit(t, source, "add", "tracked.txt")
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("unstaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hookSentinel := filepath.Join(t.TempDir(), "hook-ran")
	t.Setenv("ADOPTION_HOOK_SENTINEL", hookSentinel)
	if err := os.WriteFile(filepath.Join(source, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch \"$ADOPTION_HOOK_SENTINEL\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := sourceState(t, source)
	// A terminal can inherit an index binding from another Git operation. The
	// service must use its private index even while preserving native auth.
	t.Setenv("GIT_INDEX_FILE", filepath.Join(source, ".git", "index"))
	globalBefore := []byte("[credential]\n\thelper = native-fixture\n")
	if err := os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"), globalBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	ghSentinel := filepath.Join(t.TempDir(), "gh-ran")
	t.Setenv("ADOPTION_GH_SENTINEL", ghSentinel)
	s := newDesktopTestService(t, `touch "$ADOPTION_GH_SENTINEL"; exit 4`)
	status, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source})
	if err != nil {
		t.Fatal(err)
	}
	if status.Account != nil || len(status.Repositories) != 1 {
		t.Fatalf("adoption required GitHub or returned an unexpected catalogue: %+v", status)
	}
	repo := status.Repositories[0]
	physicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != "local/project" || repo.Source != "manual" || repo.CloneURL != physicalSource || repo.HTMLURL != "" || repo.DefaultBranch != "trunk" || repo.State != "virtual" {
		t.Fatalf("manual source metadata = %+v", repo)
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("adoption downloaded the source eagerly: %+v, %v", configs, err)
	}
	if !reflect.DeepEqual(before, sourceState(t, source)) {
		t.Fatal("adoption changed the original checkout's HEAD, index, config, or local work")
	}
	if _, err := s.ensureRepository(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	configs, err = s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("separate preparation failed: %+v, %v", configs, err)
	}
	cfg := configs[0]
	if cfg.CredentialHelper != "" || pathsLexicallyOverlap(cfg.GitDir, source) {
		t.Fatalf("manual checkout borrowed a GitHub helper or the source storage: %+v", cfg)
	}
	if got := adoptionGit(t, s.opts.StateDir, "--git-dir", cfg.GitDir, "show", "HEAD:binary.dat"); !bytes.Equal(got, binary) {
		t.Fatal("separate checkout changed committed binary bytes")
	}
	if got := adoptionGit(t, s.opts.StateDir, "--git-dir", cfg.GitDir, "show", "HEAD:tracked.txt"); string(got) != "committed\n" {
		t.Fatalf("uncommitted source changes appeared in the separate checkout: %q", got)
	}
	if !reflect.DeepEqual(before, sourceState(t, source)) {
		t.Fatal("preparation changed the original checkout")
	}
	for _, path := range []string{hookSentinel, ghSentinel} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("adoption invoked a source hook or GitHub")
		}
	}
	globalAfter, err := os.ReadFile(os.Getenv("GIT_CONFIG_GLOBAL"))
	if err != nil || !bytes.Equal(globalBefore, globalAfter) {
		t.Fatal("adoption changed the user's Git configuration")
	}
}

func TestAdoptBareSourceExplicitBranchAndPersistentCatalogue(t *testing.T) {
	source, _ := adoptionSource(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	adoptionGit(t, filepath.Dir(bare), "clone", "--bare", source, bare)
	s := newDesktopTestService(t, "exit 99")
	status, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: "file://" + bare, Owner: "Work.Group", Name: "mirror", Branch: "feature/alternate"})
	if err != nil {
		t.Fatal(err)
	}
	repo := status.Repositories[0]
	physicalBare, err := filepath.EvalSymlinks(bare)
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != "Work.Group/mirror" || repo.CloneURL != physicalBare || repo.DefaultBranch != "feature/alternate" {
		t.Fatalf("explicit identity and branch were not honored: %+v", repo)
	}
	loaded, err := readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
	if err != nil || !reflect.DeepEqual(loaded.Repositories, status.Repositories) {
		t.Fatalf("manual catalogue could not reload: %+v, %v", loaded.Repositories, err)
	}
	if _, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: bare, Name: "missing", Branch: "does-not-exist"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing branch was accepted: %v", err)
	}
	if len(s.Status().Repositories) != 1 {
		t.Fatal("failed branch probe changed the catalogue")
	}
}

func TestAdoptRejectsCollisionsAndCanonicalizesOwner(t *testing.T) {
	source, _ := adoptionSource(t)
	s := newDesktopTestService(t, "exit 99")
	seedDesktopCatalogue(t, s, catalogueRepository("Team", "existing"))
	request := AdoptionRequest{RemoteURL: source, Owner: "TEAM", Name: "other"}
	status, err := s.Adopt(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if status.Repositories[1].ID != "Team/other" {
		t.Fatalf("existing owner casing was not retained: %+v", status.Repositories)
	}
	status.Repositories[1].Pinned, status.Repositories[1].Disabled = true, true
	seedDesktopCatalogue(t, s, status.Repositories...)
	before := s.Status()
	if status, err := s.Adopt(context.Background(), request); err != nil || !reflect.DeepEqual(status, before) {
		t.Fatalf("repeated adoption changed saved policy: %+v, %v", status, err)
	}
	request.Name = "EXISTING"
	if _, err := s.Adopt(context.Background(), request); err == nil {
		t.Fatal("case-insensitive discovered identity collision was accepted")
	}
	request.Name, request.Branch = "other", "feature/alternate"
	if _, err := s.Adopt(context.Background(), request); err == nil {
		t.Fatal("changing a managed remote's branch through adoption was accepted")
	}
	if !reflect.DeepEqual(before, s.Status()) {
		t.Fatal("collision changed the saved catalogue")
	}
}

func TestAdoptManualEntriesAndVisibilitySurviveGitHubDiscovery(t *testing.T) {
	source, _ := adoptionSource(t)
	s := newDesktopTestService(t, `
case "$*" in
  'api --hostname github.com user') printf '%s' '{"login":"octocat"}' ;;
  *) printf '%s' '[{"name":"mirror","owner":{"login":"LOCAL"},"default_branch":"main"},{"name":"new","owner":{"login":"octocat"},"default_branch":"main"}]' ;;
esac
`)
	status, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source, Name: "mirror"})
	if err != nil {
		t.Fatal(err)
	}
	manual := status.Repositories[0]
	manual.Disabled, manual.Pinned, manual.Error = true, true, "local error retained"
	seedDesktopCatalogue(t, s, manual)
	status, err = s.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Repositories) != 2 || !reflect.DeepEqual(status.Repositories[0], manual) {
		t.Fatalf("GitHub discovery replaced a manual source or saved policy: %+v", status.Repositories)
	}
	loaded, err := readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
	if err != nil || !reflect.DeepEqual(loaded.Repositories, status.Repositories) {
		t.Fatalf("discovery result failed to persist: %v", err)
	}
}

func TestAdoptRejectsOwnFoldersBeforeAnyGitProbe(t *testing.T) {
	s := newDesktopTestService(t, "exit 99")
	alias := filepath.Join(t.TempDir(), "state-alias")
	if err := os.Symlink(s.opts.StateDir, alias); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{s.opts.StateDir, filepath.Join(s.opts.StateDir, "engine"), s.opts.MountRoot, filepath.Join(s.opts.MountRoot, "owner", "repo"), filepath.Dir(s.opts.StateDir), alias} {
		t.Run(source, func(t *testing.T) {
			_, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source, Name: "alias"})
			if err == nil || !strings.Contains(err.Error(), "outside EnoughRepos") {
				t.Fatalf("own-folder source was not rejected at the path boundary: %v", err)
			}
		})
	}
	if len(s.Status().Repositories) != 0 {
		t.Fatal("unsafe source added a catalogue entry")
	}
}

func TestAdoptLocalAliasesAndLinkedWorktreeMetadata(t *testing.T) {
	source, _ := adoptionSource(t)
	s := newDesktopTestService(t, "exit 99")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	status, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: alias, Name: "alias"})
	if err != nil {
		t.Fatal(err)
	}
	physicalSource, err := filepath.EvalSymlinks(source)
	if err != nil || status.Repositories[0].CloneURL != physicalSource {
		t.Fatalf("source alias was not fixed to its physical repository: %+v, %v", status.Repositories, err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	adoptionGit(t, source, "worktree", "add", linked, "feature/alternate")
	status, err = s.Adopt(context.Background(), AdoptionRequest{RemoteURL: linked, Name: "linked"})
	if err != nil {
		t.Fatalf("ordinary linked worktree was refused: %v", err)
	}
	if len(status.Repositories) != 2 || status.Repositories[1].DefaultBranch != "feature/alternate" {
		t.Fatalf("linked worktree's committed branch was not discovered: %+v", status.Repositories)
	}
	for _, layout := range []string{"gitfile", "gitdir symlink", "commondir"} {
		t.Run(layout, func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), "source")
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(s.opts.StateDir, "engine")
			switch layout {
			case "gitfile":
				if err := os.WriteFile(filepath.Join(outside, ".git"), []byte("gitdir: "+target+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "gitdir symlink":
				if err := os.Symlink(target, filepath.Join(outside, ".git")); err != nil {
					t.Fatal(err)
				}
			case "commondir":
				gitDir := filepath.Join(outside, ".git")
				if err := os.Mkdir(gitDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte(target+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := s.Status()
			if _, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: outside, Name: "protected"}); err == nil || !strings.Contains(err.Error(), "outside EnoughRepos") {
				t.Fatalf("protected Git metadata redirect was accepted: %v", err)
			}
			if !reflect.DeepEqual(before, s.Status()) {
				t.Fatal("refused Git metadata alias changed the catalogue")
			}
		})
	}
}

func TestAdoptMetadataRedirectsResolveDotSegmentsAfterSymlinks(t *testing.T) {
	s := newDesktopTestService(t, "exit 99")
	for _, layout := range []string{"gitdir symlink", "gitfile", "commondir", "source path"} {
		t.Run(layout, func(t *testing.T) {
			outside := t.TempDir()
			source := filepath.Join(outside, "source")
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			// Cleaning pivot/../repo produces this safe decoy. The kernel
			// follows pivot first, producing protected state/repo instead.
			adoptionGit(t, outside, "init", "--bare", filepath.Join(outside, "repo"))
			child := filepath.Join(s.opts.StateDir, "child")
			if err := os.MkdirAll(child, 0o700); err != nil {
				t.Fatal(err)
			}
			adoptionGit(t, outside, "init", "--bare", filepath.Join(s.opts.StateDir, "repo"))
			if err := os.Symlink(child, filepath.Join(outside, "pivot")); err != nil {
				t.Fatal(err)
			}
			rawTarget := outside + "/pivot/../repo"
			switch layout {
			case "gitdir symlink":
				if err := os.Symlink(rawTarget, filepath.Join(source, ".git")); err != nil {
					t.Fatal(err)
				}
			case "gitfile":
				if err := os.WriteFile(filepath.Join(source, ".git"), []byte("gitdir: ../pivot/../repo\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "commondir":
				if err := os.Mkdir(filepath.Join(source, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, ".git", "commondir"), []byte("../../pivot/../repo\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "source path":
				source = rawTarget
			}
			if _, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source, Name: "protected"}); err == nil || !strings.Contains(err.Error(), "outside EnoughRepos") {
				t.Fatalf("dot-segment redirect reached protected source metadata: %v", err)
			}
		})
	}
}

func TestAdoptLocalDotSegmentsPreserveNativeSymlinkMeaning(t *testing.T) {
	source, _ := adoptionSource(t)
	child := filepath.Join(filepath.Dir(source), "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(child, filepath.Join(outside, "pivot")); err != nil {
		t.Fatal(err)
	}
	s := newDesktopTestService(t, "exit 99")
	raw := outside + "/pivot/../" + filepath.Base(source)
	status, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: raw})
	if err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(source)
	if err != nil || status.Repositories[0].CloneURL != physical || status.Repositories[0].DefaultBranch != "trunk" {
		t.Fatalf("adoption normalized away a symlink before native resolution: %+v, %v", status.Repositories, err)
	}
}

func TestAdoptDeferredPreparationRechecksChangedGitMetadata(t *testing.T) {
	source, _ := adoptionSource(t)
	s := newDesktopTestService(t, "exit 99")
	status, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source})
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(source, ".git")
	if err := os.Rename(gitDir, gitDir+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitDir, []byte("gitdir: "+filepath.Join(s.opts.StateDir, "engine")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureRepository(context.Background(), status.Repositories[0].ID); err == nil || !strings.Contains(err.Error(), "outside EnoughRepos") {
		t.Fatalf("deferred acquisition followed changed source metadata into private storage: %v", err)
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("unsafe deferred acquisition registered a clone: %+v, %v", configs, err)
	}
}

func TestAdoptRefreshRechecksSourceAndRetainsCachedCheckout(t *testing.T) {
	source, binary := adoptionSource(t)
	s := newDesktopTestService(t, "exit 99")
	status, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source})
	if err != nil {
		t.Fatal(err)
	}
	repo := status.Repositories[0]
	if _, err := s.ensureRepository(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(source, ".git")
	if err := os.Rename(gitDir, gitDir+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitDir, []byte("gitdir: "+filepath.Join(s.opts.StateDir, "engine")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	op, err := s.Action(repo.ID, "refresh")
	if err != nil {
		t.Fatal(err)
	}
	awaitDesktop(t, func() bool {
		for _, current := range s.Status().Operations {
			if current.ID == op.ID {
				return current.Status != "running"
			}
		}
		return false
	})
	status = s.Status()
	if status.Operations[0].Status != "failed" || !strings.Contains(status.Operations[0].Error, "outside EnoughRepos") {
		t.Fatalf("refresh followed changed source metadata into private storage: %+v", status.Operations)
	}
	if _, err := s.ensureRepository(context.Background(), repo.ID); err != nil {
		t.Fatalf("cached checkout became unavailable after refusing unsafe refresh: %v", err)
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("refused refresh discarded cached storage: %+v, %v", configs, err)
	}
	if got := adoptionGit(t, s.opts.StateDir, "--git-dir", configs[0].GitDir, "show", "HEAD:binary.dat"); !bytes.Equal(got, binary) {
		t.Fatal("refused refresh changed cached committed bytes")
	}
}

func TestAdoptFreeRefusesPrivateSourceAliasAndRetainsPin(t *testing.T) {
	source, binary := adoptionSource(t)
	s := newDesktopTestService(t, "exit 99")
	status, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source})
	if err != nil {
		t.Fatal(err)
	}
	repo := status.Repositories[0]
	keep, err := s.Action(repo.ID, "keep")
	if err != nil {
		t.Fatal(err)
	}
	awaitDesktop(t, func() bool {
		for _, op := range s.Status().Operations {
			if op.ID == keep.ID {
				return op.Status != "running"
			}
		}
		return false
	})
	status = s.Status()
	if status.Operations[0].Status != "complete" || !status.Repositories[0].Pinned {
		t.Fatalf("fixture keep did not complete: %+v", status)
	}
	before := status.Repositories[0]
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("fixture private checkout missing: %+v, %v", configs, err)
	}
	privateGitDir := configs[0].GitDir
	gitDir := filepath.Join(source, ".git")
	if err := os.Rename(gitDir, gitDir+".retained"); err != nil {
		t.Fatal(err)
	}
	// The apparent recovery remote now refers to the very private clone Free
	// would remove. It must not count as an independent source of recovery.
	if err := os.WriteFile(gitDir, []byte("gitdir: "+privateGitDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	free, err := s.Action(repo.ID, "free")
	if err != nil {
		t.Fatal(err)
	}
	awaitDesktop(t, func() bool {
		for _, op := range s.Status().Operations {
			if op.ID == free.ID {
				return op.Status != "running"
			}
		}
		return false
	})
	status = s.Status()
	if last := status.Operations[len(status.Operations)-1]; last.Status != "failed" || !strings.Contains(last.Error, "outside EnoughRepos") {
		t.Fatalf("free accepted its own private clone as the remote recovery source: %+v", last)
	}
	retained := status.Repositories[0]
	if !retained.Pinned || retained.State != before.State || retained.DownloadedBytes != before.DownloadedBytes {
		t.Fatalf("refused free changed pin or storage state: before=%+v after=%+v", before, retained)
	}
	if _, err := s.ensureRepository(context.Background(), repo.ID); err != nil {
		t.Fatalf("cached checkout became unavailable after refusing free: %v", err)
	}
	if got := adoptionGit(t, s.opts.StateDir, "--git-dir", privateGitDir, "show", "HEAD:binary.dat"); !bytes.Equal(got, binary) {
		t.Fatal("refused free deleted or changed the cached Git objects")
	}
	loaded, err := readState(filepath.Join(s.opts.StateDir, "catalogue.json"), s.opts.MountRoot)
	if err != nil || !loaded.Repositories[0].Pinned {
		t.Fatalf("refused free did not preserve durable pin intent: %+v, %v", loaded.Repositories, err)
	}
}

func TestAdoptionPrivateGitConfigIgnoresInheritedRepositoryBindings(t *testing.T) {
	source, _ := adoptionSource(t)
	s := newDesktopTestService(t, "exit 99")
	status, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureRepository(context.Background(), status.Repositories[0].ID); err != nil {
		t.Fatal(err)
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("fixture private checkout missing: %+v, %v", configs, err)
	}
	sourceConfig := filepath.Join(source, ".git", "config")
	beforeSource, err := os.ReadFile(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel-config")
	beforeSentinel := []byte("[core]\n\tworktree = /original/sentinel\n")
	if err := os.WriteFile(sentinel, beforeSentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_COMMON_DIR", filepath.Join(source, ".git"))
	t.Setenv("GIT_CONFIG", sentinel)
	worktree := filepath.Join(s.opts.MountRoot, "local", "updated-worktree")
	if err := configureGitWorktree(context.Background(), configs[0].GitDir, worktree); err != nil {
		t.Fatal(err)
	}
	for path, before := range map[string][]byte{sourceConfig: beforeSource, sentinel: beforeSentinel} {
		content, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(content, before) {
			t.Fatalf("private configuration rewrote an inherited configuration target: %s", filepath.Base(path))
		}
	}
	cmd := exec.Command("git", "--git-dir", configs[0].GitDir, "config", "--local", "--get", "core.worktree")
	cmd.Env = nativeAdoptionEnvironment(os.Environ())
	value, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(value)) != worktree {
		t.Fatalf("configuration was not applied to the private clone: %q, %v", value, err)
	}
}

func TestAdoptInvalidRequestsNeverPersistCredentials(t *testing.T) {
	s := newDesktopTestService(t, "exit 99")
	for _, request := range []AdoptionRequest{
		{RemoteURL: "https://user:ghp_test_secret@github.com/owner/repo.git"},
		{RemoteURL: "https://github.com/owner/repo.git", Owner: "../escape"},
		{RemoteURL: "https://github.com/owner/repo.git", Name: "a/b"},
		{RemoteURL: "https://github.com/owner/repo.git", Branch: "@{-1}"},
		{RemoteURL: "ext::sh -c anything"},
	} {
		if _, err := s.Adopt(context.Background(), request); err == nil || strings.Contains(err.Error(), "ghp_test_secret") {
			t.Fatalf("unsafe request was accepted or credentials appeared in error: %v", err)
		}
	}
	content, err := os.ReadFile(filepath.Join(s.opts.StateDir, "catalogue.json"))
	if err != nil || bytes.Contains(content, []byte("ghp_test_secret")) {
		t.Fatal("unsafe adoption persisted credentials")
	}
}

func TestAdoptionInfersIdentityForStandardGitTransports(t *testing.T) {
	for _, fixture := range []struct {
		remote, owner, name, web string
	}{
		{"https://github.com/Team/project.git", "Team", "project", "https://github.com/Team/project"},
		{"http://git.internal:8080/group/project.git", "group", "project", "http://git.internal:8080/group/project"},
		{"git://git.example.invalid/group/project.git", "group", "project", ""},
		{"https://gitlab.com/company/nested/project.git", "nested", "project", "https://gitlab.com/company/nested/project"},
		{"ssh://git@git.example.invalid:2222/group/project.git", "group", "project", ""},
		{"git@git.example.invalid:group/project.git", "group", "project", ""},
		{"git@[2001:db8::1]:group/project.git", "group", "project", ""},
		{"ssh://git@[2001:db8::1]:2222/group/project.git", "group", "project", ""},
		{"git.example.invalid:project.git", "git.example.invalid", "project", ""},
	} {
		t.Run(fixture.remote, func(t *testing.T) {
			remote, err := parseAdoptionRemote(fixture.remote)
			if err != nil || remote.url != fixture.remote || remote.owner != fixture.owner || remote.name != fixture.name || remote.htmlURL != fixture.web {
				t.Fatalf("standard Git source inference = %+v, %v", remote, err)
			}
		})
	}
}

func TestAdoptRollsBackFailedPersistence(t *testing.T) {
	source, _ := adoptionSource(t)
	s := newDesktopTestService(t, "exit 99")
	before := s.Status()
	path := filepath.Join(s.opts.StateDir, "catalogue.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Adopt(context.Background(), AdoptionRequest{RemoteURL: source}); err == nil {
		t.Fatal("failed persistence was not reported")
	}
	if !reflect.DeepEqual(before, s.Status()) {
		t.Fatal("failed persistence changed the in-memory catalogue")
	}
}

func TestAdoptionHTTPContract(t *testing.T) {
	source, _ := adoptionSource(t)
	s := newDesktopTestService(t, "exit 99")
	body, err := json.Marshal(AdoptionRequest{RemoteURL: source})
	if err != nil {
		t.Fatal(err)
	}
	// Handler coverage uses the same strict request decoder as the live socket.
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/v1/repositories/adopt", bytes.NewReader(body)))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"source":"manual"`) {
		t.Fatalf("adoption response = %d %s", response.Code, response.Body.String())
	}
}
