package provider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"skifflet/internal/compute"
	"syscall"
)

var (
	ErrInstanceLocked = errors.New("another skifflet instance holds the runtime lock")
)

// InstanceLockDir is where the lock of this node's runtime lives: the node's scratch directory,
// which also holds the pods' runtime state (.pods) the lock protects. Not the state directory:
// that is on the shared home, where one lock would be shared by the Skifflets of every node (on
// NFS flock is network-wide) and the second node to start would be refused.
func InstanceLockDir() string {
	return compute.Skiff.TmpDir()
}

// InstanceLock represents an advisory lock held on the Skiff runtime directory, for as long as
// Skifflet runs (the kernel gives it back when the process ends).
type InstanceLock struct {
	file *os.File
}

// AcquireInstanceLock obtains an exclusive non-blocking lock on <skiffDir>/skifflet.lock
// (see InstanceLockDir for which directory).
func AcquireInstanceLock(skiffDir string) (*InstanceLock, error) {
	if err := os.MkdirAll(skiffDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create runtime directory for lock: %w", err)
	}

	lockPath := filepath.Join(skiffDir, "skifflet.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open instance lock file '%s': %w", lockPath, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w at '%s': %v", ErrInstanceLocked, lockPath, err)
	}

	return &InstanceLock{file: f}, nil
}
