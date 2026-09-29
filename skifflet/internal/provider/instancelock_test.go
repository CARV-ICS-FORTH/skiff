package provider

import (
	"errors"
	"testing"
)

func TestInstanceLock_MutualExclusion(t *testing.T) {
	tmpDir := t.TempDir()

	lock1, err := AcquireInstanceLock(tmpDir)
	if err != nil {
		t.Fatalf("first AcquireInstanceLock failed: %v", err)
	}
	defer lock1.Release()

	// Second acquire on the same directory must fail with ErrInstanceLocked
	_, err2 := AcquireInstanceLock(tmpDir)
	if !errors.Is(err2, ErrInstanceLocked) {
		t.Fatalf("expected ErrInstanceLocked on second acquire, got: %v", err2)
	}

	// After release, re-acquire succeeds
	lock1.Release()

	lock3, err3 := AcquireInstanceLock(tmpDir)
	if err3 != nil {
		t.Fatalf("AcquireInstanceLock failed after release: %v", err3)
	}
	defer lock3.Release()
}
