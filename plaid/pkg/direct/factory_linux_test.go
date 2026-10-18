//go:build linux

package direct

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func startFactory(t testing.TB) *FactoryClient {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeFactory(ctx, path) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c := NewFactoryClient(path)
	t.Cleanup(c.Close)
	return c
}

func TestFactoryGivesTCPSocketsOfBothFamilies(t *testing.T) {
	c := startFactory(t)
	for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
		fd, err := c.Socket(family)
		if err != nil {
			if family == unix.AF_INET6 && errors.Is(err, errRefused) {
				continue // a host without IPv6
			}
			t.Fatalf("family %d: %v", family, err)
		}
		if got, _ := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_DOMAIN); got != family {
			t.Errorf("family %d: got a socket of family %d", family, got)
		}
		if typ, _ := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE); typ != unix.SOCK_STREAM {
			t.Errorf("family %d: type %d, want a stream socket", family, typ)
		}
		unix.Close(fd)
	}
	if _, err := c.Socket(unix.AF_UNIX); err == nil {
		t.Error("a Unix socket was asked of the factory")
	}
}

// A socket from the factory works: it listens, accepts and carries data.
func TestFactorySocketCarriesTraffic(t *testing.T) {
	c := startFactory(t)
	fd, err := c.Socket(unix.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "listener")
	defer f.Close()
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		t.Fatal(err)
	}
	l, err := net.FileListener(f)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := net.Dial("tcp", l.Addr().String())
		if err == nil {
			conn.Write([]byte("hi"))
			conn.Close()
		}
	}()
	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	buf := make([]byte, 2)
	if _, err := conn.Read(buf); err != nil || string(buf) != "hi" {
		t.Fatalf("read %q, %v", buf, err)
	}
}

// Sockets keep coming over the same connection, and over a new one when the factory
// has closed the old.
func TestFactoryClientReconnects(t *testing.T) {
	c := startFactory(t)
	for i := 0; i < 3; i++ {
		fd, err := c.Socket(unix.AF_INET)
		if err != nil {
			t.Fatal(err)
		}
		unix.Close(fd)
	}
	c.mu.Lock()
	for _, conn := range c.idle {
		conn.Close() // the factory's end sees EOF; the client's cached end is dead
	}
	c.mu.Unlock()
	fd, err := c.Socket(unix.AF_INET)
	if err != nil {
		t.Fatalf("after the kept connection died: %v", err)
	}
	unix.Close(fd)
}

func TestFactoryRefusesAPathThatIsNotASocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ServeFactory(context.Background(), path); err == nil {
		t.Error("a regular file was replaced by a socket")
	}
}
