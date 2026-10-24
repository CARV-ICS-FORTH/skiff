//go:build linux

package direct

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// cookieOfConn is the SO_COOKIE of the socket behind a connection.
func cookieOfConn(t *testing.T, conn net.Conn) uint64 {
	t.Helper()
	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var c uint64
	if err := raw.Control(func(fd uintptr) { c = cookieOf(int(fd)) }); err != nil {
		t.Fatal(err)
	}
	if c == 0 {
		t.Fatal("a socket without a cookie")
	}
	return c
}

// resetClose closes the connection with a reset, which leaves no TIME_WAIT socket behind.
func resetClose(conn net.Conn) {
	_ = conn.(*net.TCPConn).SetLinger(0)
	conn.Close()
}

// connPair is a connection over the loopback of the given network ("tcp4" or "tcp6"):
// the client's end and the accepted one. ok is false if the host has no such loopback.
func connPair(t *testing.T, network, addr string) (client, server net.Conn, ok bool) {
	t.Helper()
	l, err := net.Listen(network, addr)
	if err != nil {
		return nil, nil, false
	}
	defer l.Close()
	client, err = net.Dial(network, l.Addr().String())
	if err != nil {
		t.Skipf("the loopback does not connect: %v", err)
	}
	server, err = l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	return client, server, true
}

// What the kernel lists are the sockets that exist: both ends of connections over the
// IPv4 and IPv6 loopbacks while they are open, the listener, a socket whose peer has closed
// (CLOSE_WAIT), and none of those that were closed.
func TestLiveTCPCookiesFollowTheSockets(t *testing.T) {
	var open, closed, halfClosed []uint64
	for _, p := range [][2]string{{"tcp4", "127.0.0.1:0"}, {"tcp6", "[::1]:0"}} {
		for i := 0; i < 3; i++ {
			client, server, ok := connPair(t, p[0], p[1])
			if !ok {
				if p[0] == "tcp4" {
					t.Skip("no IPv4 loopback")
				}
				t.Log("no IPv6 loopback")
				break // a host without IPv6
			}
			defer client.Close()
			defer server.Close()
			cc, sc := cookieOfConn(t, client), cookieOfConn(t, server)
			switch i {
			case 0:
				open = append(open, cc, sc)
			case 1:
				closed = append(closed, cc, sc)
				resetClose(client)
				resetClose(server)
			case 2:
				// The client closes in order and the server does not: its end is in CLOSE_WAIT.
				client.Close()
				halfClosed = append(halfClosed, sc)
			}
		}
	}
	live, err := liveTCPCookies()
	if err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPROTONOSUPPORT) {
			t.Skipf("the sandbox does not allow the list of sockets: %v", err)
		}
		t.Fatal(err)
	}
	for _, c := range append(open, halfClosed...) {
		if _, ok := live[c]; !ok {
			t.Errorf("the cookie %d of an open socket is not listed", c)
		}
	}
	for _, c := range closed {
		if _, ok := live[c]; ok {
			t.Errorf("the cookie %d of a closed socket is listed", c)
		}
	}
}

// The factory answers a request for the host's cookies with what the kernel lists, over
// the connection that also gives sockets.
func TestFactoryListsTheLiveSockets(t *testing.T) {
	c := startFactory(t)
	client, server, ok := connPair(t, "tcp4", "127.0.0.1:0")
	if !ok {
		t.Skip("no IPv4 loopback")
	}
	defer server.Close()
	cc, sc := cookieOfConn(t, client), cookieOfConn(t, server)
	live, err := c.LiveCookies()
	if errors.Is(err, errRefused) {
		t.Skip("the sandbox does not allow the list of sockets")
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range []uint64{cc, sc} {
		if _, ok := live[cookie]; !ok {
			t.Errorf("the cookie %d of an open socket is not listed", cookie)
		}
	}
	// The list is asked for again over the same connection: the socket that was closed is
	// gone from it, and the other connection's are still there.
	other, otherServer, _ := connPair(t, "tcp4", "127.0.0.1:0")
	defer otherServer.Close()
	oc := cookieOfConn(t, other)
	resetClose(client)
	live, err = c.LiveCookies()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := live[cc]; ok {
		t.Error("the cookie of a closed socket is listed")
	}
	if _, ok := live[oc]; !ok {
		t.Error("the cookie of a socket that is still open is not listed")
	}
	other.Close()
	fd, err := c.Socket(unix.AF_INET) // the connection serves both requests
	if err != nil {
		t.Fatalf("a socket after the list: %v", err)
	}
	unix.Close(fd)
}

// A factory that does not know the request, which says '-' to any byte that is not a
// family, is refused, not mistaken for a list.
func TestFactoryClientIsRefusedByAFactoryThatDoesNotListSockets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := l.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		var req [1]byte
		for {
			if _, err := conn.Read(req[:]); err != nil {
				return
			}
			conn.Write([]byte{'-'})
		}
	}()
	c := NewFactoryClient(path)
	defer c.Close()
	done := make(chan error, 1)
	go func() {
		_, err := c.LiveCookies()
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errRefused) {
			t.Errorf("got %v, want the factory's refusal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request was not answered")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
