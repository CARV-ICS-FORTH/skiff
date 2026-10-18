//go:build linux && (amd64 || arm64)

package direct

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// What the supervisor does to a held task's memory and descriptors needs the right to
// trace it. Yama (ptrace_scope 1, the nodes' setting) gives it to an ancestor, which the
// supervisor is: it started the container's launcher.

// targets keeps, for the threads that were held, the process they belong to and a pidfd of
// it. A held call names the thread (the pid of the notification is a thread id; there is
// no PIDFD_THREAD before Linux 6.9), pidfd_open wants the process, and finding it out
// takes a read of /proc/<tid>/status, the largest part of a lookup. A kept entry is good
// while /proc/<tgid>/task/<tid> exists: a thread id belongs to one process at a time. A
// pidfd of a process that ended fails with ESRCH, and then the entry is dropped.
type targets struct {
	mu      sync.Mutex
	known   map[int]target
	maxSize int
}

type target struct{ tgid, pidfd int }

const maxTargets = 4096

func newTargets() *targets { return &targets{known: make(map[int]target), maxSize: maxTargets} }

// getfd duplicates descriptor fd of thread tid's process into this process.
func (ts *targets) getfd(tid, fd int) (int, error) {
	for attempt := 0; ; attempt++ {
		t, err := ts.lookup(tid, attempt > 0)
		if err != nil {
			return -1, err
		}
		dup, err := unix.PidfdGetfd(t.pidfd, fd, 0)
		if err == nil {
			return dup, nil
		}
		if errors.Is(err, unix.ESRCH) && attempt == 0 {
			ts.drop(tid) // the process ended and the thread id was taken by another
			continue
		}
		return -1, fmt.Errorf("pidfd_getfd(%d) of thread %d: %w", fd, tid, err)
	}
}

func (ts *targets) lookup(tid int, fresh bool) (target, error) {
	if !fresh {
		ts.mu.Lock()
		t, ok := ts.known[tid]
		ts.mu.Unlock()
		if ok {
			var st unix.Stat_t
			if unix.Stat("/proc/"+strconv.Itoa(t.tgid)+"/task/"+strconv.Itoa(tid), &st) == nil {
				return t, nil
			}
			ts.drop(tid)
		}
	}
	tgid, err := tgidOf(tid)
	if err != nil {
		return target{}, err
	}
	pidfd, err := unix.PidfdOpen(tgid, 0)
	if err != nil {
		return target{}, fmt.Errorf("pidfd_open(%d): %w", tgid, err)
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if old, ok := ts.known[tid]; ok {
		unix.Close(old.pidfd)
	}
	if len(ts.known) >= ts.maxSize {
		for k, v := range ts.known { // not the oldest, but some: the cache is for speed
			unix.Close(v.pidfd)
			delete(ts.known, k)
			break
		}
	}
	t := target{tgid: tgid, pidfd: pidfd}
	ts.known[tid] = t
	return t, nil
}

func (ts *targets) drop(tid int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if t, ok := ts.known[tid]; ok {
		unix.Close(t.pidfd)
		delete(ts.known, tid)
	}
}

func (ts *targets) closeAll() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for tid, t := range ts.known {
		unix.Close(t.pidfd)
		delete(ts.known, tid)
	}
}

// tgidOf is the process a thread belongs to.
func tgidOf(tid int) (int, error) {
	status, err := os.ReadFile("/proc/" + strconv.Itoa(tid) + "/status")
	if err != nil {
		return 0, err
	}
	for _, line := range bytes.Split(status, []byte("\n")) {
		if v, ok := bytes.CutPrefix(line, []byte("Tgid:")); ok {
			return strconv.Atoi(string(bytes.TrimSpace(v)))
		}
	}
	return 0, errors.New("no Tgid in /proc/" + strconv.Itoa(tid) + "/status")
}

// fdCloexec says whether descriptor fd of the thread's process is close-on-exec, which a
// duplicate of it cannot tell (the flag belongs to the descriptor, not the file).
func fdCloexec(tid, fd int) (bool, error) {
	info, err := os.ReadFile("/proc/" + strconv.Itoa(tid) + "/fdinfo/" + strconv.Itoa(fd))
	if err != nil {
		return false, err
	}
	for _, line := range bytes.Split(info, []byte("\n")) {
		if v, ok := bytes.CutPrefix(line, []byte("flags:")); ok {
			flags, err := strconv.ParseUint(string(bytes.TrimSpace(v)), 8, 64)
			if err != nil {
				return false, err
			}
			return flags&unix.O_CLOEXEC != 0, nil
		}
	}
	return false, errors.New("no flags in fdinfo")
}

// inEpoll says whether descriptor fd of the thread's process is registered in an epoll
// instance of the process. Swapping the socket behind the descriptor would leave the
// registration with the old one, which is closed by it, and no event would ever arrive: a
// runtime that registers a socket before it connects or binds it (the ones that do not,
// register it after the call returns) must keep its own. It reads the process's descriptors
// and the registrations of its epoll instances (`tfd:` lines of their fdinfo).
func (ts *targets) inEpoll(tid, fd int) (bool, error) {
	t, err := ts.lookup(tid, false)
	if err != nil {
		return false, err
	}
	dir := "/proc/" + strconv.Itoa(t.tgid)
	d, err := os.Open(dir + "/fd")
	if err != nil {
		return false, err
	}
	names, err := d.Readdirnames(-1) // the names only: a stat of each would double the cost
	d.Close()
	if err != nil {
		return false, err
	}
	want := []byte("tfd:")
	var link [32]byte
	for _, name := range names {
		n, err := unix.Readlink(dir+"/fd/"+name, link[:])
		if err != nil || string(link[:n]) != "anon_inode:[eventpoll]" {
			continue
		}
		info, err := os.ReadFile(dir + "/fdinfo/" + name)
		if err != nil {
			return false, err
		}
		for _, line := range bytes.Split(info, []byte("\n")) {
			if rest, ok := bytes.CutPrefix(line, want); ok {
				if f := bytes.Fields(rest); len(f) > 0 && string(f[0]) == strconv.Itoa(fd) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// readRemote reads len(buf) bytes at addr of thread tid's process.
func readRemote(tid int, addr uint64, buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	local := []unix.Iovec{{Base: &buf[0], Len: uint64(len(buf))}}
	remote := []unix.RemoteIovec{{Base: uintptr(addr), Len: len(buf)}}
	n, err := unix.ProcessVMReadv(tid, local, remote, 0)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return unix.EFAULT
	}
	return nil
}

// writeRemote writes buf at addr of thread tid's process. Where process_vm_writev refuses
// (a page that is not writable to it) the process's memory is written through /proc.
func writeRemote(tid int, addr uint64, buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	local := []unix.Iovec{{Base: &buf[0], Len: uint64(len(buf))}}
	remote := []unix.RemoteIovec{{Base: uintptr(addr), Len: len(buf)}}
	n, err := unix.ProcessVMWritev(tid, local, remote, 0)
	if err == nil && n == len(buf) {
		return nil
	}
	f, ferr := os.OpenFile("/proc/"+strconv.Itoa(tid)+"/mem", os.O_WRONLY, 0)
	if ferr != nil {
		return unix.EFAULT
	}
	defer f.Close()
	if _, werr := f.WriteAt(buf, int64(addr)); werr != nil {
		return unix.EFAULT
	}
	return nil
}
