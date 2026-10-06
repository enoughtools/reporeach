package gitstore

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// This is a real filtered smart-HTTP source, rather than a fake git executable:
// request counts expose accidentally falling back to one lazy fetch per blob.
type prefetchHTTPFixture struct {
	store      *Store
	repo       model.RepoConfig
	blobs      map[string][]byte
	oids       []string
	historical string
	unrelated  string
	requests   atomic.Int64
	online     atomic.Bool
	blocked    atomic.Bool
	started    chan struct{}
	released   chan struct{}
	canceled   chan struct{}
	startOnce  sync.Once
	cancelOnce sync.Once
}

func newPrefetchHTTPFixture(t *testing.T, count int) *prefetchHTTPFixture {
	t.Helper()
	f := &prefetchHTTPFixture{blobs: make(map[string][]byte), started: make(chan struct{}), released: make(chan struct{}), canceled: make(chan struct{})}
	f.online.Store(true)
	root := t.TempDir()
	source := filepath.Join(root, "source")
	run(t, "git", "init", "--initial-branch=main", source)
	run(t, "git", "-C", source, "config", "user.name", "prefetch fixture")
	run(t, "git", "-C", source, "config", "user.email", "fixture@example.invalid")
	write := func(name string, content []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(source, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("historical.bin", []byte{0, 0xff, 0x80, 'h', 'i', 's', 't', 'o', 'r', 'y', '\n'})
	run(t, "git", "-C", source, "add", ".")
	run(t, "git", "-C", source, "commit", "-m", "historical content")
	f.historical = strings.TrimSpace(runOutput(t, "git", "-C", source, "rev-parse", "HEAD:historical.bin"))
	firstCommit := strings.TrimSpace(runOutput(t, "git", "-C", source, "rev-parse", "HEAD"))
	run(t, "git", "-C", source, "rm", "historical.bin")
	for i := 0; i < count; i++ {
		content := make([]byte, 2048+i%13)
		for j := range content {
			content[j] = byte(j*37 + i*17)
		}
		binary.LittleEndian.PutUint32(content, uint32(i))
		content[4], content[5], content[len(content)-1] = 0, 0xff, '\n'
		write(fmt.Sprintf("selected-%03d.bin", i), content)
	}
	run(t, "git", "-C", source, "add", ".")
	run(t, "git", "-C", source, "commit", "-m", "current binary tree")
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("selected-%03d.bin", i)
		oid := strings.TrimSpace(runOutput(t, "git", "-C", source, "rev-parse", "HEAD:"+name))
		content, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		f.blobs[oid], f.oids = content, append(f.oids, oid)
	}
	run(t, "git", "-C", source, "checkout", "-b", "unrelated", firstCommit)
	write("unrelated.bin", []byte{0xff, 0, 0x81, 'u', 'n', 'r', 'e', 'l', 'a', 't', 'e', 'd'})
	run(t, "git", "-C", source, "add", ".")
	run(t, "git", "-C", source, "commit", "-m", "unrelated branch")
	f.unrelated = strings.TrimSpace(runOutput(t, "git", "-C", source, "rev-parse", "HEAD:unrelated.bin"))
	run(t, "git", "-C", source, "checkout", "main")
	bare := filepath.Join(root, "source.git")
	run(t, "git", "clone", "--bare", source, bare)
	run(t, "git", "--git-dir", bare, "config", "uploadpack.allowFilter", "true")
	run(t, "git", "--git-dir", bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if !f.online.Load() {
			http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
			return
		}
		if f.blocked.Load() {
			f.startOnce.Do(func() { close(f.started) })
			select {
			case <-r.Context().Done():
				f.cancelOnce.Do(func() { close(f.canceled) })
				return
			case <-f.released:
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		var request io.Reader = http.MaxBytesReader(w, r.Body, 1<<20)
		requestLength := r.ContentLength
		// Git compresses larger want lists. A real smart-HTTP web server
		// decodes Content-Encoding before invoking its CGI backend.
		if r.Header.Get("Content-Encoding") == "gzip" {
			compressed, err := gzip.NewReader(request)
			if err != nil {
				http.Error(w, "invalid compressed request", 400)
				return
			}
			body, err := io.ReadAll(io.LimitReader(compressed, (1<<20)+1))
			compressed.Close()
			if err != nil || len(body) > 1<<20 {
				http.Error(w, "invalid compressed request size", 400)
				return
			}
			request, requestLength = bytes.NewReader(body), int64(len(body))
		}
		cmd := exec.CommandContext(ctx, "git", "http-backend")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_PROJECT_ROOT="+root, "GIT_HTTP_EXPORT_ALL=1", "REQUEST_METHOD="+r.Method,
			"QUERY_STRING="+r.URL.RawQuery, "PATH_INFO="+r.URL.Path, "CONTENT_TYPE="+r.Header.Get("Content-Type"),
			"CONTENT_LENGTH="+strconv.FormatInt(requestLength, 10), "HTTP_GIT_PROTOCOL="+r.Header.Get("Git-Protocol"), "GATEWAY_INTERFACE=CGI/1.1")
		cmd.Stdin = request
		cmd.WaitDelay = time.Second
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			http.Error(w, "backend pipe", 500)
			return
		}
		if err := cmd.Start(); err != nil {
			stdout.Close()
			http.Error(w, "backend start", 500)
			return
		}
		defer func() { stdout.Close(); _ = cmd.Wait() }()
		reader := bufio.NewReader(stdout)
		headers, err := textproto.NewReader(reader).ReadMIMEHeader()
		if err != nil {
			cancel()
			http.Error(w, "backend headers", 502)
			return
		}
		status := http.StatusOK
		if value := headers.Get("Status"); value != "" {
			status, _ = strconv.Atoi(strings.Fields(value)[0])
			headers.Del("Status")
		}
		for key, values := range headers {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(status)
		_, _ = io.Copy(w, io.LimitReader(reader, 8<<20))
	}))
	t.Cleanup(func() { close(f.released); server.Close() })
	f.store = New(nil)
	// Keep permanent transport-failure tests fast; retries themselves are tested
	// separately in gitstore_test.go.
	f.store.gitRetryDelays = nil
	t.Cleanup(f.store.Close)
	f.repo = model.RepoConfig{ID: "prefetch", Name: "prefetch", Branch: "main", HistoryDepth: 1,
		GitDir: filepath.Join(root, "client.git"), RemoteURL: server.URL + "/source.git", BlobCacheDir: filepath.Join(root, "cache")}
	if err := f.store.CloneBloblessNonInteractive(context.Background(), f.repo); err != nil {
		t.Fatal(err)
	}
	prefetchAssertMissing(t, f, append(append([]string{}, f.oids...), f.historical, f.unrelated))
	return f
}

func prefetchAssertMissing(t *testing.T, f *prefetchHTTPFixture, oids []string) {
	t.Helper()
	missing, err := missingBlobObjects(context.Background(), f.repo, oids)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, oids) {
		t.Fatalf("missing objects = %v, want %v", missing, oids)
	}
}

// Compare native repository metadata byte-for-byte, including loose refs that
// would be deleted if inherited prune settings or refmaps leaked into fetch.
func prefetchNativeState(t *testing.T, gitDir string) map[string][]byte {
	t.Helper()
	state := make(map[string][]byte)
	for _, name := range []string{"HEAD", "index", "config", "FETCH_HEAD", "packed-refs", "shallow"} {
		content, err := os.ReadFile(filepath.Join(gitDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		state[name] = content
	}
	for _, directory := range []string{"refs", "logs"} {
		err := filepath.WalkDir(filepath.Join(gitDir, directory), func(path string, entry os.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil || entry.IsDir() {
				return err
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			name, err := filepath.Rel(gitDir, path)
			state[name] = content
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func TestPrefetchBlobsBulkBinarySelectedOnlyAndNativeStatePreserved(t *testing.T) {
	f := newPrefetchHTTPFixture(t, 189)
	// Keep a batch cat-file reader alive before the bulk fetch. It must see
	// newly installed pack files without restarting or falling back to lazy I/O.
	warmOID := f.oids[0]
	if _, err := f.store.BlobToCache(context.Background(), f.repo, warmOID, filepath.Join(f.repo.BlobCacheDir, warmOID)); err != nil {
		t.Fatalf("warm existing batch reader: %v", err)
	}
	git := func(args ...string) {
		t.Helper()
		run(t, "git", append([]string{"--git-dir", f.repo.GitDir}, args...)...)
	}
	for _, key := range []string{"fetch.prune", "fetch.pruneTags", "remote.origin.prune", "remote.origin.pruneTags"} {
		git("config", "--local", key, "true")
	}
	git("update-ref", "refs/remotes/origin/native-only", "HEAD")
	git("update-ref", "refs/tags/native-only", "HEAD")
	git("config", "--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*")
	if err := os.WriteFile(filepath.Join(f.repo.GitDir, "FETCH_HEAD"), []byte("native fetch state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := prefetchNativeState(t, f.repo.GitDir)
	requests := f.requests.Load()
	// Duplicate inputs must not create duplicate fetches.
	selected := append(append([]string{}, f.oids[1:]...), f.oids[1], f.oids[188])
	if err := f.store.PrefetchBlobs(context.Background(), f.repo, selected); err != nil {
		t.Fatal(err)
	}
	if got := f.requests.Load() - requests; got <= 0 || got > 5 {
		t.Fatalf("188 selected blobs used %d HTTP requests, want one bounded bulk transfer (1..5 requests)", got)
	} else {
		t.Logf("188 selected binary blobs fetched in %d HTTP requests", got)
	}
	if after := prefetchNativeState(t, f.repo.GitDir); !reflect.DeepEqual(before, after) {
		t.Fatalf("bulk prefetch changed native metadata: before keys=%v after keys=%v", prefetchStateKeys(before), prefetchStateKeys(after))
	}
	prefetchAssertMissing(t, f, []string{f.historical, f.unrelated})
	f.online.Store(false)
	requests = f.requests.Load()
	if err := f.store.PrefetchBlobs(context.Background(), f.repo, selected); err != nil {
		t.Fatalf("repeat offline prefetch: %v", err)
	}
	for _, oid := range f.oids {
		path := filepath.Join(f.repo.BlobCacheDir, oid)
		size, err := f.store.BlobToCache(context.Background(), f.repo, oid, path)
		if err != nil {
			t.Fatalf("binary cache %s: %v", oid, err)
		}
		got, err := os.ReadFile(path)
		if err != nil || size != int64(len(f.blobs[oid])) || !bytes.Equal(got, f.blobs[oid]) {
			t.Fatalf("binary cache %s changed: size=%d error=%v", oid, size, err)
		}
	}
	if got := f.requests.Load() - requests; got != 0 {
		t.Fatalf("repeat offline prefetch / binary extraction made %d HTTP requests", got)
	}
	if after := prefetchNativeState(t, f.repo.GitDir); !reflect.DeepEqual(before, after) {
		t.Fatal("offline extraction changed native refs, index, config or fetch state")
	}
}

func prefetchStateKeys(state map[string][]byte) []string {
	keys := make([]string, 0, len(state))
	for key := range state {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestPrefetchBlobsNativeRepositoryStaysNonPromisor(t *testing.T) {
	f := newPrefetchHTTPFixture(t, 2)
	native := f.repo
	native.GitDir = filepath.Join(t.TempDir(), "native.git")
	run(t, "git", "init", "--bare", "--initial-branch=main", native.GitDir)
	run(t, "git", "--git-dir", native.GitDir, "remote", "add", "origin", native.RemoteURL)
	if err := os.WriteFile(filepath.Join(native.GitDir, "FETCH_HEAD"), []byte("native fetch sentinel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := prefetchNativeState(t, native.GitDir)
	requests := f.requests.Load()
	if err := f.store.PrefetchBlobs(context.Background(), native, f.oids); err != nil {
		t.Fatalf("prefetch into ordinary unborn native repository: %v", err)
	}
	if got := f.requests.Load() - requests; got <= 0 || got > 5 {
		t.Fatalf("native bulk fetch used %d HTTP requests, want 1..5", got)
	}
	if after := prefetchNativeState(t, native.GitDir); !reflect.DeepEqual(before, after) {
		t.Fatal("prefetch changed native HEAD, index, config, FETCH_HEAD or refs")
	}
	if missing, err := missingBlobObjects(context.Background(), native, f.oids); err != nil || len(missing) != 0 {
		t.Fatalf("native repository did not receive selected blobs: %v %v", missing, err)
	}
	missing, err := missingBlobObjects(context.Background(), native, []string{f.historical, f.unrelated})
	if err != nil || !reflect.DeepEqual(missing, []string{f.historical, f.unrelated}) {
		t.Fatalf("native repository acquired unrelated history: %v %v", missing, err)
	}
	cmd := exec.Command("git", "--git-dir", native.GitDir, "config", "--local", "--get-regexp", `^(extensions\.partialclone|remote\.origin\.(promisor|partialclonefilter))$`)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if len(output) != 0 || !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("native repository gained partial-clone configuration: output=%q error=%v", output, err)
	}
}

func TestPrefetchBlobsRejectsInvalidMissingAndNonBlobObjects(t *testing.T) {
	f := newPrefetchHTTPFixture(t, 2)
	before := prefetchNativeState(t, f.repo.GitDir)
	commit := strings.TrimSpace(runOutput(t, "git", "--git-dir", f.repo.GitDir, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(runOutput(t, "git", "--git-dir", f.repo.GitDir, "rev-parse", "HEAD^{tree}"))
	for _, oid := range []string{"", "HEAD", "--upload-pack=bad", strings.Repeat("z", 40), strings.Repeat("1", 39), commit, tree} {
		t.Run(oid, func(t *testing.T) {
			requests := f.requests.Load()
			if err := f.store.PrefetchBlobs(context.Background(), f.repo, []string{oid}); err == nil {
				t.Fatal("invalid or non-blob input succeeded")
			}
			if f.requests.Load() != requests {
				t.Fatal("invalid or present non-blob object contacted the source")
			}
		})
	}
	if err := f.store.PrefetchBlobs(context.Background(), f.repo, []string{strings.Repeat("1", 40)}); err == nil {
		t.Fatal("missing remote blob falsely succeeded")
	}
	prefetchAssertMissing(t, f, f.oids)
	if after := prefetchNativeState(t, f.repo.GitDir); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected object request changed native repository metadata")
	}
}

func TestPrefetchBlobsTransportFailureDoesNotClaimSuccessAndRetryWorks(t *testing.T) {
	f := newPrefetchHTTPFixture(t, 3)
	before := prefetchNativeState(t, f.repo.GitDir)
	f.online.Store(false)
	requests := f.requests.Load()
	if err := f.store.PrefetchBlobs(context.Background(), f.repo, f.oids); err == nil {
		t.Fatal("unavailable source falsely succeeded")
	}
	if f.requests.Load() <= requests {
		t.Fatal("failure fixture was never contacted")
	}
	prefetchAssertMissing(t, f, f.oids)
	if after := prefetchNativeState(t, f.repo.GitDir); !reflect.DeepEqual(before, after) {
		t.Fatal("transport failure changed native repository metadata")
	}
	f.online.Store(true)
	if err := f.store.PrefetchBlobs(context.Background(), f.repo, f.oids); err != nil {
		t.Fatalf("retry after transport failure: %v", err)
	}
	if missing, err := missingBlobObjects(context.Background(), f.repo, f.oids); err != nil || len(missing) != 0 {
		t.Fatalf("retry did not acquire selected blobs: %v %v", missing, err)
	}
}

func TestPrefetchBlobsCancellationReleasesBlockedHTTPAndRetryWorks(t *testing.T) {
	f := newPrefetchHTTPFixture(t, 3)
	before := prefetchNativeState(t, f.repo.GitDir)
	f.blocked.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { result <- f.store.PrefetchBlobs(ctx, f.repo, f.oids) }()
	select {
	case <-f.started:
	case err := <-result:
		t.Fatalf("fetch returned before blocked request: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not reach blocked HTTP source")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled bulk fetch did not return")
	}
	select {
	case <-f.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled fetch left its HTTP request running")
	}
	prefetchAssertMissing(t, f, f.oids)
	if after := prefetchNativeState(t, f.repo.GitDir); !reflect.DeepEqual(before, after) {
		t.Fatal("cancellation changed native repository metadata")
	}
	f.blocked.Store(false)
	if err := f.store.PrefetchBlobs(context.Background(), f.repo, f.oids); err != nil {
		t.Fatalf("retry after cancellation: %v", err)
	}
	if missing, err := missingBlobObjects(context.Background(), f.repo, f.oids); err != nil || len(missing) != 0 {
		t.Fatalf("retry did not acquire selected blobs: %v %v", missing, err)
	}
}
