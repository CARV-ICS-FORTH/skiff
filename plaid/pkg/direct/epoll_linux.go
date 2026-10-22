//go:build linux && (amd64 || arm64)

package direct

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The container's epoll registrations of a socket that is swapped (a runtime that adds the
// socket to its epoll set before it calls connect or bind: Boost.Asio, Netty's epoll
// transport, nginx's connections to an upstream).
//
// An instance keys an entry by the file and the descriptor number in the table of the caller
// of epoll_ctl, and the container will modify and remove it (MOD, DEL) with its own number.
// So the new socket's entry must be made from a table in which that number is the new file:
// moveEntries does it on a thread of the supervisor with a descriptor table of its own
// (unshare(CLONE_FILES), taking the instances from the container with pidfd_getfd), where it
// puts the new file on the number, adds it to each instance with the old entry's events (with
// EPOLLET, EPOLLEXCLUSIVE, EPOLLWAKEUP) and data, puts the old file on the number and removes
// its entry. The thread ends with its table (so, as for the filter's thread, the program must
// keep its main goroutine on the main thread, which does not end: see Start).
//
// The order of a swap that has entries, for connect and bind, is
//
//  1. the new entries are added, in every instance;
//  2. the old entries are removed;
//  3. the new socket is put under the container's number (ADDFD with SETFD), the call answered.
//
// If an add fails the added ones are removed and nothing has changed; if the put fails (the
// call was cancelled, the container is gone) the move is made the other way (undo), so that a
// call that is restarted finds what it left. What this order is for:
//
//   - The move comes before the put: after it the container's own epoll_ctl on its number
//     finds an entry, and from then on the number is the new file. The other way round there
//     is a time in which the container's thread, back from its call, modifies an entry that is
//     not there (ENOENT), and one in which the instance reports the old socket under the
//     container's data: an event is made from the file's state when it is delivered, and a
//     socket that was never connected is EPOLLOUT|EPOLLHUP for ever.
//   - The old entry is removed before the put, and not left to go with the old file: the
//     supervisor holds the old file for as long as the call lasts (and for ever, as the
//     pod-side socket, for a listener), and the entry only goes with the last reference to it.
//   - The new entry is added before the old one is removed: the container's socket is never
//     without an entry, whichever thread of the container is in epoll_wait.
//
// What remains is the time between 1 and 3, tens of microseconds, in which the instance can
// report the new socket (connected: writable) under the number that is still the old one in
// the container's table. It is seen by a thread of the container that waits for the set while
// another one is inside connect or bind, which would have to act on a socket it has no
// operation under way for; the runtimes tried (Asio, Netty, nginx) ignore such an event.
// An entry of a one-shot registration that has reported has no events left, and epoll_ctl
// cannot make one (epollEntry.armed): such a call is left to the pod network.

// moveEntries registers the file `to` in the instances of the entries and removes `from`
// from them, both under descriptor fd of the container's thread tid (from and to are
// descriptors of the supervisor).
func (ts *targets) moveEntries(tid, fd, from, to int, entries []epollEntry) error {
	t, err := ts.lookup(tid, false)
	if err != nil {
		return err
	}
	return ts.mover.do(moveJob{tgid: t.tgid, fd: fd, from: from, to: to, entries: entries})
}

// mover is the supervisor's thread with a table of its own. It is made when the first move
// is asked for, and works one move at a time.
type mover struct {
	once sync.Once
	jobs chan moveJob
	stop sync.Once
}

type moveJob struct {
	tgid, fd, from, to int
	entries            []epollEntry
	done               chan error
}

func (m *mover) do(job moveJob) error {
	m.once.Do(func() {
		m.jobs = make(chan moveJob)
		go m.run()
	})
	job.done = make(chan error, 1)
	select {
	case m.jobs <- job:
		return <-job.done
	case <-time.After(5 * time.Second):
		return errors.New("the thread that moves epoll registrations does not answer")
	}
}

// close ends the thread (if it was made).
func (m *mover) close() {
	m.stop.Do(func() {
		m.once.Do(func() {}) // nothing was made: nothing will be
		if m.jobs != nil {
			close(m.jobs)
		}
	})
}

// run is the thread: its table is its own (the program's other threads keep theirs), emptied
// but for a pidfd of the supervisor, which is how it gets the files that the supervisor has.
// The thread is not unlocked: it is not a thread for anything else, and it ends with the
// program (or with close).
func (m *mover) run() {
	runtime.LockOSThread()
	fail := func(err error) {
		for job := range m.jobs {
			job.done <- err
		}
	}
	if err := unix.Unshare(unix.CLONE_FILES); err != nil {
		fail(fmt.Errorf("unshare: %w", err))
		return
	}
	closeFrom(0)
	self, err := unix.PidfdOpen(unix.Getpid(), 0)
	if err != nil {
		fail(fmt.Errorf("pidfd_open: %w", err))
		return
	}
	for job := range m.jobs {
		job.done <- moveInTable(self, job)
		closeFrom(self + 1)
	}
}

// closeFrom closes the thread's descriptors from n up.
func closeFrom(n int) {
	if err := unix.CloseRange(uint(n), ^uint(0), 0); err != nil {
		for fd := n; fd < 1<<16; fd++ {
			if unix.Close(fd) == unix.EBADF && fd > n+64 {
				break
			}
		}
	}
}

// moveInTable does the move with the thread's descriptors. The files are taken from the
// supervisor (pidfd_getfd of its own pidfd) and from the container; none may be on the
// number fd, which is where the old file and then the new one are put, one at a time.
func moveInTable(self int, job moveJob) error {
	fd := job.fd
	if fd <= self {
		return fmt.Errorf("descriptor %d is the thread's own", fd)
	}
	elsewhere := func(got int, err error) (int, error) { // a descriptor that is not fd
		if err != nil || got != fd {
			return got, err
		}
		moved, err := unix.FcntlInt(uintptr(got), unix.F_DUPFD_CLOEXEC, fd+1)
		unix.Close(got)
		return moved, err
	}
	oldFile, err := elsewhere(unix.PidfdGetfd(self, job.from, 0))
	if err != nil {
		return fmt.Errorf("pidfd_getfd of the old socket: %w", err)
	}
	newFile, err := elsewhere(unix.PidfdGetfd(self, job.to, 0))
	if err != nil {
		return fmt.Errorf("pidfd_getfd of the new socket: %w", err)
	}
	target, err := unix.PidfdOpen(job.tgid, 0)
	if err != nil {
		return fmt.Errorf("pidfd_open(%d): %w", job.tgid, err)
	}
	instances := make(map[int]int) // the container's number of an instance, ours
	for _, e := range job.entries {
		if _, ok := instances[e.epfd]; ok {
			continue
		}
		if instances[e.epfd], err = elsewhere(unix.PidfdGetfd(target, e.epfd, 0)); err != nil {
			return fmt.Errorf("pidfd_getfd(%d): %w", e.epfd, err)
		}
	}
	// Only the new file's entries are added until all are: a failure then removes them.
	if err := unix.Dup3(newFile, fd, unix.O_CLOEXEC); err != nil {
		return err
	}
	var added []epollEntry
	for _, e := range job.entries {
		ev := unix.EpollEvent{Events: e.events, Fd: int32(e.data), Pad: int32(e.data >> 32)}
		switch err := unix.EpollCtl(instances[e.epfd], unix.EPOLL_CTL_ADD, fd, &ev); err {
		case nil:
			added = append(added, e)
		case unix.EEXIST: // the same instance, under another number of the container
		default:
			for _, a := range added {
				_ = unix.EpollCtl(instances[a.epfd], unix.EPOLL_CTL_DEL, fd, nil)
			}
			return fmt.Errorf("epoll_ctl(ADD) in instance %d: %w", e.epfd, err)
		}
	}
	if err := unix.Dup3(oldFile, fd, unix.O_CLOEXEC); err != nil {
		return err
	}
	for _, e := range job.entries {
		if err := unix.EpollCtl(instances[e.epfd], unix.EPOLL_CTL_DEL, fd, nil); err != nil && err != unix.ENOENT {
			return fmt.Errorf("epoll_ctl(DEL) in instance %d: %w", e.epfd, err)
		}
	}
	return nil
}
