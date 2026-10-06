//go:build linux

package desktop

import (
	"errors"
	"os"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"golang.org/x/sys/unix"
)

func checkoutValidateNativeMetadata(path string) error { return nil }

func checkoutCloneRegular(source, destination string) (bool, error) { return false, nil }

func checkoutCopySymlinkMode(path string, mode os.FileMode) error {
	if mode.Perm() != 0777 || mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New("the source symlink permissions cannot be preserved on this filesystem")
	}
	return nil // Linux symlink permissions are fixed at 0777.
}

func checkoutMountedViewMetadataValidator(mounted fusefs.MountedFS, root string) (func(string) error, error) {
	return checkoutValidateNativeMetadata, nil
}

func publishCheckoutDirectory(parent *os.File, stage, destination string) error {
	return unix.Renameat2(int(parent.Fd()), stage, int(parent.Fd()), destination, unix.RENAME_NOREPLACE)
}
