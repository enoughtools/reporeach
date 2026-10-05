//go:build !windows

package fsbridge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/jacobsa/fuse/fuseops"
)

// The optional executable is compiled from the production Swift bridge client.
// Its HTTP responses come from the real Go server and catalogue, not a socket
// response fixture. No FSKit framework, kernel mount or remote repository is used.
func TestNativeClientInterop(t *testing.T) {
	client := os.Getenv("AFS_NATIVE_BRIDGE_CLIENT")
	if client == "" {
		t.Skip("set AFS_NATIVE_BRIDGE_CLIENT to the compiled Swift bridge smoke executable")
	}
	if !filepath.IsAbs(client) {
		t.Fatal("AFS_NATIVE_BRIDGE_CLIENT must be an absolute executable path")
	}
	fixture := newBridgeCatalogFixture(t, "interop/project")
	entries := []catalogfs.Entry{{ID: "interop/project", Owner: "interop", Name: "project"}}
	// Long names exceed one 64KiB engine directory page. The metadata response
	// also exceeds net/http's small-response buffer, testing real HTTP framing.
	for i := range 300 {
		owner := fmt.Sprintf("owner-%03d-", i) + strings.Repeat("x", 200)
		entries = append(entries, catalogfs.Entry{ID: fmt.Sprintf("repo-%d", i), Owner: owner, Name: "project"})
	}
	var activations atomic.Int64
	fs, err := catalogfs.New(entries, func(_ context.Context, entry catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		if entry.ID != "interop/project" {
			return nil, fmt.Errorf("unexpected repository activation: %s", entry.ID)
		}
		activations.Add(1)
		return fixture.backend, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Darwin's Unix socket pathname limit is short; Go's default test temporary
	// directory may be too long. This fresh, private session is always removed.
	directory, err := os.MkdirTemp("/tmp", "rr-interop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	server, err := Start(context.Background(), directory, fs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.CloseDrain(ctx); err != nil {
			t.Errorf("bridge shutdown: %v", err)
		}
		select {
		case <-server.Done():
			if err := server.Err(); err != nil {
				t.Errorf("bridge serve: %v", err)
			}
		case <-ctx.Done():
			t.Error("bridge serve did not exit after shutdown")
		}
		for _, name := range []string{"bridge.sock", "connection.json"} {
			if _, err := os.Lstat(filepath.Join(directory, name)); !os.IsNotExist(err) {
				t.Errorf("session artifact %s remains after shutdown: %v", name, err)
			}
		}
		server.handler.mu.Lock()
		defer server.handler.mu.Unlock()
		if len(server.handler.handles) != 0 || len(server.handler.lookups) != 0 {
			t.Errorf("session retained handles=%d lookups=%d", len(server.handler.handles), len(server.handler.lookups))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, client, filepath.Join(directory, "connection.json"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("actual Swift bridge client failed: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("PASS: native client and real Go filesystem bridge")) {
		t.Fatalf("Swift smoke did not report completion: %s", output)
	}
	t.Logf("%s", bytes.TrimSpace(output))
	if activations.Load() != 1 || fixture.hydrator.calls.Load() == 0 {
		t.Fatalf("real engine path not exercised: activations=%d hydrations=%d", activations.Load(), fixture.hydrator.calls.Load())
	}

	lookup := func(parent fuseops.InodeID, name string) fuseops.InodeID {
		t.Helper()
		op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
		if err := fs.LookUpInode(context.Background(), op); err != nil {
			t.Fatalf("backend lookup %q: %v", name, err)
		}
		return op.Entry.Child
	}
	repo := lookup(lookup(fuseops.RootInodeID, "interop"), "project")
	inode := lookup(repo, "雪🧪.bin")
	open := &fuseops.OpenFileOp{Inode: inode}
	if err := fs.OpenFile(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: open.Handle})
	}()
	read := &fuseops.ReadFileOp{Inode: inode, Handle: open.Handle, Size: 1024, Dst: make([]byte, 1024)}
	if err := fs.ReadFile(context.Background(), read); err != nil {
		t.Fatal(err)
	}
	if read.Callback != nil {
		defer read.Callback()
	}
	got := read.Dst[:read.BytesRead]
	if read.Data != nil {
		got = bytes.Join(read.Data, nil)
	}
	want := make([]byte, 260)
	for i := range 256 {
		want[i] = byte(i)
	}
	copy(want[256:], []byte{0, 255, 254, 128})
	copy(want[13:], []byte{255, 0, 128, 13, 10})
	if !bytes.Equal(got, want) {
		t.Fatal("Swift binary mutations did not reach the real writable engine intact")
	}
	baseline, err := os.ReadFile(fixture.hydrator.path)
	if err != nil || !bytes.Equal(baseline, fixture.content) {
		t.Fatal("Swift overlay writes changed the committed hydration source")
	}
}
