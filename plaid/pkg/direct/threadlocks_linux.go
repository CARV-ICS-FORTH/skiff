//go:build linux && (amd64 || arm64)

package direct

import "sync"

// What the supervisor writes into a task's memory (the address a held accept or getsockname
// is to fill in) it writes for the notification it holds, and a notification that was given
// up (a signal interrupted the task, which then restarted the call) is no longer anyone's
// to write for: the buffer's address is the stack's of a call that may have returned and been
// followed by others. The write is checked for the notification (SECCOMP_IOCTL_NOTIF_ID_VALID)
// and made under the task's lock, and the task's call that restarted cannot be answered, and
// so the task cannot go on with its stack, until the write is done: every answer is made
// under the same lock. (A Go server that took 10000 connections of 8 clients under churn
// crashed on a return address that had 28, the length of a sockaddr_in6, in its upper half:
// the length written for an accept that had been restarted.)
type threadLocks struct {
	mu   sync.Mutex
	held map[int]*threadLock
}

type threadLock struct {
	mu    sync.Mutex
	users int
}

// lock takes the lock of thread tid; the lock it returns is given back to unlock.
func (l *threadLocks) lock(tid int) *threadLock {
	l.mu.Lock()
	t := l.held[tid]
	if t == nil {
		if l.held == nil {
			l.held = make(map[int]*threadLock)
		}
		t = &threadLock{}
		l.held[tid] = t
	}
	t.users++
	l.mu.Unlock()
	t.mu.Lock()
	return t
}

func (l *threadLocks) unlock(tid int, t *threadLock) {
	t.mu.Unlock()
	l.mu.Lock()
	if t.users--; t.users == 0 {
		delete(l.held, tid)
	}
	l.mu.Unlock()
}
