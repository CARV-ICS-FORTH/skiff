//go:build linux && (amd64 || arm64)

package direct

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The lock of a thread is exclusive, another thread's is not the same lock, and what is not
// used any more is not kept.
func TestThreadLocks(t *testing.T) {
	var l threadLocks
	a := l.lock(1)
	other := l.lock(2) // another thread: no wait
	other.unlock()

	entered := make(chan struct{})
	go func() {
		b := l.lock(1)
		close(entered)
		b.unlock()
	}()
	select {
	case <-entered:
		t.Fatal("the lock of thread 1 was taken twice")
	case <-time.After(100 * time.Millisecond):
	}
	a.unlock()
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
				k.unlock()
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

func TestTurnMakesAThreadsCallWaitForTheOneItGaveUp(t *testing.T) {
	var l threadLocks
	valid := func() bool { return true }
	first := l.enter(7, valid)
	if other := l.enter(8, valid); other == nil { // another thread does not wait
		t.Fatal("another thread waited")
	} else {
		other.leave()
	}
	var entered atomic.Bool
	got := make(chan *threadLock)
	go func() { turn := l.enter(7, valid); entered.Store(true); got <- turn }()
	time.Sleep(150 * time.Millisecond)
	if entered.Load() {
		t.Fatal("the restarted call did not wait for the one that was given up")
	}
	first.leave()
	(<-got).leave()

	// A call that has been given up meanwhile does not wait for ever.
	first = l.enter(7, valid)
	defer first.leave()
	var live atomic.Bool
	live.Store(true)
	done := make(chan *threadLock, 1)
	go func() { done <- l.enter(7, live.Load) }()
	live.Store(false)
	select {
	case l := <-done:
		if l != nil {
			t.Error("a call that was given up entered")
		}
	case <-time.After(2 * time.Second):
		t.Error("a call that was given up waits")
	}
}
