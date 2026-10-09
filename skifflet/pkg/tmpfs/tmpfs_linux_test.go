//go:build linux

package tmpfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestMountLimitsTheSizeAndUnmountRemovesIt(t *testing.T) {
	dir := t.TempDir()
	if err := Mount(dir, Options{Size: 1 << 20, Mode: 0o777}); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("mounting needs authority this test does not have")
		}
		t.Fatal(err)
	}
	mounted := true
	defer func() {
		if mounted {
			_ = Unmount(dir)
		}
	}()

	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	if got := int64(st.Blocks) * int64(st.Bsize); got != 1<<20 {
		t.Errorf("the tmpfs holds %d bytes; want %d", got, 1<<20)
	}
	if err := os.WriteFile(filepath.Join(dir, "big"), make([]byte, 2<<20), 0o644); err == nil {
		t.Error("a file larger than the tmpfs was written")
	}
	_ = os.Remove(filepath.Join(dir, "big")) // what fitted of it filled the tmpfs
	if err := os.WriteFile(filepath.Join(dir, "small"), make([]byte, 100<<10), 0o644); err != nil {
		t.Errorf("a small file: %v", err)
	}

	if err := Unmount(dir); err != nil {
		t.Fatal(err)
	}
	mounted = false
	if _, err := os.Stat(filepath.Join(dir, "small")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file is still there after the unmount: %v", err)
	}
}

func TestRestrictedMountsRefuseExecutablesAndDevices(t *testing.T) {
	dir := t.TempDir()
	if err := Mount(dir, Options{Size: 1 << 20, Mode: 0o1777, Restricted: true}); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("mounting needs authority this test does not have")
		}
		t.Fatal(err)
	}
	defer func() { _ = Unmount(dir) }()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	for name, flag := range map[string]int64{"nosuid": syscall.MS_NOSUID, "nodev": syscall.MS_NODEV, "noexec": syscall.MS_NOEXEC} {
		if int64(st.Flags)&flag == 0 {
			t.Errorf("the mount is not %s", name)
		}
	}
}
