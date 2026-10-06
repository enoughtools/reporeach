//go:build darwin

package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// This uses the filtered HTTP transport without starting an app or mounting a
// filesystem. Hydrated promisor packs must not prevent reclaiming a clean,
// remotely recoverable checkout, while genuinely local objects remain protected.
func TestVerifySafeToDiscardHydratedFilteredHTTP(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "global-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "")
	}
	source, bare := filepath.Join(root, "source"), filepath.Join(root, "remote.git")
	fsKitStorageGit(t, root, nil, "init", "--initial-branch=trunk", source)
	fsKitWrite(t, filepath.Join(source, "history.txt"), []byte("earlier history only\n"), 0o600)
	fsKitStorageGit(t, source, nil, "add", ".")
	fsKitStorageGit(t, source, nil, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "first")
	firstCommit := strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-parse", "HEAD")))
	firstTree := strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-parse", "HEAD^{tree}")))
	historyBlob := strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-parse", "HEAD:history.txt")))
	if err := os.Remove(filepath.Join(source, "history.txt")); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		".DS_Store": []byte("tracked Finder data\n"), "Icon\r": []byte("tracked icon\n"),
		"tracked.txt": []byte("tracked content\n"), "binary.dat": {0, 255, 128, 'b'}, "duplicate.dat": {0, 255, 128, 'b'},
	} {
		fsKitWrite(t, filepath.Join(source, name), data, 0o600)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	fsKitStorageGit(t, source, nil, "add", "-A")
	fsKitStorageGit(t, source, nil, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "second")
	head := strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-parse", "HEAD")))
	fsKitStorageGit(t, root, nil, "clone", "--bare", source, bare)
	fsKitStorageGit(t, bare, nil, "config", "uploadpack.allowFilter", "true")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	transport := &fsKitStorageTransport{backend: &fsKitStorageBackend{git: git, root: root, logger: log.New(io.Discard, "", 0), stderr: io.Discard}}
	transport.online.Store(true)
	server := httptest.NewServer(transport)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := model.RepoConfig{ID: "discard", Name: "discard", GitDir: filepath.Join(root, "view", ".git"),
		RemoteURL: server.URL + "/remote.git", Branch: "trunk", RequiredCommit: head, MountPath: filepath.Join(root, "view")}
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	if _, err := store.PrepareSource(ctx, cfg, model.SourceRequirement{Ref: "refs/heads/trunk", RequiredCommit: head}); err != nil {
		t.Fatal(err)
	}
	fsKitStorageGit(t, cfg.GitDir, nil, "update-ref", "refs/heads/trunk", head)
	fsKitStorageGit(t, cfg.GitDir, nil, "symbolic-ref", "HEAD", "refs/heads/trunk")
	fsKitStorageGit(t, cfg.GitDir, nil, "config", "core.worktree", cfg.MountPath)
	fsKitStorageGit(t, cfg.GitDir, nil, "config", "core.fsmonitor", "false")
	if err := store.PinWorkingTreeBaseline(ctx, cfg, head); err != nil {
		t.Fatal(err)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, head)
	if err != nil {
		t.Fatal(err)
	}
	hydrated := make(map[string]bool)
	for _, node := range nodes {
		if node.Type == "dir" || hydrated[node.ObjectOID] {
			continue
		}
		if _, err := store.BlobToCache(ctx, cfg, node.ObjectOID, filepath.Join(root, "cache", node.ObjectOID)); err != nil {
			t.Fatal(err)
		}
		hydrated[node.ObjectOID] = true
	}
	store.CloseRepository(cfg.GitDir)
	if len(hydrated) != 5 {
		t.Fatalf("expected five hydrated current blobs; got %d", len(hydrated))
	}
	assertHistoryMissing := func() {
		t.Helper()
		out := fsKitStorageGitNoFetch(t, cfg.GitDir, []byte(historyBlob+"\n"), "cat-file", "--batch-check")
		if !strings.Contains(string(out), " missing") {
			t.Fatal("discard verification fetched the removed history blob")
		}
	}
	assertHistoryMissing()
	// Verification must use the requested source without rewriting a checkout's
	// configured native origin or creating URL-named promisor remotes.
	fsKitStorageGitNoFetch(t, cfg.GitDir, nil, "config", "remote.origin.url", "https://fixture.invalid/preserved-origin.git")
	// Native URL rewriting must resolve the selected and requested URL equally.
	fsKitStorageGitNoFetch(t, root, nil, "config", "--file", filepath.Join(root, "global-config"),
		"url."+server.URL+"/.insteadOf", "http://requested-source.invalid/")
	cfg.RemoteURL = "http://requested-source.invalid/remote.git"
	configBefore, err := os.ReadFile(filepath.Join(cfg.GitDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	assertConfigPreserved := func() {
		t.Helper()
		actual, err := os.ReadFile(filepath.Join(cfg.GitDir, "config"))
		if err != nil || !bytes.Equal(actual, configBefore) {
			t.Fatalf("discard verification changed native Git configuration: %v", err)
		}
	}
	unreachable := fsKitStorageGitNoFetch(t, cfg.GitDir, nil, "fsck", "--connectivity-only", "--unreachable", "--no-progress")
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(unreachable)), "\n") {
		if fields := strings.Fields(line); len(fields) == 3 && fields[0] == "unreachable" {
			counts[fields[1]]++
		}
	}
	reachable := fsKitStorageGitNoFetch(t, cfg.GitDir, nil, "rev-list", "--objects", "--missing=allow-promisor", "--no-object-names", "refs/remotes/artifact-fs/source", "--")
	t.Logf("hydrated=%d fsck_unreachable_types=%v old_commit_reported=%v old_tree_reported=%v old_commit_reachable=%v old_tree_reachable=%v", len(hydrated), counts,
		strings.Contains(string(unreachable), firstCommit), strings.Contains(string(unreachable), firstTree),
		strings.Contains(string(reachable), firstCommit), strings.Contains(string(reachable), firstTree))
	if err := store.VerifySafeToDiscard(ctx, cfg); err != nil {
		t.Fatalf("clean remotely recoverable hydrated checkout refused: %v", err)
	}
	assertConfigPreserved()
	assertHistoryMissing()
	for _, kind := range []string{"blob", "tree", "commit", "tag"} {
		t.Run("unpublished-"+kind, func(t *testing.T) {
			var oid string
			switch kind {
			case "blob":
				oid = strings.TrimSpace(string(fsKitStorageGitNoFetch(t, cfg.GitDir, []byte("unpublished recovery data\n"), "hash-object", "-w", "--stdin")))
			case "tree":
				blob := strings.TrimSpace(string(fsKitStorageGitNoFetch(t, cfg.GitDir, nil, "rev-parse", "HEAD:tracked.txt")))
				oid = strings.TrimSpace(string(fsKitStorageGitNoFetch(t, cfg.GitDir, []byte("100644 blob "+blob+"\tlocal-only.txt\n"), "mktree")))
			case "commit":
				oid = strings.TrimSpace(string(fsKitStorageGitNoFetch(t, cfg.GitDir, nil, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
					"commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "unattached local recovery commit")))
			case "tag":
				input := "object " + head + "\ntype commit\ntag unpublished\ntagger Fixture <fixture@example.invalid> 1700000123 +0000\n\nlocal recovery tag\n"
				oid = strings.TrimSpace(string(fsKitStorageGitNoFetch(t, cfg.GitDir, []byte(input), "mktag")))
			}
			if len(oid) != 40 {
				t.Fatal("fixture object did not produce an SHA-1 identifier")
			}
			if err := store.VerifySafeToDiscard(ctx, cfg); err == nil || !strings.Contains(err.Error(), "unreachable Git objects") {
				t.Fatalf("unpublished %s did not receive an object recovery refusal: %v", kind, err)
			}
			assertConfigPreserved()
			metadata := fsKitStorageGitNoFetch(t, cfg.GitDir, []byte(oid+"\n"), "cat-file", "--batch-check")
			if !strings.Contains(string(metadata), " "+kind+" ") {
				t.Fatalf("refused %s was not preserved", kind)
			}
			assertHistoryMissing()
			// Remove only this deliberately created loose control object so the
			// next type is tested independently in the disposable private fixture.
			if err := os.Remove(filepath.Join(cfg.GitDir, "objects", oid[:2], oid[2:])); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := store.VerifySafeToDiscard(ctx, cfg); err != nil {
		t.Fatalf("clean checkout refused after removing disposable controls: %v", err)
	}
	assertHistoryMissing()
	assertConfigPreserved()
}
