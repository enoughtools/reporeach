//go:build darwin

package desktop

import (
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const nativeMountFSIDLimit = 4096

// nativeMountFSIDs reads the kernel's retained mount list without entering any
// filesystem. Getfsstat can omit a mount after mount_iterdrain, while its sync
// and unmount callbacks are still running. The read-only vfsidlist sysctl walks
// mountlist directly; XNU removes that entry only after VFS_UNMOUNT succeeds.
// A missing or malformed response is an error, never evidence of detachment.
func nativeMountFSIDs() ([][2]int32, error) {
	return readNativeMountFSIDs(func() ([]byte, error) {
		return unix.SysctlRaw("vfs.generic.vfsidlist")
	})
}

func readNativeMountFSIDs(read func() ([]byte, error)) ([][2]int32, error) {
	for attempt := 0; attempt < 3; attempt++ {
		data, err := read()
		if err != nil {
			// A mount added between SysctlRaw's size and data queries can make
			// its buffer too small. Do not retry permission or unsupported-node
			// errors, and never accept partial bytes from a failed query.
			if attempt < 2 && (errors.Is(err, unix.ENOMEM) || errors.Is(err, unix.EAGAIN)) {
				continue
			}
			return nil, fmt.Errorf("read retained mount identities: %w", err)
		}
		const fsidSize = 8 // Darwin fsid_t contains two native-endian int32 values.
		if len(data) == 0 || len(data)%fsidSize != 0 || len(data)/fsidSize > nativeMountFSIDLimit {
			return nil, fmt.Errorf("invalid retained mount identity response size: %d", len(data))
		}
		fsids := make([][2]int32, len(data)/fsidSize)
		for i := range fsids {
			offset := i * fsidSize
			fsids[i] = [2]int32{
				int32(binary.NativeEndian.Uint32(data[offset : offset+4])),
				int32(binary.NativeEndian.Uint32(data[offset+4 : offset+8])),
			}
		}
		return fsids, nil
	}
	return nil, errors.New("retained mount identity query retries exhausted")
}
