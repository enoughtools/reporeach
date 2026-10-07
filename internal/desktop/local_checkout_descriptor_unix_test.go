//go:build darwin || linux

package desktop

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenCheckoutRegularDescriptorCannotBeInheritedByGit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binary.dat")
	writeCheckoutFixture(t, path, []byte{0, 255, 128, 0, 1}, 0o600)
	file, err := openCheckoutRegular(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("export descriptor can be inherited by a concurrently started Git child")
	}
}
