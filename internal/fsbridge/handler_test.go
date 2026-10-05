//go:build !windows

package fsbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

type protocolFilesystem struct {
	fuseutil.NotImplementedFileSystem
	calls    atomic.Int64
	reads    atomic.Int64
	writes   atomic.Int64
	releases atomic.Int64
	forgets  atomic.Int64
	callback atomic.Int64
	getattr  func(context.Context, *fuseops.GetInodeAttributesOp) error
	read     func(context.Context, *fuseops.ReadFileOp) error
	forget   func(context.Context, *fuseops.ForgetInodeOp) error
}

func (f *protocolFilesystem) GetInodeAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) error {
	f.calls.Add(1)
	if f.getattr != nil {
		return f.getattr(ctx, op)
	}
	op.Attributes = fuseops.InodeAttributes{Mode: os.ModeDir | 0o750, Nlink: 2, Uid: 123, Gid: 456}
	return nil
}
func (f *protocolFilesystem) OpenFile(context.Context, *fuseops.OpenFileOp) error {
	return syscall.ENOSYS
}
func (f *protocolFilesystem) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) error {
	f.reads.Add(1)
	if f.read != nil {
		return f.read(ctx, op)
	}
	op.Data = [][]byte{[]byte{0, 255}, []byte{1, 254}}
	op.BytesRead = 4
	op.Callback = func() { f.callback.Add(1) }
	return nil
}
func (f *protocolFilesystem) WriteFile(context.Context, *fuseops.WriteFileOp) error {
	f.writes.Add(1)
	return nil
}
func (f *protocolFilesystem) ReleaseFileHandle(context.Context, *fuseops.ReleaseFileHandleOp) error {
	f.releases.Add(1)
	return nil
}
func (f *protocolFilesystem) ForgetInode(ctx context.Context, op *fuseops.ForgetInodeOp) error {
	f.forgets.Add(1)
	if f.forget != nil {
		return f.forget(ctx, op)
	}
	return nil
}

func protocolHandler(t *testing.T, f *protocolFilesystem) (*Handler, string) {
	t.Helper()
	token := bytes.Repeat([]byte{0x53}, 32)
	h, err := New(f, token)
	if err != nil {
		t.Fatal(err)
	}
	return h, "Bearer " + hex.EncodeToString(token)
}

func protocolRequest(h *Handler, authorization, method, url, body string) (*httptest.ResponseRecorder, Response) {
	r := httptest.NewRequest(method, url, strings.NewReader(body))
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var response Response
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(w.Body.Bytes(), &response)
	}
	return w, response
}

func TestProtocolRejectsUnauthorizedAndMalformedBeforeDispatch(t *testing.T) {
	f := &protocolFilesystem{}
	h, authorization := protocolHandler(t, f)
	for _, token := range []string{"", "Bearer invalid", authorization + "\n", strings.ToLower(authorization)} {
		w, response := protocolRequest(h, token, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`)
		if w.Code != http.StatusUnauthorized || response.Errno != int(syscall.EACCES) {
			t.Fatalf("unauthorized request: status %d errno %d", w.Code, response.Errno)
		}
	}
	for _, body := range []string{`{`, `{"version":2,"op":"getattr","inode":1}`, `{"version":1,"op":"getattr","inode":1,"extra":"value"}`, `{"version":1,"op":"getattr","inode":1} {}`, strings.Repeat(" ", MaxMetadataSize+1)} {
		w, response := protocolRequest(h, authorization, http.MethodPost, "/v1/fs", body)
		if w.Code != http.StatusBadRequest || response.Errno != int(syscall.EINVAL) {
			t.Fatalf("malformed request: status %d errno %d", w.Code, response.Errno)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("invalid request reached the filesystem")
	}
	w, response := protocolRequest(h, authorization, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`)
	if w.Code != http.StatusOK || response.Errno != 0 || response.Node.Attributes.Mode != syscall.S_IFDIR|0o750 || response.Node.Attributes.UID != 123 {
		t.Fatal("POSIX attributes were not preserved")
	}
}

func TestProtocolCancellationAndErrorsExposeOnlyErrno(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want syscall.Errno
	}{{fmt.Errorf("wrapped: %w", context.Canceled), syscall.EINTR}, {context.DeadlineExceeded, syscall.ETIMEDOUT}, {fmt.Errorf("wrapped: %w", syscall.EXDEV), syscall.EXDEV}, {errors.New("synthetic-credential-must-not-appear"), syscall.EIO}} {
		f := &protocolFilesystem{getattr: func(context.Context, *fuseops.GetInodeAttributesOp) error { return tc.err }}
		h, authorization := protocolHandler(t, f)
		w, response := protocolRequest(h, authorization, http.MethodPost, "/v1/fs", `{"version":1,"op":"getattr","inode":1}`)
		if response.Errno != int(tc.want) || w.Header().Get("X-RepoReach-Errno") != fmt.Sprint(int(tc.want)) {
			t.Fatalf("errno %d, want %d", response.Errno, tc.want)
		}
		if strings.Contains(w.Body.String(), "synthetic-credential") || strings.Contains(w.Body.String(), "wrapped") {
			t.Fatal("backend error detail crossed the protocol")
		}
	}
}

func TestBinaryBoundsCannotReachFilesystemOrAliasHandle(t *testing.T) {
	f := &protocolFilesystem{}
	h, authorization := protocolHandler(t, f)
	h.register(9, 4, false)
	for _, query := range []string{"inode=4&handle=9&offset=-1&size=4", "inode=4&handle=9&offset=0&size=1048577", "inode=4&handle=9&offset=9223372036854775807&size=4", "inode=4&handle=9&offset=0&size=4&size=4", "inode=4&handle=9&offset=0&size=4&unknown=1", "inode=5&handle=9&offset=0&size=4", "inode=4&handle=99&offset=0&size=4"} {
		w, response := protocolRequest(h, authorization, http.MethodGet, "/v1/fs/read?"+query, "")
		if w.Code == http.StatusOK || response.Errno == 0 {
			t.Fatal("invalid binary request succeeded")
		}
	}
	for _, tc := range []struct {
		offset int64
		body   string
	}{{0, strings.Repeat("x", MaxChunkSize+1)}, {math.MaxInt64, "x"}} {
		w, response := protocolRequest(h, authorization, http.MethodPut, fmt.Sprintf("/v1/fs/write?inode=4&handle=9&offset=%d", tc.offset), tc.body)
		if w.Code == http.StatusOK || response.Errno == 0 {
			t.Fatal("invalid write succeeded")
		}
	}
	if f.reads.Load() != 0 || f.writes.Load() != 0 {
		t.Fatal("invalid binary request reached the filesystem")
	}
	w, _ := protocolRequest(h, authorization, http.MethodGet, "/v1/fs/read?inode=4&handle=9&offset=0&size=4", "")
	if !bytes.Equal(w.Body.Bytes(), []byte{0, 255, 1, 254}) || w.Header().Get("Content-Length") != "4" || f.callback.Load() != 1 {
		t.Fatal("binary chunks, length or callback were not preserved")
	}
}

func TestBinaryMalformedBackendCannotExceedBound(t *testing.T) {
	f := &protocolFilesystem{read: func(_ context.Context, op *fuseops.ReadFileOp) error {
		op.Data = [][]byte{make([]byte, 5)}
		op.BytesRead = 5
		return nil
	}}
	h, authorization := protocolHandler(t, f)
	h.register(9, 4, false)
	w, response := protocolRequest(h, authorization, http.MethodGet, "/v1/fs/read?inode=4&handle=9&offset=0&size=4", "")
	if w.Code == http.StatusOK || response.Errno != int(syscall.EIO) {
		t.Fatal("oversized backend response was sent as file content")
	}
}

func TestCleanupRetriesFailedLookupRelease(t *testing.T) {
	var failures atomic.Int64
	filesystem := &protocolFilesystem{forget: func(context.Context, *fuseops.ForgetInodeOp) error {
		if failures.Add(1) == 1 {
			return context.Canceled
		}
		return nil
	}}
	h, _ := protocolHandler(t, filesystem)
	h.remember(Node{Inode: 7})
	if err := h.closeResources(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal("failed forget was not surfaced")
	}
	if err := h.closeResources(context.Background()); err != nil {
		t.Fatal(err)
	}
	if filesystem.forgets.Load() != 2 {
		t.Fatal("failed lookup reference was lost instead of retried")
	}
}
