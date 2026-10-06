//go:build darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sort"
	"testing"
	"unicode/utf8"

	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sys/unix"
)

const fsKitXattrPrefix = "com.enoughtools.reporeach.acceptance."

type fsKitXattrTarget struct {
	path     string
	noFollow bool
}

type fsKitXattrOperations struct {
	get    func(string, string, []byte) (int, error)
	set    func(string, string, []byte, int) error
	list   func(string, []byte) (int, error)
	remove func(string, string) error
}

func (target fsKitXattrTarget) operations() fsKitXattrOperations {
	if target.noFollow {
		return fsKitXattrOperations{unix.Lgetxattr, unix.Lsetxattr, unix.Llistxattr, unix.Lremovexattr}
	}
	return fsKitXattrOperations{unix.Getxattr, unix.Setxattr, unix.Listxattr, unix.Removexattr}
}

type fsKitXattrExpectation struct {
	target fsKitXattrTarget
	label  string
	values map[string][]byte
}

func (expectation fsKitXattrExpectation) names() []string {
	names := make([]string, 0, len(expectation.values))
	for name := range expectation.values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Probe the installed Darwin API, including its native flag values and errno
// mapping. Leave binary and empty attributes for later persistence assertions.
func fsKitProbeNativeXattrs(t *testing.T, target fsKitXattrTarget, label string) fsKitXattrExpectation {
	t.Helper()
	t.Logf("native xattr stage: %s", label)
	ops := target.operations()
	binaryName, emptyName, removedName := fsKitXattrPrefix+"binary", fsKitXattrPrefix+"empty", fsKitXattrPrefix+"removed"
	fsKitAssertNativeXattrsMissing(t, target, []string{binaryName, emptyName, removedName})
	if err := ops.set(target.path, binaryName, []byte{1}, unix.XATTR_REPLACE); !errors.Is(err, unix.ENOATTR) {
		t.Fatalf("%s: replace missing xattr error=%v; want ENOATTR", label, err)
	}
	if err := ops.remove(target.path, removedName); !errors.Is(err, unix.ENOATTR) {
		t.Fatalf("%s: remove missing xattr error=%v; want ENOATTR", label, err)
	}
	initial := []byte{0, 255, 128, '\n', 0, 254}
	if err := ops.set(target.path, binaryName, initial, unix.XATTR_CREATE); err != nil {
		t.Fatalf("%s: create binary xattr: %v", label, err)
	}
	fsKitAssertNativeXattrValue(t, target, binaryName, initial, label)
	if err := ops.set(target.path, binaryName, []byte{9}, unix.XATTR_CREATE); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("%s: create existing xattr error=%v; want EEXIST", label, err)
	}
	fsKitAssertNativeXattrValue(t, target, binaryName, initial, label)
	binary := append([]byte{128, 0, 255, '\r', '\n', 0}, []byte(label)...)
	if err := ops.set(target.path, binaryName, []byte{}, unix.XATTR_REPLACE); err != nil {
		t.Fatalf("%s: replace binary xattr with empty bytes: %v", label, err)
	}
	fsKitAssertNativeXattrValue(t, target, binaryName, []byte{}, label)
	if err := ops.set(target.path, binaryName, initial, 0); err != nil {
		t.Fatalf("%s: overwrite existing xattr with default policy: %v", label, err)
	}
	fsKitAssertNativeXattrValue(t, target, binaryName, initial, label)
	if err := ops.set(target.path, binaryName, binary, unix.XATTR_REPLACE); err != nil {
		t.Fatalf("%s: replace binary xattr: %v", label, err)
	}
	fsKitAssertNativeXattrValue(t, target, binaryName, binary, label)
	if _, err := ops.get(target.path, binaryName, make([]byte, 1)); !errors.Is(err, unix.ERANGE) {
		t.Fatalf("%s: short xattr read error=%v; want ERANGE", label, err)
	}
	if err := ops.set(target.path, emptyName, []byte{}, unix.XATTR_CREATE); err != nil {
		t.Fatalf("%s: create empty xattr: %v", label, err)
	}
	fsKitAssertNativeXattrValue(t, target, emptyName, []byte{}, label)
	fsKitAssertNativeXattrNames(t, target, []string{binaryName, emptyName}, nil, label)
	if _, err := ops.list(target.path, make([]byte, 1)); !errors.Is(err, unix.ERANGE) {
		t.Fatalf("%s: short xattr list error=%v; want ERANGE", label, err)
	}
	if err := ops.remove(target.path, emptyName); err != nil {
		t.Fatalf("%s: remove empty xattr: %v", label, err)
	}
	fsKitAssertNativeXattrsMissing(t, target, []string{emptyName})
	if err := ops.remove(target.path, emptyName); !errors.Is(err, unix.ENOATTR) {
		t.Fatalf("%s: remove previously removed empty xattr error=%v; want ENOATTR", label, err)
	}
	if err := ops.set(target.path, emptyName, []byte{}, 0); err != nil {
		t.Fatalf("%s: set empty xattr with default policy: %v", label, err)
	}
	if err := ops.set(target.path, removedName, initial, unix.XATTR_CREATE); err != nil {
		t.Fatalf("%s: create removal probe: %v", label, err)
	}
	if err := ops.remove(target.path, removedName); err != nil {
		t.Fatalf("%s: remove binary xattr: %v", label, err)
	}
	fsKitAssertNativeXattrsMissing(t, target, []string{removedName})
	fsKitAssertNativeXattrNames(t, target, []string{binaryName, emptyName}, []string{removedName}, label)
	expectation := fsKitXattrExpectation{target: target, label: label, values: map[string][]byte{binaryName: binary, emptyName: {}}}
	fsKitAssertNativeXattrs(t, "initial probe", []fsKitXattrExpectation{expectation})
	return expectation
}

func fsKitAssertNativeXattrs(t *testing.T, stage string, expectations []fsKitXattrExpectation) {
	t.Helper()
	for _, expectation := range expectations {
		for _, name := range expectation.names() {
			fsKitAssertNativeXattrValue(t, expectation.target, name, expectation.values[name], stage+": "+expectation.label)
		}
		fsKitAssertNativeXattrNames(t, expectation.target, expectation.names(), []string{fsKitXattrPrefix + "removed"}, stage+": "+expectation.label)
	}
}

func fsKitAssertNativeXattrValue(t *testing.T, target fsKitXattrTarget, name string, want []byte, label string) {
	t.Helper()
	get := target.operations().get
	got, err := fsKitReadXattrBytes(func(dst []byte) (int, error) { return get(target.path, name, dst) }, model.MaxXattrValueBytes)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s: native xattr %s did not retain exact bytes (got size=%d want=%d error=%v)", label, name, len(got), len(want), err)
	}
}

func fsKitAssertNativeXattrsMissing(t *testing.T, target fsKitXattrTarget, names []string) {
	t.Helper()
	get := target.operations().get
	for _, name := range names {
		if _, err := get(target.path, name, nil); !errors.Is(err, unix.ENOATTR) {
			t.Fatalf("native xattr %s on %s: missing error=%v; want ENOATTR (no-follow=%v)", name, target.path, err, target.noFollow)
		}
	}
}

func fsKitNativeXattrNames(t *testing.T, target fsKitXattrTarget) []string {
	t.Helper()
	list := target.operations().list
	data, err := fsKitReadXattrBytes(func(dst []byte) (int, error) { return list(target.path, dst) }, model.MaxXattrObjectBytes)
	if err != nil {
		t.Fatalf("native xattr list (no-follow=%v): %v", target.noFollow, err)
	}
	names, err := fsKitDecodeXattrNames(data)
	if err != nil {
		t.Fatalf("native xattr list encoding: %v", err)
	}
	return names
}

func fsKitAssertNativeXattrNames(t *testing.T, target fsKitXattrTarget, present, absent []string, label string) {
	t.Helper()
	listed := make(map[string]bool)
	for _, name := range fsKitNativeXattrNames(t, target) {
		listed[name] = true
	}
	for _, name := range present {
		if !listed[name] {
			t.Fatalf("%s: native xattr list omitted %s", label, name)
		}
	}
	for _, name := range absent {
		if listed[name] {
			t.Fatalf("%s: native xattr list retained removed %s", label, name)
		}
	}
}

func fsKitSnapshotNativeXattrs(t *testing.T, target fsKitXattrTarget) map[string][]byte {
	t.Helper()
	get := target.operations().get
	values := make(map[string][]byte)
	for _, name := range fsKitNativeXattrNames(t, target) {
		value, err := fsKitReadXattrBytes(func(dst []byte) (int, error) { return get(target.path, name, dst) }, model.MaxXattrValueBytes)
		if err != nil {
			t.Fatalf("snapshot fixture GitDir attributes: %v", err)
		}
		values[name] = value
	}
	return values
}

// A NULL buffer is Darwin's size query. Keep a successful zero-size value
// distinct from ENOATTR, and reject a changed size rather than truncate bytes.
func fsKitReadXattrBytes(read func([]byte) (int, error), limit int) ([]byte, error) {
	size, err := read(nil)
	if err != nil {
		return nil, err
	}
	if size < 0 || size > limit {
		return nil, errors.New("native xattr size exceeds acceptance bound")
	}
	if size == 0 {
		return []byte{}, nil
	}
	data := make([]byte, size)
	count, err := read(data)
	if err != nil {
		return nil, err
	}
	if count != size {
		return nil, fmt.Errorf("native xattr size changed: query=%d read=%d", size, count)
	}
	return data, nil
}

func fsKitDecodeXattrNames(data []byte) ([]string, error) {
	names := []string{}
	seen := make(map[string]bool)
	for len(data) != 0 {
		end := bytes.IndexByte(data, 0)
		if end <= 0 || !utf8.Valid(data[:end]) || end > model.MaxXattrNameBytes {
			return nil, errors.New("malformed native xattr name list")
		}
		name := string(data[:end])
		if seen[name] {
			return nil, errors.New("duplicate native xattr name")
		}
		seen[name] = true
		names = append(names, name)
		data = data[end+1:]
	}
	return names, nil
}

func TestFSKitXattrRead(t *testing.T) {
	for _, test := range []struct {
		name       string
		size, read int
		value      []byte
		queryErr   error
		readErr    error
		bad        bool
	}{
		{name: "binary bytes", size: 6, read: 6, value: []byte{0, 255, 128, 10, 0, 254}},
		{name: "empty exists"},
		{name: "missing differs from empty", queryErr: unix.ENOATTR, bad: true},
		{name: "negative query", size: -1, bad: true},
		{name: "bounded query", size: 9, bad: true},
		{name: "short result rejected", size: 6, read: 5, bad: true},
		{name: "oversized result rejected", size: 6, read: 7, bad: true},
		{name: "range failure retained", size: 6, readErr: unix.ERANGE, bad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			value, err := fsKitReadXattrBytes(func(dst []byte) (int, error) {
				calls++
				if calls == 1 {
					if dst != nil {
						t.Fatal("size query was not NULL")
					}
					return test.size, test.queryErr
				}
				if len(dst) != test.size {
					t.Fatal("read buffer differs from bounded size")
				}
				copy(dst, test.value)
				return test.read, test.readErr
			}, 8)
			if (err != nil) != test.bad || (!test.bad && !bytes.Equal(value, test.value)) {
				t.Fatalf("read result size=%d error=%v", len(value), err)
			}
			if test.queryErr != nil && !errors.Is(err, test.queryErr) || test.readErr != nil && !errors.Is(err, test.readErr) {
				t.Fatal("native error mapping was discarded")
			}
			wantCalls := 1
			if test.queryErr == nil && test.size > 0 && test.size <= 8 {
				wantCalls = 2
			}
			if calls != wantCalls || !test.bad && value == nil {
				t.Fatalf("empty/malformed result performed extra reads or became missing: calls=%d", calls)
			}
		})
	}
}

func TestFSKitXattrNames(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
		want []string
		bad  bool
	}{
		{name: "empty", want: []string{}},
		{name: "NUL separated arbitrary names", data: []byte("meta/path+?&=é#\n\x00user.binary\x00"), want: []string{"meta/path+?&=é#\n", "user.binary"}},
		{name: "missing terminator", data: []byte("user.binary"), bad: true},
		{name: "empty name", data: []byte{0}, bad: true},
		{name: "duplicate", data: []byte("binary\x00binary\x00"), bad: true},
		{name: "invalid UTF8", data: []byte{255, 0}, bad: true},
		{name: "name length limit", data: append(bytes.Repeat([]byte{'x'}, model.MaxXattrNameBytes+1), 0), bad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := fsKitDecodeXattrNames(test.data)
			if (err != nil) != test.bad || !test.bad && !slices.Equal(got, test.want) {
				t.Fatalf("decoded names=%q error=%v", got, err)
			}
		})
	}
}
