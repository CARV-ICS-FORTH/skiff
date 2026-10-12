package image

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Only another holder makes a pull wait for the lock between the nodes, and not for ever; a
// filesystem that refuses the lock, or a lock file that cannot be made, is a pull without it.
func TestLockBetweenNodes(t *testing.T) {
	old, oldUnlock, oldWait := lockFile, unlockFile, lockWait
	t.Cleanup(func() { lockFile, unlockFile, lockWait = old, oldUnlock, oldWait })
	path := filepath.Join(t.TempDir(), "image.sif.flock")

	unlock := lockBetweenNodes(context.Background(), path)
	if unlock == nil {
		t.Fatal("a lock that works was not taken")
	}
	// Another open file description of the same file is refused it while it is held, as
	// another node's would be.
	other, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockFile(int(other.Fd())); !heldByAnother(err) {
		t.Fatalf("a second holder: %v, want the lock held by another", err)
	}
	unlock()
	if err := lockFile(int(other.Fd())); err != nil {
		t.Fatalf("after the release: %v", err)
	}
	_ = other.Close()

	for _, refused := range []error{syscall.ENOLCK, syscall.EOPNOTSUPP, syscall.ENOSYS} {
		lockFile = func(int) error { return refused }
		start := time.Now()
		if unlock := lockBetweenNodes(context.Background(), path); unlock != nil || time.Since(start) > time.Second {
			t.Fatalf("%v: a refused lock made the pull wait or was taken for held", refused)
		}
	}

	lockFile = func(int) error { return syscall.EAGAIN }
	lockWait = 300 * time.Millisecond
	start := time.Now()
	if unlock := lockBetweenNodes(context.Background(), path); unlock != nil {
		t.Fatal("a lock held by another was taken")
	}
	if waited := time.Since(start); waited < lockWait || waited > lockWait+time.Second {
		t.Fatalf("waited %v for a held lock, want about %v", waited, lockWait)
	}
	lockWait = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if unlock := lockBetweenNodes(ctx, path); unlock != nil || ctx.Err() == nil {
		t.Fatal("the wait for a held lock did not end with the pull's context")
	}

	if unlock := lockBetweenNodes(context.Background(), filepath.Join(t.TempDir(), "missing", "x.flock")); unlock != nil {
		t.Fatal("a lock file that cannot be made gave a lock")
	}
}
