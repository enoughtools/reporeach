//go:build !windows

package fsbridge

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestNativePreviewHandleGetattrHonorsRequireSize(t *testing.T) {
	fixture := newBridgeCatalogFixtureWithSizeState(t, "alice/project", "unknown")
	var activations, contentCalls atomic.Int64
	fs, err := catalogfs.NewWithPreviewContent(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return fixture.backend, nil
	}, nil, func(context.Context, catalogfs.Entry, string) (catalogfs.PreviewDirectory, error) {
		return catalogfs.PreviewDirectory{Revision: "commit", Entries: []model.BaseNode{
			{Path: "README.md", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "unknown"},
		}}, nil
	}, func(context.Context, catalogfs.Entry, string, string) (*os.File, error) {
		contentCalls.Add(1)
		return os.Open(fixture.hydrator.path)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fs.Destroy)
	c, _ := directoryTestClient(t, fs)
	root := c.repository(t, "alice")
	file := c.lookup(t, root, "README.md")
	opened := c.call(t, map[string]any{"op": "open", "inode": file, "access": 1}, 0)
	metadata := c.call(t, map[string]any{"op": "getattr", "inode": file, "handle": opened.Handle, "require_size": false}, 0).Node
	if metadata.Attributes.SizeKnown == nil || *metadata.Attributes.SizeKnown || metadata.Attributes.Size != 0 || contentCalls.Load() != 0 {
		t.Fatalf("metadata handle stat acquired bytes or invented size: %+v calls=%d", metadata, contentCalls.Load())
	}
	c.call(t, map[string]any{"op": "getattr", "inode": file + 100, "handle": opened.Handle, "require_size": true}, syscall.EBADF)
	c.call(t, map[string]any{"op": "forget", "inode": file, "n": 1}, 0)
	exact := c.call(t, map[string]any{"op": "getattr", "inode": file, "handle": opened.Handle, "require_size": true}, 0).Node
	if exact.Attributes.SizeKnown != nil || exact.Attributes.Size != uint64(len(fixture.content)) || contentCalls.Load() != 1 {
		t.Fatalf("exact handle stat did not acquire one blob: %+v calls=%d", exact, contentCalls.Load())
	}
	metadata = c.call(t, map[string]any{"op": "getattr", "inode": file, "handle": opened.Handle, "require_size": false}, 0).Node
	if metadata.Attributes.SizeKnown != nil || metadata.Attributes.Size != exact.Attributes.Size || contentCalls.Load() != 1 || activations.Load() != 0 || fixture.hydrator.calls.Load() != 0 {
		t.Fatal("cached metadata handle stat lost authoritative size or prepared the engine")
	}
	c.call(t, map[string]any{"op": "release", "inode": file, "handle": opened.Handle}, 0)
	c.call(t, map[string]any{"op": "getattr", "inode": file, "handle": opened.Handle}, syscall.EBADF)
}
