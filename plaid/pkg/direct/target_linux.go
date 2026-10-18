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
type targets struct {
	mu      sync.Mutex
	known   map[int]target
	epolls  map[int]epollSet // by process
	maxSize int
}

type target struct{ tgid, pidfd int }

const maxTargets = 4096

func newTargets() *targets {
	return &targets{known: make(map[int]target), epolls: make(map[int]epollSet), maxSize: maxTargets}
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
		t, ok := ts.known[tid]
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

// inEpoll says whether descriptor fd of the thread's process is registered in an epoll
// instance of the process. Swapping the socket behind the descriptor would leave the
// registration with the old one, which is closed by it, and no event would ever arrive: a
// runtime that registers a socket before it connects or binds it (the ones that do not,
// register it after the call returns) must keep its own. It finds the process's epoll
// instances and reads the registrations of each (`tfd:` lines of their fdinfo).
//
// Finding the instances is a readlink for each descriptor of the process, and is not
// repeated while the process has the same descriptors (their numbers, from one read of the
// directory) and the instances that were found still are epoll instances. What it does not
// see is an instance made since the last look that has the number of a descriptor that was
// closed since: between two connects, and only a registration of the socket made before
// the connect would then be missed.
func (ts *targets) inEpoll(tid, fd int) (bool, error) {
	t, err := ts.lookup(tid, false)
	if err != nil {
		return false, err
	}
	dir := "/proc/" + strconv.Itoa(t.tgid)
	d, err := unix.Open(dir+"/fd", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(d)
	nums, sig, err := listDescriptors(d)
	if err != nil {
		return false, err
	}
	ts.mu.Lock()
	known, ok := ts.epolls[t.tgid]
	ts.mu.Unlock()
	if !ok || known.sig != sig || !areEpolls(d, known.fds) {
		known = epollSet{sig: sig}
		for _, n := range nums {
			if isEpoll(d, n) {
				known.fds = append(known.fds, n)
			}
		}
		ts.mu.Lock()
		if len(ts.epolls) >= ts.maxSize {
			for k := range ts.epolls { // some, not the oldest: the cache is for speed
				delete(ts.epolls, k)
				break
			}
		}
		ts.epolls[t.tgid] = known
		ts.mu.Unlock()
	}
	for _, n := range known.fds {
		if registered, err := epollHas(dir+"/fdinfo/"+strconv.Itoa(n), fd); err != nil || registered {
			return registered, err
		}
	}
	return false, nil
}

// listDescriptors reads the directory of a process's descriptors (/proc/<pid>/fd, open as d):
// their numbers, and a hash of them.
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
		complete := len(buf)-n >= 64 // what fits is all there is if a whole entry would have fitted too
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
		if complete {
			return nums, sig, nil
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

// epollHas says whether the fdinfo of an epoll instance lists descriptor fd as registered.
func epollHas(path string, fd int) (bool, error) {
	f, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
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
			return false, err
		}
		if n == 0 {
			break
		}
		data = data[:len(data)+n]
	}
	for rest := data; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		if v, ok := bytes.CutPrefix(line, []byte("tfd:")); ok {
			if f := bytes.Fields(v); len(f) > 0 && bytes.Equal(f[0], want) {
				return true, nil
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

// writeRemote2 writes two buffers, in one call if the process's memory allows it.
func writeRemote2(tid int, addr1 uint64, buf1 []byte, addr2 uint64, buf2 []byte) error {
	local := []unix.Iovec{{Base: &buf1[0], Len: uint64(len(buf1))}, {Base: &buf2[0], Len: uint64(len(buf2))}}
	remote := []unix.RemoteIovec{{Base: uintptr(addr1), Len: len(buf1)}, {Base: uintptr(addr2), Len: len(buf2)}}
	if n, err := unix.ProcessVMWritev(tid, local, remote, 0); err == nil && n == len(buf1)+len(buf2) {
		return nil
	}
	if err := writeRemote(tid, addr1, buf1); err != nil {
		return err
	}
	return writeRemote(tid, addr2, buf2)
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
