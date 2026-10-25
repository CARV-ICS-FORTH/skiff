//go:build linux && (amd64 || arm64)

package direct

import (
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
