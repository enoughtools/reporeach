//go:build !windows && !darwin

package fsbridge

import (
	"errors"
	"syscall"
)

func isMissingXattr(err error) bool { return errors.Is(err, syscall.ENODATA) }

func missingXattrErrno() syscall.Errno { return syscall.ENODATA }
