//go:build !darwin && !windows

package fusefs

func xattrInodeFlags(flags uint32) uint32 { return flags }
