//go:build linux && (amd64 || arm64)

package direct

import (
	"sync"
	"time"
)

// A call that the container gives up (a signal interrupts its thread: Go's runtime sends
// SIGURG to preempt a goroutine, and the kernel restarts the call as a new one) is found out
// only when the supervisor answers it, and by then it may have done what it could not undo: an
// accept has taken a connection and told the client it is accepted, a connect has made the
// connection and has the server's ack. What it made is kept for the call that is made again.
//
// A thread has one call at a time, and the supervisor keeps two things in order for it, in
// the record it has of the thread (threadLock): the answers, and the calls that hold something.
//
// What the supervisor writes into a task's memory (the address a held accept or getsockname
// is to fill in) it writes for the notification it holds. A notification that was given up
// is no longer anyone's to write for: the buffer's address is that of a stack that may have
// moved on. The write is checked for the notification (SECCOMP_IOCTL_NOTIF_ID_VALID) and
// made under the thread's answer lock, and every answer is made under the same lock, so that
// the call that restarted cannot be answered, and the task cannot go on with its stack, until
// the write is done.
//
// A call that holds something (a connection taken from a listener, a connection being made)
// has the thread's turn. A call that finds the turn taken has found an earlier call of its
// thread that was given up and is still deciding, and waits for it to put what it holds where
// the restarted call can take it. The answer lock and the turn are separate: an accept gives
// up its turn while it waits for a connection again, and takes the answer lock only at the end.
type threadLocks struct {
	mu    sync.Mutex
	held  map[int]*threadLock // the threads with a call in hand, by thread
	spare sync.Pool           // records of threads that have none, for the next
}

// threadLock is the supervisor's record of one thread, while a call of it is in hand.
type threadLock struct {
	owner  *threadLocks
	tid    int
	users  int           // the calls that have the record; guarded by owner.mu
	answer sync.Mutex    // see lock
	turn   chan struct{} // has a token while a call holds the thread's turn (see enter)
}

// turnPoll is how often a call that waits for its thread's turn looks whether it has been
// given up itself.
const turnPoll = 100 * time.Millisecond

// get gives the record of thread tid to one more call.
func (l *threadLocks) get(tid int) *threadLock {
	l.mu.Lock()
	t := l.held[tid]
	if t == nil {
		if t, _ = l.spare.Get().(*threadLock); t == nil {
			t = &threadLock{owner: l, turn: make(chan struct{}, 1)}
		}
		t.tid = tid
		if l.held == nil {
			l.held = make(map[int]*threadLock)
		}
		l.held[tid] = t
	}
	t.users++
	l.mu.Unlock()
	return t
}

// put takes the record from a call, and from the table when no call has it.
func (t *threadLock) put() {
	l := t.owner
	l.mu.Lock()
	t.users--
	if t.users > 0 {
		l.mu.Unlock()
		return
	}
	delete(l.held, t.tid)
	l.mu.Unlock()
	l.spare.Put(t)
}

// lock takes the answer lock of thread tid, to write to its memory and answer under it. The
// record it returns is unlocked with its unlock.
func (l *threadLocks) lock(tid int) *threadLock {
	t := l.get(tid)
	t.answer.Lock()
	return t
}

func (t *threadLock) unlock() {
	t.answer.Unlock()
	t.put()
}

// enter waits for the earlier call of thread tid that holds something, and takes the thread's
// turn. It returns nil if the call has been given up meanwhile (valid says it has not). The
// turn is given back with leave.
func (l *threadLocks) enter(tid int, valid func() bool) *threadLock {
	t := l.get(tid)
	select {
	case t.turn <- struct{}{}:
		return t
	default:
	}
	for {
		select {
		case t.turn <- struct{}{}:
			return t
		case <-time.After(turnPoll):
			if !valid() {
				t.put()
				return nil
			}
		}
	}
}

func (t *threadLock) leave() {
	<-t.turn
	t.put()
}
