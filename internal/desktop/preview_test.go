package desktop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
)

const previewCommit = "1111111111111111111111111111111111111111"
const previewRootTree = "2222222222222222222222222222222222222222"
const previewChildTree = "3333333333333333333333333333333333333333"
const previewBlob = "4444444444444444444444444444444444444444"

func previewEntry(name, typ, oid string, mode uint32, size int64) githubPreviewEntry {
	return githubPreviewEntry{NameRaw: base64.StdEncoding.EncodeToString([]byte(name)), Type: typ, OID: oid, Mode: mode, Size: &size}
}

func previewRootJSON(entries []githubPreviewEntry, count int) string {
	data := make(map[string]any)
	for i := 0; i < count; i++ {
		data["r"+strconv.Itoa(i)] = map[string]any{"object": map[string]any{"__typename": "Commit", "oid": previewCommit,
			"tree": map[string]any{"oid": previewRootTree, "entries": entries}}}
	}
	bytes, _ := json.Marshal(map[string]any{"data": data})
	return string(bytes)
}

func previewGitHubRepo(owner, name string) Repository {
	return Repository{ID: owner + "/" + name, Owner: owner, Name: name, DefaultBranch: "main", Source: "github",
		CloneURL: "https://github.com/" + owner + "/" + name + ".git", HTMLURL: "https://github.com/" + owner + "/" + name}
}

func newPreviewFixtureCache(t *testing.T, state string, github *GitHub) *PreviewCache {
	t.Helper()
	store := gitstore.New(nil)
	t.Cleanup(store.Close)
	cache, err := NewPreviewCache(context.Background(), state, store, github)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Error(err)
		}
	})
	return cache
}

func previewNames(entries []model.BaseNode) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Path)
	}
	sort.Strings(names)
	return names
}

func TestPreviewGitHubSeedRootsAndLazySubtreeWithoutGitOrBlobs(t *testing.T) {
	state := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("PREVIEW_CALLS", log)
	rawName := "raw-" + string([]byte{255})
	rootEntries := []githubPreviewEntry{previewEntry(".DS_Store", "blob", previewBlob, 0o100644, 17),
		previewEntry("Icon\r", "blob", previewBlob, 0o100644, 17), previewEntry("src", "tree", previewChildTree, 0o040000, 0),
		previewEntry(rawName, "blob", previewBlob, 0o100644, 17)}
	childEntries := []githubPreviewEntry{previewEntry("binary.dat", "blob", previewBlob, 0o100644, 987654)}
	childJSON, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"object": map[string]any{
		"__typename": "Tree", "oid": previewChildTree, "entries": childEntries}}}})
	github := fakeGitHub(t, `printf '%s\n' "$*" >> "$PREVIEW_CALLS"
case "$*" in
  *'object(oid:'*) cat <<'SUBTREE'
`+string(childJSON)+`
SUBTREE
;;
  *) cat <<'ROOTS'
`+previewRootJSON(rootEntries, 2)+`
ROOTS
;;
esac
`)
	cache := newPreviewFixtureCache(t, state, github)
	repos := []Repository{previewGitHubRepo("fixture", "first"), previewGitHubRepo("fixture", "second")}
	if err := cache.SeedGitHubRoots(context.Background(), repos); err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		preview, err := cache.Acquire(context.Background(), repo)
		if err != nil {
			t.Fatal(err)
		}
		revision, entries, err := preview.Directory(context.Background(), ".")
		if err != nil || revision != previewCommit || !reflect.DeepEqual(previewNames(entries), []string{".DS_Store", "Icon\r", rawName, "src"}) {
			t.Fatalf("root preview = %q, %+v, %v", revision, entries, err)
		}
		for _, entry := range entries {
			if entry.SizeState != "known" {
				t.Fatalf("GitHub stat would hydrate: %+v", entry)
			}
		}
		if _, _, err := preview.Directory(context.Background(), "absent"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("authoritative negative = %v", err)
		}
		if _, err := os.Lstat(filepath.Join(cache.root, previewKey(repo), "git")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("GitHub preview prepared a Git clone: %v", err)
		}
	}
	preview, _ := cache.Acquire(context.Background(), repos[0])
	revision, entries, err := preview.Directory(context.Background(), "src")
	if err != nil || revision != previewCommit || len(entries) != 1 || entries[0].Path != "src/binary.dat" || entries[0].SizeBytes != 987654 {
		t.Fatalf("subtree = %q, %+v, %v", revision, entries, err)
	}
	if _, _, err := preview.Directory(context.Background(), "src"); err != nil {
		t.Fatal(err)
	}
	if cache.SelectedCommit(repos[0].ID) != previewCommit {
		t.Fatal("selected commit requires acquisition or drifted")
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(calls), "\n"); count != 2 {
		t.Fatalf("expected one batch root request plus one lazy subtree; got %d: %s", count, calls)
	}
	if strings.Contains(string(calls), " text") || strings.Contains(string(calls), "byteSize") {
		t.Fatalf("metadata acquisition requested blob contents: %s", calls)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening the durable metadata must not make another GitHub request.
	reopened := newPreviewFixtureCache(t, state, github)
	preview, err = reopened.Acquire(context.Background(), repos[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, entries, err = preview.Directory(context.Background(), "src"); err != nil || len(entries) != 1 {
		t.Fatalf("persisted subtree = %+v, %v", entries, err)
	}
	_, entries, err = preview.Directory(context.Background(), ".")
	if err != nil || !reflect.DeepEqual(previewNames(entries), []string{".DS_Store", "Icon\r", rawName, "src"}) {
		t.Fatalf("durable metadata corrupted a raw filename: %+v, %v", entries, err)
	}
	after, _ := os.ReadFile(log)
	if string(after) != string(calls) {
		t.Fatal("reopening fetched already cached metadata")
	}
}

func TestPreviewNativeShallowFilteredMetadataPreservesSourceAndMissingBlobs(t *testing.T) {
	source, _ := adoptionSource(t)
	rawName := "name with spaces"
	for name, content := range map[string][]byte{".DS_Store": []byte("tracked metadata"), "Icon\r": []byte("tracked icon"), rawName: {0, 255, 128}, "nested/file": []byte("nested blob")} {
		path := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	adoptionGit(t, source, "add", ".")
	adoptionGit(t, source, "commit", "-m", "second commit")
	adoptionGit(t, source, "config", "uploadpack.allowFilter", "true")
	adoptionGit(t, source, "config", "uploadpack.allowAnySHA1InWant", "true")
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("staged user work"), 0o600); err != nil {
		t.Fatal(err)
	}
	adoptionGit(t, source, "add", "tracked.txt")
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("unstaged user work"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := sourceState(t, source)
	state := t.TempDir()
	github := fakeGitHub(t, "exit 99")
	cache := newPreviewFixtureCache(t, state, github)
	repo := Repository{ID: "local/project", Owner: "local", Name: "project", DefaultBranch: "trunk", CloneURL: source, Source: "manual"}
	preview, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	_, entries, err := preview.Directory(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".DS_Store", "Icon\r", rawName, "nested", "tracked.txt"} {
		found := false
		for _, entry := range entries {
			if entry.Path == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing real committed name %q: %+v", name, entries)
		}
	}
	gitDir := filepath.Join(cache.root, previewKey(repo), "git")
	if got := strings.TrimSpace(string(adoptionGit(t, state, "--git-dir", gitDir, "rev-list", "--count", "HEAD"))); got != "1" {
		t.Fatalf("preview fetched full history: %s", got)
	}
	for _, entry := range entries {
		if entry.Type != "file" {
			continue
		}
		cmd := exec.Command("git", "--git-dir", gitDir, "cat-file", "--batch-check")
		cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
		cmd.Stdin = strings.NewReader(entry.ObjectOID + "\n")
		output, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(output), " missing") || entry.SizeState != "unknown" {
			t.Fatalf("preview fetched blob %q: %s, %v, %+v", entry.Path, output, err, entry)
		}
	}
	if !reflect.DeepEqual(before, sourceState(t, source)) {
		t.Fatal("preview changed source staged/unstaged state")
	}
	if _, err := os.Stat(filepath.Join(state, "engine")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview created writable engine")
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newPreviewFixtureCache(t, state, github)
	preview, err = reopened.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	_, entries, err = preview.Directory(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Path == rawName {
			found = true
		}
	}
	if !found {
		t.Fatal("durable metadata corrupted a raw Git filename")
	}
}

func TestPreviewSeedSingleflightAndCanceledWaiter(t *testing.T) {
	state, control := t.TempDir(), t.TempDir()
	t.Setenv("PREVIEW_CONTROL", control)
	github := fakeGitHub(t, `touch "$PREVIEW_CONTROL/started"
while [ ! -f "$PREVIEW_CONTROL/release" ]; do sleep 0.01; done
cat <<'ROOT'
`+previewRootJSON([]githubPreviewEntry{previewEntry("file", "blob", previewBlob, 0o100644, 12)}, 1)+`
ROOT
`)
	cache := newPreviewFixtureCache(t, state, github)
	repo := previewGitHubRepo("fixture", "project")
	seedDone := make(chan error, 1)
	go func() { seedDone <- cache.SeedGitHubRoots(context.Background(), []Repository{repo}) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(control, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if err := cache.Invalidate(context.Background(), repo); !errors.Is(err, ErrPreviewInUse) {
		t.Fatalf("invalidation did not guard pending seed: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Acquire(canceled, repo); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter = %v", err)
	}
	var waiters sync.WaitGroup
	var resultMu sync.Mutex
	var previews []*RepositoryPreview
	for i := 0; i < 8; i++ {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			preview, err := cache.Acquire(context.Background(), repo)
			if err != nil {
				t.Error(err)
				return
			}
			resultMu.Lock()
			previews = append(previews, preview)
			resultMu.Unlock()
		}()
	}
	if err := os.WriteFile(filepath.Join(control, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-seedDone; err != nil {
		t.Fatal(err)
	}
	waiters.Wait()
	if len(previews) != 8 {
		t.Fatal("lost waiter")
	}
	for _, preview := range previews {
		if preview != previews[0] {
			t.Fatal("duplicate seed/acquisition preview")
		}
	}
}

func TestPreviewRejectsPartialMalformedAndNullMetadata(t *testing.T) {
	valid := previewEntry("file", "blob", previewBlob, 0o100644, 10)
	for name, entries := range map[string][]githubPreviewEntry{
		"missing size": {func() githubPreviewEntry { entry := valid; entry.Size = nil; return entry }()},
		"duplicate":    {valid, valid},
		"slash":        {previewEntry("a/b", "blob", previewBlob, 0o100644, 10)},
		"invalid mode": {previewEntry("file", "blob", previewBlob, 0o040000, 10)},
		"null list":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			github := fakeGitHub(t, "cat <<'RESPONSE'\n"+previewRootJSON(entries, 1)+"\nRESPONSE\n")
			cache := newPreviewFixtureCache(t, t.TempDir(), github)
			repo := previewGitHubRepo("fixture", "project")
			if preview, err := cache.Acquire(context.Background(), repo); err == nil || preview != nil {
				t.Fatalf("partial metadata became authoritative: %+v, %v", preview, err)
			}
			if cache.SelectedCommit(repo.ID) != "" {
				t.Fatal("failed preview exposed selected revision")
			}
		})
	}
}

func TestPreviewSeedUsesBoundedConcurrentBatches(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("PREVIEW_CALLS", log)
	github := fakeGitHub(t, `printf '%s\n' "start $$" >> "$PREVIEW_CALLS"
sleep 0.15
cat <<'ROOTS'
`+previewRootJSON([]githubPreviewEntry{}, githubPreviewBatchSize)+`
ROOTS
printf '%s\n' "end $$" >> "$PREVIEW_CALLS"
`)
	cache := newPreviewFixtureCache(t, t.TempDir(), github)
	repos := make([]Repository, 0, 60)
	for i := 0; i < 60; i++ {
		repos = append(repos, previewGitHubRepo("fixture", "project"+strconv.Itoa(i)))
	}
	if err := cache.SeedGitHubRoots(context.Background(), repos); err != nil {
		t.Fatal(err)
	}
	logBytes, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	active, peak, starts := 0, 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(logBytes)), "\n") {
		if strings.HasPrefix(line, "start ") {
			active++
			starts++
			peak = max(peak, active)
		} else {
			active--
		}
	}
	if starts != 5 || peak < 2 || peak > 4 || active != 0 {
		t.Fatalf("batch requests=%d, peak=%d, active=%d: %s", starts, peak, active, logBytes)
	}
	for _, repo := range repos {
		if cache.SelectedRepositoryCommit(repo) != previewCommit {
			t.Fatalf("unseeded repository: %s", repo.ID)
		}
	}
}

func TestPreviewCloseCancelsPendingSeedAndWaiter(t *testing.T) {
	control := t.TempDir()
	t.Setenv("PREVIEW_CONTROL", control)
	github := fakeGitHub(t, `touch "$PREVIEW_CONTROL/started"
while :; do sleep 1; done
`)
	cache := newPreviewFixtureCache(t, t.TempDir(), github)
	repo := previewGitHubRepo("fixture", "project")
	seedDone := make(chan error, 1)
	go func() { seedDone <- cache.SeedGitHubRoots(context.Background(), []Repository{repo}) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(control, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed did not start")
		}
		time.Sleep(time.Millisecond)
	}
	waiterDone := make(chan error, 1)
	go func() { _, err := cache.Acquire(context.Background(), repo); waiterDone <- err }()
	start := time.Now()
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("close waited for a blocked metadata request")
	}
	if err := <-seedDone; err == nil {
		t.Fatal("canceled seed succeeded")
	}
	if err := <-waiterDone; err == nil {
		t.Fatal("closed cache supplied a preview")
	}
}

func TestPreviewRejectsMismatchedSubtreeRevisionWithoutAuthoritativeEmpty(t *testing.T) {
	root := previewRootJSON([]githubPreviewEntry{previewEntry("src", "tree", previewChildTree, 0o040000, 0)}, 1)
	github := fakeGitHub(t, `case "$*" in
*'object(oid:'*) cat <<'WRONG'
{"data":{"repository":{"object":{"__typename":"Tree","oid":"5555555555555555555555555555555555555555","entries":[]}}}}
WRONG
;;
*) cat <<'ROOT'
`+root+`
ROOT
;;
esac
`)
	cache := newPreviewFixtureCache(t, t.TempDir(), github)
	preview, err := cache.Acquire(context.Background(), previewGitHubRepo("fixture", "project"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if revision, entries, err := preview.Directory(context.Background(), "src"); err == nil || revision != "" || entries != nil {
			t.Fatalf("wrong subtree became authoritative: %q, %+v, %v", revision, entries, err)
		}
	}
	_, entries, err := preview.Directory(context.Background(), ".")
	if err != nil || !reflect.DeepEqual(previewNames(entries), []string{"src"}) {
		t.Fatalf("failed subtree mutated root: %+v, %v", entries, err)
	}
}

func TestPreviewNativeInvalidationRefreshesCommitAndPreservesOldSnapshot(t *testing.T) {
	source, _ := adoptionSource(t)
	adoptionGit(t, source, "config", "uploadpack.allowFilter", "true")
	state := t.TempDir()
	cache := newPreviewFixtureCache(t, state, nil)
	repo := Repository{ID: "local/project", Owner: "local", Name: "project", DefaultBranch: "trunk", CloneURL: source, Source: "manual"}
	old, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	oldRevision, oldEntries, err := old.Directory(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	old.mu.Lock()
	err = cache.Invalidate(context.Background(), repo)
	old.mu.Unlock()
	if !errors.Is(err, ErrPreviewInUse) {
		t.Fatalf("active directory guard=%v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "new.txt"), []byte("new committed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	adoptionGit(t, source, "add", "new.txt")
	adoptionGit(t, source, "commit", "-m", "new remote revision")
	if err := cache.Invalidate(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if cache.SelectedRepositoryCommit(repo) != "" {
		t.Fatal("invalidation retained selected commit")
	}
	fresh, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	newRevision, newEntries, err := fresh.Directory(context.Background(), ".")
	if err != nil || newRevision == oldRevision || fresh == old {
		t.Fatalf("renewal=%q, %+v, %v", newRevision, newEntries, err)
	}
	found := false
	for _, entry := range newEntries {
		if entry.Path == "new.txt" {
			found = true
			if entry.SizeState != "unknown" {
				t.Fatal("renewal hydrated new blob")
			}
		}
	}
	if !found {
		t.Fatal("renewal reused old private Git HEAD")
	}
	gotRevision, gotEntries, err := old.Directory(context.Background(), ".")
	if err != nil || gotRevision != oldRevision || !reflect.DeepEqual(oldEntries, gotEntries) {
		t.Fatalf("retired preview baseline changed: %q, %+v, %v", gotRevision, gotEntries, err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newPreviewFixtureCache(t, state, nil)
	loaded, err := reopened.Acquire(context.Background(), repo)
	if err != nil || loaded.Commit != newRevision {
		t.Fatalf("new baseline was not durable: %+v, %v", loaded, err)
	}
}

func TestPreviewRetiredLazySubtreeCannotOverwriteFreshReceipt(t *testing.T) {
	control := t.TempDir()
	t.Setenv("PREVIEW_CONTROL", control)
	oldRoot := previewRootJSON([]githubPreviewEntry{previewEntry("src", "tree", previewChildTree, 0o040000, 0)}, 1)
	newRoot := previewRootJSON([]githubPreviewEntry{previewEntry("new.txt", "blob", previewBlob, 0o100644, 14)}, 1)
	newCommit := "6666666666666666666666666666666666666666"
	newRoot = strings.ReplaceAll(newRoot, previewCommit, newCommit)
	newRoot = strings.ReplaceAll(newRoot, previewRootTree, "7777777777777777777777777777777777777777")
	github := fakeGitHub(t, `case "$*" in
*'object(oid:'*) cat <<'SUBTREE'
{"data":{"repository":{"object":{"__typename":"Tree","oid":"3333333333333333333333333333333333333333","entries":[]}}}}
SUBTREE
;;
*) if [ -f "$PREVIEW_CONTROL/new" ]; then cat <<'NEW'
`+newRoot+`
NEW
else cat <<'OLD'
`+oldRoot+`
OLD
fi
;;
esac
`)
	state := t.TempDir()
	cache := newPreviewFixtureCache(t, state, github)
	repo := previewGitHubRepo("fixture", "project")
	old, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Invalidate(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "new"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh, err := cache.Acquire(context.Background(), repo)
	if err != nil || fresh.Commit != newCommit {
		t.Fatalf("fresh preview=%+v, %v", fresh, err)
	}
	revision, entries, err := old.Directory(context.Background(), "src")
	if err != nil || revision != previewCommit || len(entries) != 0 {
		t.Fatalf("retired lazy preview=%q, %+v, %v", revision, entries, err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newPreviewFixtureCache(t, state, github)
	loaded, err := reopened.Acquire(context.Background(), repo)
	if err != nil || loaded.Commit != newCommit {
		t.Fatalf("retired object replaced current receipt: %+v, %v", loaded, err)
	}
}

func TestPreviewFailedRefreshPreservesOfflineMetadataAcrossRestart(t *testing.T) {
	control, state := t.TempDir(), t.TempDir()
	t.Setenv("PREVIEW_CONTROL", control)
	root := previewRootJSON([]githubPreviewEntry{previewEntry("cached.txt", "blob", previewBlob, 0o100644, 42)}, 1)
	github := fakeGitHub(t, `printf '%s\n' request >> "$PREVIEW_CONTROL/calls"
if [ -f "$PREVIEW_CONTROL/offline" ]; then exit 1; fi
cat <<'ROOT'
`+root+`
ROOT
`)
	cache := newPreviewFixtureCache(t, state, github)
	repo := previewGitHubRepo("fixture", "project")
	old, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "offline"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if fresh, err := cache.Refresh(context.Background(), repo); err == nil || fresh != nil {
		t.Fatalf("offline refresh succeeded: %+v, %v", fresh, err)
	}
	current, err := cache.Acquire(context.Background(), repo)
	if err != nil || current != old || cache.SelectedRepositoryCommit(repo) != previewCommit {
		t.Fatalf("failed refresh discarded current baseline: %+v, %v", current, err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	callsBefore, err := os.ReadFile(filepath.Join(control, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	reopened := newPreviewFixtureCache(t, state, github)
	current, err = reopened.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	revision, entries, err := current.Directory(context.Background(), ".")
	if err != nil || revision != previewCommit || !reflect.DeepEqual(previewNames(entries), []string{"cached.txt"}) {
		t.Fatalf("offline restart lost cached preview: %q, %+v, %v", revision, entries, err)
	}
	callsAfter, _ := os.ReadFile(filepath.Join(control, "calls"))
	if string(callsBefore) != string(callsAfter) {
		t.Fatal("offline restore required a network request")
	}
}

func TestPreviewCanceledRefreshJoinsOwnerAndKeepsDurableOfflineBaseline(t *testing.T) {
	for _, closeCache := range []bool{false, true} {
		name := "caller cancellation"
		if closeCache {
			name = "shutdown cancellation"
		}
		t.Run(name, func(t *testing.T) {
			control, state := t.TempDir(), t.TempDir()
			t.Setenv("PREVIEW_CONTROL", control)
			root := previewRootJSON([]githubPreviewEntry{previewEntry("cached.txt", "blob", previewBlob, 0o100644, 42)}, 1)
			github := fakeGitHub(t, `if [ -f "$PREVIEW_CONTROL/block" ]; then
touch "$PREVIEW_CONTROL/started"
while :; do sleep 1; done
fi
cat <<'ROOT'
`+root+`
ROOT
`)
			cache := newPreviewFixtureCache(t, state, github)
			repo := previewGitHubRepo("fixture", "project")
			old, err := cache.Acquire(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(control, "block"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := cache.Refresh(ctx, repo); done <- err }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(control, "started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("refresh did not start")
				}
				time.Sleep(time.Millisecond)
			}
			// Normal metadata access remains on the previous immutable baseline;
			// its durable receipt is present during the blocked network request.
			current, err := cache.Acquire(context.Background(), repo)
			if err != nil || current != old {
				t.Fatal("pending refresh replaced current baseline")
			}
			bytes, err := os.ReadFile(filepath.Join(cache.root, previewKey(repo), "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			var receipt previewReceipt
			if err := json.Unmarshal(bytes, &receipt); err != nil || receipt.Commit != previewCommit {
				t.Fatal("pending refresh lost durable baseline")
			}
			if closeCache {
				if err := cache.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil || !closeCache && !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled refresh = %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("refresh returned without joining its canceled owner")
			}
			cache.mu.Lock()
			pending := cache.runs[previewKey(repo)] != nil
			cache.mu.Unlock()
			if pending {
				t.Fatal("canceled refresh owner is still pending")
			}
			if err := cache.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := newPreviewFixtureCache(t, state, github)
			current, err = reopened.Acquire(context.Background(), repo)
			if err != nil || current.Commit != previewCommit {
				t.Fatalf("canceled refresh stranded offline metadata: %+v, %v", current, err)
			}
		})
	}
}

func TestPreviewTransactionalRefreshPublishesNewAndKeepsOldObject(t *testing.T) {
	source, _ := adoptionSource(t)
	adoptionGit(t, source, "config", "uploadpack.allowFilter", "true")
	cache := newPreviewFixtureCache(t, t.TempDir(), nil)
	repo := Repository{ID: "local/project", Owner: "local", Name: "project", CloneURL: source, DefaultBranch: "trunk", Source: "manual"}
	old, err := cache.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	old.mu.Lock()
	_, err = cache.Refresh(context.Background(), repo)
	old.mu.Unlock()
	if !errors.Is(err, ErrPreviewInUse) {
		t.Fatalf("active preview guard = %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "new.txt"), []byte("new committed file"), 0o600); err != nil {
		t.Fatal(err)
	}
	adoptionGit(t, source, "add", "new.txt")
	adoptionGit(t, source, "commit", "-m", "next revision")
	fresh, err := cache.Refresh(context.Background(), repo)
	if err != nil || fresh.Commit == old.Commit || fresh == old {
		t.Fatalf("transactional refresh = %+v, %v", fresh, err)
	}
	_, entries, err := old.Directory(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Path == "new.txt" {
			t.Fatal("old immutable object advanced revision")
		}
	}
	_, entries, err = fresh.Directory(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Path == "new.txt" {
			found = true
		}
	}
	if !found || cache.SelectedRepositoryCommit(repo) != fresh.Commit {
		t.Fatal("replacement was not published")
	}
}

// Opt-in integration exercises the official server schema and the same gh
// credential delegation used by the app, against public repository metadata.
// It never asks for token output or file contents and never touches live state.
func TestPreviewLivePublicGitHub(t *testing.T) {
	if os.Getenv("AFS_PREVIEW_LIVE") != "1" {
		t.Skip("set AFS_PREVIEW_LIVE=1 to verify public GitHub metadata acquisition")
	}
	ghPath := os.Getenv("AFS_PREVIEW_GH_PATH")
	if ghPath == "" {
		ghPath = "gh"
	}
	github := NewGitHub(ghPath)
	t.Cleanup(github.Close)
	cache := newPreviewFixtureCache(t, t.TempDir(), github)
	repo := previewGitHubRepo("cloudflare", "artifact-fs")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if err := cache.SeedGitHubRoots(ctx, []Repository{repo}); err != nil {
		t.Fatal(err)
	}
	preview, err := cache.Acquire(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	revision, entries, err := preview.Directory(ctx, ".")
	if err != nil || !validPreviewOID(revision) || len(entries) == 0 {
		t.Fatalf("public root preview returned %d entries: %v", len(entries), err)
	}
	rootEntries := len(entries)
	var child string
	for _, entry := range entries {
		if entry.SizeState != "known" {
			t.Fatal("public schema did not supply an exact metadata size")
		}
		if entry.Type == "dir" && entry.Path == "internal" {
			child = entry.Path
		}
	}
	if child == "" {
		t.Fatal("public fixture no longer contains its expected subtree")
	}
	revision, entries, err = preview.Directory(ctx, child)
	if err != nil || revision != preview.Commit || len(entries) == 0 {
		t.Fatalf("public subtree preview returned %d entries: %v", len(entries), err)
	}
	for _, entry := range entries {
		if entry.SizeState != "known" {
			t.Fatal("public subtree size is unresolved")
		}
	}
	if _, err := os.Lstat(filepath.Join(cache.root, previewKey(repo), "git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("public GitHub metadata acquisition prepared Git clone")
	}
	t.Logf("public metadata result: root_entries=%d subtree_entries=%d known_sizes=true git_clones=0 blob_downloads=0 elapsed_ms=%d", rootEntries, len(entries), time.Since(start).Milliseconds())
}
