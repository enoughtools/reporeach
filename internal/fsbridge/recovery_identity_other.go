//go:build !darwin && !windows

package fsbridge

import (
	"os"

	"golang.org/x/sys/unix"
)

func recoveryInfoBirthTime(os.FileInfo) (int64, int64)     { return 0, 0 }
func recoveryStatBirthTime(*unix.Stat_t) (int64, int64)    { return 0, 0 }
func recoveryDirectoryVolumeUUID(*os.File) (string, error) { return "", nil }
func recoveryFileIdentityValid(identity FileIdentity) bool {
	return identity.Inode != 0 && identity.VolumeUUID == "" && identity.BirthSec == 0 && identity.BirthNSec == 0
}
func recoverySameIdentity(actual, expected FileIdentity) bool { return actual == expected }
