package desktop

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/jacobsa/fuse/fuseops"
)

func TestCatalogueConcurrentManualAndGitHubCredentialsStayIsolated(t *testing.T) {
	source, _ := adoptionSource(t)
	directory := t.TempDir()
	home := filepath.Join(directory, "native-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	global := filepath.Join(home, ".gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	nativeCalls, ghCalls := filepath.Join(directory, "native-calls"), filepath.Join(directory, "gh-calls")
	t.Setenv("CATALOGUE_NATIVE_CALLS", nativeCalls)
	t.Setenv("CATALOGUE_GH_CALLS", ghCalls)
	native := filepath.Join(directory, "native helper")
	if err := os.WriteFile(native, []byte(`#!/bin/sh
printf '%s\n' "$1" >> "$CATALOGUE_NATIVE_CALLS"
if [ "$1" = get ]; then
  printf '%s\n' username=native-profile password=native-fixture-password
fi
`), 0o700); err != nil {
		t.Fatal(err)
	}
	adoptionGit(t, directory, "config", "--file", global, "credential.helper", "!"+shellQuote(native))
	globalBefore, err := os.ReadFile(global)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.preloadindex")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	agent := filepath.Join(home, "native-agent.sock")
	t.Setenv("SSH_AUTH_SOCK", agent)
	service := newDesktopTestService(t, `
printf '%s\n' "$*" >> "$CATALOGUE_GH_CALLS"
case "$*" in
  'auth git-credential get') printf '%s\n' username=github-profile password=github-fixture-password ;;
  *) exit 4 ;;
esac
`)

	// The wrapper substitutes a local fixture only after asking real Git to
	// select credentials. It keeps acquisition deterministic without network
	// access while exercising the actual service and Git configuration paths.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(directory, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CATALOGUE_REAL_GIT", realGit)
	t.Setenv("CATALOGUE_SOURCE", source)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(`#!/bin/sh
if [ "$1" = clone ]; then
  umask 077
  for value do remote="$target"; target="$value"; done
  "$CATALOGUE_REAL_GIT" credential fill <<EOF | sed -n '/^username=/p' > "$target.profile"
protocol=https
host=github.com

EOF
  "$CATALOGUE_REAL_GIT" clone --filter=blob:none --no-checkout --single-branch --no-tags --branch trunk "$CATALOGUE_SOURCE" "$target" || exit $?
  exec "$CATALOGUE_REAL_GIT" --git-dir "$target/.git" remote set-url origin "$remote"
fi
exec "$CATALOGUE_REAL_GIT" "$@"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	manual, discovered := catalogueRepository("native", "project"), catalogueRepository("github", "project")
	manual.Source, discovered.Source = "manual", "github"
	manual.DefaultBranch, discovered.DefaultBranch = "trunk", "trunk"
	seedDesktopCatalogue(t, service, manual, discovered)
	service.dependencyReady = func() bool { return true }
	var catalogue *catalogfs.FileSystem
	service.mountCatalogue = func(_ context.Context, _ string, fs *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		catalogue = fs
		return newDesktopFakeMount(), nil
	}
	ctx := context.Background()
	if err := service.Mount(ctx); err != nil {
		t.Fatal(err)
	}
	lookup := func(parent fuseops.InodeID, name string) fuseops.InodeID {
		t.Helper()
		op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
		if err := catalogue.LookUpInode(ctx, op); err != nil {
			t.Fatal(err)
		}
		return op.Entry.Child
	}
	roots := make([]fuseops.InodeID, 0, 2)
	for _, repo := range []Repository{manual, discovered} {
		roots = append(roots, lookup(lookup(fuseops.RootInodeID, repo.Owner), repo.Name))
	}
	if configs, err := service.engine.ListRepos(ctx); err != nil || len(configs) != 0 {
		t.Fatalf("placeholder lookup acquired a repository: %+v, %v", configs, err)
	}
	var workers sync.WaitGroup
	errors := make(chan error, len(roots))
	start := make(chan struct{})
	for _, inode := range roots {
		workers.Add(1)
		go func(inode fuseops.InodeID) {
			defer workers.Done()
			<-start
			op := &fuseops.OpenDirOp{Inode: inode}
			if err := catalogue.OpenDir(ctx, op); err != nil {
				errors <- err
				return
			}
			if err := catalogue.ReleaseDirHandle(ctx, &fuseops.ReleaseDirHandleOp{Handle: op.Handle}); err != nil {
				errors <- err
			}
		}(inode)
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
	for _, expected := range []struct {
		repo    Repository
		profile string
	}{{manual, "native-profile"}, {discovered, "github-profile"}} {
		gitDir := filepath.Join(service.opts.StateDir, "engine", "repos", engineName(expected.repo.ID), "git")
		profiles, err := filepath.Glob(filepath.Join(filepath.Dir(gitDir), ".clone-*.profile"))
		if err != nil || len(profiles) != 1 {
			t.Fatalf("acquisition profile record for %s: %v, %v", expected.repo.ID, profiles, err)
		}
		profile, err := os.ReadFile(profiles[0])
		if err != nil || string(profile) != "username="+expected.profile+"\n" {
			t.Fatalf("acquisition credentials for %s: %q, %v", expected.repo.ID, profile, err)
		}
		config, err := os.ReadFile(filepath.Join(gitDir, "config"))
		if err != nil {
			t.Fatal(err)
		}
		usesGitHub := bytes.Contains(config, []byte("auth git-credential"))
		if usesGitHub != (expected.repo.Source == "github") {
			t.Fatalf("private clone for %s has the wrong credential helper", expected.repo.ID)
		}
	}
	for _, expected := range []struct{ path, calls string }{{nativeCalls, "get\n"}, {ghCalls, "auth git-credential get\n"}} {
		if calls, err := os.ReadFile(expected.path); err != nil || string(calls) != expected.calls {
			t.Fatalf("unexpected authentication calls: %q, %v", calls, err)
		}
	}
	if service.Status().Account != nil {
		t.Fatal("opening a manual repository required GitHub sign-in")
	}
	if after, err := os.ReadFile(global); err != nil || !bytes.Equal(globalBefore, after) {
		t.Fatal("catalogue acquisition changed the native Git profile")
	}
	if os.Getenv("GIT_CONFIG_COUNT") != "1" || os.Getenv("SSH_AUTH_SOCK") != agent {
		t.Fatal("mixed catalogue access changed inherited Git configuration or SSH agent")
	}
}
