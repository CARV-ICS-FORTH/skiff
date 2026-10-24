//go:build linux && (amd64 || arm64)

package direct

import (
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func fdIsOpen(fd int) bool { _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); return err == nil }

func TestKeptSocketsAreTakenInOrderAndExpire(t *testing.T) {
	expired := make(chan string, 4)
	k := &kept[string, string]{maxAge: 50 * time.Millisecond, perKey: 2, expired: func(v string) { expired <- v }}
	socket := func() int {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}
	a, b, c := socket(), socket(), socket()
	if !k.put("x", a, "a") || !k.put("x", b, "b") || k.put("x", c, "c") {
		t.Fatal("a key keeps perKey sockets, and no more")
	}
	unix.Close(c)
	if fd, v, ok := k.take("x"); !ok || fd != a || v != "a" {
		t.Errorf("took %d %q %v, want the first", fd, v, ok)
	}
	if _, _, ok := k.take("y"); ok {
		t.Error("took a socket kept for another key")
	}
	if !k.has("x") {
		t.Error("the second is kept")
	}
	unix.Close(a)
	select {
	case v := <-expired:
		if v != "b" || fdIsOpen(b) || k.has("x") {
			t.Errorf("expired %q, open %v, kept %v", v, fdIsOpen(b), k.has("x"))
		}
	case <-time.After(2 * time.Second):
		t.Error("the second socket did not expire")
	}

	d := socket()
	k.put("z", d, "d")
	k.drop("z")
	if fdIsOpen(d) || k.has("z") {
		t.Error("a dropped socket stays")
	}
	e := socket()
	k.put("w", e, "e")
	k.closeAll()
	if fdIsOpen(e) || k.has("w") {
		t.Error("closeAll leaves a socket")
	}
}

func TestHoldingMakesAThreadsCallWaitForTheOneItGaveUp(t *testing.T) {
	var h holding
	valid := func() bool { return true }
	first := h.enter(7, valid)
	if other := h.enter(8, valid); other == nil { // another thread does not wait
		t.Fatal("another thread waited")
	} else {
		other()
	}
	var entered atomic.Bool
	got := make(chan func())
	go func() { l := h.enter(7, valid); entered.Store(true); got <- l }()
	time.Sleep(150 * time.Millisecond)
	if entered.Load() {
		t.Fatal("the restarted call did not wait for the one that was given up")
	}
	first()
	(<-got)()

	// A call that has been given up meanwhile does not wait for ever.
	first = h.enter(7, valid)
	defer first()
	var live atomic.Bool
	live.Store(true)
	done := make(chan func(), 1)
	go func() { done <- h.enter(7, live.Load) }()
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
