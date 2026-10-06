//go:build !windows

package fsbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

func directoryTestClient(t *testing.T, filesystem fuseutil.FileSystem) (*bridgeCatalogClient, *Handler) {
	t.Helper()
	token := bytes.Repeat([]byte{0x41}, 32)
	handler, err := New(filesystem, token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &bridgeCatalogClient{server: server, token: hex.EncodeToString(token)}, handler
}

func TestDirectoryEnumerationUnknownSizesStayLazy(t *testing.T) {
	for _, catalogue := range []bool{false, true} {
		name := "repository"
		if catalogue {
			name = "catalogue"
		}
		t.Run(name, func(t *testing.T) {
			// This is the actual missing-blob snapshot state: nonempty object OID,
			// unknown size and zero placeholder bytes, unlike the old known-size
			// enumeration fixture. The hydrator exposes binary bytes only on demand.
			fixture := newBridgeCatalogFixtureWithSizeState(t, "unknown", "unknown")
			var filesystem fuseutil.FileSystem = fixture.backend
			root := uint64(1)
			if catalogue {
				fs, err := catalogfs.New(bridgeCatalogEntries[:1], func(context.Context, catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
					return fixture.backend, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				filesystem = fs
			}
			c, handler := directoryTestClient(t, filesystem)
			if catalogue {
				owner := c.lookup(t, 1, "alice")
				root = c.lookup(t, owner, "project")
			}
			handle := c.opendir(t, root)
			listing := c.call(t, map[string]any{"op": "readdir", "inode": root, "handle": handle}, 0)
			var file bridgeCatalogNode
			var forgets []Forget
			for _, entry := range listing.Entries {
				forgets = append(forgets, Forget{Inode: entry.Node.Inode, N: 1})
				if entry.Name == "README.md" {
					file = entry.Node
				}
			}
			if file.Inode == 0 || file.Attributes.SizeKnown == nil || *file.Attributes.SizeKnown || file.Attributes.Size != 0 {
				t.Fatalf("unknown-size entry = %+v", file)
			}
			if calls := fixture.hydrator.calls.Load(); calls != 0 {
				t.Fatalf("directory enumeration hydrated %d blobs", calls)
			}
			c.call(t, map[string]any{"op": "batchforget", "forgets": forgets}, 0)
			if handler.lookups[file.Inode] != 0 {
				t.Fatal("enumeration lookup reference survived batchforget")
			}
			// The directory handle retains the returned identity after all
			// transient lookup refs are forgotten. Real stat still resolves size.
			stat := c.call(t, map[string]any{"op": "getattr", "inode": file.Inode}, 0)
			if stat.Node.Attributes.Size != uint64(len(fixture.content)) || stat.Node.Attributes.SizeKnown != nil {
				t.Fatalf("authoritative stat = %+v", stat.Node)
			}
			if fixture.hydrator.calls.Load() == 0 {
				t.Fatal("authoritative stat did not resolve missing blob size")
			}
			opened := c.call(t, map[string]any{"op": "open", "inode": file.Inode, "access": 1}, 0)
			got, err := c.read(file.Inode, opened.Handle, 0, len(fixture.content))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, fixture.content) {
				t.Fatalf("binary read = %x, want %x", got, fixture.content)
			}
			c.call(t, map[string]any{"op": "release", "inode": file.Inode, "handle": opened.Handle}, 0)
			c.call(t, map[string]any{"op": "releasedir", "inode": root, "handle": handle}, 0)
			c.call(t, map[string]any{"op": "getattr", "inode": file.Inode}, syscall.ESTALE)
		})
	}
}

func TestBatchForgetValidatesWholeBatchAndAggregatesDuplicates(t *testing.T) {
	f := &protocolFilesystem{}
	h, _ := protocolHandler(t, f)
	h.lookups[10], h.lookups[20] = 3, 2
	for _, invalid := range [][]Forget{
		nil, {{Inode: 0, N: 1}}, {{Inode: 10, N: 0}}, {{Inode: 10, N: 1}, {Inode: 20, N: 3}},
		{{Inode: 10, N: math.MaxUint64}, {Inode: 10, N: 1}}, make([]Forget, MaxBatchForgets+1),
	} {
		if err := h.batchForget(context.Background(), invalid); err != syscall.EINVAL {
			t.Fatalf("invalid batch error = %v", err)
		}
		if h.lookups[10] != 3 || h.lookups[20] != 2 || f.forgets.Load() != 0 {
			t.Fatal("invalid batch dropped references")
		}
	}
	if err := h.batchForget(context.Background(), []Forget{{Inode: 10, N: 1}, {Inode: 10, N: 2}, {Inode: 20, N: 1}}); err != nil {
		t.Fatal(err)
	}
	if h.lookups[10] != 0 || h.lookups[20] != 1 || f.forgets.Load() != 2 {
		t.Fatalf("batch references = %+v, backend calls=%d", h.lookups, f.forgets.Load())
	}
}

func TestBatchForgetWorstCaseRequestFitsMetadataBound(t *testing.T) {
	forgets := make([]Forget, MaxBatchForgets)
	for i := range forgets {
		forgets[i] = Forget{Inode: math.MaxUint64, N: math.MaxUint64}
	}
	data, err := json.Marshal(Request{Version: Version, Op: "batchforget", Forgets: forgets})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > MaxMetadataSize {
		t.Fatalf("max batch request %d exceeds metadata bound %d", len(data), MaxMetadataSize)
	}
}

func TestBatchForgetKeepsFailedAndUnprocessedReferencesForDrain(t *testing.T) {
	f := &protocolFilesystem{forget: func(_ context.Context, op *fuseops.ForgetInodeOp) error {
		if op.Inode == 20 {
			return syscall.EIO
		}
		return nil
	}}
	h, _ := protocolHandler(t, f)
	h.lookups[10], h.lookups[20], h.lookups[30] = 1, 1, 1
	if err := h.batchForget(context.Background(), []Forget{{Inode: 10, N: 1}, {Inode: 20, N: 1}, {Inode: 30, N: 1}}); err != syscall.EIO {
		t.Fatalf("backend failure = %v", err)
	}
	if h.lookups[10] != 0 || h.lookups[20] != 1 || h.lookups[30] != 1 {
		t.Fatalf("remaining references = %+v", h.lookups)
	}
	f.forget = nil
	if err := h.closeResources(context.Background()); err != nil || len(h.lookups) != 0 {
		t.Fatalf("drain err=%v references=%+v", err, h.lookups)
	}
}
