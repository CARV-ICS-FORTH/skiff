package handoff

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func socketPath(t *testing.T) string {
	t.Helper()
	// Unix socket paths are short (104 bytes on macOS): keep them under /tmp.
	dir, err := os.MkdirTemp("/tmp", "ho")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}

// openFDs counts this process's open descriptors by probing the range; unlike listing
// /dev/fd it works on every platform the tests run on.
func openFDs(t *testing.T) int {
	t.Helper()
	n := 0
	for fd := 0; fd < 4096; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			n++
		}
	}
	return n
}

// await starts Await and returns once its socket exists.
func await(t *testing.T, path string, timeout time.Duration, accept AcceptFunc) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- Await(context.Background(), path, timeout, accept) }()
	waitForSocket(t, path)
	return done
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no socket at %s", path)
}

func wait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Await did not return")
		return nil
	}
}

func pipe(t *testing.T) (r, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	return r, w
}

var request = Request{Type: TypeOverlaySocket, Bind: "192.0.2.1", Port: 8472}

// rawSend writes a request with the given descriptors without waiting for the reply,
// like a helper that is killed at that point.
func rawSend(t *testing.T, path string, payload []byte, files ...*os.File) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var rights []byte
	if len(files) > 0 {
		fds := make([]int, len(files))
		for i, f := range files {
			fds[i] = int(f.Fd())
		}
		rights = syscall.UnixRights(fds...)
	}
	if _, _, err := c.(*net.UnixConn).WriteMsgUnix(payload, rights, nil); err != nil {
		t.Fatal(err)
	}
	return c
}

func framed(body string) []byte {
	b := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(b, uint32(len(body)))
	copy(b[4:], body)
	return b
}

func TestHandoffDeliversTheDescriptor(t *testing.T) {
	path := socketPath(t)
	r, w := pipe(t)
	var got Request
	done := await(t, path, 5*time.Second, func(req Request, files []*os.File) error {
		got = req
		if len(files) != 1 {
			t.Errorf("got %d descriptors, want 1", len(files))
			return errors.New("bad")
		}
		_, err := files[0].Write([]byte("through"))
		return err
	})
	if err := Send(context.Background(), path, request, w, 5*time.Second); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := wait(t, done); err != nil {
		t.Fatalf("Await: %v", err)
	}
	buf := make([]byte, 16)
	n, _ := r.Read(buf)
	if string(buf[:n]) != "through" {
		t.Errorf("the received descriptor did not reach the pipe: %q", buf[:n])
	}
	if got != request {
		t.Errorf("request = %+v, want %+v", got, request)
	}
	// One handoff only: the socket is gone and nothing listens any more.
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket file remains: %v", err)
	}
	if err := Send(context.Background(), path, request, w, 300*time.Millisecond); err == nil {
		t.Error("a second handoff was accepted after the first")
	}
}

func TestHandoffSocketIsPrivate(t *testing.T) {
	path := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Await(ctx, path, 5*time.Second, func(Request, []*os.File) error { return nil }) }()
	waitForSocket(t, path)
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
	cancel()
	if err := wait(t, done); !errors.Is(err, context.Canceled) {
		t.Errorf("Await after cancel = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket file remains after cancel")
	}
}

func TestPeerIsIdentifiedByUID(t *testing.T) {
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath(t), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := net.Dial("unix", l.Addr().String())
		if err == nil {
			defer c.Close()
			time.Sleep(200 * time.Millisecond)
		}
	}()
	c, err := l.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	uid, err := peerUID(c)
	if err != nil || uid != uint32(os.Geteuid()) {
		t.Errorf("peerUID = %d, %v; want %d", uid, err, os.Geteuid())
	}
}

func TestRefusedRequestEndsTheWait(t *testing.T) {
	path := socketPath(t)
	_, w := pipe(t)
	done := await(t, path, 5*time.Second, func(Request, []*os.File) error { return errors.New("wrong socket") })
	err := Send(context.Background(), path, request, w, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "wrong socket") {
		t.Errorf("the sender was not told why: %v", err)
	}
	if err := wait(t, done); err == nil || !strings.Contains(err.Error(), "wrong socket") {
		t.Errorf("Await after a refusal = %v", err)
	}
}

func TestEveryReceivedDescriptorIsClosed(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		path := socketPath(t)
		var files []*os.File
		for i := 0; i < 3; i++ {
			_, w := pipe(t)
			files = append(files, w)
		}
		before := openFDs(t)
		var seen int
		done := await(t, path, 5*time.Second, func(_ Request, got []*os.File) error {
			seen = len(got)
			if accepted {
				return nil
			}
			return errors.New("no")
		})
		c := rawSend(t, path, framed(`{"type":"overlay_socket"}`), files...)
		_ = wait(t, done)
		c.Close()
		if seen != 3 {
			t.Errorf("accepted=%v: the accept function saw %d descriptors, want 3", accepted, seen)
		}
		if after := openFDs(t); after != before {
			t.Errorf("accepted=%v: %d descriptors before, %d after", accepted, before, after)
		}
	}
}

func TestMalformedClientsDoNotEndTheWait(t *testing.T) {
	path := socketPath(t)
	_, w := pipe(t)
	done := await(t, path, 5*time.Second, func(Request, []*os.File) error { return nil })

	// Connects and leaves, sends nothing useful, claims a huge message, or sends JSON that is not JSON.
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
	}
	rawSend(t, path, []byte{1, 2, 3}).Close()
	rawSend(t, path, framed("not json")).Close()
	big := make([]byte, 4)
	binary.BigEndian.PutUint32(big, MaxMessage+1)
	rawSend(t, path, big).Close()
	rawSend(t, path, framed(`{"type":"overlay_socket"}`)[:10]).Close()

	select {
	case err := <-done:
		t.Fatalf("Await ended after malformed clients: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := Send(context.Background(), path, request, w, 5*time.Second); err != nil {
		t.Fatalf("a valid handoff after them failed: %v", err)
	}
	if err := wait(t, done); err != nil {
		t.Fatalf("Await: %v", err)
	}
}

func TestSenderKilledAfterSendingStillHandsOver(t *testing.T) {
	path := socketPath(t)
	_, w := pipe(t)
	var accepted bool
	done := await(t, path, 5*time.Second, func(Request, []*os.File) error { accepted = true; return nil })
	c := rawSend(t, path, framed(`{"type":"overlay_socket"}`), w)
	c.Close() // the acknowledgement finds nobody
	if err := wait(t, done); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if !accepted {
		t.Error("the request was not accepted")
	}
}

func TestSenderKilledBeforeSendingLeavesTheWaitOpen(t *testing.T) {
	path := socketPath(t)
	_, w := pipe(t)
	done := await(t, path, 5*time.Second, func(Request, []*os.File) error { return nil })
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case err := <-done:
		t.Fatalf("Await ended when a sender left: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := Send(context.Background(), path, request, w, 5*time.Second); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitIsBounded(t *testing.T) {
	path := socketPath(t)
	start := time.Now()
	err := Await(context.Background(), path, 300*time.Millisecond, func(Request, []*os.File) error { return nil })
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Await = %v, want ErrTimeout", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("Await took %v", time.Since(start))
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("the socket file remains after a timeout")
	}
}

func TestStaleSocketIsReplacedAndOtherFilesAreNot(t *testing.T) {
	path := socketPath(t)
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	old.Close() // leaves the file behind, as a killed process does
	_, w := pipe(t)
	done := await(t, path, 5*time.Second, func(Request, []*os.File) error { return nil })
	if err := Send(context.Background(), path, request, w, 5*time.Second); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(filepath.Dir(path), "regular")
	if err := os.WriteFile(other, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Await(context.Background(), other, time.Second, func(Request, []*os.File) error { return nil }); err == nil {
		t.Error("Await replaced a file that is not a socket")
	}
	if data, _ := os.ReadFile(other); string(data) != "keep" {
		t.Error("the regular file was damaged")
	}
}

func TestSendWaitsForTheReceiverAndGivesUp(t *testing.T) {
	path := socketPath(t)
	_, w := pipe(t)
	start := time.Now()
	if err := Send(context.Background(), path, request, w, 400*time.Millisecond); err == nil {
		t.Fatal("Send succeeded with nobody listening")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("Send took %v to give up", time.Since(start))
	}

	sent := make(chan error, 1)
	go func() { sent <- Send(context.Background(), path, request, w, 5*time.Second) }()
	time.Sleep(600 * time.Millisecond)
	done := await(t, path, 5*time.Second, func(Request, []*os.File) error { return nil })
	if err := <-sent; err != nil {
		t.Fatalf("Send that started first: %v", err)
	}
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestNoDescriptorsLeakAcrossManyHandoffs(t *testing.T) {
	_, w := pipe(t)
	before := openFDs(t)
	for i := 0; i < 20; i++ {
		path := socketPath(t)
		accept := func(Request, []*os.File) error { return nil }
		if i%2 == 1 {
			accept = func(Request, []*os.File) error { return errors.New("no") }
		}
		done := await(t, path, 5*time.Second, accept)
		_ = Send(context.Background(), path, request, w, 5*time.Second)
		_ = wait(t, done)
	}
	if after := openFDs(t); after != before {
		t.Errorf("%d descriptors before, %d after", before, after)
	}
}
