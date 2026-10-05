//go:build !windows

package gitstore

import (
	"os"
	"syscall"
)

func workingTreeBaselinePrivateFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
