//go:build linux && (amd64 || arm64)

package direct

import (
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// The flags of a descriptor, as the supervisor reads them for the duplicate it makes: the
// open file's O_NONBLOCK and the descriptor's O_CLOEXEC.
func TestFdFlags(t *testing.T) {
	tid := unix.Gettid()
	for _, tc := range []struct {
		typ               int
		nonblock, cloexec bool
	}{
		{unix.SOCK_STREAM, false, false},
		{unix.SOCK_STREAM | unix.SOCK_NONBLOCK, true, false},
		{unix.SOCK_STREAM | unix.SOCK_CLOEXEC, false, true},
		{unix.SOCK_STREAM | unix.SOCK_NONBLOCK | unix.SOCK_CLOEXEC, true, true},
	} {
		fd, err := unix.Socket(unix.AF_INET, tc.typ, 0)
		if err != nil {
			t.Fatal(err)
		}
		flags, err := fdFlags(tid, fd)
		unix.Close(fd)
		if err != nil || (flags&unix.O_NONBLOCK != 0) != tc.nonblock || (flags&unix.O_CLOEXEC != 0) != tc.cloexec {
			t.Errorf("type %#x: flags %#o, %v; want nonblock %t, cloexec %t", tc.typ, flags, err, tc.nonblock, tc.cloexec)
		}
	}
}

// A thread id that was kept is good only while the thread is in its process: a thread that
// ended is looked up again (and a descriptor of a process that is not there is an error).
func TestTargetsForgetAThreadThatEnded(t *testing.T) {
	ts := newTargets()
	defer ts.closeAll()
	fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	defer unix.Close(fd)
	tid := unix.Gettid()
	dup, err := ts.getfd(tid, fd)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(dup)
	if _, ok := ts.known[tid]; !ok {
		t.Fatal("the thread was not kept")
	}
	// The same number with another process: the kept entry does not hold.
	ts.known[tid] = target{tgid: 1, pidfd: ts.known[tid].pidfd}
	if _, err := ts.lookup(tid, false); err != nil || ts.known[tid].tgid != unix.Getpid() {
		t.Errorf("the entry of a thread that is not in its process was kept: %v, tgid %d", err, ts.known[tid].tgid)
	}
}

func inodeOf(t testing.TB, fd int) uint64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}

func entriesOf(t testing.TB, ts *targets, tid, fd int) []epollEntry {
	t.Helper()
	entries, err := ts.epollEntries(tid, fd, inodeOf(t, fd))
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// The epoll look follows the process: a registration made or removed, an instance made
// after the look was kept, and a descriptor that was an instance and became something else.
func TestInEpollFollowsTheProcess(t *testing.T) {
	ts := newTargets()
	defer ts.closeAll()
	tid := unix.Gettid()
	sock := func() int {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}
	in := func(fd int) bool {
		t.Helper()
		return len(entriesOf(t, ts, tid, fd)) > 0
	}
	add := func(ep, fd int) {
		if err := unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Events: unix.EPOLLIN}); err != nil {
			t.Fatal(err)
		}
	}
	a, b := sock(), sock()
	defer unix.Close(a)
	defer unix.Close(b)
	if in(a) {
		t.Error("a socket that is in no epoll set is in one")
	}
	ep1, _ := unix.EpollCreate1(0) // a new instance: the descriptors are others
	add(ep1, a)
	if !in(a) || in(b) {
		t.Errorf("registered a: in(a) %t in(b) %t", in(a), in(b))
	}
	add(ep1, b)
	if !in(b) { // the same descriptors, the instance's registrations are read again
		t.Error("a registration made after the look was not seen")
	}
	c := sock()
	ep2, _ := unix.EpollCreate1(0)
	add(ep2, c)
	if !in(c) {
		t.Error("a second instance was not found")
	}
	unix.Close(ep2)
	if in(c) {
		t.Error("an instance that was closed still has its registrations")
	}
	// The instance's number is taken by a socket: what was kept of it is not trusted.
	unix.Close(ep1)
	other := sock()
	defer unix.Close(other)
	if in(a) {
		t.Error("a descriptor that is no epoll instance any more was read as one")
	}
	unix.Close(c)
}

// The look reads what the registration was made with (the events with their flags, the
// data), in each instance, and does not take another file's entry of the same number (a
// descriptor that was closed without being removed from the set) for the socket's.
func TestEpollEntriesAreWhatWasRegistered(t *testing.T) {
	ts := newTargets()
	defer ts.closeAll()
	tid := unix.Gettid()
	sock := func() int {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}
	a := sock()
	defer unix.Close(a)
	ev := func(events uint32, data uint64) *unix.EpollEvent {
		return &unix.EpollEvent{Events: events, Fd: int32(data), Pad: int32(data >> 32)}
	}
	ep1, _ := unix.EpollCreate1(0)
	ep2, _ := unix.EpollCreate1(0)
	defer unix.Close(ep1)
	defer unix.Close(ep2)
	const flags = unix.EPOLLET | unix.EPOLLIN | unix.EPOLLRDHUP
	if err := unix.EpollCtl(ep1, unix.EPOLL_CTL_ADD, a, ev(flags, 0xfeed00000000c0de)); err != nil {
		t.Fatal(err)
	}
	if err := unix.EpollCtl(ep2, unix.EPOLL_CTL_ADD, a, ev(unix.EPOLLOUT|unix.EPOLLONESHOT, 7)); err != nil {
		t.Fatal(err)
	}
	got := map[int]epollEntry{}
	for _, e := range entriesOf(t, ts, tid, a) {
		got[e.epfd] = e
	}
	if len(got) != 2 {
		t.Fatalf("entries %+v, want one in each instance", got)
	}
	if e := got[ep1]; e.events&flags != flags || e.data != 0xfeed00000000c0de {
		t.Errorf("first instance: %+v", e)
	}
	if e := got[ep2]; e.events&(unix.EPOLLOUT|unix.EPOLLONESHOT) != unix.EPOLLOUT|unix.EPOLLONESHOT || e.data != 7 {
		t.Errorf("second instance: %+v", e)
	}
	// A descriptor that was closed without being removed (another file has its entry on)
	// and whose number the socket took.
	c := sock()
	d, err := unix.Dup(c)
	if err != nil {
		t.Fatal(err)
	}
	unix.EpollCtl(ep1, unix.EPOLL_CTL_ADD, c, ev(unix.EPOLLIN, 1))
	unix.Close(c) // the entry stays: d keeps the file
	e := sock()   // takes the number of c
	if e != c {
		t.Skipf("the number was not reused: %d, %d", e, c)
	}
	defer unix.Close(e)
	defer unix.Close(d)
	if es := entriesOf(t, ts, tid, e); len(es) != 0 {
		t.Errorf("an entry of another file under the same number was taken for the socket's: %+v", es)
	}
}

// A read of the directory of descriptors that returns fewer entries than fit is not its end:
// procfs does that when a descriptor is replaced or closed while it is read, and a listing
// that stopped there would miss the descriptors above, an epoll instance among them.
func TestListDescriptorsWhileTheyChange(t *testing.T) {
	var churned []int
	for i := 0; i < 16; i++ {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		churned = append(churned, fd)
	}
	marker, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0) // above the others
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(marker)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, fd := range churned {
		wg.Add(1)
		go func(fd int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if s, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0); err == nil {
					unix.Dup3(s, fd, 0)
					unix.Close(s)
				}
			}
		}(fd)
	}
	missed := 0
	for i := 0; i < 100000 && missed == 0; i++ {
		d, err := unix.Open("/proc/self/fd", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		nums, _, err := listDescriptors(d)
		unix.Close(d)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, n := range nums {
			found = found || n == marker
		}
		if !found {
			missed++
		}
	}
	close(stop)
	wg.Wait()
	if missed != 0 {
		t.Errorf("a listing of the descriptors lacked descriptor %d, which is open all the time", marker)
	}
}
