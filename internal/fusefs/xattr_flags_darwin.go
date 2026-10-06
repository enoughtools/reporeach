//go:build darwin

package fusefs

import "golang.org/x/sys/unix"

// XNU consumes these pathname restrictions while resolving the vnode, then
// forwards its original setter options. x/sys predates the two newer names.
// https://github.com/apple-oss-distributions/xnu/blob/f6217f891ac0bb64f3d375211650a4c1ff8ca1ea/bsd/vfs/vfs_syscalls.c
const (
	xattrNoFollowAny    uint32 = 0x0040
	xattrResolveBeneath uint32 = 0x0080
)

func xattrInodeFlags(flags uint32) uint32 {
	// Kernel MAC callbacks also pass NOSECURITY, and vn_setxattr consumes
	// NODEFAULT after the filesystem callback to control its fallback. These
	// context bits change no authorization, inode validation, write policy, or
	// backing-store behavior here; only create/replace is decoded.
	// https://github.com/apple-oss-distributions/xnu/blob/f6217f891ac0bb64f3d375211650a4c1ff8ca1ea/security/mac_vfs_subr.c
	// https://github.com/apple-oss-distributions/xnu/blob/f6217f891ac0bb64f3d375211650a4c1ff8ca1ea/bsd/vfs/vfs_xattr.c
	return flags &^ (unix.XATTR_NOFOLLOW | xattrNoFollowAny | xattrResolveBeneath | unix.XATTR_NOSECURITY | unix.XATTR_NODEFAULT)
}
