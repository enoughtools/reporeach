//go:build darwin

package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// This exercises only the disposable source transport and canonical Git store.
// It requires no FSKit helper, filesystem mount, app, account or kernel approval.
func TestCommitTimestampColdFilteredHTTPDoesNotFetchBlobs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "global-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "")
	}
	source, bare := filepath.Join(root, "source"), filepath.Join(root, "remote.git")
	fsKitStorageGit(t, root, nil, "init", "--initial-branch=main", source)
	if err := os.WriteFile(filepath.Join(source, "history.txt"), []byte("earlier history only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fsKitStorageGit(t, source, nil, "add", ".")
	fsKitStorageGit(t, source, nil, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "first")
	if err := os.Remove(filepath.Join(source, "history.txt")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(source, "file"+strconv.Itoa(i)), []byte{byte(i), 255, 128, 'a', '\n'}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fsKitStorageGit(t, source, nil, "add", "-A")
	const wantTimestamp = int64(1700000123)
	fsKitStorageGitOutput(t, source, nil, []string{"GIT_AUTHOR_DATE=1700000000 +0530", "GIT_COMMITTER_DATE=1700000123 -0700"},
		"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "second")
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg := model.RepoConfig{ID: "timestamp", Name: "timestamp", GitDir: filepath.Join(root, "private.git"),
		RemoteURL: server.URL + "/remote.git", Branch: "main", MountPath: filepath.Join(root, "view")}
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	if err := store.CloneBloblessNonInteractive(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	oid, _, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var blobs []string
	objects := strings.Split(strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-list", "--objects", "--all"))), "\n")
	for _, record := range objects {
		object := strings.Fields(record)[0]
		if strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "cat-file", "-t", object))) == "blob" {
			blobs = append(blobs, object)
		}
	}
	if len(blobs) != 6 {
		t.Fatalf("fixture should contain five HEAD blobs and one history blob; got %d", len(blobs))
	}
	assertMissing := func() {
		t.Helper()
		output := fsKitStorageGitNoFetch(t, cfg.GitDir, []byte(strings.Join(blobs, "\n")+"\n"), "cat-file", "--batch-check")
		if count := strings.Count(string(output), " missing"); count != len(blobs) {
			t.Fatalf("metadata operation fetched blobs: %d of %d remain missing", count, len(blobs))
		}
	}
	assertMissing()
	before := transport.requests.Load()
	timestamp, err := store.CommitTimestamp(ctx, cfg, oid)
	if err != nil || timestamp != wantTimestamp {
		t.Fatalf("committer timestamp = %d, %v; want %d", timestamp, err, wantTimestamp)
	}
	assertMissing()
	if after := transport.requests.Load(); after != before {
		t.Fatalf("commit metadata made %d source requests", after-before)
	}
	if _, err := os.Lstat(cfg.MountPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata operation created a checkout: %v", err)
	}
	t.Logf("exact timestamp=%d missing_blobs=%d source_requests=0", timestamp, len(blobs))
}
