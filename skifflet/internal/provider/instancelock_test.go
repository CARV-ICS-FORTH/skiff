package provider

import (
	"errors"
	"path/filepath"
	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"testing"
)

// The lock is node-local: in the scratch directory, never in the state directory, which is on
// the shared home and is the same directory on every node (on NFS a lock there is held
// against all of them: the first node's Skifflet would keep every other from starting).
func TestInstanceLockDir_IsTheNodesScratchNotTheSharedState(t *testing.T) {
	home := t.TempDir()
	scratch := filepath.Join(t.TempDir(), "scratch")
	saved := compute.Skiff
	compute.Skiff = endpoint.SkiffWithTmp(home, scratch)
	defer func() { compute.Skiff = saved }()

	if got := InstanceLockDir(); got != scratch {
		t.Fatalf("lock directory = %q, want the scratch directory %q", got, scratch)
	}
	if InstanceLockDir() == compute.Skiff.String() {
		t.Fatalf("the lock is in the state directory %q, which is shared between nodes", compute.Skiff.String())
	}
}

func TestInstanceLock_MutualExclusion(t *testing.T) {
	tmpDir := t.TempDir()

	lock1, err := AcquireInstanceLock(tmpDir)
	if err != nil {
		t.Fatalf("first AcquireInstanceLock failed: %v", err)
	}
	defer lock1.file.Close()

	// Second acquire on the same directory must fail with ErrInstanceLocked
	_, err2 := AcquireInstanceLock(tmpDir)
	if !errors.Is(err2, ErrInstanceLocked) {
		t.Fatalf("expected ErrInstanceLocked on second acquire, got: %v", err2)
	}

	// After release, re-acquire succeeds
	lock1.file.Close()

	lock3, err3 := AcquireInstanceLock(tmpDir)
	if err3 != nil {
		t.Fatalf("AcquireInstanceLock failed after release: %v", err3)
	}
	defer lock3.file.Close()
}
