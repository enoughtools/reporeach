//go:build !darwin && !windows

package desktop

import (
	"context"
	"os"
	"runtime"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

func (s *Service) platformMountCatalogue(ctx context.Context, root string, fs *catalogfs.FileSystem) (fusefs.MountedFS, error) {
	return catalogfs.Mount(ctx, root, fs)
}

func platformDependencyReady() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat("/dev/fuse")
	return err == nil
}

func platformDependencyMessage() string {
	if runtime.GOOS == "linux" {
		return "FUSE must be available to mount the repository folder"
	}
	return "Mounting the repository folder is not supported on this system"
}
