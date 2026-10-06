//go:build !windows

package fsbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
	"golang.org/x/sys/unix"
)

func validXattrName(name string) bool {
	return name != "" && len(name) <= MaxXattrNameSize && utf8.ValidString(name) && !strings.ContainsRune(name, 0)
}

// encoding/json replaces invalid UTF-8 and unpaired UTF-16 escapes with U+FFFD.
// Reject both before decoding, so an invalid wire name can never address a
// different, valid attribute. The JSON decoder still validates JSON syntax.
func validMetadataEncoding(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for index := 0; index < len(data); index++ {
		if data[index] != '"' {
			continue
		}
		index++
		for index < len(data) && data[index] != '"' {
			if data[index] != '\\' {
				index++
				continue
			}
			if index+1 >= len(data) {
				return false
			}
			if data[index+1] != 'u' {
				index += 2
				continue
			}
			code, ok := metadataUnicodeEscape(data, index)
			if !ok || code >= 0xdc00 && code <= 0xdfff {
				return false
			}
			index += 6
			if code >= 0xd800 && code <= 0xdbff {
				low, ok := metadataUnicodeEscape(data, index)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return false
				}
				index += 6
			}
		}
	}
	return true
}

func metadataUnicodeEscape(data []byte, index int) (uint16, bool) {
	if index+6 > len(data) || data[index] != '\\' || data[index+1] != 'u' {
		return 0, false
	}
	var decoded [2]byte
	if _, err := hex.Decode(decoded[:], data[index+2:index+6]); err != nil {
		return 0, false
	}
	return uint16(decoded[0])<<8 | uint16(decoded[1]), true
}

func xattrErrorResponse(err error) Response {
	response := Response{Version: Version, Errno: errno(err)}
	if isMissingXattr(err) {
		response.Errno = int(missingXattrErrno())
		response.XattrMissing = true
	}
	return response
}

func xattrBinaryError(w http.ResponseWriter, err error) {
	writeResponse(w, http.StatusConflict, xattrErrorResponse(err))
}

// Policies are names on the wire, rather than numeric FSKit policies or Darwin
// setxattr flags. Only this boundary translates them to the host's native flags
// used by its FUSE callbacks, leaving create/replace atomicity to the filesystem.
func parseXattrRequest(r *http.Request, set bool) (fuseops.InodeID, string, uint32, error) {
	// URL.Query silently discards malformed percent escapes. ParseQuery must
	// succeed so malformed/duplicate/unknown fields cannot reach the filesystem.
	if len(r.URL.RawQuery) > MaxMetadataSize {
		return 0, "", 0, syscall.EINVAL
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, "", 0, syscall.EINVAL
	}
	allowed := map[string]bool{"inode": true, "name": true}
	if set {
		allowed["policy"] = true
	}
	if len(query) != len(allowed) {
		return 0, "", 0, syscall.EINVAL
	}
	for key, values := range query {
		if !allowed[key] || len(values) != 1 {
			return 0, "", 0, syscall.EINVAL
		}
	}
	inode, err := strconv.ParseUint(query.Get("inode"), 10, 64)
	name := query.Get("name")
	if err != nil || inode == 0 || !validXattrName(name) {
		return 0, "", 0, syscall.EINVAL
	}
	var flags uint32
	if set {
		switch query.Get("policy") {
		case "always_set":
		case "must_create":
			flags = unix.XATTR_CREATE
		case "must_replace":
			flags = unix.XATTR_REPLACE
		default:
			return 0, "", 0, syscall.EINVAL
		}
	}
	return fuseops.InodeID(inode), name, flags, nil
}

func (h *Handler) getXattr(w http.ResponseWriter, r *http.Request) {
	inode, name, _, err := parseXattrRequest(r, false)
	if err != nil {
		xattrBinaryError(w, err)
		return
	}
	if err := r.Context().Err(); err != nil {
		xattrBinaryError(w, err)
		return
	}
	op := &fuseops.GetXattrOp{Inode: inode, Name: name, Dst: make([]byte, MaxXattrValueSize)}
	if err := h.filesystem.GetXattr(r.Context(), op); err != nil {
		xattrBinaryError(w, err)
		return
	}
	if op.BytesRead < 0 || op.BytesRead > MaxXattrValueSize || op.BytesRead > len(op.Dst) {
		xattrBinaryError(w, syscall.EIO)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(op.BytesRead))
	w.Header().Set("X-RepoReach-Errno", "0")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(op.Dst[:op.BytesRead])
}

func (h *Handler) setXattr(w http.ResponseWriter, r *http.Request) {
	inode, name, flags, err := parseXattrRequest(r, true)
	if err != nil {
		xattrBinaryError(w, err)
		return
	}
	if r.ContentLength > MaxXattrValueSize {
		xattrBinaryError(w, syscall.E2BIG)
		return
	}
	value, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxXattrValueSize))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			xattrBinaryError(w, syscall.E2BIG)
		} else if r.Context().Err() != nil {
			xattrBinaryError(w, r.Context().Err())
		} else {
			xattrBinaryError(w, syscall.EINVAL)
		}
		return
	}
	if err := r.Context().Err(); err != nil {
		xattrBinaryError(w, err)
		return
	}
	if err := h.filesystem.SetXattr(r.Context(), &fuseops.SetXattrOp{Inode: inode, Name: name, Value: value, Flags: flags}); err != nil {
		xattrBinaryError(w, err)
		return
	}
	writeResponse(w, http.StatusOK, Response{Version: Version, Written: len(value)})
}

func (h *Handler) listXattr(ctx context.Context, inode fuseops.InodeID) (Response, error) {
	response := Response{Version: Version, Entries: make([]DirectoryEntry, 0), XattrNames: make([]string, 0)}
	op := &fuseops.ListXattrOp{Inode: inode, Dst: make([]byte, MaxXattrListSize)}
	if err := h.filesystem.ListXattr(ctx, op); err != nil {
		if errors.Is(err, syscall.ERANGE) {
			return response, syscall.E2BIG
		}
		return response, err
	}
	if op.BytesRead < 0 || op.BytesRead > MaxXattrListSize || op.BytesRead > len(op.Dst) {
		return response, syscall.EIO
	}
	names := op.Dst[:op.BytesRead]
	seen := make(map[string]bool)
	for len(names) > 0 {
		end := bytes.IndexByte(names, 0)
		if end < 0 {
			return response, syscall.EIO
		}
		name := string(names[:end]) // Attribute names, never attribute values.
		if !validXattrName(name) || seen[name] {
			return response, syscall.EIO
		}
		seen[name] = true
		response.XattrNames = append(response.XattrNames, name)
		if len(response.XattrNames) > model.MaxXattrsPerObject {
			return response, syscall.EIO
		}
		names = names[end+1:]
	}
	// JSON escaping may be larger than the NUL-delimited backend result. Bound
	// the actual response before it can be emitted or partially truncated.
	data, err := json.Marshal(response)
	if err != nil || len(data) > MaxXattrListResponseSize {
		return response, syscall.E2BIG
	}
	return response, nil
}
