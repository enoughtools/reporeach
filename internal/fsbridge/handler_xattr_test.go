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
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
	"golang.org/x/sys/unix"
)

type xattrProtocolFilesystem struct {
	fuseutil.NotImplementedFileSystem
	calls  atomic.Int64
	get    func(context.Context, *fuseops.GetXattrOp) error
	set    func(context.Context, *fuseops.SetXattrOp) error
	list   func(context.Context, *fuseops.ListXattrOp) error
	remove func(context.Context, *fuseops.RemoveXattrOp) error
}

func (f *xattrProtocolFilesystem) GetXattr(ctx context.Context, op *fuseops.GetXattrOp) error {
	f.calls.Add(1)
	return f.get(ctx, op)
}
func (f *xattrProtocolFilesystem) SetXattr(ctx context.Context, op *fuseops.SetXattrOp) error {
	f.calls.Add(1)
	return f.set(ctx, op)
}
func (f *xattrProtocolFilesystem) ListXattr(ctx context.Context, op *fuseops.ListXattrOp) error {
	f.calls.Add(1)
	return f.list(ctx, op)
}
func (f *xattrProtocolFilesystem) RemoveXattr(ctx context.Context, op *fuseops.RemoveXattrOp) error {
	f.calls.Add(1)
	return f.remove(ctx, op)
}

func xattrHandler(t *testing.T, filesystem fuseutil.FileSystem) (*Handler, string) {
	t.Helper()
	token := bytes.Repeat([]byte{0x63}, 32)
	handler, err := New(filesystem, token)
	if err != nil {
		t.Fatal(err)
	}
	return handler, "Bearer " + hex.EncodeToString(token)
}

func xattrRequest(h *Handler, authorization, method, target string, body []byte) (*httptest.ResponseRecorder, Response) {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	var metadata Response
	if response.Header().Get("Content-Type") == "application/json" {
		_ = json.Unmarshal(response.Body.Bytes(), &metadata)
	}
	return response, metadata
}

func xattrTarget(inode uint64, name, policy string) string {
	query := url.Values{"inode": {strconv.FormatUint(inode, 10)}, "name": {name}}
	if policy != "" {
		query.Set("policy", policy)
	}
	return "/v1/fs/xattr?" + query.Encode()
}

func xattrMetadataRequest(t *testing.T, h *Handler, authorization string, request Request) (*httptest.ResponseRecorder, Response) {
	t.Helper()
	request.Version = Version
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return xattrRequest(h, authorization, http.MethodPost, "/v1/fs", data)
}

func TestXattrCapabilityRejectsAllEndpointsBeforeDispatch(t *testing.T) {
	filesystem := &xattrProtocolFilesystem{}
	handler, authorization := xattrHandler(t, filesystem)
	for _, auth := range []string{"", "Bearer invalid", authorization + "0"} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			policy := ""
			if method == http.MethodPut {
				policy = "always_set"
			}
			response, metadata := xattrRequest(handler, auth, method, xattrTarget(1, "com.apple.provenance", policy), []byte{0, 0xff})
			if response.Code != http.StatusUnauthorized || metadata.Errno != int(syscall.EACCES) {
				t.Fatal("unauthorized binary xattr request succeeded")
			}
		}
		for _, operation := range []string{"listxattr", "removexattr"} {
			response, metadata := xattrMetadataRequest(t, handler, auth, Request{Op: operation, Inode: 1, Name: "com.apple.provenance"})
			if response.Code != http.StatusUnauthorized || metadata.Errno != int(syscall.EACCES) {
				t.Fatal("unauthorized metadata xattr request succeeded")
			}
		}
	}
	request := httptest.NewRequest(http.MethodGet, xattrTarget(1, "com.apple.provenance", ""), nil)
	request.Header.Add("Authorization", authorization)
	request.Header.Add("Authorization", authorization)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || filesystem.calls.Load() != 0 {
		t.Fatal("ambiguous authorization reached the filesystem")
	}
}

func TestXattrBinaryQueriesAndBoundsFailBeforeDispatch(t *testing.T) {
	filesystem := &xattrProtocolFilesystem{}
	handler, authorization := xattrHandler(t, filesystem)
	queries := []string{
		"inode=0&name=valid", "inode=-1&name=valid", "inode=18446744073709551616&name=valid",
		"inode=1", "inode=1&name=", "inode=1&name=invalid%00name", "inode=1&name=%FF",
		"inode=1&name=" + strings.Repeat("x", MaxXattrNameSize+1),
		"inode=1&name=valid&name=valid", "inode=1&inode=2&name=valid", "inode=1&name=valid&handle=4",
		"inode=1&name=valid&bad=%GG", "inode=1&name=valid&bad=%", "inode=1&name=valid;policy=always_set",
		"inode=1&name=valid&" + strings.Repeat("x", MaxMetadataSize),
	}
	for _, query := range queries {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			if method == http.MethodPut {
				query += "&policy=always_set"
			}
			response, metadata := xattrRequest(handler, authorization, method, "/v1/fs/xattr?"+query, nil)
			if response.Code != http.StatusConflict || metadata.Errno != int(syscall.EINVAL) {
				t.Fatalf("malformed xattr query returned HTTP %d errno %d", response.Code, metadata.Errno)
			}
		}
	}
	for _, query := range []string{"inode=1&name=valid", "inode=1&name=valid&policy=3", "inode=1&name=valid&policy=delete", "inode=1&name=valid&policy=must_create&policy=must_replace"} {
		_, metadata := xattrRequest(handler, authorization, http.MethodPut, "/v1/fs/xattr?"+query, nil)
		if metadata.Errno != int(syscall.EINVAL) {
			t.Fatal("invalid set policy was accepted")
		}
	}
	for _, length := range []int64{MaxXattrValueSize + 1, -1} {
		request := httptest.NewRequest(http.MethodPut, xattrTarget(1, "valid", "always_set"), bytes.NewReader(make([]byte, MaxXattrValueSize+1)))
		request.ContentLength = length
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var metadata Response
		if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusConflict || metadata.Errno != int(syscall.E2BIG) {
			t.Fatal("oversized xattr body reached the filesystem")
		}
	}
	if filesystem.calls.Load() != 0 {
		t.Fatal("invalid xattr request reached the filesystem")
	}
}

func TestXattrBinaryValueAndTypedPolicyRemainExact(t *testing.T) {
	name := strings.Repeat("a", 121) + "é/&+="
	if len(name) != MaxXattrNameSize {
		t.Fatal("fixture does not exercise the exact UTF-8 name bound")
	}
	for _, value := range [][]byte{{}, {0, 255, 0, 254, 1}, bytes.Repeat([]byte{0, 0xff, 0x80, 0x01}, MaxXattrValueSize/4)} {
		for policy, flags := range map[string]uint32{"always_set": 0, "must_create": unix.XATTR_CREATE, "must_replace": unix.XATTR_REPLACE} {
			filesystem := &xattrProtocolFilesystem{
				set: func(_ context.Context, op *fuseops.SetXattrOp) error {
					if op.Inode != 17 || op.Name != name || op.Flags != flags || !bytes.Equal(op.Value, value) {
						t.Fatal("binary value, inode, name or host-native flags changed")
					}
					return nil
				},
				get: func(_ context.Context, op *fuseops.GetXattrOp) error {
					if op.Inode != 17 || op.Name != name || len(op.Dst) != MaxXattrValueSize {
						t.Fatal("get xattr destination or identity changed")
					}
					op.BytesRead = copy(op.Dst, value)
					return nil
				},
			}
			handler, authorization := xattrHandler(t, filesystem)
			response, metadata := xattrRequest(handler, authorization, http.MethodPut, xattrTarget(17, name, policy), value)
			if response.Code != http.StatusOK || metadata.Errno != 0 || metadata.Written != len(value) {
				t.Fatal("set xattr did not acknowledge the exact whole value")
			}
			response, _ = xattrRequest(handler, authorization, http.MethodGet, xattrTarget(17, name, ""), nil)
			if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/octet-stream" || response.Header().Get("Content-Length") != strconv.Itoa(len(value)) || response.Header().Get("X-RepoReach-Errno") != "0" || !bytes.Equal(response.Body.Bytes(), value) {
				t.Fatal("get xattr did not preserve the full binary or empty value")
			}
		}
	}
}

func TestXattrMissingTagPreservesOtherPlatformErrors(t *testing.T) {
	for _, backendError := range []error{fmt.Errorf("wrapped: %w", syscall.ENODATA), missingXattrErrno(), syscall.ENOENT, syscall.EEXIST, syscall.EIO, errors.New("private-backend-detail")} {
		filesystem := &xattrProtocolFilesystem{
			get:    func(context.Context, *fuseops.GetXattrOp) error { return backendError },
			set:    func(context.Context, *fuseops.SetXattrOp) error { return backendError },
			list:   func(context.Context, *fuseops.ListXattrOp) error { return backendError },
			remove: func(context.Context, *fuseops.RemoveXattrOp) error { return backendError },
		}
		handler, authorization := xattrHandler(t, filesystem)
		var results []Response
		_, get := xattrRequest(handler, authorization, http.MethodGet, xattrTarget(1, "valid", ""), nil)
		results = append(results, get)
		_, set := xattrRequest(handler, authorization, http.MethodPut, xattrTarget(1, "valid", "must_replace"), []byte{0, 0xff})
		results = append(results, set)
		for _, operation := range []string{"listxattr", "removexattr"} {
			_, metadata := xattrMetadataRequest(t, handler, authorization, Request{Op: operation, Inode: 1, Name: "valid"})
			results = append(results, metadata)
		}
		missing := isMissingXattr(backendError)
		want := errno(backendError)
		if missing {
			want = int(missingXattrErrno())
		}
		for _, result := range results {
			if result.Errno != want || result.XattrMissing != missing {
				t.Fatalf("xattr error = %d missing=%t, want %d missing=%t", result.Errno, result.XattrMissing, want, missing)
			}
			data, err := json.Marshal(result)
			if err != nil || bytes.Contains(data, []byte("private-backend-detail")) || bytes.Contains(data, []byte("wrapped")) {
				t.Fatal("backend error details crossed the protocol")
			}
		}
	}
}

func TestXattrMalformedBackendNeverTruncatesOrExceedsBounds(t *testing.T) {
	for _, count := range []int{-1, MaxXattrValueSize + 1} {
		filesystem := &xattrProtocolFilesystem{get: func(_ context.Context, op *fuseops.GetXattrOp) error {
			op.Dst = make([]byte, MaxXattrValueSize+1)
			op.BytesRead = count
			return nil
		}}
		handler, authorization := xattrHandler(t, filesystem)
		response, metadata := xattrRequest(handler, authorization, http.MethodGet, xattrTarget(1, "valid", ""), nil)
		if response.Code != http.StatusConflict || metadata.Errno != int(syscall.EIO) || response.Header().Get("Content-Type") != "application/json" {
			t.Fatal("malformed get xattr was emitted as a truncated binary value")
		}
	}
	for _, names := range [][]byte{[]byte("unterminated"), {0}, {0xff, 0}, []byte("duplicate\x00duplicate\x00"), append(bytes.Repeat([]byte{'a'}, MaxXattrNameSize+1), 0)} {
		filesystem := &xattrProtocolFilesystem{list: func(_ context.Context, op *fuseops.ListXattrOp) error {
			op.BytesRead = copy(op.Dst, names)
			return nil
		}}
		handler, authorization := xattrHandler(t, filesystem)
		_, metadata := xattrMetadataRequest(t, handler, authorization, Request{Op: "listxattr", Inode: 1})
		if metadata.Errno != int(syscall.EIO) {
			t.Fatal("malformed attribute name list was sent to the client")
		}
	}
	filesystem := &xattrProtocolFilesystem{list: func(_ context.Context, op *fuseops.ListXattrOp) error {
		var names []byte
		for index := 0; index < 128; index++ {
			names = append(names, []byte(fmt.Sprintf("%03d", index))...)
			names = append(names, bytes.Repeat([]byte{1}, 124)...)
			names = append(names, 0)
		}
		op.BytesRead = copy(op.Dst, names)
		return nil
	}}
	handler, authorization := xattrHandler(t, filesystem)
	response, metadata := xattrMetadataRequest(t, handler, authorization, Request{Op: "listxattr", Inode: 1})
	if metadata.Errno != 0 || response.Body.Len() <= MaxMetadataSize || response.Body.Len() > MaxXattrListResponseSize || len(metadata.XattrNames) != 128 {
		t.Fatal("the maximum accepted control-name list was rejected, truncated or exceeded its bounded response")
	}
}

func TestXattrListAndRemovalUseInodeWithoutPathNormalization(t *testing.T) {
	names := []string{"com.apple.provenance", "valid/name", "é&=+", "control\x01name"}
	filesystem := &xattrProtocolFilesystem{
		list: func(_ context.Context, op *fuseops.ListXattrOp) error {
			if op.Inode != 7 {
				t.Fatal("list xattr inode changed")
			}
			for _, name := range names {
				op.BytesRead += copy(op.Dst[op.BytesRead:], []byte(name))
				op.Dst[op.BytesRead] = 0
				op.BytesRead++
			}
			return nil
		},
		remove: func(_ context.Context, op *fuseops.RemoveXattrOp) error {
			if op.Inode != 7 || op.Name != "valid/name" {
				t.Fatal("remove xattr identity changed")
			}
			return nil
		},
	}
	handler, authorization := xattrHandler(t, filesystem)
	response, metadata := xattrMetadataRequest(t, handler, authorization, Request{Op: "listxattr", Inode: 7})
	if response.Code != http.StatusOK || metadata.Errno != 0 || !equalStrings(metadata.XattrNames, names) {
		t.Fatal("attribute name list was not preserved")
	}
	_, metadata = xattrMetadataRequest(t, handler, authorization, Request{Op: "removexattr", Inode: 7, Name: "valid/name"})
	if metadata.Errno != 0 {
		t.Fatal("valid removal failed")
	}
	for _, request := range []Request{{Op: "listxattr"}, {Op: "removexattr", Name: "valid"}, {Op: "removexattr", Inode: 7}, {Op: "removexattr", Inode: 7, Name: "invalid\x00name"}} {
		_, metadata := xattrMetadataRequest(t, handler, authorization, request)
		if metadata.Errno != int(syscall.EINVAL) {
			t.Fatal("invalid list/removal request succeeded")
		}
	}
	if filesystem.calls.Load() != 2 {
		t.Fatal("invalid list/removal reached the filesystem")
	}
}

func TestXattrMetadataRejectsInvalidEncodingWithoutAliasingNames(t *testing.T) {
	filesystem := &xattrProtocolFilesystem{remove: func(_ context.Context, op *fuseops.RemoveXattrOp) error {
		if op.Name != "emoji😀" && op.Name != "literal\\ud800" && op.Name != "valid�" {
			t.Fatal("valid JSON attribute name changed")
		}
		return nil
	}}
	handler, authorization := xattrHandler(t, filesystem)
	invalidUTF8 := append([]byte(`{"version":1,"op":"removexattr","inode":1,"name":"valid`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	for _, body := range [][]byte{
		invalidUTF8,
		[]byte(`{"version":1,"op":"removexattr","inode":1,"name":"invalid\ud800"}`),
		[]byte(`{"version":1,"op":"removexattr","inode":1,"name":"invalid\udc00"}`),
		[]byte(`{"version":1,"op":"removexattr","inode":1,"name":"invalid\ud800\u0061"}`),
	} {
		response, metadata := xattrRequest(handler, authorization, http.MethodPost, "/v1/fs", body)
		if response.Code != http.StatusBadRequest || metadata.Errno != int(syscall.EINVAL) {
			t.Fatal("invalid JSON attribute encoding aliased a valid name")
		}
	}
	if filesystem.calls.Load() != 0 {
		t.Fatal("invalid metadata encoding reached the filesystem")
	}
	for _, body := range [][]byte{
		[]byte(`{"version":1,"op":"removexattr","inode":1,"name":"emoji\ud83d\ude00"}`),
		[]byte(`{"version":1,"op":"removexattr","inode":1,"name":"literal\\ud800"}`),
		[]byte(`{"version":1,"op":"removexattr","inode":1,"name":"valid�"}`),
	} {
		response, metadata := xattrRequest(handler, authorization, http.MethodPost, "/v1/fs", body)
		if response.Code != http.StatusOK || metadata.Errno != 0 {
			t.Fatal("valid Unicode JSON attribute name was rejected")
		}
	}
}

func TestXattrUnsupportedMethodsCannotDispatch(t *testing.T) {
	filesystem := &xattrProtocolFilesystem{}
	handler, authorization := xattrHandler(t, filesystem)
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch, http.MethodHead} {
		response, metadata := xattrRequest(handler, authorization, method, xattrTarget(1, "valid", "always_set"), []byte{0, 0xff})
		if response.Code != http.StatusNotFound || metadata.Errno != int(syscall.ENOSYS) {
			t.Fatal("unsupported binary xattr method succeeded")
		}
	}
	if filesystem.calls.Load() != 0 {
		t.Fatal("unsupported method reached the filesystem")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func TestXattrCanceledOrInterruptedBodyCannotMutate(t *testing.T) {
	filesystem := &xattrProtocolFilesystem{}
	handler, authorization := xattrHandler(t, filesystem)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		policy := ""
		if method == http.MethodPut {
			policy = "always_set"
		}
		request := httptest.NewRequest(method, xattrTarget(1, "valid", policy), nil)
		request.Header.Set("Authorization", authorization)
		ctx, cancel := context.WithCancel(request.Context())
		cancel()
		request = request.WithContext(ctx)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var metadata Response
		if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil || metadata.Errno != int(syscall.EINTR) {
			t.Fatal("canceled xattr request did not fail before the filesystem")
		}
	}
	request := httptest.NewRequest(http.MethodPut, xattrTarget(1, "valid", "always_set"), nil)
	request.Body = io.NopCloser(xattrInterruptedBody{})
	request.ContentLength = -1
	request.Header.Set("Authorization", authorization)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var metadata Response
	if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil || metadata.Errno != int(syscall.EINVAL) || filesystem.calls.Load() != 0 {
		t.Fatal("an incomplete xattr body mutated the filesystem")
	}
}

type xattrInterruptedBody struct{}

func (xattrInterruptedBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
