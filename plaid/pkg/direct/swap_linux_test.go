//go:build linux && (amd64 || arm64)

package direct

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// What the "containers" of these tests do, in a user and network namespace of their own.
// Each prints what it saw as key=value words on a line.
func init() {
	// serve PORT: a Go server on ":PORT" that takes one connection and counts what it gets.
	helpers["serve"] = func(args []string) int {
		l, err := net.Listen("tcp", ":"+args[0])
		if err != nil {
			fmt.Println("error", err)
			return 2
		}
		fmt.Println("listening", describe(l.(*net.TCPListener), nil))
		c, err := l.Accept()
		if err != nil {
			fmt.Println("error", err)
			return 2
		}
		fmt.Println("accepted", describe(nil, c.(*net.TCPConn)))
		n, _ := io.Copy(io.Discard, c)
		fmt.Println("received", n)
		return 0
	}
	// dial ADDR BYTES: a Go client.
	helpers["dial"] = func(args []string) int {
		c, err := net.Dial("tcp", args[0])
		if err != nil {
			fmt.Println("error", err)
			return 2
		}
		fmt.Println("dialed", describe(nil, c.(*net.TCPConn)))
		size, _ := strconv.Atoi(args[1])
		if _, err := c.Write(make([]byte, size)); err != nil {
			fmt.Println("error", err)
			return 2
		}
		c.Close()
		return 0
	}
	// rawserve PORT: system calls as a C program makes them, on a non-blocking socket.
	helpers["rawserve"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			return 2
		}
		if err := unix.Bind(fd, &unix.SockaddrInet4{Port: port}); err != nil {
			fmt.Println("error bind", err)
			return 2
		}
		unix.Listen(fd, 8)
		_, _, err = unix.Accept4(fd, 0)
		fmt.Println("empty accept", err)
		fmt.Println("listening")
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		unix.Poll(fds, 10000)
		nfd, from, err := unix.Accept4(fd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err != nil {
			fmt.Println("error accept", err)
			return 2
		}
		fl, _ := unix.FcntlInt(uintptr(nfd), unix.F_GETFL, 0)
		fd2, _ := unix.FcntlInt(uintptr(nfd), unix.F_GETFD, 0)
		local, _ := unix.Getsockname(nfd)
		peer, _ := unix.Getpeername(nfd)
		fmt.Println("accepted from", addrPortOf(from), "local", addrPortOf(local), "peer", addrPortOf(peer),
			"nonblock", fl&unix.O_NONBLOCK != 0, "cloexec", fd2&unix.FD_CLOEXEC != 0)
		return 0
	}
	// blockserve PORT: a listener that blocks in accept4, and takes a signal while it does.
	helpers["blockserve"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		signal.Notify(make(chan os.Signal, 1), syscall.SIGUSR1)
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err := unix.Bind(fd, &unix.SockaddrInet4{Port: port}); err != nil {
			fmt.Println("error bind", err)
			return 2
		}
		unix.Listen(fd, 8)
		fmt.Println("listening")
		nfd, from, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		for err == unix.EINTR {
			nfd, from, err = unix.Accept4(fd, unix.SOCK_CLOEXEC)
		}
		if err != nil {
			fmt.Println("error accept", err)
			return 2
		}
		peer, _ := unix.Getpeername(nfd)
		fl, _ := unix.FcntlInt(uintptr(nfd), unix.F_GETFL, 0)
		fmt.Println("accepted from", addrPortOf(from), "peer", addrPortOf(peer), "nonblock", fl&unix.O_NONBLOCK != 0)
		return 0
	}
	// rawdial ADDR: a blocking connect, then the names of the socket.
	helpers["rawdial"] = func(args []string) int {
		ap := netip.MustParseAddrPort(args[0])
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		err := unix.Connect(fd, sockaddrFor(ap, unix.AF_INET))
		local, _ := unix.Getsockname(fd)
		peer, _ := unix.Getpeername(fd)
		fmt.Println("connect", err, "local", addrPortOf(local), "peer", addrPortOf(peer))
		return 0
	}
}

// describe prints the addresses of a listener or a connection: what Go reports, and what
// the system calls report.
func describe(l *net.TCPListener, c *net.TCPConn) string {
	var rc syscall.RawConn
	var local, remote string
	if l != nil {
		rc, _ = l.SyscallConn()
		local = l.Addr().String()
	} else {
		rc, _ = c.SyscallConn()
		local, remote = c.LocalAddr().String(), c.RemoteAddr().String()
	}
	var rawLocal, rawRemote string
	rc.Control(func(fd uintptr) {
		if sa, err := unix.Getsockname(int(fd)); err == nil {
			rawLocal = addrPortOf(sa).String()
		}
		if sa, err := unix.Getpeername(int(fd)); err == nil {
			rawRemote = addrPortOf(sa).String()
		}
	})
	return fmt.Sprintf("local=%s remote=%s rawlocal=%s rawremote=%s", local, remote, rawLocal, rawRemote)
}

// syncBuffer collects a helper's output while the test waits for words in it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) waitFor(t *testing.T, word string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if strings.Contains(b.String(), word) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %q in the output: %s", word, b.String())
}

// field returns the value of key= in the line that starts with the word.
func field(t *testing.T, output, word, key string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, word+" ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, key+"="); ok {
				return v
			}
		}
	}
	t.Fatalf("no %s %s= in the output: %s", word, key, output)
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runPod runs scenario name as a pod's container under a supervisor with cfg, and returns
// the supervisor and the container's output.
func runPod(t *testing.T, cfg Config, name string, args ...string) (*Supervisor, *syncBuffer) {
	t.Helper()
	requireNamespaces(t)
	out := &syncBuffer{}
	cmd := helperCmd(name, true, args...)
	cmd.Stdout, cmd.Stderr = out, out
	s, err := Start(cfg, cmd)
	if err != nil {
		t.Skipf("cannot start under a seccomp user notification: %v", err)
	}
	return s, out
}

func podConfig(t *testing.T, ip string, ports Ports, backends fakeResolver) Config {
	cfg := testConfig(t)
	cfg.PodIP = netip.MustParseAddr(ip)
	cfg.Ports = ports
	cfg.Resolver = backends
	return cfg
}

// Two pods on a node, a Service address between them that exists nowhere in the client's
// network namespace (which has no network at all): the connection runs on the host's, and
// each side sees the pod addresses.
func TestDirectConnectionBetweenPods(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	vip := "10.43.0.77:" + p
	marked := Ports{uint16(port): {}}

	server, serverOut := runPod(t, podConfig(t, "10.244.5.4", marked, fakeResolver{}), "serve", p)
	serverOut.waitFor(t, "listening")

	// Connections to the server's host port that are not a client of ours are closed, and
	// the next is accepted; none of them is told anything (no ack).
	forgeries := map[string]net.Conn{
		"not-a-preamble": forged(t, p, func(pre *Preamble, key *[]byte) { *key = nil }),
		"bad-mac":        forged(t, p, func(pre *Preamble, key *[]byte) { *key = DeriveKey([]byte("another cluster")) }),
		"stale":          forged(t, p, func(pre *Preamble, _ *[]byte) { pre.Time = pre.Time.Add(-2 * time.Minute) }),
		"wrong-host":     forged(t, p, func(pre *Preamble, _ *[]byte) { pre.Host = netip.MustParseAddrPort("127.0.0.1:9") }),
		"wrong-backend":  forged(t, p, func(pre *Preamble, _ *[]byte) { pre.Backend = netip.MustParseAddrPort("10.244.7.7:" + p) }),
		"wrong-client":   forged(t, p, func(pre *Preamble, _ *[]byte) { pre.Client = netip.MustParseAddrPort("10.9.9.9:1") }),
	}

	resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	client, clientOut := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "dial", vip, "1000000")
	if err := client.Wait(); err != nil {
		t.Fatalf("client: %v\n%s", err, clientOut)
	}
	if err := server.Wait(); err != nil {
		t.Fatalf("server: %v\n%s", err, serverOut)
	}
	for reason, c := range forgeries {
		c.SetReadDeadline(time.Now().Add(time.Second))
		if n, err := c.Read(make([]byte, 1)); n != 0 || (err != io.EOF && !isReset(err)) {
			t.Errorf("%s: the connection stayed open or was acknowledged: %d bytes, %v", reason, n, err)
		}
		c.Close()
	}
	rejected := server.Stats().Rejected.Snapshot()
	for reason := range forgeries {
		if rejected[reason] != 1 {
			t.Errorf("rejections: %v; want one %s", rejected, reason)
		}
	}

	// Criterion: the client sees itself as its pod IP and its peer as the Service address ...
	cl, so := clientOut.String(), serverOut.String()
	clientLocal := field(t, cl, "dialed", "local")
	clientPort := strings.TrimPrefix(clientLocal, "10.244.1.9:")
	if clientPort == clientLocal || clientPort == "0" {
		t.Errorf("the client's own address is %s, want its pod IP and a port", clientLocal)
	}
	for _, key := range []string{"remote", "rawremote"} {
		if got := field(t, cl, "dialed", key); got != vip {
			t.Errorf("client %s = %s, want the Service address %s", key, got, vip)
		}
	}
	if got := field(t, cl, "dialed", "rawlocal"); got != clientLocal {
		t.Errorf("client rawlocal = %s, want %s", got, clientLocal)
	}
	// ... the server sees itself as its pod IP and port, and its peer as the client's pod IP and port.
	for _, key := range []string{"local", "rawlocal"} {
		if got := field(t, so, "accepted", key); got != "10.244.5.4:"+p {
			t.Errorf("server %s = %s, want its pod address", key, got)
		}
	}
	for _, key := range []string{"remote", "rawremote"} {
		if got := field(t, so, "accepted", key); got != clientLocal {
			t.Errorf("server %s = %s, want the client %s", key, got, clientLocal)
		}
	}
	if got := field(t, so, "listening", "rawlocal"); got != "[::]:"+p {
		t.Errorf("listener rawlocal = %s", got)
	}
	if !strings.Contains(so, "received 1000000") {
		t.Errorf("the server got %q", so)
	}
	if server.Stats().Accepted.Load() != 1 || client.Stats().Swapped.Load() != 1 {
		t.Errorf("accepted %d, swapped %d; want 1, 1", server.Stats().Accepted.Load(), client.Stats().Swapped.Load())
	}
}

// forged connects to the server's host port as a client would and sends it a preamble
// that is true of the connection, changed by edit (and signed with the key, which edit may
// change; none is a message that is no preamble at all).
func forged(t *testing.T, port string, edit func(pre *Preamble, key *[]byte)) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	host := c.LocalAddr().(*net.TCPAddr).AddrPort()
	pre, _ := NewPreamble(PreambleDirect, netip.AddrPortFrom(netip.MustParseAddr("10.244.1.9"), host.Port()),
		netip.MustParseAddrPort("10.244.5.4:"+port), host)
	key := testKey
	edit(&pre, &key)
	wire := pre.Marshal(key)
	if key == nil {
		wire = [PreambleSize]byte{'G', 'E', 'T', ' ', '/'}
	}
	if _, err := c.Write(wire[:]); err != nil {
		t.Fatal(err)
	}
	return c
}

func isReset(err error) bool {
	return err != nil && strings.Contains(err.Error(), "reset")
}

// The calls of a C program: a blocking connect that returns 0, a non-blocking listener that
// says EAGAIN while it is empty and whose accept4 flags and address are honoured.
func TestDirectRawSockets(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	vip := "10.43.0.78:" + p
	marked := Ports{uint16(port): {}}

	server, serverOut := runPod(t, podConfig(t, "10.244.5.4", marked, fakeResolver{}), "rawserve", p)
	serverOut.waitFor(t, "listening")
	resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	client, clientOut := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "rawdial", vip)
	if err := client.Wait(); err != nil {
		t.Fatalf("client: %v\n%s", err, clientOut)
	}
	if err := server.Wait(); err != nil {
		t.Fatalf("server: %v\n%s", err, serverOut)
	}
	so, cl := serverOut.String(), clientOut.String()
	if !strings.Contains(so, "empty accept resource temporarily unavailable") {
		t.Errorf("an empty non-blocking listener did not say EAGAIN: %s", so)
	}
	if !strings.Contains(cl, "connect <nil> local 10.244.1.9:") || !strings.Contains(cl, "peer "+vip) {
		t.Errorf("client: %s", cl)
	}
	clientPort := strings.TrimSuffix(strings.Split(strings.Split(cl, "local ")[1], " ")[0], "")
	if !strings.Contains(so, "accepted from "+clientPort) ||
		!strings.Contains(so, "local 10.244.5.4:"+p) || !strings.Contains(so, "peer "+clientPort) ||
		!strings.Contains(so, "nonblock true cloexec true") {
		t.Errorf("server: %s (client %s)", so, cl)
	}
}

// What is not marked is not touched: another port, an address that is no Service's.
func TestUnmarkedConnectionsAreLeftAlone(t *testing.T) {
	requireNamespaces(t)
	cfg := podConfig(t, "10.244.1.9", Ports{1: {}}, fakeResolver{})
	s, out := runPod(t, cfg, "rawdial", "127.0.0.1:9") // nothing listens on the discard port of an empty namespace
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if s.Stats().Swapped.Load() != 0 || !strings.Contains(out.String(), "connect network is unreachable") {
		t.Errorf("swapped %d; output %q", s.Stats().Swapped.Load(), out.String())
	}
}

// A blocking accept waits in the supervisor, which notices that the container's thread gave
// up the call (the signal interrupted it) and does not answer it; the call the kernel
// restarts is held again and served.
func TestDirectBlockingAccept(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	vip := "10.43.0.79:" + p
	marked := Ports{uint16(port): {}}

	server, serverOut := runPod(t, podConfig(t, "10.244.5.4", marked, fakeResolver{}), "blockserve", p)
	serverOut.waitFor(t, "listening")
	time.Sleep(400 * time.Millisecond) // in accept, and held
	server.cmd.Process.Signal(syscall.SIGUSR1)
	time.Sleep(400 * time.Millisecond)
	resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	client, clientOut := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "rawdial", vip)
	if err := client.Wait(); err != nil {
		t.Fatalf("client: %v\n%s", err, clientOut)
	}
	if err := server.Wait(); err != nil {
		t.Fatalf("server: %v\n%s", err, serverOut)
	}
	if so := serverOut.String(); !strings.Contains(so, "accepted from 10.244.1.9:") || !strings.Contains(so, "nonblock false") {
		t.Errorf("server: %s", so)
	}
}

// admit on its own, on a listener of the test's: a client that closes before the
// container accepts is not handed over, and the others are acknowledged.
func TestAdmit(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	lf, err := l.(*net.TCPListener).File() // a descriptor of the listener, to accept on
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	p := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	s := &Supervisor{cfg: Config{PodIP: netip.MustParseAddr("10.244.5.4"), Key: testKey, Resolver: fakeResolver{}}}
	listener := Socket{Kind: KindListener, Local: netip.MustParseAddrPort("[::]:" + p)}

	// connect makes a client that sends a preamble of kind; admit decides it on the accepted end.
	connect := func(kind PreambleKind, backend string) (client net.Conn, sent Preamble) {
		c, err := net.Dial("tcp", "127.0.0.1:"+p)
		if err != nil {
			t.Fatal(err)
		}
		host := c.LocalAddr().(*net.TCPAddr).AddrPort()
		sent, _ = NewPreamble(kind, netip.AddrPortFrom(netip.MustParseAddr("10.244.1.9"), host.Port()), netip.MustParseAddrPort(backend), host)
		wire := sent.Marshal(testKey)
		c.Write(wire[:])
		return c, sent
	}
	admit := func() (Preamble, string, error) {
		fd, _, err := unix.Accept(int(lf.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		return s.admit(fd, listener)
	}

	// The client wrote its preamble and went away.
	c, _ := connect(PreambleDirect, "10.244.5.4:"+p)
	c.Close()
	if _, reason, err := admit(); reason != "client-gone" {
		t.Errorf("a client that closed: %q, %v", reason, err)
	}
	// A client that waits is acknowledged, with the ack of its own preamble.
	c, sent := connect(PreambleDirect, "10.244.5.4:"+p)
	defer c.Close()
	pre, reason, err := admit()
	if reason != "" || err != nil || pre.Client != sent.Client {
		t.Fatalf("admitted %v, %q, %v", pre, reason, err)
	}
	ack := make([]byte, AckSize)
	c.SetReadDeadline(time.Now().Add(time.Second))
	want := sent.Ack(testKey)
	if _, err := io.ReadFull(c, ack); err != nil || string(ack) != string(want[:]) {
		t.Errorf("ack %x, %v", ack, err)
	}
	// The pod's own proxy connects on the loopback and says the connection is a local one.
	c, _ = connect(PreambleLocal, "127.0.0.1:"+p)
	defer c.Close()
	if _, reason, err := admit(); reason != "" {
		t.Errorf("a local connection from the loopback: %q, %v", reason, err)
	}
}

// A direct attempt that fails in any way leaves the container's connect to the pod network
// (here a network with nothing in it: the call fails as it would without a supervisor), and
// is counted with its reason.
func TestFallbackToThePodNetwork(t *testing.T) {
	requireNamespaces(t)
	tests := []struct {
		reason string
		// serve is the backend's node: nothing listens (nil), or a listener whose accepted connections it handles.
		serve func(c net.Conn)
	}{
		{"refused", nil},
		{"no-ack", func(c net.Conn) { io.Copy(io.Discard, c) }},
		{"bad-ack", func(c net.Conn) { c.Write(make([]byte, AckSize)); io.Copy(io.Discard, c) }},
	}
	for _, tc := range tests {
		t.Run(tc.reason, func(t *testing.T) {
			port := freePort(t)
			p := strconv.Itoa(port)
			if tc.serve != nil {
				l, err := net.Listen("tcp", "127.0.0.1:"+p)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
				go func() {
					for {
						c, err := l.Accept()
						if err != nil {
							return
						}
						go tc.serve(c)
					}
				}()
			}
			vip := "10.43.0.77:" + p
			resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
			cfg := podConfig(t, "10.244.1.9", Ports{uint16(port): {}}, resolver)
			cfg.AckTimeout = 200 * time.Millisecond
			start := time.Now()
			client, out := runPod(t, cfg, "rawdial", vip)
			if err := client.Wait(); err != nil {
				t.Fatalf("client: %v\n%s", err, out)
			}
			if !strings.Contains(out.String(), "connect network is unreachable") {
				t.Errorf("the connect did not go on to the pod network: %s", out)
			}
			if got := client.Stats().Fallbacks.Snapshot(); got[tc.reason] != 1 || len(got) != 1 || client.Stats().Swapped.Load() != 0 {
				t.Errorf("fallbacks %v, swapped %d; want one %s", got, client.Stats().Swapped.Load(), tc.reason)
			}
			t.Logf("%s: the container's connect returned after %v", tc.reason, time.Since(start))
		})
	}
}
