package provider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var (
	ErrInstanceLocked = errors.New("another skifflet instance holds the runtime lock")
)

// InstanceLock represents an advisory lock held on the Skiff runtime directory.
type InstanceLock struct {
	file *os.File
}

// AcquireInstanceLock obtains an exclusive non-blocking lock on <skiffDir>/skifflet.lock.
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

// Release releases the advisory instance lock.
func (l *InstanceLock) Release() {
	if l != nil && l.file != nil {
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		_ = l.file.Close()
	}
}
