//go:build linux && (amd64 || arm64)

package direct

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"unsafe"

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

// An entry of a one-shot registration that has reported is disabled: it has no events left
// to make another entry from.
func TestAnEntryThatHasReportedIsNotArmed(t *testing.T) {
	ts := newTargets()
	defer ts.closeAll()
	tid := unix.Getpid()
	ep, _ := unix.EpollCreate1(0)
	defer unix.Close(ep)
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Events: unix.EPOLLOUT | unix.EPOLLONESHOT}); err != nil {
		t.Fatal(err)
	}
	if es := entriesOf(t, ts, tid, fd); len(es) != 1 || !es[0].armed() {
		t.Fatalf("a one-shot entry that has not reported: %+v", es)
	}
	if n, _ := unix.EpollWait(ep, make([]unix.EpollEvent, 4), 1000); n < 1 { // a fresh socket is writable
		t.Fatalf("no event: %d", n)
	}
	if es := entriesOf(t, ts, tid, fd); len(es) != 1 || es[0].armed() {
		t.Errorf("a one-shot entry that reported: %+v", es)
	}
}

// moveEntries puts the registrations of a socket under a descriptor number on another file,
// in every instance: afterwards the number, once it has the new file (the swap's ADDFD),
// is what epoll_ctl(MOD) and (DEL) find, and the old file has no entry left.
func TestMoveEntries(t *testing.T) {
	ts := newTargets()
	defer ts.closeAll()
	tid := unix.Getpid() // the main thread: a locked goroutine may take the thread that ran the test and end it
	sock := func() int {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}
	ev := func(events uint32, data uint64) *unix.EpollEvent {
		return &unix.EpollEvent{Events: events, Fd: int32(data), Pad: int32(data >> 32)}
	}
	ep1, _ := unix.EpollCreate1(0)
	ep2, _ := unix.EpollCreate1(0)
	defer unix.Close(ep1)
	defer unix.Close(ep2)
	// The container's descriptor, and the host socket that replaces it.
	old, fresh := sock(), sock()
	defer unix.Close(old)
	defer unix.Close(fresh)
	oldIno, freshIno := inodeOf(t, old), inodeOf(t, fresh)
	const flags = unix.EPOLLET | unix.EPOLLIN | unix.EPOLLOUT
	if err := unix.EpollCtl(ep1, unix.EPOLL_CTL_ADD, old, ev(flags, 0xabcd0000000000ef)); err != nil {
		t.Fatal(err)
	}
	if err := unix.EpollCtl(ep2, unix.EPOLL_CTL_ADD, old, ev(unix.EPOLLIN|unix.EPOLLEXCLUSIVE, 42)); err != nil {
		t.Fatal(err)
	}
	entries := entriesOf(t, ts, tid, old)
	if len(entries) != 2 {
		t.Fatalf("entries %+v", entries)
	}

	if err := ts.moveEntries(tid, old, old, fresh, entries); err != nil {
		t.Fatal(err)
	}
	if es, _ := ts.epollEntries(tid, old, oldIno); len(es) != 0 {
		t.Errorf("the old file has entries left: %+v", es)
	}
	moved, _ := ts.epollEntries(tid, old, freshIno)
	byInstance := map[int]epollEntry{}
	for _, e := range moved {
		byInstance[e.epfd] = e
	}
	if len(byInstance) != 2 || byInstance[ep1].events&flags != flags || byInstance[ep1].data != 0xabcd0000000000ef ||
		byInstance[ep2].events&unix.EPOLLEXCLUSIVE == 0 || byInstance[ep2].data != 42 {
		t.Errorf("the new file's entries under the number %d: %+v (instances %d, %d)", old, moved, ep1, ep2)
	}

	// The swap puts the new file on the container's number; its own epoll_ctl finds the entries.
	if err := unix.Dup3(fresh, old, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.EpollCtl(ep1, unix.EPOLL_CTL_MOD, old, ev(unix.EPOLLIN|unix.EPOLLOUT|unix.EPOLLET, 0x1234)); err != nil {
		t.Errorf("EPOLL_CTL_MOD with the container's number: %v", err)
	}
	// An event of the new socket carries the data: a connection to a listener.
	l := sock()
	defer unix.Close(l)
	if err := unix.Bind(l, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Skipf("no loopback: %v", err)
	}
	unix.Listen(l, 1)
	la, _ := unix.Getsockname(l)
	if err := unix.Connect(fresh, la); err != nil && err != unix.EINPROGRESS {
		t.Fatal(err)
	}
	events := make([]unix.EpollEvent, 4)
	n, err := unix.EpollWait(ep1, events, 2000)
	if err != nil || n != 1 || events[0].Fd != 0x1234 {
		t.Errorf("the event of the new socket: %d events %+v, %v", n, events[:max(n, 0)], err)
	}
	for _, ep := range []int{ep1, ep2} {
		if err := unix.EpollCtl(ep, unix.EPOLL_CTL_DEL, old, nil); err != nil {
			t.Errorf("EPOLL_CTL_DEL with the container's number: %v", err)
		}
	}
}

// A move that fails leaves the registrations where they were.
func TestMoveEntriesThatFailLeavesThem(t *testing.T) {
	ts := newTargets()
	defer ts.closeAll()
	tid := unix.Getpid() // the main thread: a locked goroutine may take the thread that ran the test and end it
	sock := func() int {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}
	ep1, _ := unix.EpollCreate1(0)
	ep2, _ := unix.EpollCreate1(0)
	defer unix.Close(ep1)
	defer unix.Close(ep2)
	old := sock()
	defer unix.Close(old)
	for _, ep := range []int{ep1, ep2} {
		if err := unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, old, &unix.EpollEvent{Events: unix.EPOLLIN}); err != nil {
			t.Fatal(err)
		}
	}
	entries := entriesOf(t, ts, tid, old)
	// A file that epoll cannot watch: the first instance takes the new registration, the second refuses.
	regular, err := unix.Open("/proc/self/exe", unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(regular)
	if err := ts.moveEntries(tid, old, old, regular, entries); err == nil {
		t.Fatal("moved registrations to a file that epoll cannot watch")
	}
	if es := entriesOf(t, ts, tid, old); len(es) != 2 {
		t.Errorf("the old file's entries after the failed move: %+v", es)
	}
	if es, _ := ts.epollEntries(tid, old, inodeOf(t, regular)); len(es) != 0 {
		t.Errorf("the failed move left entries for the new file: %+v", es)
	}
	// An instance that is not the container's.
	bad := append([]epollEntry{}, entries...)
	bad[1].epfd = 987654
	other := sock()
	defer unix.Close(other)
	if err := ts.moveEntries(tid, old, old, other, bad); err == nil {
		t.Fatal("moved registrations to an instance that does not exist")
	}
	if es := entriesOf(t, ts, tid, old); len(es) != 2 {
		t.Errorf("the old file's entries after the failed move: %+v", es)
	}
}

// The mover's table keeps the Go runtime's netpoller descriptors and 0, 1 and 2: a move of
// a descriptor that has one of those numbers is refused, before the look at the registrations
// (the call is left to the pod network) and by the thread, and the registrations stay.
func TestMoveOfAKeptNumberIsRefused(t *testing.T) {
	kept := runtimeDescriptors()
	t.Logf("kept descriptors: %v", kept)
	for _, fd := range []int{0, 1, 2} {
		if !slices.Contains(kept, fd) {
			t.Errorf("descriptor %d is not kept: %v", fd, kept)
		}
	}
	if len(kept) < 5 {
		t.Errorf("the runtime's epoll instance and eventfd are not among the kept descriptors: %v", kept)
	}
	ts := newTargets()
	defer ts.closeAll()
	tid := unix.Getpid()
	sock := func() int {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}
	ep, _ := unix.EpollCreate1(0)
	defer unix.Close(ep)
	old, fresh := sock(), sock()
	defer unix.Close(old)
	defer unix.Close(fresh)
	if err := unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, old, &unix.EpollEvent{Events: unix.EPOLLIN, Fd: 7}); err != nil {
		t.Fatal(err)
	}
	entries := entriesOf(t, ts, tid, old)
	if len(entries) != 1 {
		t.Fatalf("entries %+v", entries)
	}
	for _, fd := range kept {
		if err := ts.moveEntries(tid, fd, old, fresh, entries); err == nil {
			t.Errorf("a move under the kept number %d was made", fd)
		}
	}
	if es := entriesOf(t, ts, tid, old); len(es) != 1 || es[0].data != 7 {
		t.Errorf("the registrations after the refused moves: %+v", es)
	}
	if es, _ := ts.epollEntries(tid, old, inodeOf(t, fresh)); len(es) != 0 {
		t.Errorf("a refused move left entries for the new file: %+v", es)
	}
	// The look at the registrations leaves such a socket to the pod network, and not another.
	s := &Supervisor{targets: ts}
	if _, ok := s.registrations(tid, old, &heldSocket{dup: old}); !ok {
		t.Error("a socket under a number that is not kept was refused")
	}
	more := newTargets() // (its mover has not started: the list can be changed)
	defer more.closeAll()
	more.mover.keep = append(slices.Clone(more.mover.keep), old)
	slices.Sort(more.mover.keep)
	s = &Supervisor{targets: more}
	if _, ok := s.registrations(tid, old, &heldSocket{dup: old}); ok {
		t.Error("a socket in an epoll set under a kept number was not refused")
	}
	if entries, ok := s.registrations(tid, fresh, &heldSocket{dup: fresh}); !ok || len(entries) != 0 {
		t.Errorf("a socket in no epoll set: %+v, %v", entries, ok)
	}
}

// Moves asked for by many goroutines at once are made one after the other, each on its own
// descriptor number, and the thread's table is empty again after each.
func TestMoveEntriesAtOnce(t *testing.T) {
	ts := newTargets()
	defer ts.closeAll()
	tid := unix.Getpid()
	ep, _ := unix.EpollCreate1(0)
	defer unix.Close(ep)
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n*20)
	for i := 0; i < n; i++ {
		old, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(old)
		if err := unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, old, &unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(i)}); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(old, i int) {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				fresh, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
				if err != nil {
					errs <- err
					return
				}
				entries, err := ts.epollEntries(tid, old, inodeOf(t, old))
				if err != nil || len(entries) != 1 || entries[0].data != uint64(i) {
					errs <- fmt.Errorf("round %d: entries %+v, %v", round, entries, err)
					unix.Close(fresh)
					return
				}
				// The number `old` is the container's: the moves put each new file on it in the
				// thread's table, and the test puts it there in its own, as a swap does.
				if err := ts.moveEntries(tid, old, old, fresh, entries); err != nil {
					errs <- err
					unix.Close(fresh)
					return
				}
				if err := unix.Dup3(fresh, old, 0); err != nil {
					errs <- err
					return
				}
				unix.Close(fresh)
			}
		}(old, i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
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

// Memory the process cannot write is not written for it (EFAULT), as the kernel's own copy
// to a task's memory would not either; a page it can write is.
func TestWriteRemoteHonoursPagePermissions(t *testing.T) {
	mem, err := unix.Mmap(-1, 0, unix.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(mem)
	addr := uint64(uintptr(unsafe.Pointer(&mem[0])))
	if err := writeRemote(unix.Getpid(), addr, []byte("ok")); err != nil || string(mem[:2]) != "ok" {
		t.Fatalf("a writable page: %v, %q", err, mem[:2])
	}
	if err := unix.Mprotect(mem, unix.PROT_READ); err != nil {
		t.Fatal(err)
	}
	if err := writeRemote(unix.Getpid(), addr, []byte("no")); err == nil || string(mem[:2]) != "ok" {
		t.Fatalf("a read-only page: %v, %q", err, mem[:2])
	}
}
