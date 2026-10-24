//go:build linux && (amd64 || arm64)

package direct

import (
	"sync"
	"testing"
	"time"
)

// The lock of a thread is exclusive, another thread's is not the same lock, and what is not
// used any more is not kept.
func TestThreadLocks(t *testing.T) {
	var l threadLocks
	a := l.lock(1)
	other := l.lock(2) // another thread: no wait
	l.unlock(2, other)

	entered := make(chan struct{})
	go func() {
		b := l.lock(1)
		close(entered)
		l.unlock(1, b)
	}()
	select {
	case <-entered:
		t.Fatal("the lock of thread 1 was taken twice")
	case <-time.After(100 * time.Millisecond):
	}
	l.unlock(1, a)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the lock of thread 1 was not given to the waiter")
	}
	time.Sleep(50 * time.Millisecond)
	l.mu.Lock()
	kept := len(l.held)
	l.mu.Unlock()
	if kept != 0 {
		t.Errorf("%d locks kept although nobody uses them", kept)
	}

	var wg sync.WaitGroup
	inside := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				k := l.lock(7)
				inside++
				if inside != 1 {
					t.Error("two goroutines hold the lock of thread 7")
				}
				inside--
				l.unlock(7, k)
			}
		}()
	}
	wg.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.held) != 0 {
		t.Errorf("%d locks kept", len(l.held))
	}
}
