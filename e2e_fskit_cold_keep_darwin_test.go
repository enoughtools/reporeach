//go:build darwin

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/desktop"
	"golang.org/x/sys/unix"
)

// Keep must bind dormant preview items to the writable view before its first
// export scan. A cached preview root and an unvisited descendant must not expose
// different timestamp authorities halfway through an otherwise unchanged tree.
func TestFSKitMountedColdKeepAcceptance(t *testing.T) {
	if os.Getenv("AFS_RUN_FSKIT_E2E_TESTS") != "1" {
		t.Skip("set AFS_RUN_FSKIT_E2E_TESTS=1 for real mounted FSKit dormant Keep acceptance")
	}
	release, err := unix.Sysctl("kern.osrelease")
	if err != nil {
		t.Fatal(err)
	}
	major, err := strconv.Atoi(strings.Split(release, ".")[0])
	if err != nil || major < 25 {
		t.Skipf("mounted dormant Keep acceptance requires macOS 26+; Darwin %s supplies no mounted proof", release)
	}
	prerequisites := fsKitAcceptanceModule(t)
	root, err := os.MkdirTemp("/tmp", "rr-cold-keep-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	h := &fsKitAcceptanceHarness{root: root, state: filepath.Join(root, "state"), mount: filepath.Join(root, "mount"), prerequisites: prerequisites}
	t.Cleanup(func() { h.cleanup(t) })
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "global-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_OPTIONAL_LOCKS", "0")
	t.Setenv("GH_CONFIG_DIR", filepath.Join(root, "gh-config"))
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost,::1")
	t.Setenv("no_proxy", "127.0.0.1,localhost,::1")
	ghSentinel := filepath.Join(root, "gh-called")
	t.Setenv("AFS_E2E_DESKTOP_GH_SENTINEL", ghSentinel)
	h.gh = filepath.Join(root, "gh")
	fsKitWrite(t, h.gh, []byte("#!/bin/sh\nprintf '%s\\n' called >> \"$AFS_E2E_DESKTOP_GH_SENTINEL\"\nexit 99\n"), 0o700)

	source, bare := filepath.Join(root, "original"), filepath.Join(root, "remote.git")
	if err := os.MkdirAll(filepath.Join(source, "unvisited", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	fsKitStorageGit(t, source, nil, "init", "--initial-branch=trunk")
	fsKitStorageGit(t, source, nil, "config", "user.name", "Dormant Keep fixture")
	fsKitStorageGit(t, source, nil, "config", "user.email", "fixture@example.invalid")
	files := map[string][]byte{
		"tracked.txt":                []byte("readonly before Keep\n"),
		"root.bin":                   {0, 255, 128, 0, 1, 2},
		"unvisited/alpha.txt":        []byte("unvisited first descendant\n"),
		"unvisited/beta.bin":         {1, 254, 129, 0, 3, 4},
		"unvisited/deep/gamma.txt":   []byte("deep gamma\n"),
		"unvisited/deep/delta.bin":   {2, 253, 130, 0, 5, 6},
		"unvisited/deep/epsilon.txt": []byte("deep epsilon\n"),
	}
	for name, value := range files {
		fsKitWrite(t, filepath.Join(source, name), value, 0o644)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	fsKitStorageGit(t, source, nil, "add", ".")
	// Make commit time distinguishable from preview creation even on a fast
	// machine, without sleeping or modifying the mounted tree during export.
	fsKitStorageGitOutput(t, source, nil, []string{"GIT_AUTHOR_DATE=2020-01-02T03:04:05Z", "GIT_COMMITTER_DATE=2020-01-02T03:04:05Z"}, "commit", "-m", "dormant Keep fixture")
	head := strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-parse", "HEAD")))
	indexEntries := fsKitStorageGit(t, source, nil, "ls-files", "--stage")
	unique, blobNames := make(map[string][]byte), make(map[string][]string)
	for name, value := range files {
		oid := strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-parse", "HEAD:"+name)))
		unique[oid], blobNames[oid] = value, []string{name}
	}
	linkOID := strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-parse", "HEAD:link")))
	unique[linkOID], blobNames[linkOID] = []byte("tracked.txt"), []string{"link"}
	if len(unique) != 8 {
		t.Fatalf("dormant Keep fixture needs eight unique blobs, got %d", len(unique))
	}
	selectedOID := strings.TrimSpace(string(fsKitStorageGit(t, source, nil, "rev-parse", "HEAD:tracked.txt")))
	fsKitStorageGit(t, root, nil, "clone", "--bare", source, bare)
	fsKitStorageGit(t, bare, nil, "config", "uploadpack.allowFilter", "true")
	beforeSource, beforeBare := snapshotDesktopAdoptionSource(t, source), snapshotDesktopAdoptionSource(t, bare)
	transport := fsKitStorageHTTP(t, h)
	gate := newFSKitColdKeepGate(transport.backend)
	transport.backend = gate
	t.Cleanup(gate.release)
	t.Cleanup(func() {
		if t.Failed() {
			fsKitStorageLogTransport(t, transport, blobNames)
		}
	})

	h.copyServerImage(t)
	h.start(t)
	const owner, id = "ColdKeepAcceptance", "ColdKeepAcceptance/project"
	repo := filepath.Join(h.mount, owner, "project")
	var status desktop.Status
	h.request(t, http.MethodPost, "/v1/repositories/adopt", desktop.AdoptionRequest{RemoteURL: transport.URL + "/remote.git", Owner: owner, Name: "project"}, &status)
	h.request(t, http.MethodPost, "/v1/mount", nil, &status)
	h.identity(t)
	waitDesktopCatalogueNames(t, repo, []string{".git", "link", "root.bin", "tracked.txt", "unvisited"})
	previewGit := fsKitStoragePreviewGit(t, h.state)
	oids := fsKitStorageOIDs(unique)
	if err := fsKitStorageRequireMissing(fsKitStorageGitNoFetch(t, root, []byte(strings.Join(oids, "\n")+"\n"), "--git-dir", previewGit, "cat-file", "--batch-check"), oids); err != nil {
		t.Fatalf("dormant Keep root listing acquired blobs: %v", err)
	}
	// Read only a root file. Do not traverse unvisited, read the virtual .git
	// pointer or run Git against the mounted repository before submitting Keep.
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), files["tracked.txt"])
	// Prime retained kernel file and directory attributes too, without listing
	// the directory's children. Keep must handle both these cached attributes and
	// its first discovery of the deeper descendants coherently.
	beforeMetadataRequests := transport.requests.Load()
	if info, err := os.Lstat(filepath.Join(repo, "tracked.txt")); err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(files["tracked.txt"])) {
		t.Fatalf("dormant Keep could not prime readonly file metadata: info=%v error=%v", info, err)
	}
	if info, err := os.Lstat(filepath.Join(repo, "unvisited")); err != nil || !info.IsDir() || info.Mode().Perm()&0o500 != 0o500 {
		t.Fatalf("dormant Keep could not prime directory metadata: info=%v error=%v", info, err)
	}
	if transport.requests.Load() != beforeMetadataRequests {
		t.Fatal("priming cached dormant file/directory metadata acquired additional source data")
	}
	contentPaths, previewSourceRoot := fsKitStoragePreviewContentPaths(t, h.state, head)
	fsKitStorageCachedBlob(t, contentPaths.cache, selectedOID, files["tracked.txt"])
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	if entry := desktopAdoptedRepository(t, status, id); status.Account != nil || entry.State != "virtual" || entry.LocalPath != "" {
		t.Fatalf("dormant Keep precondition lost the unauthenticated virtual preview: %+v", entry)
	}
	if _, err := fsKitStorageRegistry(h.state, repo); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dormant Keep precondition prepared a writable engine: %v", err)
	}

	// Hold acquisition at this disposable source so an immediate accepted
	// operation cannot be confused with a synchronous, already-completed Keep.
	gate.held.Store(true)
	data, acceptedDuration := fsKitColdKeepRequest(t, h, http.MethodPost, "/v1/repositories/action", map[string]string{"id": id, "action": "keep"}, http.StatusAccepted)
	var accepted struct {
		Operation desktop.Operation `json:"operation"`
	}
	if err := fsKitDecodeResponse(data, &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Operation.ID == "" || accepted.Operation.RepositoryID != id || accepted.Operation.Action != "keep" || accepted.Operation.Status != "running" {
		t.Fatalf("Keep did not promptly accept a progress operation: %+v", accepted.Operation)
	}
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted dormant Keep did not reach the gated source acquisition")
	}
	data, statusDuration := fsKitColdKeepRequest(t, h, http.MethodGet, "/v1/status", nil, http.StatusOK)
	if err := fsKitDecodeResponse(data, &status); err != nil {
		t.Fatal(err)
	}
	visible := false
	for _, operation := range status.Operations {
		if operation.ID == accepted.Operation.ID && operation.RepositoryID == id && operation.Action == "keep" && operation.Status == "running" {
			visible = true
		}
	}
	if !visible {
		t.Fatal("accepted Keep progress was not visible while source acquisition was blocked")
	}
	gate.release()
	operation := fsKitStorageWaitOperation(t, h, accepted.Operation.ID, "keep", "complete")
	wantBytes := int64(0)
	for _, value := range unique {
		wantBytes += int64(len(value))
	}
	if operation.TotalBlobs != int64(len(unique)) || operation.CompletedBlobs != operation.TotalBlobs || operation.TotalBytes != wantBytes || operation.DownloadedBytes != wantBytes {
		t.Fatalf("dormant Keep did not preserve exact committed accounting: %+v; want blobs=%d bytes=%d", operation, len(unique), wantBytes)
	}
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	if entry := desktopAdoptedRepository(t, status, id); entry.State != "local" || entry.LocalKind != "materialized" || entry.LocalPath != repo || !entry.Pinned || entry.DownloadedBytes != wantBytes || entry.Error != "" {
		t.Fatalf("dormant Keep did not publish an ordinary local checkout: %+v", entry)
	}
	fsKitOrdinaryCheckout(t, repo)
	fsKitStorageTree(t, repo, files, head)
	if index := fsKitStorageGit(t, repo, nil, "ls-files", "--stage"); !bytes.Equal(index, indexEntries) {
		t.Fatal("dormant Keep changed tracked index entries")
	}
	if _, err := fsKitStorageRegistry(h.state, repo); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dormant Keep retained a second active engine checkout: %v", err)
	}
	if _, err := os.Lstat(previewSourceRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dormant Keep retained active preview Git storage: %v", err)
	}
	transport.online.Store(false)
	beforeOfflineRequests := transport.requests.Load()
	if !h.stop(t) {
		t.Fatal("could not normally stop after dormant Keep")
	}
	fsKitOrdinaryCheckout(t, repo)
	fsKitStorageTree(t, repo, files, head)
	if transport.requests.Load() != beforeOfflineRequests {
		t.Fatal("ordinary dormant-Keep checkout contacted its offline source with the app stopped")
	}
	if !reflect.DeepEqual(beforeSource, snapshotDesktopAdoptionSource(t, source)) || !reflect.DeepEqual(beforeBare, snapshotDesktopAdoptionSource(t, bare)) {
		t.Fatal("dormant Keep changed its original checkout or immutable bare source")
	}
	if _, err := os.Stat(ghSentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dormant native Git Keep invoked GitHub CLI: %v", err)
	}
	t.Logf("dormant Keep acceptance completed: accepted_ms=%d blocked_progress_status_ms=%d unique_blobs=%d downloaded_bytes=%d root_preview_cached=true native_file_directory_metadata_cached=true nested_contents_unvisited=true no_git_activation_before_keep=true ordinary_app_off_checkout=true offline_requests=0", acceptedDuration.Milliseconds(), statusDuration.Milliseconds(), len(unique), wantBytes)
}

// Only this fixture's read-only Git source is gated. Register release before
// making any request so normal fixture cleanup cannot wait on the gate.
type fsKitColdKeepGate struct {
	backend                  http.Handler
	held                     atomic.Bool
	entered, released        chan struct{}
	enteredOnce, releaseOnce sync.Once
}

func newFSKitColdKeepGate(backend http.Handler) *fsKitColdKeepGate {
	return &fsKitColdKeepGate{backend: backend, entered: make(chan struct{}), released: make(chan struct{})}
}

func (g *fsKitColdKeepGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.held.Load() {
		g.enteredOnce.Do(func() { close(g.entered) })
		select {
		case <-g.released:
		case <-r.Context().Done():
			return
		}
	}
	g.backend.ServeHTTP(w, r)
}

func (g *fsKitColdKeepGate) release() {
	g.held.Store(false)
	g.releaseOnce.Do(func() { close(g.released) })
}

func fsKitColdKeepRequest(t *testing.T, h *fsKitAcceptanceHarness, method, path string, body any, expectedCode int) ([]byte, time.Duration) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	data, code, err := desktop.Request(ctx, h.server.socket, method, path, payload)
	duration := time.Since(started)
	if err != nil || code != expectedCode {
		t.Fatalf("prompt dormant Keep %s %s failed: code=%d want=%d error=%s; log=%s", method, path, code, expectedCode, auth.RedactString(fmt.Sprint(err)), h.server.logPath)
	}
	return data, duration
}
