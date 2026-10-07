//go:build darwin

package desktop

import (
	"encoding/binary"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func encodeNativeMountFSIDs(fsids ...[2]int32) []byte {
	data := make([]byte, len(fsids)*8)
	for i, fsid := range fsids {
		binary.NativeEndian.PutUint32(data[i*8:i*8+4], uint32(fsid[0]))
		binary.NativeEndian.PutUint32(data[i*8+4:i*8+8], uint32(fsid[1]))
	}
	return data
}

func TestNativeMountFSIDsDecodeRetainedIdentities(t *testing.T) {
	want := [][2]int32{{805306662, 24}, {-2147483648, -1}, {1, 26}}
	got, err := readNativeMountFSIDs(func() ([]byte, error) { return encodeNativeMountFSIDs(want...), nil })
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
}

func TestNativeMountFSIDsRejectIncompleteOrOversizedResponses(t *testing.T) {
	for _, size := range []int{0, 1, 7, 9, (nativeMountFSIDLimit + 1) * 8} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			got, err := readNativeMountFSIDs(func() ([]byte, error) { return make([]byte, size), nil })
			if err == nil || got != nil {
				t.Fatalf("invalid size %d produced %v, %v", size, got, err)
			}
		})
	}
}

func TestNativeMountFSIDsRetryMountListGrowth(t *testing.T) {
	calls := 0
	want := [][2]int32{{805306662, 24}}
	got, err := readNativeMountFSIDs(func() ([]byte, error) {
		calls++
		switch calls {
		case 1:
			return encodeNativeMountFSIDs([2]int32{1, 26}), unix.ENOMEM
		case 2:
			return nil, unix.EAGAIN
		default:
			return encodeNativeMountFSIDs(want...), nil
		}
	})
	if err != nil || calls != 3 || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v after %d calls", got, err, calls)
	}
}

func TestNativeMountFSIDsFailClosedAfterBoundedRetries(t *testing.T) {
	calls := 0
	got, err := readNativeMountFSIDs(func() ([]byte, error) {
		calls++
		return encodeNativeMountFSIDs([2]int32{1, 26}), unix.ENOMEM
	})
	if got != nil || !errors.Is(err, unix.ENOMEM) || calls != 3 {
		t.Fatalf("got %v, %v after %d calls", got, err, calls)
	}
}

func TestNativeMountFSIDsDoNotRetryUnsupportedOrDeniedQueries(t *testing.T) {
	for _, queryErr := range []error{unix.ENOENT, unix.EPERM, unix.EIO} {
		t.Run(queryErr.Error(), func(t *testing.T) {
			calls := 0
			got, err := readNativeMountFSIDs(func() ([]byte, error) {
				calls++
				return encodeNativeMountFSIDs([2]int32{1, 26}), queryErr
			})
			if got != nil || !errors.Is(err, queryErr) || calls != 1 {
				t.Fatalf("got %v, %v after %d calls", got, err, calls)
			}
		})
	}
}
