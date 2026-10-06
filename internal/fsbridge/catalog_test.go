//go:build !windows

package fsbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

var bridgeCatalogEntries = []catalogfs.Entry{
	{ID: "alice/project", Owner: "alice", Name: "project"},
	{ID: "team/project", Owner: "team", Name: "project"},
}

type bridgeCatalogFixture struct {
	backend  *fusefs.ArtifactFuse
	hydrator *bridgeCatalogHydrator
	content  []byte
}

// This fixture satisfies the canonical hydrator interface and keeps the blob
// binary throughout the real snapshot, overlay and filesystem adapter path.
type bridgeCatalogHydrator struct {
	path    string
	content []byte
	calls   atomic.Int64
}

var _ model.Hydrator = (*bridgeCatalogHydrator)(nil)

func (*bridgeCatalogHydrator) Enqueue(model.HydrationTask)        {}
func (*bridgeCatalogHydrator) EnqueueBatch([]model.HydrationTask) {}
func (*bridgeCatalogHydrator) QueueDepth(model.RepoID) int        { return 0 }
func (h *bridgeCatalogHydrator) EnsureHydrated(context.Context, model.RepoConfig, model.BaseNode) (string, int64, error) {
	h.calls.Add(1)
	return h.path, int64(len(h.content)), nil
}
func (h *bridgeCatalogHydrator) OpenHydrated(context.Context, model.RepoConfig, model.BaseNode) (*os.File, int64, error) {
	h.calls.Add(1)
	f, err := os.Open(h.path)
	return f, int64(len(h.content)), err
}
func (h *bridgeCatalogHydrator) ReadBlob(context.Context, model.RepoConfig, model.BaseNode, int64) ([]byte, error) {
	h.calls.Add(1)
	return bytes.Clone(h.content), nil
}

func newBridgeCatalogFixture(t *testing.T, id string) bridgeCatalogFixture {
	return newBridgeCatalogFixtureWithSizeState(t, id, "known")
}

func newBridgeCatalogFixtureWithSizeState(t *testing.T, id, sizeState string) bridgeCatalogFixture {
	t.Helper()
	root := t.TempDir()
	ctx := context.Background()
	cfg := model.RepoConfig{
		ID: model.RepoID(id), Name: "project", GitDir: filepath.Join(root, "git"),
		OverlayDir: filepath.Join(root, "overlay"), BlobCacheDir: filepath.Join(root, "cache"),
		MetaDBPath: filepath.Join(root, "snapshot.db"), OverlayDBPath: filepath.Join(root, "overlay.db"),
	}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snap.Close() })
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })
	content := append([]byte(id), 0, 0xff, 0x01, 0xfe)
	sizeBytes := int64(0)
	if sizeState == "known" {
		sizeBytes = int64(len(content))
	}
	gen, err := snap.PublishGeneration(ctx, "commit", "main", []model.BaseNode{
		{RepoID: cfg.ID, Path: ".", Type: "dir", Mode: 0o755, SizeState: "known", SizeBytes: 4096},
		{RepoID: cfg.ID, Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "blob", SizeState: sizeState, SizeBytes: sizeBytes},
	})
	if err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(root, "blob")
	if err := os.WriteFile(blobPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	hydrator := &bridgeCatalogHydrator{path: blobPath, content: content}
	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	engine := &fusefs.Engine{Repo: cfg, Resolver: resolver, Overlay: ov, Hydrator: hydrator}
	return bridgeCatalogFixture{backend: fusefs.NewArtifactFuse(cfg, resolver, engine), hydrator: hydrator, content: content}
}

// Separate wire structs ensure that these tests exercise the HTTP protocol and
// JSON field names rather than constructing handler-internal operations.
type bridgeCatalogNode struct {
	Inode      uint64 `json:"inode"`
	Generation uint64 `json:"generation"`
	Attributes struct {
		Size      uint64 `json:"size"`
		SizeKnown *bool  `json:"size_known"`
		Nlink     uint32 `json:"nlink"`
		Mode      uint32 `json:"mode"`
		MtimeNS   int64  `json:"mtime_ns"`
		Type      string `json:"type"`
	} `json:"attributes"`
}

type bridgeCatalogResponse struct {
	Version int               `json:"version"`
	Errno   int               `json:"errno"`
	Node    bridgeCatalogNode `json:"node"`
	Handle  uint64            `json:"handle"`
	Entries []struct {
		Name   string            `json:"name"`
		Offset uint64            `json:"offset"`
		Node   bridgeCatalogNode `json:"node"`
	} `json:"entries"`
	NextOffset uint64 `json:"next_offset"`
	EOF        bool   `json:"eof"`
	Written    int    `json:"written"`
	Target     string `json:"target"`
	Stat       *struct {
		BlockSize uint32 `json:"block_size"`
		Blocks    uint64 `json:"blocks"`
		Inodes    uint64 `json:"inodes"`
	} `json:"stat"`
	wireFields map[string]json.RawMessage
}

func (r *bridgeCatalogResponse) UnmarshalJSON(data []byte) error {
	type wireResponse bridgeCatalogResponse
	if err := json.Unmarshal(data, (*wireResponse)(r)); err != nil {
		return err
	}
	return json.Unmarshal(data, &r.wireFields)
}

func assertBridgeEmptyResponseFields(t *testing.T, result bridgeCatalogResponse) {
	t.Helper()
	for field, want := range map[string]string{"entries": "[]", "next_offset": "0", "eof": "false", "written": "0"} {
		got, present := result.wireFields[field]
		if !present || !bytes.Equal(bytes.TrimSpace(got), []byte(want)) {
			t.Fatalf("wire field %q = %s (present=%t), want %s", field, got, present, want)
		}
	}
}

type bridgeCatalogClient struct {
	server *httptest.Server
	token  string
}

func newBridgeCatalogClient(t *testing.T, fs *catalogfs.FileSystem) *bridgeCatalogClient {
	t.Helper()
	token := bytes.Repeat([]byte{0x41}, 32)
	handler, err := New(fs, token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &bridgeCatalogClient{server: server, token: hex.EncodeToString(token)}
}

func (c *bridgeCatalogClient) exchange(fields map[string]any) (bridgeCatalogResponse, error) {
	fields["version"] = 1
	data, err := json.Marshal(fields)
	if err != nil {
		return bridgeCatalogResponse{}, err
	}
	req, err := http.NewRequest(http.MethodPost, c.server.URL+"/v1/fs", bytes.NewReader(data))
	if err != nil {
		return bridgeCatalogResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.server.Client().Do(req)
	if err != nil {
		return bridgeCatalogResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return bridgeCatalogResponse{}, fmt.Errorf("%s returned HTTP %d", fields["op"], resp.StatusCode)
	}
	var result bridgeCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return result, err
	}
	if result.Version != 1 {
		return result, fmt.Errorf("response version = %d", result.Version)
	}
	return result, nil
}

func (c *bridgeCatalogClient) call(t *testing.T, fields map[string]any, errno syscall.Errno) bridgeCatalogResponse {
	t.Helper()
	result, err := c.exchange(fields)
	if err != nil {
		t.Fatal(err)
	}
	if result.Errno != int(errno) {
		t.Fatalf("%v: errno = %d, want %d", fields, result.Errno, errno)
	}
	return result
}

func (c *bridgeCatalogClient) lookup(t *testing.T, parent uint64, name string) uint64 {
	t.Helper()
	node := c.call(t, map[string]any{"op": "lookup", "parent": parent, "name": name}, 0).Node
	if node.Inode == 0 {
		t.Fatalf("lookup %q returned inode zero", name)
	}
	return node.Inode
}

func (c *bridgeCatalogClient) repository(t *testing.T, owner string) uint64 {
	t.Helper()
	return c.lookup(t, c.lookup(t, 1, owner), "project")
}

func (c *bridgeCatalogClient) opendir(t *testing.T, inode uint64) uint64 {
	t.Helper()
	return c.call(t, map[string]any{"op": "opendir", "inode": inode}, 0).Handle
}

func (c *bridgeCatalogClient) read(inode, handle uint64, offset, size int) ([]byte, error) {
	url := fmt.Sprintf("%s/v1/fs/read?inode=%d&handle=%d&offset=%d&size=%d", c.server.URL, inode, handle, offset, size)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.server.Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("binary read returned HTTP %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		return nil, fmt.Errorf("binary read returned Content-Type %q", got)
	}
	return io.ReadAll(resp.Body)
}

func (c *bridgeCatalogClient) readFile(t *testing.T, inode, handle uint64) []byte {
	t.Helper()
	data, err := c.read(inode, handle, 0, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (c *bridgeCatalogClient) write(t *testing.T, inode, handle uint64, offset int, data []byte) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/fs/write?inode=%d&handle=%d&offset=%d", c.server.URL, inode, handle, offset)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("binary write returned HTTP %d", resp.StatusCode)
	}
	var result bridgeCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Version != 1 || result.Errno != 0 || result.Written != len(data) {
		t.Fatalf("binary write response: %+v", result)
	}
	if len(data) == 0 {
		assertBridgeEmptyResponseFields(t, result)
	}
}

func (c *bridgeCatalogClient) fileFailure(t *testing.T, method, path string, body []byte, errno syscall.Errno) {
	t.Helper()
	req, err := http.NewRequest(method, c.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("binary failure returned HTTP %d", resp.StatusCode)
	}
	var result bridgeCatalogResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Version != 1 || result.Errno != int(errno) {
		t.Fatalf("binary failure: version=%d, errno=%d; want errno=%d", result.Version, result.Errno, errno)
	}
}

func TestCatalogBridgeBrowsingDoesNotActivateOrHydrate(t *testing.T) {
	var activations atomic.Int64
	fs, err := catalogfs.New(bridgeCatalogEntries, func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return nil, errors.New("unexpected repository activation")
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	assertBridgeEmptyResponseFields(t, c.call(t, map[string]any{"op": "getattr", "inode": 1}, 0))
	rootHandle := c.opendir(t, 1)
	listing := c.call(t, map[string]any{"op": "readdir", "inode": 1, "handle": rootHandle, "offset": 0, "size": 4096}, 0)
	if len(listing.Entries) != 2 || listing.Entries[0].Name != "alice" || listing.Entries[1].Name != "team" {
		t.Fatalf("catalogue owners: %+v", listing.Entries)
	}
	if listing.NextOffset != listing.Entries[len(listing.Entries)-1].Offset {
		t.Fatal("readdir continuation does not match the final entry")
	}
	lastPage := c.call(t, map[string]any{"op": "readdir", "inode": 1, "handle": rootHandle, "offset": listing.NextOffset}, 0)
	if !lastPage.EOF || len(lastPage.Entries) != 0 || lastPage.NextOffset != listing.NextOffset {
		t.Fatalf("readdir end-of-directory response: %+v", lastPage)
	}
	for _, entry := range listing.Entries {
		if entry.Node.Inode == 0 || entry.Offset == 0 {
			t.Fatalf("directory entry lacks identity or continuation: %+v", entry)
		}
		if entry.Node.Attributes.Type != "dir" || entry.Node.Attributes.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			t.Fatalf("owner lacks directory attributes: %+v", entry.Node)
		}
		handle := c.opendir(t, entry.Node.Inode)
		repos := c.call(t, map[string]any{"op": "readdir", "inode": entry.Node.Inode, "handle": handle, "size": 4096}, 0)
		if len(repos.Entries) != 1 || repos.Entries[0].Name != "project" {
			t.Fatalf("repositories under %s: %+v", entry.Name, repos.Entries)
		}
		c.call(t, map[string]any{"op": "getattr", "inode": repos.Entries[0].Node.Inode}, 0)
		c.call(t, map[string]any{"op": "releasedir", "handle": handle}, 0)
	}
	stat := c.call(t, map[string]any{"op": "statfs"}, 0).Stat
	if stat == nil || stat.BlockSize == 0 || stat.Blocks == 0 || stat.Inodes == 0 {
		t.Fatalf("filesystem statistics: %+v", stat)
	}
	c.call(t, map[string]any{"op": "releasedir", "handle": rootHandle}, 0)
	if activations.Load() != 0 {
		t.Fatalf("catalogue browsing activated %d repositories", activations.Load())
	}
}

func TestCatalogBridgeReaddirContinuationAcrossNonemptyPages(t *testing.T) {
	entries := make([]catalogfs.Entry, 300)
	want := make(map[string]bool, len(entries))
	for i := range entries {
		owner := fmt.Sprintf("owner-%03d-", i) + strings.Repeat("x", 200)
		entries[i] = catalogfs.Entry{ID: fmt.Sprintf("repo-%d", i), Owner: owner, Name: "project"}
		want[owner] = true
	}
	var activations atomic.Int64
	fs, err := catalogfs.New(entries, func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return nil, errors.New("unexpected repository activation")
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	handle := c.opendir(t, 1)
	var offset uint64
	nonemptyPages := 0
	seen := make(map[string]bool)
	for page := 0; ; page++ {
		if page == 10 {
			t.Fatal("directory pagination did not reach EOF")
		}
		listing := c.call(t, map[string]any{"op": "readdir", "inode": 1, "handle": handle, "offset": offset}, 0)
		if listing.EOF {
			if len(listing.Entries) != 0 || listing.NextOffset != offset {
				t.Fatal("EOF changed the continuation or returned entries")
			}
			break
		}
		nonemptyPages++
		for _, entry := range listing.Entries {
			if !want[entry.Name] || seen[entry.Name] || entry.Offset <= offset || entry.Node.Inode == 0 {
				t.Fatalf("invalid continued directory entry: %+v", entry)
			}
			seen[entry.Name] = true
			offset = entry.Offset
		}
		if len(listing.Entries) == 0 || listing.NextOffset != offset {
			t.Fatal("nonempty directory page did not advance its cookie")
		}
	}
	if nonemptyPages < 2 || len(seen) != len(want) {
		t.Fatalf("pagination returned %d owners in %d nonempty pages", len(seen), nonemptyPages)
	}
	c.call(t, map[string]any{"op": "releasedir", "handle": handle}, 0)
	if activations.Load() != 0 {
		t.Fatal("paginated catalogue browsing activated a repository")
	}
}

func TestCatalogBridgeSameNameRepositoriesKeepIndependentIdentitiesAndBytes(t *testing.T) {
	fixtures := map[string]bridgeCatalogFixture{}
	for _, entry := range bridgeCatalogEntries {
		fixtures[entry.ID] = newBridgeCatalogFixture(t, entry.ID)
	}
	var activations atomic.Int64
	fs, err := catalogfs.New(bridgeCatalogEntries, func(_ context.Context, entry catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return fixtures[entry.ID].backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	var inodes, handles []uint64
	for _, entry := range bridgeCatalogEntries {
		root := c.repository(t, entry.Owner)
		dir := c.opendir(t, root)
		listing := c.call(t, map[string]any{"op": "readdir", "inode": root, "handle": dir, "size": 4096}, 0)
		var file uint64
		seen := make(map[uint64]bool)
		for _, child := range listing.Entries {
			if child.Node.Inode == 0 || seen[child.Node.Inode] {
				t.Fatalf("missing or aliased directory inode: %+v", child)
			}
			seen[child.Node.Inode] = true
			if child.Name == "README.md" {
				file = child.Node.Inode
			}
		}
		if file == 0 {
			t.Fatal("repository listing omitted README.md")
		}
		if fixtures[entry.ID].hydrator.calls.Load() != 0 {
			t.Fatal("repository directory listing hydrated a blob")
		}
		// Readdir returns a lookup reference: its child remains valid after the
		// directory descriptor has been released and before any explicit lookup.
		c.call(t, map[string]any{"op": "releasedir", "handle": dir}, 0)
		attrs := c.call(t, map[string]any{"op": "getattr", "inode": file}, 0)
		if attrs.Node.Attributes.Size != uint64(len(fixtures[entry.ID].content)) {
			t.Fatalf("README size = %d", attrs.Node.Attributes.Size)
		}
		handle := c.call(t, map[string]any{"op": "open", "inode": file}, 0).Handle
		if got := c.readFile(t, file, handle); !bytes.Equal(got, fixtures[entry.ID].content) {
			t.Fatalf("wrong repository bytes: %x", got)
		}
		c.call(t, map[string]any{"op": "forget", "inode": file, "n": 2}, syscall.EINVAL)
		c.call(t, map[string]any{"op": "forget", "inode": file, "n": 1}, 0)
		c.call(t, map[string]any{"op": "getattr", "inode": file}, syscall.ESTALE)
		if got := c.readFile(t, file, handle); !bytes.Equal(got, fixtures[entry.ID].content) {
			t.Fatalf("forget invalidated an open file: %x", got)
		}
		inodes, handles = append(inodes, file), append(handles, handle)
	}
	if inodes[0] == inodes[1] || handles[0] == handles[1] {
		t.Fatal("same-name repositories alias inode or handle namespaces")
	}
	if activations.Load() != 2 {
		t.Fatalf("repository activation count = %d", activations.Load())
	}
	c.fileFailure(t, http.MethodGet, fmt.Sprintf("/v1/fs/read?inode=%d&handle=%d&offset=0&size=1024", inodes[0], handles[1]), nil, syscall.EBADF)
	c.fileFailure(t, http.MethodPut, fmt.Sprintf("/v1/fs/write?inode=%d&handle=%d&offset=0", inodes[0], handles[1]), []byte{0, 0xff}, syscall.EBADF)
	for i := range handles {
		if got := c.readFile(t, inodes[i], handles[i]); !bytes.Equal(got, fixtures[bridgeCatalogEntries[i].ID].content) {
			t.Fatal("rejected handle pairing changed repository bytes")
		}
		c.call(t, map[string]any{"op": "release", "handle": handles[i]}, 0)
		c.fileFailure(t, http.MethodGet, fmt.Sprintf("/v1/fs/read?inode=%d&handle=%d&offset=0&size=1024", inodes[i], handles[i]), nil, syscall.EBADF)
	}
}

func TestCatalogBridgeBinaryMutationsAndPOSIXErrors(t *testing.T) {
	fixtures := map[string]bridgeCatalogFixture{}
	for _, entry := range bridgeCatalogEntries {
		fixtures[entry.ID] = newBridgeCatalogFixture(t, entry.ID)
	}
	fs, err := catalogfs.New(bridgeCatalogEntries, func(_ context.Context, entry catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixtures[entry.ID].backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	root, otherRoot := c.repository(t, "alice"), c.repository(t, "team")
	c.call(t, map[string]any{"op": "mkdir", "parent": 1, "name": "local", "mode": 0o755}, syscall.EROFS)
	c.call(t, map[string]any{"op": "open", "inode": root}, syscall.EISDIR)
	c.call(t, map[string]any{"op": "lookup", "parent": root, "name": "missing"}, syscall.ENOENT)
	for _, name := range []string{"..", ".", "../outside", "nested/file", "bad\x00name"} {
		c.call(t, map[string]any{"op": "create", "parent": root, "name": name, "mode": 0o644}, syscall.EINVAL)
	}
	created := c.call(t, map[string]any{"op": "create", "parent": root, "name": "binary.dat", "mode": 0o640}, 0)
	data := []byte{0, 1, 0xff, 0xfe, 'x', 0, 'y'}
	c.write(t, created.Node.Inode, created.Handle, 0, data)
	c.write(t, created.Node.Inode, created.Handle, 0, nil)
	c.write(t, created.Node.Inode, created.Handle, 2, []byte{0x80, 0, 0xfd})
	copy(data[2:], []byte{0x80, 0, 0xfd})
	if got := c.readFile(t, created.Node.Inode, created.Handle); !bytes.Equal(got, data) {
		t.Fatalf("binary writes were changed: %x", got)
	}
	part, err := c.read(created.Node.Inode, created.Handle, 1, 3)
	if err != nil || !bytes.Equal(part, data[1:4]) {
		t.Fatalf("offset read: %x, %v", part, err)
	}
	c.call(t, map[string]any{"op": "fsync", "inode": created.Node.Inode, "handle": created.Handle}, 0)
	c.call(t, map[string]any{"op": "flush", "inode": created.Node.Inode, "handle": created.Handle}, 0)
	c.call(t, map[string]any{"op": "rename", "old_parent": root, "new_parent": root, "old_name": "binary.dat", "new_name": "renamed.dat"}, 0)
	if renamed := c.lookup(t, root, "renamed.dat"); renamed != created.Node.Inode {
		t.Fatal("rename changed inode identity")
	}
	c.call(t, map[string]any{"op": "rename", "old_parent": root, "new_parent": otherRoot, "old_name": "renamed.dat", "new_name": "copied.dat"}, syscall.EXDEV)
	c.call(t, map[string]any{"op": "unlink", "parent": root, "name": "renamed.dat"}, 0)
	c.call(t, map[string]any{"op": "lookup", "parent": root, "name": "renamed.dat"}, syscall.ENOENT)
	if got := c.readFile(t, created.Node.Inode, created.Handle); !bytes.Equal(got, data) {
		t.Fatal("unlink lost an open file's bytes")
	}
	c.call(t, map[string]any{"op": "release", "handle": created.Handle}, 0)
	dir := c.call(t, map[string]any{"op": "mkdir", "parent": root, "name": "empty", "mode": 0o755}, 0)
	if dir.Node.Inode == 0 {
		t.Fatal("mkdir returned inode zero")
	}
	c.call(t, map[string]any{"op": "rmdir", "parent": root, "name": "empty"}, 0)
	link := c.call(t, map[string]any{"op": "symlink", "parent": root, "name": "link", "target": "README.md"}, 0)
	if target := c.call(t, map[string]any{"op": "readlink", "inode": link.Node.Inode}, 0).Target; target != "README.md" {
		t.Fatalf("symlink target = %q", target)
	}
}

func TestCatalogBridgeSetattrTruncatesAndUpdatesPermissions(t *testing.T) {
	fixture := newBridgeCatalogFixture(t, "alice/project")
	fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	root := c.repository(t, "alice")
	created := c.call(t, map[string]any{"op": "create", "parent": root, "name": "attrs.bin", "mode": 0o644}, 0)
	c.write(t, created.Node.Inode, created.Handle, 0, []byte{0, 0xff, 0xfe, 1})
	c.call(t, map[string]any{"op": "setattr", "inode": root, "handle": created.Handle, "attributes": map[string]any{"size": 0}}, syscall.EBADF)
	const mtimeNS = int64(1_750_000_000_123_456_789)
	updated := c.call(t, map[string]any{"op": "setattr", "inode": created.Node.Inode, "handle": created.Handle, "attributes": map[string]any{"size": 2, "mode": 0o600, "mtime_ns": mtimeNS}}, 0)
	if updated.Node.Attributes.Size != 2 || updated.Node.Attributes.Mode&0o777 != 0o600 || updated.Node.Attributes.MtimeNS != mtimeNS {
		t.Fatalf("setattr attributes: %+v", updated.Node.Attributes)
	}
	if got := c.readFile(t, created.Node.Inode, created.Handle); !bytes.Equal(got, []byte{0, 0xff}) {
		t.Fatalf("truncate result = %x", got)
	}
	c.call(t, map[string]any{"op": "release", "handle": created.Handle}, 0)
}

func TestCatalogBridgeRefreshPreservesAnOpenHandle(t *testing.T) {
	fixture := newBridgeCatalogFixture(t, "alice/project")
	fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	root := c.repository(t, "alice")
	file := c.lookup(t, root, "README.md")
	handle := c.call(t, map[string]any{"op": "open", "inode": file}, 0).Handle
	if err := fs.SetEntries(nil); err != nil {
		t.Fatal(err)
	}
	c.call(t, map[string]any{"op": "lookup", "parent": 1, "name": "alice"}, syscall.ENOENT)
	c.call(t, map[string]any{"op": "getattr", "inode": root}, syscall.ESTALE)
	if got := c.readFile(t, file, handle); !bytes.Equal(got, fixture.content) {
		t.Fatalf("refresh changed an open file: %x", got)
	}
	if err := fs.SetEntries(bridgeCatalogEntries[:1]); err != nil {
		t.Fatal(err)
	}
	if newRoot := c.repository(t, "alice"); newRoot == root {
		t.Fatal("re-added repository reused the stale placeholder")
	}
	c.call(t, map[string]any{"op": "release", "handle": handle}, 0)
}

func TestCatalogBridgeUnlinkedOpenFileSupportsHandleAttributesAndTruncate(t *testing.T) {
	fixture := newBridgeCatalogFixture(t, "alice/project")
	fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	root := c.repository(t, "alice")
	created := c.call(t, map[string]any{"op": "create", "parent": root, "name": "detached.bin", "mode": 0o640}, 0)
	file, handle := created.Node.Inode, created.Handle
	data := []byte{0, 0xff, 0x80, 1, 0xfe, 0, 'x'}
	c.write(t, file, handle, 0, data)
	c.call(t, map[string]any{"op": "unlink", "parent": root, "name": "detached.bin"}, 0)
	c.call(t, map[string]any{"op": "forget", "inode": file, "n": 1}, 0)
	c.call(t, map[string]any{"op": "getattr", "inode": file}, syscall.ESTALE)
	c.call(t, map[string]any{"op": "getattr", "inode": root, "handle": handle}, syscall.EBADF)
	attrs := c.call(t, map[string]any{"op": "getattr", "inode": file, "handle": handle}, 0)
	if attrs.Node.Inode != file || attrs.Node.Attributes.Size != uint64(len(data)) || attrs.Node.Attributes.Type != "file" || attrs.Node.Attributes.Nlink != 0 {
		t.Fatalf("unlinked file handle attributes: %+v", attrs.Node)
	}
	c.call(t, map[string]any{"op": "setattr", "inode": file, "handle": handle, "attributes": map[string]any{"size": 0, "uid": 1}}, syscall.ENOTSUP)
	if got := c.readFile(t, file, handle); !bytes.Equal(got, data) {
		t.Fatal("unsupported detached attributes partially truncated the file")
	}
	const detachedMtime = int64(1_750_000_000_123_456_789)
	truncated := c.call(t, map[string]any{"op": "setattr", "inode": file, "handle": handle, "attributes": map[string]any{"size": 4, "mode": 0o600, "mtime_ns": detachedMtime}}, 0)
	if truncated.Node.Attributes.Size != 4 || truncated.Node.Attributes.Mode&0o777 != 0o600 || truncated.Node.Attributes.MtimeNS != detachedMtime {
		t.Fatalf("unlinked file updated attributes: %+v", truncated.Node.Attributes)
	}
	if got := c.readFile(t, file, handle); !bytes.Equal(got, data[:4]) {
		t.Fatalf("unlinked file truncate bytes: %x", got)
	}
	c.write(t, file, handle, 4, []byte{0xfd, 0})
	c.call(t, map[string]any{"op": "fsync", "inode": file, "handle": handle}, 0)
	c.call(t, map[string]any{"op": "flush", "inode": file, "handle": handle}, 0)
	want := append(bytes.Clone(data[:4]), 0xfd, 0)
	if got := c.readFile(t, file, handle); !bytes.Equal(got, want) {
		t.Fatalf("unlinked file write bytes: %x", got)
	}
	attrs = c.call(t, map[string]any{"op": "getattr", "inode": file, "handle": handle}, 0)
	if attrs.Node.Attributes.Size != uint64(len(want)) {
		t.Fatalf("unlinked file final size = %d", attrs.Node.Attributes.Size)
	}
	c.call(t, map[string]any{"op": "lookup", "parent": root, "name": "detached.bin"}, syscall.ENOENT)
	c.call(t, map[string]any{"op": "release", "handle": handle}, 0)
	c.call(t, map[string]any{"op": "getattr", "inode": file, "handle": handle}, syscall.EBADF)
}

func TestCatalogBridgeRecreatedFileDoesNotAliasUnlinkedOpenFile(t *testing.T) {
	fixture := newBridgeCatalogFixture(t, "alice/project")
	fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	root := c.repository(t, "alice")
	old := c.call(t, map[string]any{"op": "create", "parent": root, "name": "replaced.bin", "mode": 0o640}, 0)
	oldData := []byte{0, 0xff, 0x80, 1, 0xfe, 0, 'x'}
	c.write(t, old.Node.Inode, old.Handle, 0, oldData)
	if id := c.lookup(t, root, "replaced.bin"); id != old.Node.Inode {
		t.Fatal("original lookup disagrees with create identity")
	}
	c.call(t, map[string]any{"op": "unlink", "parent": root, "name": "replaced.bin"}, 0)
	fresh := c.call(t, map[string]any{"op": "create", "parent": root, "name": "replaced.bin", "mode": 0o600}, 0)
	if fresh.Node.Inode == old.Node.Inode || fresh.Handle == old.Handle {
		t.Fatal("recreated file reused the unlinked file's identity or handle")
	}
	freshData := []byte{0xfe, 0, 'n', 0xff, 2}
	c.write(t, fresh.Node.Inode, fresh.Handle, 0, freshData)
	c.call(t, map[string]any{"op": "getattr", "inode": old.Node.Inode}, syscall.ESTALE)
	c.call(t, map[string]any{"op": "setattr", "inode": old.Node.Inode, "attributes": map[string]any{"size": 0}}, syscall.ESTALE)
	oldAttrs := c.call(t, map[string]any{"op": "getattr", "inode": old.Node.Inode, "handle": old.Handle}, 0)
	if oldAttrs.Node.Attributes.Nlink != 0 || oldAttrs.Node.Attributes.Size != uint64(len(oldData)) {
		t.Fatalf("replacement altered old handle attributes: %+v", oldAttrs.Node.Attributes)
	}
	if got := c.readFile(t, old.Node.Inode, old.Handle); !bytes.Equal(got, oldData) {
		t.Fatalf("replacement changed old handle bytes: %x", got)
	}
	c.call(t, map[string]any{"op": "setattr", "inode": old.Node.Inode, "handle": old.Handle, "attributes": map[string]any{"size": 2}}, 0)
	if got := c.readFile(t, old.Node.Inode, old.Handle); !bytes.Equal(got, oldData[:2]) {
		t.Fatalf("old handle truncate bytes: %x", got)
	}
	assertFresh := func() {
		t.Helper()
		attrs := c.call(t, map[string]any{"op": "getattr", "inode": fresh.Node.Inode}, 0)
		if attrs.Node.Attributes.Size != uint64(len(freshData)) || attrs.Node.Attributes.Mode&0o777 != 0o600 {
			t.Fatalf("old file operation altered replacement attributes: %+v", attrs.Node.Attributes)
		}
		if got := c.readFile(t, fresh.Node.Inode, fresh.Handle); !bytes.Equal(got, freshData) {
			t.Fatalf("old file operation altered replacement bytes: %x", got)
		}
		if id := c.lookup(t, root, "replaced.bin"); id != fresh.Node.Inode {
			t.Fatalf("replacement lookup identity = %d, want %d", id, fresh.Node.Inode)
		}
	}
	assertFresh()
	c.call(t, map[string]any{"op": "forget", "inode": old.Node.Inode, "n": 2}, 0)
	assertFresh()
	oldAttrs = c.call(t, map[string]any{"op": "getattr", "inode": old.Node.Inode, "handle": old.Handle}, 0)
	if oldAttrs.Node.Attributes.Size != 2 || oldAttrs.Node.Attributes.Nlink != 0 {
		t.Fatal("forget lost the detached file handle's attributes")
	}
	c.call(t, map[string]any{"op": "release", "handle": old.Handle}, 0)
	assertFresh()
	c.call(t, map[string]any{"op": "release", "handle": fresh.Handle}, 0)
}

func TestCatalogBridgeRecreatedDirectoriesAndSymlinksUseFreshInodes(t *testing.T) {
	for _, kind := range []string{"dir", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newBridgeCatalogFixture(t, "alice/project")
			fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
				return fixture.backend, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			c := newBridgeCatalogClient(t, fs)
			root := c.repository(t, "alice")
			create := map[string]any{"parent": root, "name": "replaced"}
			remove := map[string]any{"parent": root, "name": "replaced"}
			if kind == "dir" {
				create["op"], create["mode"] = "mkdir", 0o755
				remove["op"] = "rmdir"
			} else {
				create["op"], create["target"] = "symlink", "old-target"
				remove["op"] = "unlink"
			}
			old := c.call(t, create, 0).Node.Inode
			c.call(t, remove, 0)
			if kind == "symlink" {
				create["target"] = "new-target"
			}
			fresh := c.call(t, create, 0).Node.Inode
			if fresh == old {
				t.Fatal("recreated entry reused the removed inode")
			}
			c.call(t, map[string]any{"op": "getattr", "inode": old}, syscall.ESTALE)
			c.call(t, map[string]any{"op": "setattr", "inode": old, "attributes": map[string]any{"mode": 0o700}}, syscall.ESTALE)
			c.call(t, map[string]any{"op": "forget", "inode": old, "n": 1}, 0)
			if id := c.lookup(t, root, "replaced"); id != fresh {
				t.Fatal("forgetting removed inode invalidated replacement mapping")
			}
			attrs := c.call(t, map[string]any{"op": "getattr", "inode": fresh}, 0).Node.Attributes
			if attrs.Type != kind {
				t.Fatalf("replacement type = %q, want %q", attrs.Type, kind)
			}
			if kind == "dir" && attrs.Mode&0o777 != 0o755 {
				t.Fatal("stale directory changed replacement permissions")
			}
			if kind == "symlink" {
				if target := c.call(t, map[string]any{"op": "readlink", "inode": fresh}, 0).Target; target != "new-target" {
					t.Fatalf("replacement symlink target = %q", target)
				}
			}
		})
	}
}

func TestCatalogBridgeDetachedHandlesShareBytesAndPreciseMtimeAcrossReplacementMutations(t *testing.T) {
	fixture := newBridgeCatalogFixture(t, "alice/project")
	fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	root := c.repository(t, "alice")
	old := c.call(t, map[string]any{"op": "create", "parent": root, "name": "shared.bin", "mode": 0o640}, 0)
	data := []byte{0, 0xff, 0x80, 1, 0xfe, 0, 'x'}
	c.write(t, old.Node.Inode, old.Handle, 0, data)
	c.call(t, map[string]any{"op": "forget", "inode": old.Node.Inode, "n": 1}, 0)
	secondInode := c.lookup(t, root, "shared.bin")
	if secondInode == old.Node.Inode {
		t.Fatal("lookup reused a forgotten inode identity")
	}
	secondHandle := c.call(t, map[string]any{"op": "open", "inode": secondInode}, 0).Handle
	if got := c.readFile(t, secondInode, secondHandle); !bytes.Equal(got, data) {
		t.Fatal("second handle did not open the original bytes")
	}
	c.call(t, map[string]any{"op": "unlink", "parent": root, "name": "shared.bin"}, 0)
	const mtime = int64(1_750_000_000_123_456_789)
	c.call(t, map[string]any{"op": "setattr", "inode": old.Node.Inode, "handle": old.Handle, "attributes": map[string]any{"mtime_ns": mtime}}, 0)
	for _, pair := range [][2]uint64{{old.Node.Inode, old.Handle}, {secondInode, secondHandle}} {
		attrs := c.call(t, map[string]any{"op": "getattr", "inode": pair[0], "handle": pair[1]}, 0).Node.Attributes
		if attrs.MtimeNS != mtime || attrs.Nlink != 0 || attrs.Size != uint64(len(data)) {
			t.Fatalf("unlinked handles do not share precise metadata: %+v", attrs)
		}
	}
	c.write(t, secondInode, secondHandle, 2, []byte{0xe1, 0})
	copy(data[2:], []byte{0xe1, 0})
	firstAttrs := c.call(t, map[string]any{"op": "getattr", "inode": old.Node.Inode, "handle": old.Handle}, 0).Node.Attributes
	secondAttrs := c.call(t, map[string]any{"op": "getattr", "inode": secondInode, "handle": secondHandle}, 0).Node.Attributes
	if firstAttrs.MtimeNS == mtime || firstAttrs.MtimeNS != secondAttrs.MtimeNS {
		t.Fatalf("write did not update shared mtime: first=%d second=%d", firstAttrs.MtimeNS, secondAttrs.MtimeNS)
	}
	const retainedMtime = int64(1_750_000_111_987_654_321)
	c.call(t, map[string]any{"op": "setattr", "inode": secondInode, "handle": secondHandle, "attributes": map[string]any{"mtime_ns": retainedMtime}}, 0)
	assertOld := func() {
		t.Helper()
		for _, pair := range [][2]uint64{{old.Node.Inode, old.Handle}, {secondInode, secondHandle}} {
			if got := c.readFile(t, pair[0], pair[1]); !bytes.Equal(got, data) {
				t.Fatalf("replacement mutation repinned the old handle: %x", got)
			}
			attrs := c.call(t, map[string]any{"op": "getattr", "inode": pair[0], "handle": pair[1]}, 0).Node.Attributes
			if attrs.MtimeNS != retainedMtime || attrs.Nlink != 0 || attrs.Size != uint64(len(data)) {
				t.Fatalf("replacement mutation changed old shared metadata: %+v", attrs)
			}
		}
	}
	replacement := c.call(t, map[string]any{"op": "create", "parent": root, "name": "shared.bin", "mode": 0o600}, 0)
	c.write(t, replacement.Node.Inode, replacement.Handle, 0, []byte{0xfd, 0, 'a'})
	c.call(t, map[string]any{"op": "unlink", "parent": root, "name": "shared.bin"}, 0)
	assertOld()
	c.call(t, map[string]any{"op": "release", "handle": replacement.Handle}, 0)
	replacement = c.call(t, map[string]any{"op": "create", "parent": root, "name": "shared.bin", "mode": 0o600}, 0)
	c.write(t, replacement.Node.Inode, replacement.Handle, 0, []byte{0xfc, 0, 'b'})
	c.call(t, map[string]any{"op": "rename", "old_parent": root, "new_parent": root, "old_name": "shared.bin", "new_name": "moved.bin"}, 0)
	assertOld()
	c.call(t, map[string]any{"op": "unlink", "parent": root, "name": "moved.bin"}, 0)
	assertOld()
	c.call(t, map[string]any{"op": "release", "handle": replacement.Handle}, 0)
	replacement = c.call(t, map[string]any{"op": "create", "parent": root, "name": "shared.bin", "mode": 0o600}, 0)
	c.write(t, replacement.Node.Inode, replacement.Handle, 0, []byte{0xfb, 0, 'c'})
	source := c.call(t, map[string]any{"op": "create", "parent": root, "name": "source.bin", "mode": 0o600}, 0)
	c.write(t, source.Node.Inode, source.Handle, 0, []byte{0xfa, 0, 'd'})
	c.call(t, map[string]any{"op": "rename", "old_parent": root, "new_parent": root, "old_name": "source.bin", "new_name": "shared.bin"}, 0)
	assertOld()
	c.call(t, map[string]any{"op": "unlink", "parent": root, "name": "shared.bin"}, 0)
	assertOld()
	c.call(t, map[string]any{"op": "release", "handle": replacement.Handle}, 0)
	c.call(t, map[string]any{"op": "release", "handle": source.Handle}, 0)
	c.call(t, map[string]any{"op": "forget", "inode": secondInode, "n": 1}, 0)
	assertOld()
	c.call(t, map[string]any{"op": "release", "handle": secondHandle}, 0)
	if got := c.readFile(t, old.Node.Inode, old.Handle); !bytes.Equal(got, data) {
		t.Fatal("closing one shared handle lost the other's bytes")
	}
	c.call(t, map[string]any{"op": "release", "handle": old.Handle}, 0)
}

func TestCatalogBridgeCreationRetainsWritableDescriptorForRestrictivePermissions(t *testing.T) {
	for _, mode := range []uint32{0o400, 0o000} {
		t.Run(fmt.Sprintf("%03o", mode), func(t *testing.T) {
			fixture := newBridgeCatalogFixture(t, "alice/project")
			fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
				return fixture.backend, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			c := newBridgeCatalogClient(t, fs)
			root := c.repository(t, "alice")
			created := c.call(t, map[string]any{"op": "create", "parent": root, "name": "restricted.bin", "mode": mode}, 0)
			data := []byte{0, 0xff, 0x80, 'r'}
			c.write(t, created.Node.Inode, created.Handle, 0, data)
			c.write(t, created.Node.Inode, created.Handle, 2, []byte{0xfe, 0})
			copy(data[2:], []byte{0xfe, 0})
			if got := c.readFile(t, created.Node.Inode, created.Handle); !bytes.Equal(got, data) {
				t.Fatalf("create with permissions %03o lost authorized descriptor bytes: %x", mode, got)
			}
			attrs := c.call(t, map[string]any{"op": "getattr", "inode": created.Node.Inode, "handle": created.Handle}, 0).Node.Attributes
			if attrs.Mode&0o777 != mode || attrs.Size != uint64(len(data)) {
				t.Fatalf("descriptor write changed restrictive permissions: %+v", attrs)
			}
			c.call(t, map[string]any{"op": "fsync", "inode": created.Node.Inode, "handle": created.Handle}, 0)
			c.call(t, map[string]any{"op": "release", "handle": created.Handle}, 0)
		})
	}
}

func TestCatalogBridgeBaseReaderSeesSeparateWriterAndRemainsReadOnly(t *testing.T) {
	fixture := newBridgeCatalogFixture(t, "alice/project")
	fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	root := c.repository(t, "alice")
	file := c.lookup(t, root, "README.md")
	reader := c.call(t, map[string]any{"op": "open", "inode": file, "access": 1}, 0).Handle
	if got := c.readFile(t, file, reader); !bytes.Equal(got, fixture.content) {
		t.Fatal("initial reader did not open the committed binary blob")
	}
	writer := c.call(t, map[string]any{"op": "open", "inode": file, "access": 2}, 0).Handle
	changed := bytes.Clone(fixture.content)
	changed[0], changed[len(changed)-1] = 0xfd, 0xfc
	c.write(t, file, writer, 0, changed)
	if got := c.readFile(t, file, reader); !bytes.Equal(got, changed) {
		t.Fatalf("existing base reader returned stale bytes after separate writer: %x", got)
	}
	c.fileFailure(t, http.MethodPut, fmt.Sprintf("/v1/fs/write?inode=%d&handle=%d&offset=0", file, reader), []byte{0xfe}, syscall.EBADF)
	c.fileFailure(t, http.MethodGet, fmt.Sprintf("/v1/fs/read?inode=%d&handle=%d&offset=0&size=1024", file, writer), nil, syscall.EBADF)
	if got := c.readFile(t, file, reader); !bytes.Equal(got, changed) {
		t.Fatal("rejected read-only handle write changed the shared file")
	}
	c.call(t, map[string]any{"op": "release", "handle": writer}, 0)
	c.call(t, map[string]any{"op": "release", "handle": reader}, 0)
}

func TestCatalogBridgeBaseReaderRebindsBeforeChmodWriteOnly(t *testing.T) {
	fixture := newBridgeCatalogFixture(t, "alice/project")
	fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	root := c.repository(t, "alice")
	file := c.lookup(t, root, "README.md")
	reader := c.call(t, map[string]any{"op": "open", "inode": file, "access": 1}, 0).Handle
	if got := c.readFile(t, file, reader); !bytes.Equal(got, fixture.content) {
		t.Fatal("initial reader did not open the committed binary blob")
	}
	c.call(t, map[string]any{"op": "setattr", "inode": file, "attributes": map[string]any{"mode": 0o200}}, 0)
	writer := c.call(t, map[string]any{"op": "open", "inode": file, "access": 2}, 0).Handle
	changed := bytes.Clone(fixture.content)
	changed[0], changed[len(changed)-1] = 0xfb, 0xfa
	c.write(t, file, writer, 0, changed)
	if got := c.readFile(t, file, reader); !bytes.Equal(got, changed) {
		t.Fatalf("reader lost current bytes after chmod0200 and separate writer: %x", got)
	}
	attrs := c.call(t, map[string]any{"op": "getattr", "inode": file, "handle": reader}, 0).Node.Attributes
	if attrs.Mode&0o777 != 0o200 || attrs.Size != uint64(len(changed)) {
		t.Fatalf("rebinding changed restrictive attributes: %+v", attrs)
	}
	c.fileFailure(t, http.MethodPut, fmt.Sprintf("/v1/fs/write?inode=%d&handle=%d&offset=0", file, reader), []byte{0xfe}, syscall.EBADF)
	c.call(t, map[string]any{"op": "fsync", "inode": file, "handle": writer}, 0)
	c.call(t, map[string]any{"op": "release", "handle": writer}, 0)
	c.call(t, map[string]any{"op": "release", "handle": reader}, 0)
}

func TestCatalogBridgeWritableHandleSurvivesChmodNoPermissions(t *testing.T) {
	for _, byHandle := range []bool{false, true} {
		t.Run(fmt.Sprintf("handle=%t", byHandle), func(t *testing.T) {
			fixture := newBridgeCatalogFixture(t, "alice/project")
			fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
				return fixture.backend, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			c := newBridgeCatalogClient(t, fs)
			root := c.repository(t, "alice")
			file := c.lookup(t, root, "README.md")
			handle := c.call(t, map[string]any{"op": "open", "inode": file, "access": 3}, 0).Handle
			if got := c.readFile(t, file, handle); !bytes.Equal(got, fixture.content) {
				t.Fatal("initial writable handle did not open the committed bytes")
			}
			setattr := map[string]any{"op": "setattr", "inode": file, "attributes": map[string]any{"mode": 0o000}}
			if byHandle {
				setattr["handle"] = handle
			}
			c.call(t, setattr, 0)
			changed := bytes.Clone(fixture.content)
			changed[0], changed[len(changed)-1] = 0xf9, 0xf8
			c.write(t, file, handle, 0, changed)
			if got := c.readFile(t, file, handle); !bytes.Equal(got, changed) {
				t.Fatalf("retained writable handle lost bytes after chmod0000: %x", got)
			}
			c.call(t, map[string]any{"op": "fsync", "inode": file, "handle": handle}, 0)
			attrs := c.call(t, map[string]any{"op": "getattr", "inode": file, "handle": handle}, 0).Node.Attributes
			if attrs.Mode&0o777 != 0 || attrs.Size != uint64(len(changed)) {
				t.Fatalf("descriptor write changed mode0000 attributes: %+v", attrs)
			}
			c.call(t, map[string]any{"op": "release", "handle": handle}, 0)
		})
	}
}

func TestCatalogBridgeFileAccessFlagsLimitHandleOperations(t *testing.T) {
	fixture := newBridgeCatalogFixture(t, "alice/project")
	fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	root := c.repository(t, "alice")
	readCreated := c.call(t, map[string]any{"op": "create", "parent": root, "name": "read-created.bin", "mode": 0o400, "access": 1}, 0)
	if got := c.readFile(t, readCreated.Node.Inode, readCreated.Handle); len(got) != 0 {
		t.Fatal("read-only create returned a nonempty file")
	}
	c.fileFailure(t, http.MethodPut, fmt.Sprintf("/v1/fs/write?inode=%d&handle=%d&offset=0", readCreated.Node.Inode, readCreated.Handle), []byte{0xfd}, syscall.EBADF)
	c.call(t, map[string]any{"op": "release", "handle": readCreated.Handle}, 0)
	writeCreated := c.call(t, map[string]any{"op": "create", "parent": root, "name": "write-created.bin", "mode": 0o000, "access": 2}, 0)
	c.write(t, writeCreated.Node.Inode, writeCreated.Handle, 0, []byte{0, 0xfe})
	c.fileFailure(t, http.MethodGet, fmt.Sprintf("/v1/fs/read?inode=%d&handle=%d&offset=0&size=1024", writeCreated.Node.Inode, writeCreated.Handle), nil, syscall.EBADF)
	c.call(t, map[string]any{"op": "release", "handle": writeCreated.Handle}, 0)
	c.call(t, map[string]any{"op": "create", "parent": root, "name": "invalid-access.bin", "mode": 0o600, "access": 4}, syscall.EINVAL)
	c.call(t, map[string]any{"op": "lookup", "parent": root, "name": "invalid-access.bin"}, syscall.ENOENT)
	created := c.call(t, map[string]any{"op": "create", "parent": root, "name": "access.bin", "mode": 0o600, "access": 3}, 0)
	data := []byte{0, 0xff, 0x80, 'a'}
	c.write(t, created.Node.Inode, created.Handle, 0, data)
	c.call(t, map[string]any{"op": "release", "handle": created.Handle}, 0)
	readOnly := c.call(t, map[string]any{"op": "open", "inode": created.Node.Inode, "access": 1}, 0).Handle
	if got := c.readFile(t, created.Node.Inode, readOnly); !bytes.Equal(got, data) {
		t.Fatal("read-only handle did not return file bytes")
	}
	c.fileFailure(t, http.MethodPut, fmt.Sprintf("/v1/fs/write?inode=%d&handle=%d&offset=0", created.Node.Inode, readOnly), []byte{0xfd}, syscall.EBADF)
	c.call(t, map[string]any{"op": "release", "handle": readOnly}, 0)
	writeOnly := c.call(t, map[string]any{"op": "open", "inode": created.Node.Inode, "access": 2}, 0).Handle
	c.fileFailure(t, http.MethodGet, fmt.Sprintf("/v1/fs/read?inode=%d&handle=%d&offset=0&size=1024", created.Node.Inode, writeOnly), nil, syscall.EBADF)
	c.write(t, created.Node.Inode, writeOnly, 1, []byte{0xfc})
	data[1] = 0xfc
	c.call(t, map[string]any{"op": "release", "handle": writeOnly}, 0)
	for _, access := range []int{0, 3} {
		handle := c.call(t, map[string]any{"op": "open", "inode": created.Node.Inode, "access": access}, 0).Handle
		if got := c.readFile(t, created.Node.Inode, handle); !bytes.Equal(got, data) {
			t.Fatalf("access %d handle did not return file bytes", access)
		}
		c.write(t, created.Node.Inode, handle, 2, []byte{0xfb})
		data[2] = 0xfb
		c.call(t, map[string]any{"op": "release", "handle": handle}, 0)
	}
	c.call(t, map[string]any{"op": "open", "inode": created.Node.Inode, "access": 4}, syscall.EINVAL)
}

func TestCatalogBridgeDuplicateCreationReturnsEEXISTWithoutChangingExistingNodes(t *testing.T) {
	fixture := newBridgeCatalogFixture(t, "alice/project")
	fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	root := c.repository(t, "alice")
	file := c.call(t, map[string]any{"op": "create", "parent": root, "name": "existing-file", "mode": 0o640}, 0)
	data := []byte{0, 0xff, 0x80, 'e'}
	c.write(t, file.Node.Inode, file.Handle, 0, data)
	dir := c.call(t, map[string]any{"op": "mkdir", "parent": root, "name": "existing-dir", "mode": 0o750}, 0)
	link := c.call(t, map[string]any{"op": "symlink", "parent": root, "name": "existing-link", "target": "original-target"}, 0)
	gitfile := c.lookup(t, root, ".git")
	gitHandle := c.call(t, map[string]any{"op": "open", "inode": gitfile, "access": 1}, 0).Handle
	gitData := c.readFile(t, gitfile, gitHandle)
	for _, existing := range []struct {
		name  string
		inode uint64
	}{
		{"existing-file", file.Node.Inode},
		{"existing-dir", dir.Node.Inode},
		{"existing-link", link.Node.Inode},
		{".git", gitfile},
	} {
		t.Run(existing.name, func(t *testing.T) {
			before := c.call(t, map[string]any{"op": "getattr", "inode": existing.inode}, 0).Node.Attributes
			for _, attempt := range []map[string]any{
				{"op": "create", "parent": root, "name": existing.name, "mode": 0o000},
				{"op": "mkdir", "parent": root, "name": existing.name, "mode": 0o700},
				{"op": "symlink", "parent": root, "name": existing.name, "target": "replacement-target"},
			} {
				c.call(t, attempt, syscall.EEXIST)
				after := c.call(t, map[string]any{"op": "getattr", "inode": existing.inode}, 0).Node.Attributes
				if existing.name == ".git" {
					// The synthetic gitfile may use the current time when the
					// fixture has no repository timestamp; its content is stable.
					after.MtimeNS = before.MtimeNS
				}
				if after != before || c.lookup(t, root, existing.name) != existing.inode {
					t.Fatalf("failed %s changed the existing node", attempt["op"])
				}
				if got := c.readFile(t, file.Node.Inode, file.Handle); !bytes.Equal(got, data) {
					t.Fatalf("failed %s changed existing file bytes: %x", attempt["op"], got)
				}
				if target := c.call(t, map[string]any{"op": "readlink", "inode": link.Node.Inode}, 0).Target; target != "original-target" {
					t.Fatal("failed creation changed the existing symlink target")
				}
				if got := c.readFile(t, gitfile, gitHandle); !bytes.Equal(got, gitData) {
					t.Fatal("failed creation changed the synthetic .git bytes")
				}
			}
		})
	}
	c.call(t, map[string]any{"op": "release", "handle": file.Handle}, 0)
	c.call(t, map[string]any{"op": "release", "handle": gitHandle}, 0)
}

func TestCatalogBridgeConcurrentReadsDirectoryEnumerationAndMutations(t *testing.T) {
	fixture := newBridgeCatalogFixture(t, "alice/project")
	fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newBridgeCatalogClient(t, fs)
	root := c.repository(t, "alice")
	file := c.lookup(t, root, "README.md")
	handle := c.call(t, map[string]any{"op": "open", "inode": file}, 0).Handle
	dir := c.opendir(t, root)
	start := make(chan struct{})
	errs := make(chan error, 3)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		for range 32 {
			data, err := c.read(file, handle, 0, 1024)
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(data, fixture.content) {
				errs <- fmt.Errorf("concurrent read changed binary blob: %x", data)
				return
			}
		}
	})
	wg.Go(func() {
		<-start
		for range 32 {
			listing, err := c.exchange(map[string]any{"op": "readdir", "inode": root, "handle": dir, "size": 4096})
			if err != nil || listing.Errno != 0 {
				errs <- fmt.Errorf("concurrent readdir: errno=%d, error=%v", listing.Errno, err)
				return
			}
			seen := make(map[string]bool)
			for _, entry := range listing.Entries {
				if entry.Node.Inode == 0 || seen[entry.Name] {
					errs <- fmt.Errorf("concurrent readdir identity: %+v", entry)
					return
				}
				seen[entry.Name] = true
			}
			if !seen["README.md"] {
				errs <- errors.New("concurrent mutation removed the untouched base file")
				return
			}
		}
	})
	wg.Go(func() {
		<-start
		for i := range 16 {
			name := fmt.Sprintf("local-%d", i)
			created, err := c.exchange(map[string]any{"op": "create", "parent": root, "name": name, "mode": 0o644})
			if err != nil || created.Errno != 0 {
				errs <- fmt.Errorf("concurrent create: errno=%d, error=%v", created.Errno, err)
				return
			}
			for _, fields := range []map[string]any{
				{"op": "release", "handle": created.Handle},
				{"op": "rename", "old_parent": root, "new_parent": root, "old_name": name, "new_name": name + ".renamed"},
				{"op": "unlink", "parent": root, "name": name + ".renamed"},
			} {
				result, err := c.exchange(fields)
				if err != nil || result.Errno != 0 {
					errs <- fmt.Errorf("concurrent %s: errno=%d, error=%v", fields["op"], result.Errno, err)
					return
				}
			}
		}
	})
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	c.call(t, map[string]any{"op": "releasedir", "handle": dir}, 0)
	c.call(t, map[string]any{"op": "release", "handle": handle}, 0)
}
