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
// while tgkill(tgid, tid, 0) finds the thread: a thread id belongs to one process at a
// time. A pidfd of a process that ended fails with ESRCH, and then the entry is dropped.
//
// There is one pidfd for a process, whatever the number of its threads that were held: a
// process that runs many short-lived threads (a Go program does) would otherwise cost the
// supervisor a descriptor for each. The processes that ended are dropped by a sweep, when
// a process is to be kept and there are twice as many as there were after the last sweep
// (at least 64): one poll, without waiting, of all the pidfds, an ended process's being
// readable. A process that ended takes its threads, and what was found of its epoll
// instances, with it. A lookup that hits is a read of a map and a tgkill, whatever the
// sweeps do. maxTargets bounds the processes whatever a sweep finds.
type targets struct {
	mu      sync.Mutex
	known   map[int]*process // by thread
	procs   map[int]*process // by process
	sweepAt int              // the number of processes at which the next one kept sweeps
	polls   []unix.PollFd    // for a sweep
	swept   []*process       // the processes of a sweep, in the order of polls
	mover   mover            // the thread that moves epoll registrations (epoll_linux.go)
}

type target struct{ tgid, pidfd int }

// process is a kept process: its pidfd, how many of its threads are in known, and what was
// found of its epoll instances. A process that is not kept any more has pidfd -1.
type process struct {
	target
	tids   int
	epolls *epollSet
}

const (
	maxTargets = 4096
	minSweep   = 64
)

func newTargets() *targets {
	return &targets{
		known:   make(map[int]*process),
		procs:   make(map[int]*process),
		sweepAt: minSweep,
		mover:   mover{keep: runtimeDescriptors()},
	}
}

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
		p, ok := ts.known[tid]
		var t target
		if ok {
			t = p.target
		}
		ts.mu.Unlock()
		if ok {
			if unix.Tgkill(t.tgid, tid, 0) == nil {
				return t, nil
			}
			ts.drop(tid)
		}
	}
	tgid, err := tgidOf(tid)
	if err != nil {
		return target{}, err
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if old, ok := ts.known[tid]; ok { // a stale entry: the thread id is another process's now
		delete(ts.known, tid)
		if old.tids--; old.tids == 0 && old.tgid != tgid {
			ts.retire(old)
		}
	}
	p := ts.procs[tgid]
	if p != nil && pollEnded([]unix.PollFd{{Fd: int32(p.pidfd), Events: unix.POLLIN}}) > 0 {
		ts.reap(p) // the process number is another process's now
		p = nil
	}
	if p == nil {
		pidfd, err := unix.PidfdOpen(tgid, 0)
		if err != nil {
			return target{}, fmt.Errorf("pidfd_open(%d): %w", tgid, err)
		}
		if len(ts.procs) >= ts.sweepAt {
			ts.sweep()
		}
		if len(ts.procs) >= maxTargets {
			for _, v := range ts.procs { // not the oldest, but some: the cache is for speed
				ts.reap(v)
				break
			}
		}
		p = &process{target: target{tgid: tgid, pidfd: pidfd}}
		ts.procs[tgid] = p
	}
	p.tids++
	ts.known[tid] = p
	return p.target, nil
}

// sweep drops the processes that ended, with their threads. The next one is due when
// there are twice as many as are left (and in any case before maxTargets).
func (ts *targets) sweep() {
	ts.polls, ts.swept = ts.polls[:0], ts.swept[:0]
	for _, p := range ts.procs {
		ts.polls = append(ts.polls, unix.PollFd{Fd: int32(p.pidfd), Events: unix.POLLIN})
		ts.swept = append(ts.swept, p)
	}
	pollEnded(ts.polls)
	threads := false
	for i, p := range ts.swept {
		if ts.polls[i].Revents&unix.POLLIN != 0 {
			threads = threads || p.tids > 0
			ts.retire(p)
		}
	}
	if threads {
		for tid, p := range ts.known {
			if p.pidfd < 0 {
				delete(ts.known, tid)
			}
		}
	}
	clear(ts.swept) // for the collector: they are not kept now
	ts.sweepAt = min(max(minSweep, 2*len(ts.procs)), maxTargets)
}

// pollEnded polls pidfds without waiting, and says how many are readable: how many processes
// ended. A poll that fails says none.
func pollEnded(fds []unix.PollFd) int {
	for {
		n, err := unix.Poll(fds, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0
		}
		return n
	}
}

// retire closes a process's pidfd and forgets it (and what was found of its epoll
// instances with it), but not its threads (which have pidfd -1 now): that is a pass over known, for the caller to make.
func (ts *targets) retire(p *process) {
	unix.Close(p.pidfd)
	p.pidfd = -1
	delete(ts.procs, p.tgid)
}

// reap retires a process and drops its threads.
func (ts *targets) reap(p *process) {
	ts.retire(p)
	if p.tids > 0 {
		for tid, q := range ts.known {
			if q == p {
				delete(ts.known, tid)
			}
		}
	}
}

// drop forgets a thread, and its process if no thread of it is kept or it ended.
func (ts *targets) drop(tid int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	p, ok := ts.known[tid]
	if !ok {
		return
	}
	delete(ts.known, tid)
	if p.tids--; p.tids == 0 {
		ts.retire(p)
	} else if pollEnded([]unix.PollFd{{Fd: int32(p.pidfd), Events: unix.POLLIN}}) > 0 {
		ts.reap(p)
	}
}

func (ts *targets) closeAll() {
	ts.mover.close()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, p := range ts.procs {
		unix.Close(p.pidfd)
		p.pidfd = -1
	}
	clear(ts.procs)
	clear(ts.known)
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

// fdFlags is the flags of descriptor fd of the thread's process as /proc shows them: those of
// the open file (O_NONBLOCK among them, which a duplicate has too) and O_CLOEXEC if the
// descriptor is close-on-exec, which a duplicate of it cannot tell (the flag belongs to the
// descriptor, not the file).
func fdFlags(tid, fd int) (int, error) {
	f, err := unix.Open("/proc/"+strconv.Itoa(tid)+"/fdinfo/"+strconv.Itoa(fd), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(f)
	var buf [256]byte
	n, err := unix.Read(f, buf[:])
	if err != nil {
		return 0, err
	}
	for _, line := range bytes.Split(buf[:n], []byte("\n")) {
		if v, ok := bytes.CutPrefix(line, []byte("flags:")); ok {
			flags, err := strconv.ParseUint(string(bytes.TrimSpace(v)), 8, 64)
			return int(flags), err
		}
	}
	return 0, errors.New("no flags in fdinfo")
}

// epollSet is what the supervisor found of a process's epoll instances when it last looked.
type epollSet struct {
	fds []int  // the descriptors that were epoll instances
	sig uint64 // of the numbers of all the process's descriptors then
}

// epollEntry is a registration of a socket in an epoll instance of the container: the
// instance's descriptor in the container's table, and what epoll_ctl was given.
type epollEntry struct {
	epfd   int
	events uint32 // as the instance keeps them: EPOLLONESHOT, EPOLLET and EPOLLEXCLUSIVE are in them
	data   uint64
}

// armed says whether the entry has events to report. An EPOLLONESHOT entry that has
// reported is disabled: the instance keeps no event bits of it, and epoll_ctl cannot make
// another disabled one (EPOLLERR and EPOLLHUP are always added to what it is given).
func (e epollEntry) armed() bool {
	return e.events&^(unix.EPOLLET|unix.EPOLLONESHOT|unix.EPOLLEXCLUSIVE|unix.EPOLLWAKEUP) != 0
}

// epollEntries finds the registrations of descriptor fd (whose socket has inode number
// ino) of the thread's process in the process's epoll instances. A socket that is
// registered cannot have another one put in its place without them: the instance keeps the
// old file under the descriptor's number, the replacement closes it, and no event would
// ever arrive (see moveEntries). A runtime that registers a socket before it connects or
// binds it (the ones that do not, register it after the call returns) is one that has them.
// It finds the process's epoll instances and reads the registrations of each (`tfd:` lines
// of their fdinfo); an entry of the same number for another file is a stale one, of a
// descriptor that was closed without being removed, and is not the socket's.
//
// Finding the instances is a readlink for each descriptor of the process, and is not
// repeated while the process has the same descriptors (their numbers, from one read of the
// directory) and the instances that were found still are epoll instances. What it does not
// see is an instance made since the last look that has the number of a descriptor that was
// closed since: between two connects, and only a registration of the socket made before
// the connect would then be missed.
func (ts *targets) epollEntries(tid, fd int, ino uint64) ([]epollEntry, error) {
	t, err := ts.lookup(tid, false)
	if err != nil {
		return nil, err
	}
	dir := "/proc/" + strconv.Itoa(t.tgid)
	d, err := unix.Open(dir+"/fd", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(d)
	nums, sig, err := listDescriptors(d)
	if err != nil {
		return nil, err
	}
	ts.mu.Lock()
	var known *epollSet
	if p := ts.procs[t.tgid]; p != nil {
		known = p.epolls
	}
	ts.mu.Unlock()
	if known == nil || known.sig != sig || !areEpolls(d, known.fds) {
		known = &epollSet{sig: sig}
		for _, n := range nums {
			if isEpoll(d, n) {
				known.fds = append(known.fds, n)
			}
		}
		ts.mu.Lock()
		if p := ts.procs[t.tgid]; p != nil && p.pidfd == t.pidfd { // not one that ended meanwhile
			p.epolls = known
		}
		ts.mu.Unlock()
	}
	var entries []epollEntry
	for _, n := range known.fds {
		es, err := epollRegistrations(dir+"/fdinfo/"+strconv.Itoa(n), fd, ino)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			e.epfd = n
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// listDescriptors reads the directory of a process's descriptors (/proc/<pid>/fd, open as d):
// their numbers, and a hash of them. A read of a directory that returns fewer entries than
// fit has not reached its end (procfs does that while the process opens and closes
// descriptors): only a read that returns nothing has.
func listDescriptors(d int) (nums []int, sig uint64, err error) {
	buf := make([]byte, 4096)
	sig = 14695981039346656037 // FNV-1a
	for {
		n, err := unix.ReadDirent(d, buf)
		if err != nil {
			return nil, 0, err
		}
		if n == 0 {
			return nums, sig, nil
		}
		// linux_dirent64: ino (8), off (8), reclen (2), type (1), the name and a NUL.
		for b := buf[:n]; len(b) >= 19; {
			reclen := int(b[16]) | int(b[17])<<8
			if reclen < 19 || reclen > len(b) {
				return nil, 0, errors.New("a damaged directory entry")
			}
			name := b[19:reclen]
			if i := bytes.IndexByte(name, 0); i >= 0 {
				name = name[:i]
			}
			if num, err := strconv.Atoi(string(name)); err == nil { // not "." and ".."
				nums = append(nums, num)
				sig = (sig ^ uint64(num)) * 1099511628211
			}
			b = b[reclen:]
		}
	}
}

const epollLink = "anon_inode:[eventpoll]"

// isEpoll says whether descriptor n of the process (whose descriptors are the directory d)
// is an epoll instance.
func isEpoll(d, n int) bool {
	var link [32]byte
	m, err := unix.Readlinkat(d, strconv.Itoa(n), link[:])
	return err == nil && string(link[:m]) == epollLink
}

func areEpolls(d int, fds []int) bool {
	for _, n := range fds {
		if !isEpoll(d, n) {
			return false
		}
	}
	return true
}

// epollRegistrations reads the fdinfo of an epoll instance (at path) for the registrations
// of descriptor fd whose file has inode number ino: `tfd: N events: E data: D pos:P ino:I
// sdev:S`, the numbers in hexadecimal but the descriptor's.
func epollRegistrations(path string, fd int, ino uint64) ([]epollEntry, error) {
	f, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(f)
	want := []byte(strconv.Itoa(fd))
	data := make([]byte, 0, 4096)
	for {
		if len(data) == cap(data) {
			data = append(data, 0)[:len(data)] // room for more: an instance with many registrations
		}
		n, err := unix.Read(f, data[len(data):cap(data)])
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		data = data[:len(data)+n]
	}
	var found []epollEntry
	for rest := data; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		v, ok := bytes.CutPrefix(line, []byte("tfd:"))
		if !ok {
			continue
		}
		fields := bytes.Fields(v)
		if len(fields) == 0 || !bytes.Equal(fields[0], want) {
			continue
		}
		e, thisIno, err := parseEpollEntry(fields)
		if err != nil {
			return nil, err
		}
		if thisIno == ino {
			found = append(found, e)
		}
	}
	return found, nil
}

// parseEpollEntry reads the fields that follow "tfd:": N events: E data: D pos:P ino:I sdev:S.
func parseEpollEntry(fields [][]byte) (e epollEntry, ino uint64, err error) {
	var haveEvents, haveData, haveIno bool
	for i := 1; i < len(fields); i++ {
		name, value, _ := bytes.Cut(fields[i], []byte(":"))
		if len(value) == 0 && i+1 < len(fields) && (string(name) == "events" || string(name) == "data") {
			i++
			value = fields[i]
		}
		switch string(name) {
		case "events":
			var v uint64
			if v, err = strconv.ParseUint(string(value), 16, 32); err != nil {
				return
			}
			e.events, haveEvents = uint32(v), true
		case "data":
			if e.data, err = strconv.ParseUint(string(value), 16, 64); err != nil {
				return
			}
			haveData = true
		case "ino":
			if ino, err = strconv.ParseUint(string(value), 16, 64); err != nil {
				return
			}
			haveIno = true
		}
	}
	if !haveEvents || !haveData || !haveIno {
		err = fmt.Errorf("an epoll entry that is not understood: %q", bytes.Join(fields, []byte(" ")))
	}
	return
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

// writeRemote writes buf at addr of thread tid's process. Memory the process cannot write
// is EFAULT, as it is for the kernel's own copy to the process (a write through
// /proc/<tid>/mem would succeed on it: it forces the page's permissions).
func writeRemote(tid int, addr uint64, buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	local := []unix.Iovec{{Base: &buf[0], Len: uint64(len(buf))}}
	remote := []unix.RemoteIovec{{Base: uintptr(addr), Len: len(buf)}}
	n, err := unix.ProcessVMWritev(tid, local, remote, 0)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return unix.EFAULT
	}
	return nil
}
