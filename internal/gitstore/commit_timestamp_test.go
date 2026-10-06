package gitstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestCommitTimestampOverridesInheritedLazyFetch(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n[ \"$GIT_NO_LAZY_FETCH\" = 1 ] || exit 9\nprintf '%s\\n' '1700000123 1111111111111111111111111111111111111111'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_NO_LAZY_FETCH", "0")
	store := New(nil)
	t.Cleanup(store.Close)
	timestamp, err := store.CommitTimestamp(context.Background(), model.RepoConfig{GitDir: root}, "fixture")
	if err != nil || timestamp != 1700000123 {
		t.Fatalf("local-only timestamp = %d, %v", timestamp, err)
	}
}
