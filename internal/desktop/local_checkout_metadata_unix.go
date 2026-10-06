//go:build darwin || linux

package desktop

import (
	"bytes"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Extended attributes include macOS resource forks and Finder metadata. The
// link variants preserve symlink attributes without reading their targets.
func checkoutReadXattrs(path string) (map[string][]byte, error) {
	size, err := unix.Llistxattr(path, nil)
	if errors.Is(err, unix.ENOTSUP) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	if size > 1<<20 {
		return nil, errors.New("checkout extended attribute names exceed the supported limit")
	}
	names := make([]byte, size)
	size, err = unix.Llistxattr(path, names)
	if err != nil {
		return nil, err
	}
	attrs := make(map[string][]byte)
	for _, name := range bytes.Split(names[:size], []byte{0}) {
		if len(name) == 0 {
			continue
		}
		key := string(name) // Attribute names are metadata, never blob bytes.
		size, err := unix.Lgetxattr(path, key, nil)
		if err != nil {
			return nil, err
		}
		if size > 64<<20 {
			return nil, errors.New("checkout extended attribute exceeds the supported limit")
		}
		value := make([]byte, size)
		read, err := unix.Lgetxattr(path, key, value)
		if err != nil || read != size {
			return nil, errors.New("checkout extended attributes changed while being copied")
		}
		attrs[key] = value
	}
	return attrs, nil
}

func checkoutWriteXattrs(path string, attrs map[string][]byte) error {
	existing, err := checkoutReadXattrs(path)
	if err != nil {
		return err
	}
	for key, value := range attrs {
		// macOS may assign a protected provenance attribute automatically to
		// newly created files. An already identical value needs no privileged
		// rewrite; a different uncopyable value still fails closed.
		if current, exists := existing[key]; exists && bytes.Equal(current, value) {
			continue
		}
		if err := unix.Lsetxattr(path, key, value, 0); err != nil {
			return fmt.Errorf("cannot preserve checkout extended attribute: %w", err)
		}
	}
	return nil
}
