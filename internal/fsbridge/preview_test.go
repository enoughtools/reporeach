//go:build !windows

package fsbridge

import (
	"bytes"
	"context"
	"net/http"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestNativePreviewLookupGetattrAndReaddirRemainMetadataOnly(t *testing.T) {
	fixture := newBridgeCatalogFixtureWithSizeState(t, "alice/project", "unknown")
	var activations atomic.Int64
	fs, err := catalogfs.NewWithPreview(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return fixture.backend, nil
	}, nil, func(context.Context, catalogfs.Entry, string) (catalogfs.PreviewDirectory, error) {
		return catalogfs.PreviewDirectory{Revision: "commit", Entries: []model.BaseNode{{Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "blob", SizeState: "unknown"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, handler := directoryTestClient(t, fs)
	root := c.repository(t, "alice")
	c.call(t, map[string]any{"op": "lookup", "parent": root, "name": ".DS_Store"}, syscall.ENOENT)
	file := c.call(t, map[string]any{"op": "lookup", "parent": root, "name": "README.md"}, 0).Node
	if file.Attributes.SizeKnown == nil || *file.Attributes.SizeKnown || file.Attributes.Size != 0 {
		t.Fatalf("lookup fabricated size: %+v", file)
	}
	stat := c.call(t, map[string]any{"op": "getattr", "inode": file.Inode}, 0).Node
	if stat.Attributes.SizeKnown == nil || *stat.Attributes.SizeKnown || stat.Attributes.Size != 0 {
		t.Fatalf("metadata getattr fabricated size: %+v", stat)
	}
	directory := c.opendir(t, root)
	listing := c.call(t, map[string]any{"op": "readdir", "inode": root, "handle": directory}, 0)
	if len(listing.Entries) != 1 || listing.Entries[0].Node.Inode != file.Inode {
		t.Fatal("preview enumeration changed lookup identity")
	}
	if activations.Load() != 0 || fixture.hydrator.calls.Load() != 0 {
		t.Fatalf("metadata activated=%d hydrated=%d", activations.Load(), fixture.hydrator.calls.Load())
	}
	opened := c.call(t, map[string]any{"op": "open", "inode": file.Inode, "access": 1}, 0)
	if got := c.readFile(t, file.Inode, opened.Handle); !bytes.Equal(got, fixture.content) {
		t.Fatal("promotion changed binary content")
	}
	if activations.Load() != 1 {
		t.Fatal("file content did not prepare writable checkout")
	}
	actual := c.call(t, map[string]any{"op": "lookup", "parent": root, "name": "README.md"}, 0).Node
	if actual.Inode != file.Inode || actual.Attributes.SizeKnown == nil || *actual.Attributes.SizeKnown {
		t.Fatalf("promotion changed native item or fabricated unknown size: %+v", actual)
	}
	exact := c.call(t, map[string]any{"op": "getattr", "inode": actual.Inode, "require_size": true}, 0).Node
	if exact.Attributes.SizeKnown != nil || exact.Attributes.Size != uint64(len(fixture.content)) {
		t.Fatalf("exact runtime stat omitted real size: %+v", exact)
	}
	c.call(t, map[string]any{"op": "release", "inode": file.Inode, "handle": opened.Handle}, 0)
	c.call(t, map[string]any{"op": "batchforget", "forgets": []Forget{{Inode: file.Inode, N: 3}}}, 0)
	c.call(t, map[string]any{"op": "releasedir", "inode": root, "handle": directory}, 0)
	c.call(t, map[string]any{"op": "getattr", "inode": file.Inode}, syscall.ESTALE)
	if handler.lookups[file.Inode] != 0 {
		t.Fatal("native preview lookup ledger retained references")
	}
}

func TestNativePreparedMetadataAndPreviewPathBindingStayLazy(t *testing.T) {
	fixture := newBridgeCatalogFixtureWithSizeState(t, "alice/project", "unknown")
	gitContent := []byte("gitdir: " + fixture.config.GitDir + "\n")
	var activations atomic.Int64
	fs, err := catalogfs.NewWithPreview(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return fixture.backend, nil
	}, nil, func(context.Context, catalogfs.Entry, string) (catalogfs.PreviewDirectory, error) {
		return catalogfs.PreviewDirectory{Revision: "commit", GitFileSize: uint64(len(gitContent)), Entries: []model.BaseNode{
			{Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "blob", SizeState: "unknown"},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, handler := directoryTestClient(t, fs)
	root := c.repository(t, "alice")
	file := c.lookup(t, root, "README.md")
	git := c.lookup(t, root, ".git")
	opened := c.call(t, map[string]any{"op": "open", "inode": git, "access": 1}, 0)
	if !bytes.Equal(c.readFile(t, git, opened.Handle), gitContent) || activations.Load() != 1 {
		t.Fatal("Git pointer did not activate exact runtime")
	}
	// The retained preview ID has no backend binding yet. Xattr mutation must
	// bind its real inode without resolving the unrelated base blob's size.
	value := []byte{0, 0xff, 0xfe, 10}
	set, status := xattrRequest(handler, handler.authorization, http.MethodPut, xattrTarget(file, "user.local", "always_set"), value)
	if set.Code != http.StatusOK || status.Errno != 0 {
		t.Fatalf("metadata binding mutation: HTTP %d errno %d", set.Code, status.Errno)
	}
	get, _ := xattrRequest(handler, handler.authorization, http.MethodGet, xattrTarget(file, "user.local", ""), nil)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), value) {
		t.Fatal("metadata binding lost binary xattr")
	}
	stat := c.call(t, map[string]any{"op": "getattr", "inode": file}, 0).Node
	lookup := c.call(t, map[string]any{"op": "lookup", "parent": root, "name": "README.md"}, 0).Node
	if stat.Inode != file || lookup.Inode != file || stat.Attributes.SizeKnown == nil || *stat.Attributes.SizeKnown || lookup.Attributes.SizeKnown == nil || *lookup.Attributes.SizeKnown {
		t.Fatal("prepared metadata changed preview identity or fabricated size")
	}
	if fixture.hydrator.calls.Load() != 0 {
		t.Fatalf("prepared metadata fetched %d blobs", fixture.hydrator.calls.Load())
	}
	exact := c.call(t, map[string]any{"op": "getattr", "inode": file, "require_size": true}, 0).Node
	if exact.Attributes.SizeKnown != nil || exact.Attributes.Size != uint64(len(fixture.content)) || fixture.hydrator.calls.Load() != 1 {
		t.Fatal("explicit size did not resolve exact base bytes once")
	}
	c.call(t, map[string]any{"op": "release", "inode": git, "handle": opened.Handle}, 0)
	c.call(t, map[string]any{"op": "batchforget", "forgets": []Forget{{Inode: file, N: 2}, {Inode: git, N: 1}}}, 0)
	c.call(t, map[string]any{"op": "getattr", "inode": file}, syscall.ESTALE)
	if handler.lookups[file] != 0 || handler.lookups[git] != 0 {
		t.Fatal("prepared metadata retained native lookup references")
	}
}

func TestNativePreviewExplicitSizeRequestResolvesExactBytes(t *testing.T) {
	fixture := newBridgeCatalogFixtureWithSizeState(t, "alice/project", "unknown")
	var activations atomic.Int64
	fs, err := catalogfs.NewWithPreview(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return fixture.backend, nil
	}, nil, func(context.Context, catalogfs.Entry, string) (catalogfs.PreviewDirectory, error) {
		return catalogfs.PreviewDirectory{Revision: "commit", Entries: []model.BaseNode{{Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "blob", SizeState: "unknown"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := directoryTestClient(t, fs)
	root := c.repository(t, "alice")
	id := c.lookup(t, root, "README.md")
	metadata := c.call(t, map[string]any{"op": "getattr", "inode": id, "require_size": false}, 0)
	if metadata.Node.Attributes.SizeKnown == nil || *metadata.Node.Attributes.SizeKnown || activations.Load() != 0 {
		t.Fatal("type-only stat acquired content")
	}
	exact := c.call(t, map[string]any{"op": "getattr", "inode": id, "require_size": true}, 0)
	if exact.Node.Attributes.SizeKnown != nil || exact.Node.Attributes.Size != uint64(len(fixture.content)) || exact.Node.Inode != id {
		t.Fatalf("POSIX size request was not exact: %+v", exact.Node)
	}
	if activations.Load() != 1 || fixture.hydrator.calls.Load() == 0 {
		t.Fatal("exact unknown size did not acquire required blob")
	}
	c.call(t, map[string]any{"op": "forget", "inode": id, "n": 1}, 0)
	c.call(t, map[string]any{"op": "getattr", "inode": id, "require_size": true}, syscall.ESTALE)
}

func TestNativeGitPointerDiscoveryStaysColdAndListingSurvivesPromotion(t *testing.T) {
	fixture := newBridgeCatalogFixture(t, "alice/project")
	gitContent := []byte("gitdir: " + fixture.config.GitDir + "\n")
	var activations atomic.Int64
	fs, err := catalogfs.NewWithPreview(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return fixture.backend, nil
	}, nil, func(context.Context, catalogfs.Entry, string) (catalogfs.PreviewDirectory, error) {
		return catalogfs.PreviewDirectory{Revision: "commit", GitFileSize: uint64(len(gitContent)), Entries: []model.BaseNode{
			{Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "blob", SizeState: "known", SizeBytes: int64(len(fixture.content))},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := directoryTestClient(t, fs)
	root := c.repository(t, "alice")
	git := c.call(t, map[string]any{"op": "lookup", "parent": root, "name": ".git"}, 0).Node
	if git.Attributes.Type != "file" || git.Attributes.SizeKnown != nil || git.Attributes.Size != uint64(len(gitContent)) {
		t.Fatalf("synthetic Git discovery metadata invalid: %+v", git)
	}
	exact := c.call(t, map[string]any{"op": "getattr", "inode": git.Inode, "require_size": true}, 0)
	if exact.Node.Attributes.Size != uint64(len(gitContent)) || exact.Node.Attributes.SizeKnown != nil {
		t.Fatal("cold .git exact stat was not authoritative")
	}
	previewDir := c.opendir(t, root)
	before := c.call(t, map[string]any{"op": "readdir", "inode": root, "handle": previewDir}, 0)
	if len(before.Entries) != 2 || before.Entries[0].Name != ".git" || before.Entries[0].Node.Inode != git.Inode || activations.Load() != 0 || fixture.hydrator.calls.Load() != 0 {
		t.Fatal("Git metadata browsing activated checkout or omitted pointer")
	}
	opened := c.call(t, map[string]any{"op": "open", "inode": git.Inode, "access": 1}, 0)
	if got := c.readFile(t, git.Inode, opened.Handle); !bytes.Equal(got, gitContent) || activations.Load() != 1 || fixture.hydrator.calls.Load() != 0 {
		t.Fatal("Git pointer read did not wait for exact runtime content")
	}
	realDir := c.opendir(t, root)
	after := c.call(t, map[string]any{"op": "readdir", "inode": root, "handle": realDir}, 0)
	if len(after.Entries) != len(before.Entries) {
		t.Fatal("promotion changed complete repository names")
	}
	var forgets []Forget
	for i, entry := range after.Entries {
		if entry.Name != before.Entries[i].Name || entry.Node.Inode != before.Entries[i].Node.Inode {
			t.Fatal("promotion changed committed or synthetic native identities")
		}
		count := uint64(2)
		if entry.Name == ".git" {
			count++
		}
		forgets = append(forgets, Forget{Inode: entry.Node.Inode, N: count})
	}
	c.call(t, map[string]any{"op": "release", "inode": git.Inode, "handle": opened.Handle}, 0)
	c.call(t, map[string]any{"op": "batchforget", "forgets": forgets}, 0)
	c.call(t, map[string]any{"op": "releasedir", "inode": root, "handle": previewDir}, 0)
	c.call(t, map[string]any{"op": "releasedir", "inode": root, "handle": realDir}, 0)
	c.call(t, map[string]any{"op": "getattr", "inode": git.Inode}, syscall.ESTALE)
}
