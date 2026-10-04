package gitstore

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestInheritedGitEnvironmentPreservesNativeConfigurationAndExplicitBindings(t *testing.T) {
	retained := []string{
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=native-helper",
		"GIT_CONFIG_GLOBAL=/native/global", "GIT_CONFIG_SYSTEM=/native/system", "GIT_CONFIG_NOSYSTEM=1",
		"SSH_AUTH_SOCK=/native/agent", "GIT_SSH_COMMAND=ssh -i /native/key", "GIT_SSH=/native/ssh",
		"GIT_ALLOW_PROTOCOL=ssh:https:file", "GIT_PROTOCOL=version=2", "PATH=/native/bin",
	}
	bindings := []string{
		"GIT_DIR=/source/git", "GIT_WORK_TREE=/source/tree", "GIT_COMMON_DIR=/source/common",
		"GIT_INDEX_FILE=/source/index", "GIT_OBJECT_DIRECTORY=/source/objects", "GIT_ALTERNATE_OBJECT_DIRECTORIES=/source/alternates",
		"GIT_PREFIX=source/", "GIT_SHALLOW_FILE=/source/shallow", "GIT_NAMESPACE=source", "GIT_REPLACE_REF_BASE=refs/source/",
		"GIT_GRAFT_FILE=/source/grafts", "GIT_CONFIG=/source/config",
	}
	base := append(append([]string(nil), retained...), bindings...)
	before := append([]string(nil), base...)
	cleaned := inheritedGitEnvironment(base)
	if !reflect.DeepEqual(cleaned, retained) || !reflect.DeepEqual(base, before) {
		t.Fatalf("inherited environment isolation changed native config or input: %q", cleaned)
	}
	merged, err := gitCommandEnv(cleaned, []string{"GIT_INDEX_FILE=/explicit/index"})
	if err != nil {
		t.Fatal(err)
	}
	if index, _ := environmentValue(merged, "GIT_INDEX_FILE"); index != "/explicit/index" {
		t.Fatalf("explicit command binding changed: %q", index)
	}
}

func TestPrivateCloneAndObjectCommandsDoNotModifyInheritedSourceState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	run(t, "git", "init", "--initial-branch=main", source)
	tracked := filepath.Join(source, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", source, "add", "tracked.txt")
	run(t, "git", "-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base")
	if err := os.WriteFile(tracked, []byte("staged source work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", source, "add", "tracked.txt")
	sourceGitDir := filepath.Join(source, ".git")
	sourceIndex := filepath.Join(sourceGitDir, "index")
	indexBefore, err := os.ReadFile(sourceIndex)
	if err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(filepath.Join(sourceGitDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	commands := filepath.Join(dir, "commands")
	// Every boundary must scrub these bindings before handing control to Git.
	// Fail without printing their values, so the fixture also avoids accidental
	// exposure of repository or credential environment contents.
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$REPOSITORY_TEST_COMMANDS\"\n" +
		"if [ \"${GIT_WORK_TREE+x}${GIT_COMMON_DIR+x}${GIT_INDEX_FILE+x}${GIT_OBJECT_DIRECTORY+x}${GIT_ALTERNATE_OBJECT_DIRECTORIES+x}${GIT_PREFIX+x}${GIT_SHALLOW_FILE+x}${GIT_NAMESPACE+x}${GIT_REPLACE_REF_BASE+x}${GIT_GRAFT_FILE+x}${GIT_CONFIG+x}\" != '' ]; then echo 'inherited repository binding reached Git' >&2; exit 90; fi\n" +
		"if [ \"$GIT_DIR\" = \"$REPOSITORY_TEST_SOURCE_GIT_DIR\" ]; then echo 'source repository reached Git' >&2; exit 91; fi\n" +
		"exec \"$REPOSITORY_TEST_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REPOSITORY_TEST_REAL_GIT", realGit)
	t.Setenv("REPOSITORY_TEST_COMMANDS", commands)
	t.Setenv("REPOSITORY_TEST_SOURCE_GIT_DIR", sourceGitDir)
	for key, value := range map[string]string{
		"GIT_DIR": sourceGitDir, "GIT_WORK_TREE": source, "GIT_COMMON_DIR": sourceGitDir,
		"GIT_INDEX_FILE": sourceIndex, "GIT_OBJECT_DIRECTORY": filepath.Join(sourceGitDir, "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(sourceGitDir, "objects"), "GIT_PREFIX": "source/",
		"GIT_SHALLOW_FILE": filepath.Join(sourceGitDir, "source-shallow"), "GIT_NAMESPACE": "source",
		"GIT_REPLACE_REF_BASE": "refs/source/", "GIT_GRAFT_FILE": filepath.Join(sourceGitDir, "source-grafts"),
		"GIT_CONFIG": filepath.Join(sourceGitDir, "config"),
	} {
		t.Setenv(key, value)
	}
	store := New(nil)
	defer store.Close()
	cfg := model.RepoConfig{ID: "private", Name: "private", RemoteURL: source, Branch: "main", GitDir: filepath.Join(dir, "private.git")}
	if err := store.CloneBloblessNonInteractive(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.ReadTreeHEAD(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	head, _, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, head)
	var trackedNode model.BaseNode
	for _, node := range nodes {
		if node.Path == "tracked.txt" {
			trackedNode = node
		}
	}
	if err != nil || trackedNode.Type != "file" || trackedNode.SizeState != "known" {
		t.Fatalf("private tree = %#v, error = %v", nodes, err)
	}
	cache := filepath.Join(dir, "blob-cache")
	if _, err := store.BlobToCache(ctx, cfg, trackedNode.ObjectOID, cache); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(cache)
	if err != nil || !bytes.Equal(contents, []byte("committed\n")) {
		t.Fatalf("private blob = %q, error = %v", contents, err)
	}
	indexAfter, err := os.ReadFile(sourceIndex)
	if err != nil || !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("private clone or read-tree changed the source's staged index")
	}
	configAfter, err := os.ReadFile(filepath.Join(sourceGitDir, "config"))
	if err != nil || !bytes.Equal(configBefore, configAfter) {
		t.Fatal("private clone changed the source's repository config")
	}
	logged, err := os.ReadFile(commands)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"clone --filter=blob:none", "ls-tree -r -t -z", "cat-file --batch-check --buffer", "cat-file --batch\n"} {
		if !strings.Contains(string(logged), command) {
			t.Fatalf("Git boundary %q was not exercised: %s", command, logged)
		}
	}
}
