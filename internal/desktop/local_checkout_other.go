//go:build !darwin && !linux

package desktop

import (
	"errors"
	"os"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

func checkoutValidateNativeMetadata(path string) error {
	return errors.New("preserving native checkout metadata is unsupported on this platform")
}

func checkoutCloneRegular(source, destination string) (bool, error) { return false, nil }

func checkoutCopySymlinkMode(path string, mode os.FileMode) error {
	return errors.New("preserving symbolic link permissions is unsupported on this platform")
}

func checkoutMountedViewMetadataValidator(mounted fusefs.MountedFS, root string) (func(string) error, error) {
	return checkoutValidateNativeMetadata, nil
}

func checkoutReadXattrs(path string) (map[string][]byte, error) {
	return nil, errors.New("preserving checkout extended metadata is unsupported on this platform")
}

func checkoutWriteXattrs(path string, attrs map[string][]byte) error {
	return errors.New("preserving checkout extended metadata is unsupported on this platform")
}

func publishCheckoutDirectory(parent *os.File, stage, destination string) error {
	return errors.New("atomic no-replace checkout publication is unsupported on this platform")
}
