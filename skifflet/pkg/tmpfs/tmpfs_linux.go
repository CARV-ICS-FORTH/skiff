//go:build linux

package tmpfs

import (
	"fmt"
	"syscall"
)

// Mount mounts a new tmpfs at dir, which exists. The process needs the authority to mount
// (in the bubble it is root in the user namespace of the bubble, which has it).
func Mount(dir string, o Options) error {
	var flags uintptr
	if o.Restricted {
		flags = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC
	}
	data := fmt.Sprintf("size=%d,mode=%o", o.Size, uint32(o.Mode.Perm()))
	if err := syscall.Mount("tmpfs", dir, "tmpfs", flags, data); err != nil {
		return fmt.Errorf("mount a tmpfs of %d bytes at %s: %w", o.Size, dir, err)
	}
	return nil
}

// Unmount removes the tmpfs at dir, and with it what it holds. A container that still has
// the directory open does not keep the mount (it ends when the last of them lets go).
func Unmount(dir string) error {
	if err := syscall.Unmount(dir, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount %s: %w", dir, err)
	}
	return nil
}
