//go:build !linux

package tmpfs

import "errors"

// Mount is not supported here: Skifflet runs on Linux.
func Mount(dir string, o Options) error {
	return errors.New("tmpfs mounts are supported on Linux only")
}

// Unmount is not supported here.
func Unmount(dir string) error { return errors.New("tmpfs mounts are supported on Linux only") }
