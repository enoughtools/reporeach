//go:build darwin

package fsbridge

import (
	"errors"
	"syscall"
)

// jacobsa/fuse calls ENODATA ENOATTR on every platform. FSKit requires the
// actual Darwin missing-attribute errno instead of Darwin ENODATA.
func isMissingXattr(err error) bool {
	return errors.Is(err, syscall.ENOATTR) || errors.Is(err, syscall.ENODATA)
}

func missingXattrErrno() syscall.Errno { return syscall.ENOATTR }
