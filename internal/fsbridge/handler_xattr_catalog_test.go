//go:build !windows

package fsbridge

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
)

func TestXattrRPCUsesPersistentMetadataWithoutHydratingOrAliasingRepos(t *testing.T) {
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
	var activations atomic.Int64
	catalog, err := catalogfs.NewWithMetadata(bridgeCatalogEntries, func(_ context.Context, entry catalogfs.Entry) (*fusefs.ArtifactFuse, error) {
		activations.Add(1)
		return fixtures[entry.ID].backend, nil
	}, metadata)
	if err != nil {
		t.Fatal(err)
	}
	handler, authorization := xattrHandler(t, catalog)
	t.Cleanup(func() {
		if err := handler.closeResources(context.Background()); err != nil {
			t.Error(err)
		}
	})
	lookup := func(parent uint64, name string) uint64 {
		t.Helper()
		_, response := xattrMetadataRequest(t, handler, authorization, Request{Op: "lookup", Parent: parent, Name: name})
		if response.Errno != 0 || response.Node == nil || response.Node.Inode == 0 {
			t.Fatalf("fixture lookup failed with errno %d", response.Errno)
		}
		return response.Node.Inode
	}
	set := func(inode uint64, name, policy string, value []byte, code syscall.Errno) {
		t.Helper()
		response, result := xattrRequest(handler, authorization, http.MethodPut, xattrTarget(inode, name, policy), value)
		status := http.StatusOK
		if code != 0 {
			status = http.StatusConflict
		}
		if response.Code != status || result.Errno != int(code) || code == 0 && result.Written != len(value) {
			t.Fatalf("xattr mutation returned HTTP %d errno %d", response.Code, result.Errno)
		}
	}
	get := func(inode uint64, name string, value []byte) {
		t.Helper()
		response, _ := xattrRequest(handler, authorization, http.MethodGet, xattrTarget(inode, name, ""), nil)
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/octet-stream" || !bytes.Equal(response.Body.Bytes(), value) {
			t.Fatal("actual filesystem xattr value changed or became missing")
		}
	}
	missing := func(inode uint64, name string) {
		t.Helper()
		response, result := xattrRequest(handler, authorization, http.MethodGet, xattrTarget(inode, name, ""), nil)
		if response.Code != http.StatusConflict || result.Errno != int(missingXattrErrno()) || !result.XattrMissing {
			t.Fatal("actual filesystem missing xattr did not have a portable missing result")
		}
	}
	owner := lookup(1, "alice")
	repository := lookup(owner, "project")
	const provenance = "com.apple.provenance"
	for index, inode := range []uint64{1, owner, repository} {
		value := []byte{byte(index), 0, 0xff, 0x80}
		set(inode, provenance, "must_create", value, 0)
		get(inode, provenance, value)
	}
	if activations.Load() != 0 {
		t.Fatal("root, organization or repository placeholder attributes activated a repository")
	}
	file := lookup(repository, "README.md")
	otherFile := lookup(lookup(lookup(1, "team"), "project"), "README.md")
	if file == otherFile {
		t.Fatal("repositories aliased an inode")
	}
	value := []byte{0, 0xff, 0x80, 1, 0, 0xfe}
	missing(file, provenance)
	set(file, provenance, "must_replace", value, missingXattrErrno())
	set(file, provenance, "must_create", value, 0)
	set(file, provenance, "must_create", []byte{1}, syscall.EEXIST)
	get(file, provenance, value)
	missing(otherFile, provenance)
	set(file, provenance, "must_replace", []byte{}, 0)
	get(file, provenance, []byte{})
	_, listed := xattrMetadataRequest(t, handler, authorization, Request{Op: "listxattr", Inode: file})
	if listed.Errno != 0 || !equalStrings(listed.XattrNames, []string{provenance}) {
		t.Fatal("empty actual filesystem attribute was missing from its name list")
	}
	_, removed := xattrMetadataRequest(t, handler, authorization, Request{Op: "removexattr", Inode: file, Name: provenance})
	if removed.Errno != 0 {
		t.Fatal("actual filesystem attribute removal failed")
	}
	missing(file, provenance)
	_, removed = xattrMetadataRequest(t, handler, authorization, Request{Op: "removexattr", Inode: file, Name: provenance})
	if removed.Errno != int(missingXattrErrno()) || !removed.XattrMissing {
		t.Fatal("missing removal did not remain distinct from an unsupported operation")
	}
	for index := 0; index < model.MaxXattrsPerObject; index++ {
		name := fmt.Sprintf("%03d", index) + strings.Repeat("\x01", model.MaxXattrNameBytes-3)
		set(file, name, "must_create", []byte{}, 0)
	}
	set(file, "overflow", "must_create", []byte{}, syscall.ENOSPC)
	response, listed := xattrMetadataRequest(t, handler, authorization, Request{Op: "listxattr", Inode: file})
	if listed.Errno != 0 || len(listed.XattrNames) != model.MaxXattrsPerObject || response.Body.Len() <= MaxMetadataSize || response.Body.Len() > MaxXattrListResponseSize {
		t.Fatal("the actual store's maximum accepted escaped-name list could not be returned whole")
	}
	for index, name := range listed.XattrNames {
		if name != fmt.Sprintf("%03d", index)+strings.Repeat("\x01", model.MaxXattrNameBytes-3) {
			t.Fatal("the bounded name list was reordered incorrectly, changed or truncated")
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		policy := ""
		if method == http.MethodPut {
			policy = "always_set"
		}
		_, result := xattrRequest(handler, authorization, method, xattrTarget(^uint64(0), provenance, policy), value)
		if result.Errno != int(syscall.ESTALE) || result.XattrMissing {
			t.Fatal("forged inode accessed metadata or became a missing-attribute result")
		}
	}
	for _, fixture := range fixtures {
		if fixture.hydrator.calls.Load() != 0 {
			t.Fatal("attribute RPCs hydrated repository file contents")
		}
	}
}
