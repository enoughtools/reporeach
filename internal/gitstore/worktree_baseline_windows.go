//go:build windows

package gitstore

import "os"

// Persistent catalogue workingtrees currently require the Unix host adapters.
// Fail closed where mutable metadata ownership cannot be established.
func workingTreeBaselinePrivateFile(_ os.FileInfo) bool { return false }
