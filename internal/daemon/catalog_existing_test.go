package daemon

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestExistingCatalogRepositoryDoesNotPrepareOrWaitForDormantRuntime(t *testing.T) {
	// No registry or Git store is installed: this lookup must never consult
	// either, even if another preparation has reserved the repository.
	s := &Service{
		running:   make(map[model.RepoID]*repoRuntime),
		preparing: map[model.RepoID]int64{"dormant": 1},
	}
	result := make(chan bool, 1)
	go func() {
		fs, ok := s.ExistingCatalogRepository("dormant")
		result <- ok || fs != nil
	}()
	select {
	case present := <-result:
		if present {
			t.Fatal("dormant preparation returned a catalogue runtime")
		}
	case <-time.After(time.Second):
		t.Fatal("existing-runtime lookup waited for preparation")
	}
	if len(s.running) != 0 || s.preparing["dormant"] != 1 || s.catalogPolicyLocked {
		t.Fatal("existing-runtime lookup mutated preparation or catalogue policy")
	}
}

func TestExistingCatalogRepositoryPreservesStorageAndRejectsStoppingRuntime(t *testing.T) {
	ctx := context.Background()
	s, cfg, binary := newCatalogTestRepository(t)
	index := readCatalogFile(t, filepath.Join(cfg.GitDir, "index"))
	config := readCatalogFile(t, filepath.Join(cfg.GitDir, "config"))
	if fs, ok := s.ExistingCatalogRepository(cfg.Name); ok || fs != nil {
		t.Fatal("prepared storage without a running runtime was opened")
	}
	if len(s.running) != 0 {
		t.Fatal("existing-runtime lookup prepared a runtime")
	}
	if _, err := s.OpenCatalogRepository(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	fs, ok := s.ExistingCatalogRepository(cfg.Name)
	if !ok || fs == nil {
		t.Fatal("running catalogue repository was not returned")
	}
	if got := readCatalogFuseFile(t, fs, "binary.bin", 0, len(binary)); !bytes.Equal(got, binary) {
		t.Fatalf("existing-runtime view changed binary contents: got %x want %x", got, binary)
	}
	if got := readCatalogFile(t, filepath.Join(cfg.GitDir, "index")); !bytes.Equal(got, index) {
		t.Fatal("existing-runtime lookup rewrote the index")
	}
	if got := readCatalogFile(t, filepath.Join(cfg.GitDir, "config")); !bytes.Equal(got, config) {
		t.Fatal("existing-runtime lookup rewrote Git configuration")
	}
	s.mu.Lock()
	rt := s.running[cfg.ID]
	rt.stopping = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		rt.stopping = false
		s.closing = false
		s.mu.Unlock()
	}()
	if fs, ok := s.ExistingCatalogRepository(cfg.Name); ok || fs != nil {
		t.Fatal("stopping runtime admitted another catalogue view")
	}
	s.mu.Lock()
	rt.stopping = false
	s.closing = true
	s.mu.Unlock()
	if fs, ok := s.ExistingCatalogRepository(cfg.Name); ok || fs != nil {
		t.Fatal("closing service admitted another catalogue view")
	}
	s.mu.Lock()
	s.closing = false
	s.mu.Unlock()
}
