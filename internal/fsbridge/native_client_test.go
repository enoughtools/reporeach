//go:build !windows

package fsbridge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
)

// TestNativeFSKitBridgeClient runs an externally compiled Swift protocol/volume
// harness against the real Go catalogue without mounting a filesystem. This is
// opt-in because it requires Apple's SDK and the native harness. The only
// process argument is the private source directory; capabilities stay in the
// descriptor, never in process arguments or diagnostic output.
func TestNativeFSKitBridgeClient(t *testing.T) {
	client := os.Getenv("REPOREACH_FSBRIDGE_NATIVE_CLIENT")
	if client == "" {
		t.Skip("set REPOREACH_FSBRIDGE_NATIVE_CLIENT to the native bridge test harness")
	}
	fixtures := make(map[string]bridgeCatalogFixture)
	for _, entry := range bridgeCatalogEntries {
		fixtures[entry.ID] = newBridgeCatalogFixture(t, entry.ID)
	}
	metadataRoot := t.TempDir()
	metadata, err := overlay.New(context.Background(), model.RepoConfig{
		ID: "catalog", Name: "catalog", OverlayDir: filepath.Join(metadataRoot, "overlay"),
		OverlayDBPath: filepath.Join(metadataRoot, "overlay.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	catalog, err := catalogfs.NewWithMetadata(bridgeCatalogEntries, func(_ context.Context, entry catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		return fixtures[entry.ID].backend, nil
	}, metadata)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.MkdirTemp("", "rrfs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(source) })
	server, err := Start(context.Background(), source, catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.CloseDrain(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, client, source)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native filesystem bridge harness failed: %v\n%s", err, output)
	}
	if len(output) != 0 {
		t.Logf("native filesystem bridge harness: %s", output)
	}
}
