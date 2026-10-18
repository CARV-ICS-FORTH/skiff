//go:build linux && (amd64 || arm64)

package direct

import (
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
