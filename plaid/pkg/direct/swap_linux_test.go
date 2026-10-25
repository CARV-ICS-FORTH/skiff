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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"plaid/pkg/api"
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
	// acceptloop PORT: a Go server that takes connections and closes them, for ever.
	helpers["acceptloop"] = func(args []string) int {
		l, err := net.Listen("tcp", ":"+args[0])
		if err != nil {
			return 2
		}
		fmt.Println("listening")
		for {
			c, err := l.Accept()
			if err != nil {
				return 2
			}
			c.Close()
		}
	}
	// dialloop ADDR N: N connects in a row, each closed, and the median and 90th percentile
	// of how long a connect took, in microseconds.
	helpers["dialloop"] = func(args []string) int {
		n, _ := strconv.Atoi(args[1])
		took := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			t0 := time.Now()
			c, err := net.Dial("tcp", args[0])
			if err != nil {
				fmt.Println("error", err)
				return 2
			}
			took = append(took, time.Since(t0))
			c.Close()
		}
		sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
		fmt.Println("connect p50", took[n/2].Microseconds(), "p90", took[n*9/10].Microseconds())
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

func (b *syncBuffer) waitFor(t testing.TB, word string) {
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
		if v := tokenOf(line, key); v != "" {
			return v
		}
	}
	t.Fatalf("no %s %s= in the output: %s", word, key, output)
	return ""
}

func freePort(t testing.TB) int {
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
func runPod(t testing.TB, cfg Config, name string, args ...string) (*Supervisor, *syncBuffer) {
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

// podConfig is the configuration of a pod that marks ports for both roles (the Service
// ports it connects to and the ports it listens on are the same numbers).
func podConfig(t testing.TB, ip string, ports Ports, backends Resolver) Config {
	cfg := testConfig(t)
	cfg.PodIP = netip.MustParseAddr(ip)
	cfg.ConnectPorts, cfg.ListenPorts = ports, ports
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
	if server.Stats().Accepted.Load() != 1 || swapped(client) != 1 {
		t.Errorf("accepted %d, swapped %d; want 1, 1", server.Stats().Accepted.Load(), swapped(client))
	}
}

// A connect to a pod's own address on a marked port takes the direct path as one to a
// Service does: the client's peer is the pod address it dialled, and the server sees the
// client's pod address.
func TestDirectConnectionToAPodAddress(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	pod := "10.244.5.4:" + p
	marked := Ports{uint16(port): {}}

	server, serverOut := runPod(t, podConfig(t, "10.244.5.4", marked, fakeResolver{}), "serve", p)
	serverOut.waitFor(t, "listening")
	resolver := fakeResolver{pod: {Pod: netip.MustParseAddrPort(pod), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	client, clientOut := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "dial", pod, "1000")
	if err := client.Wait(); err != nil {
		t.Fatalf("client: %v\n%s", err, clientOut)
	}
	if err := server.Wait(); err != nil {
		t.Fatalf("server: %v\n%s", err, serverOut)
	}
	cl, so := clientOut.String(), serverOut.String()
	clientLocal := field(t, cl, "dialed", "local")
	for _, key := range []string{"remote", "rawremote"} {
		if got := field(t, cl, "dialed", key); got != pod {
			t.Errorf("client %s = %s, want the pod address %s", key, got, pod)
		}
	}
	for _, key := range []string{"remote", "rawremote"} {
		if got := field(t, so, "accepted", key); got != clientLocal || !strings.HasPrefix(got, "10.244.1.9:") {
			t.Errorf("server %s = %s, want the client %s", key, got, clientLocal)
		}
	}
	if server.Stats().Accepted.Load() != 1 || swapped(client) != 1 {
		t.Errorf("accepted %d, swapped %d; want 1, 1", server.Stats().Accepted.Load(), swapped(client))
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
	if swapped(s) != 0 || !strings.Contains(out.String(), "connect network is unreachable") {
		t.Errorf("swapped %d; output %q", swapped(s), out.String())
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

// pollserve FAMILY PORT N: what iperf3 and many C servers do, on a blocking listener of
// the family (4 or 6): wait with poll until it is readable, then accept (blocking), read
// once and close, N times. It prints, for each connection, the bytes it carried and the
// peer's address.
func init() {
	helpers["pollserve"] = func(args []string) int {
		family, port, n := unix.AF_INET, 0, 0
		if args[0] == "6" {
			family = unix.AF_INET6
		}
		port, _ = strconv.Atoi(args[1])
		n, _ = strconv.Atoi(args[2])
		fd, _ := unix.Socket(family, unix.SOCK_STREAM, 0)
		var sa unix.Sockaddr = &unix.SockaddrInet4{Port: port}
		if family == unix.AF_INET6 {
			sa = &unix.SockaddrInet6{Port: port}
		}
		if err := unix.Bind(fd, sa); err != nil {
			fmt.Println("error bind", err)
			return 2
		}
		unix.Listen(fd, 8)
		fmt.Println("listening")
		if len(args) > 3 { // wait before the first accept, with the listener open: connections queue up
			d, _ := strconv.Atoi(args[3])
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
		for i := 0; i < n; i++ {
			unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 10000)
			nfd, peer, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
			if err != nil {
				fmt.Println("error accept", err)
				return 2
			}
			buf := make([]byte, 16)
			got, _ := unix.Read(nfd, buf)
			// What the container is told about the connection, by accept and by the calls
			// that ask for the names: the listener's family, whatever socket it is.
			sock, _ := unix.Getsockname(nfd)
			peerName, _ := unix.Getpeername(nfd)
			unix.Close(nfd)
			fmt.Printf("conn %d bytes=%d peer=%s family=%T sockname=%s peername=%s\n", i, got, addrPortOf(peer), peer, addrPortOf(sock), addrPortOf(peerName))
		}
		return 0
	}
}

// A connection that the supervisor turns away, with nothing else waiting, is answered with
// a connection that has closed, not with a wait for the next one (a server that polled
// its blocking listener would stand in accept, and so would its timers) and not with an
// error (a server that does not expect one would end): the server reads end of file, as
// from a client that went away, and the next connection is the real client's.
func TestATurnedAwayConnectionIsAnsweredWithAClosedOne(t *testing.T) {
	for _, family := range []string{"4", "6"} {
		t.Run("IPv"+family, func(t *testing.T) { turnedAway(t, family) })
	}
}

// An IPv6 listener on a host that has no ::1 (IPv6 disabled on its loopback: the sockets
// work, the address is not there) is answered with a closed connection as well. The test
// runs the IPv6 case again as a host of that kind: a network namespace of its own, with a
// loopback that has no IPv6 (DIRECT_TEST_NO_LOOPBACK6, see TestMain).
func TestATurnedAwayConnectionOnAHostWithoutIPv6Loopback(t *testing.T) {
	requireLoopback(t)
	if os.Getenv("DIRECT_TEST_NO_LOOPBACK6") == "" {
		cmd := helperCmd("", true)
		cmd.Env = append(os.Environ(), "DIRECT_TEST_NO_LOOPBACK6=1")
		cmd.Args = append(cmd.Args, "-test.run=^"+t.Name()+"$", "-test.v")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if strings.Contains(string(out), "SKIP:") {
			t.Skipf("cannot run as that host:\n%s", out)
		}
		return
	}
	if l, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		l.Close()
		t.Fatal("the host has ::1")
	}
	turnedAway(t, "6")
}

func turnedAway(t *testing.T, family string) {
	port := freePort(t)
	p := strconv.Itoa(port)
	vip := "10.43.0.77:" + p
	marked := Ports{uint16(port): {}}
	// The server waits 800 ms before its first accept, so that the two connections it turns
	// away are both in the queue, and the second one's client has closed, when the supervisor
	// looks at them: something that is not a preamble, and a direct client that has gone.
	server, out := runPod(t, podConfig(t, "10.244.5.4", marked, fakeResolver{}), "pollserve", family, p, "2", "800")
	out.waitFor(t, "listening")
	forged(t, p, func(pre *Preamble, key *[]byte) { *key = nil }).Close()
	forged(t, p, func(pre *Preamble, key *[]byte) {}).Close()
	time.Sleep(1200 * time.Millisecond) // the server has accepted: nothing is queued, and it was handed a closed connection

	resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	client, clientOut := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "dial", vip, "4")
	if err := client.Wait(); err != nil {
		t.Fatalf("client: %v\n%s", err, clientOut)
	}
	if err := server.Wait(); err != nil {
		t.Fatalf("server: %v\n%s", err, out)
	}
	// The socket the container is handed is of its listener's family and says the pod's
	// addresses, the closed ones with the peer of the connection that was turned away.
	text := out.String()
	sock := "*unix.SockaddrInet4"
	if family == "6" {
		sock = "*unix.SockaddrInet6"
	}
	for i, peer := range []string{"127.0.0.1:", "10.244.1.9:"} {
		line := ""
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, fmt.Sprintf("conn %d ", i)) {
				line = l
			}
		}
		fields := map[string]string{}
		for _, f := range strings.Fields(line) {
			if k, v, ok := strings.Cut(f, "="); ok {
				fields[k] = v
			}
		}
		wantBytes := map[bool]string{true: "0", false: "4"}[i < 1]
		if fields["bytes"] != wantBytes || !strings.HasPrefix(fields["peer"], peer) || fields["peername"] != fields["peer"] ||
			fields["sockname"] != "10.244.5.4:"+p || !strings.HasSuffix(sock, fields["family"]) {
			t.Errorf("connection %d: want %s bytes, a peer from %s named alike by accept and getpeername, the pod's address as sockname and %s\n%s", i, wantBytes, peer, sock, text)
		}
	}
	if r := server.Stats().Rejected.Snapshot(); r["not-a-preamble"] != 1 || r["client-gone"] != 1 {
		t.Errorf("rejections: %v", r)
	}
}

// admit on its own, on a listener of the test's: a direct client that closes before the
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
	s := &Supervisor{cfg: Config{PodIP: netip.MustParseAddr("10.244.5.4"), Key: testKey, Resolver: fakeResolver{}, PreambleTimeout: DefaultPreambleTimeout}}
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
		return s.admit(fd, listener, nil)
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
	// A local one whose client has closed is handed over all the same (a probe of the pod).
	c, _ = connect(PreambleLocal, "127.0.0.1:"+p)
	c.Close()
	if _, reason, err := admit(); reason != "" {
		t.Errorf("a local connection whose client closed: %q, %v", reason, err)
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
			if got := client.Stats().Fallbacks.Snapshot(); got[tc.reason] != 1 || len(got) != 1 || swapped(client) != 0 {
				t.Errorf("fallbacks %v, swapped %d; want one %s", got, swapped(client), tc.reason)
			}
			t.Logf("%s: the container's connect returned after %v", tc.reason, time.Since(start))
		})
	}
}

// hostPorts is a resolver for a pod whose ports are served at other ports of the host.
type hostPorts struct {
	fakeResolver
	ports map[uint16]uint16
}

func (h hostPorts) HostPort(pod netip.AddrPort) (uint16, bool) {
	p, ok := h.ports[pod.Port()]
	return p, ok
}

// Two pods of a node may mark the same port: each is served at a host port of its own, and
// a client reaches the one its Service address leads to.
func TestPodsOfANodeMarkTheSamePort(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	marked := Ports{uint16(port): {}}
	var servers []*Supervisor
	var outs []*syncBuffer
	var resolver = fakeResolver{}
	for i, ip := range []string{"10.244.5.4", "10.244.5.5"} {
		hostPort := freePort(t)
		cfg := podConfig(t, ip, marked, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(hostPort)}})
		s, out := runPod(t, cfg, "serve", p)
		out.waitFor(t, "listening")
		servers, outs = append(servers, s), append(outs, out)
		vip := fmt.Sprintf("10.43.0.8%d:%s", i, p)
		resolver[vip] = Backend{Pod: netip.MustParseAddrPort(ip + ":" + p), Host: netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", hostPort))}
	}
	for i, s := range servers {
		vip := fmt.Sprintf("10.43.0.8%d:%s", i, p)
		client, out := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "dial", vip, "1000")
		if err := client.Wait(); err != nil {
			t.Fatalf("client %d: %v\n%s", i, err, out)
		}
		if err := s.Wait(); err != nil {
			t.Fatalf("server %d: %v\n%s", i, err, outs[i])
		}
		if got, want := field(t, outs[i].String(), "accepted", "local"), fmt.Sprintf("10.244.5.%d:%s", 4+i, p); got != want {
			t.Errorf("server %d is %s, want %s", i, got, want)
		}
	}
}

// A host port that a process of the host holds is not the pod's: the listener stays a socket
// of the pod network, and a client that comes to the host port finds no ack there and
// leaves its connect to the pod network.
func TestHostPortInUse(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	marked := Ports{uint16(port): {}}
	foreign, err := net.Listen("tcp", ":0") // a process of the host
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	hostPort := foreign.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := foreign.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c)
		}
	}()

	cfg := podConfig(t, "10.244.5.4", marked, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(hostPort)}})
	server, serverOut := runPod(t, cfg, "serve", p)
	serverOut.waitFor(t, "listening")
	if got := field(t, serverOut.String(), "listening", "rawlocal"); got != "[::]:"+p {
		t.Errorf("the listener is at %s", got)
	}
	resolver := fakeResolver{"10.43.0.77:" + p: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", hostPort))}}
	ccfg := podConfig(t, "10.244.1.9", marked, resolver)
	ccfg.AckTimeout = 200 * time.Millisecond
	client, clientOut := runPod(t, ccfg, "rawdial", "10.43.0.77:"+p)
	if err := client.Wait(); err != nil {
		t.Fatalf("client: %v\n%s", err, clientOut)
	}
	server.cmd.Process.Kill()
	server.Wait()
	if got := server.Stats().Fallbacks.Snapshot(); got["host-port-in-use"] != 1 || swapped(server) != 0 {
		t.Errorf("server: fallbacks %v, swapped %d", got, swapped(server))
	}
	if got := client.Stats().Fallbacks.Snapshot(); got["no-ack"] != 1 || !strings.Contains(clientOut.String(), "connect network is unreachable") {
		t.Errorf("client: fallbacks %v: %s", got, clientOut)
	}
}

func init() {
	// epoll ORDER CALL ADDR EVENTS: a non-blocking socket that is added to an epoll set before
	// (or after) the connect (or bind) that the supervisor may swap, with the events (names
	// joined by ",") and the data 0x1122334455667788. It then waits for the set to report the
	// socket (a connected one is writable; a listener that is bound and has listened is
	// readable when the helper connects to it), and changes and removes the entry by the
	// socket's number as a runtime does.
	helpers["epoll"] = func(args []string) int {
		order, call, ap := args[0], args[1], netip.MustParseAddrPort(args[2])
		var events uint32
		for _, name := range strings.Split(args[3], ",") {
			events |= map[string]uint32{"in": unix.EPOLLIN, "out": unix.EPOLLOUT, "et": unix.EPOLLET, "exclusive": unix.EPOLLEXCLUSIVE, "rdhup": unix.EPOLLRDHUP}[name]
		}
		const data = 0x1122334455667788
		ev := func(events uint32, data uint64) *unix.EpollEvent {
			return &unix.EpollEvent{Events: events, Fd: int32(data), Pad: int32(data >> 32)}
		}
		_ = loUp() // the pod's own client reaches the pod-side socket by the loopback
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			return 2
		}
		// The socket's number is a high one: a low one can be the number of a descriptor that
		// the supervisor's mover keeps for itself (see runtimeDescriptors), which depends on
		// the program the supervisor is in, and such a socket is left to the pod network.
		if high, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD, 200); err == nil {
			unix.Close(fd)
			fd = high
		}
		ep, _ := unix.EpollCreate1(0)
		add := func() {
			if err := unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, fd, ev(events, data)); err != nil {
				fmt.Println("error epoll_ctl", err)
			}
		}
		if order == "before" {
			add()
		}
		if call == "bind" {
			unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			err = unix.Bind(fd, sockaddrFor(ap, unix.AF_INET))
		} else {
			err = unix.Connect(fd, sockaddrFor(ap, unix.AF_INET))
		}
		if order == "after" {
			add()
		}
		local, _ := unix.Getsockname(fd)
		fmt.Println(call, err, "local", addrPortOf(local))
		if call == "bind" {
			if err := unix.Listen(fd, 8); err != nil {
				fmt.Println("listen", err)
				return 2
			}
			// A client in the pod's own network: its connection reaches the pod-side
			// socket, which the supervisor carries to the listener.
			c, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
			defer unix.Close(c)
			_ = unix.Connect(c, sockaddrFor(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), ap.Port()), unix.AF_INET))
		}
		// What the set holds of the socket's number: one entry, for the socket that is there.
		info, _ := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(ep))
		var st unix.Stat_t
		unix.Fstat(fd, &st)
		mine, others := 0, 0
		for _, line := range strings.Split(string(info), "\n") {
			if f := strings.Fields(line); len(f) > 0 && f[0] == "tfd:" {
				if f[1] == strconv.Itoa(fd) && strings.Contains(line, fmt.Sprintf("ino:%x ", st.Ino)) {
					mine++
				} else {
					others++
				}
			}
		}
		fmt.Println("entries", mine, "others", others)
		got := make([]unix.EpollEvent, 4)
		n, _ := unix.EpollWait(ep, got, 3000)
		for i := 0; i < n; i++ {
			fmt.Printf("epoll event data %x events %x\n", uint64(uint32(got[i].Fd))|uint64(uint32(got[i].Pad))<<32, got[i].Events&(unix.EPOLLIN|unix.EPOLLOUT))
		}
		fmt.Println("epoll events", n)
		if call == "bind" {
			conn, _, err := unix.Accept4(fd, unix.SOCK_NONBLOCK)
			fmt.Println("accept", err)
			if err == nil {
				unix.Close(conn)
			}
		}
		// An entry that is there is modified (not an exclusive one: the kernel refuses that,
		// with another error than that the entry is not there).
		fmt.Println("mod", unix.EpollCtl(ep, unix.EPOLL_CTL_MOD, fd, ev(events, data+1)))
		fmt.Println("del", unix.EpollCtl(ep, unix.EPOLL_CTL_DEL, fd, nil))
		return 0
	}
}

func init() {
	// epolloneshot ADDR: two sockets that are added to an epoll set before they are connected,
	// one after the other, as one-shot entries that have reported: an entry that has no
	// events left cannot be made again for another socket.
	helpers["epolloneshot"] = func(args []string) int {
		ap := netip.MustParseAddrPort(args[0])
		ep, _ := unix.EpollCreate1(0)
		for i := 0; i < 2; i++ {
			fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
			if err != nil {
				return 2
			}
			unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Events: unix.EPOLLOUT | unix.EPOLLONESHOT})
			unix.EpollWait(ep, make([]unix.EpollEvent, 1), 1000) // a new socket is writable: reported, now disabled
			fmt.Println("connect", unix.Connect(fd, sockaddrFor(ap, unix.AF_INET)))
		}
		return 0
	}
}

// The connection to the backend is started before the container's socket has been looked at
// all the way; a container whose socket is in an epoll set that it cannot be taken out of
// pays for that with a connection that never sends a preamble, once: its supervisor looks
// first from then on.
func TestEpollGuardDoesNotConnectTwice(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	marked := Ports{uint16(port): {}}
	l, err := net.Listen("tcp", "127.0.0.1:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var mu sync.Mutex
	accepted := 0
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted++
			mu.Unlock()
			c.Close()
		}
	}()
	vip := "10.43.0.77:" + p
	resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	client, out := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "epolloneshot", vip)
	if err := client.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if got := client.Stats().Fallbacks.Snapshot(); got["epoll"] != 2 || swapped(client) != 0 {
		t.Errorf("fallbacks %v, swapped %d: %s", got, swapped(client), out)
	}
	if accepted > 1 {
		t.Errorf("the backend's node accepted %d connections for two connects that were not swapped, want at most the one started before the look", accepted)
	}
}

func init() {
	// stalewait PORT MODE: a listener (C style, MODE block or nonblock) on a marked port whose
	// accept is made on a thread of its own, with a buffer for the peer's address (a
	// non-blocking one is called when a connection is queued). It interrupts that thread with
	// a signal while the supervisor still decides the call (the Go runtime's own signal, which
	// the call is restarted after), and says what the restarted call returned, and the
	// address in the buffer.
	helpers["stalewait"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		typ, wait := unix.SOCK_STREAM, time.Duration(0)
		if args[1] == "nonblock" {
			typ, wait = typ|unix.SOCK_NONBLOCK, 300*time.Millisecond
		}
		fd, err := unix.Socket(unix.AF_INET, typ, 0)
		if err != nil {
			return 2
		}
		unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		if err := unix.Bind(fd, &unix.SockaddrInet4{Port: port}); err != nil {
			fmt.Println("bind", err)
			return 2
		}
		unix.Listen(fd, 8)
		buf := make([]byte, 128)
		addrlen := uint32(len(buf))
		tids := make(chan int)
		returned := make(chan struct{})
		go func() {
			runtime.LockOSThread()
			tids <- unix.Gettid()
			time.Sleep(wait)
			nfd, _, errno := unix.Syscall6(unix.SYS_ACCEPT4, uintptr(fd), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&addrlen)), 0, 0, 0)
			fmt.Println("accept returned", errno, "fd", int(nfd) > 0, "length", addrlen, "peer",
				netip.AddrPortFrom(netip.AddrFrom4([4]byte(buf[4:8])), uint16(buf[2])<<8|uint16(buf[3])))
			close(returned)
		}()
		tid := <-tids
		fmt.Println("accepting")
		time.Sleep(wait + 400*time.Millisecond)
		unix.Tgkill(os.Getpid(), tid, unix.SIGURG)
		fmt.Println("interrupted")
		select {
		case <-returned:
		case <-time.After(3 * time.Second):
			fmt.Println("accept did not return")
		}
		return 0
	}
}

// A blocking accept that is interrupted (the thread is signalled, and the kernel makes the
// call again) while the supervisor holds a connection that it is waiting to hear the preamble
// of is not answered, and the buffer of its address is not written for it: the call that is
// made again is, with the connection, which was acked meanwhile (the client is slow with its
// preamble, so the thread is interrupted while the first call waits). The client's connection
// is the one the container accepts, and stays open. A non-blocking listener is told EAGAIN by
// the call that is made again if it does not wait for the first (Go and other event loops
// would then wait for a readiness that does not come).
func TestAnInterruptedAcceptKeepsItsConnection(t *testing.T) {
	for _, mode := range []string{"block", "nonblock"} {
		t.Run(mode, func(t *testing.T) { interruptedAccept(t, mode) })
	}
}

func interruptedAccept(t *testing.T, mode string) {
	port := freePort(t)
	p := strconv.Itoa(port)
	marked := Ports{uint16(port): {}}
	cfg := podConfig(t, "10.244.5.4", marked, fakeResolver{})
	cfg.PreambleTimeout = 2 * time.Second // the client is slow: the supervisor waits for it
	server, out := runPod(t, cfg, "stalewait", p, mode)
	out.waitFor(t, "accepting")
	c, err := net.Dial("tcp", "127.0.0.1:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// The thread is interrupted 400 ms after its call (at once for a blocking one, after
	// 300 ms for a non-blocking one, which is made when a connection is queued).
	time.Sleep(map[string]time.Duration{"block": 700, "nonblock": 1000}[mode] * time.Millisecond)
	host := c.LocalAddr().(*net.TCPAddr).AddrPort()
	pre, _ := NewPreamble(PreambleDirect, netip.AddrPortFrom(netip.MustParseAddr("10.244.1.9"), host.Port()),
		netip.MustParseAddrPort("10.244.5.4:"+p), host)
	wire := pre.Marshal(testKey)
	if _, err := c.Write(wire[:]); err != nil {
		t.Fatal(err)
	}
	ack := make([]byte, AckSize)
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(c, ack); err != nil {
		t.Fatalf("no ack: %v", err)
	}
	if err := server.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := fmt.Sprintf("accept returned errno 0 fd true length 16 peer 10.244.1.9:%d", host.Port())
	if !strings.Contains(out.String(), "interrupted") || !strings.Contains(out.String(), want) {
		t.Errorf("want %q:\n%s", want, out)
	}
	if server.Stats().Accepted.Load() != 1 || len(server.Stats().Rejected.Snapshot()) != 0 {
		t.Errorf("accepted %d, rejected %v", server.Stats().Accepted.Load(), server.Stats().Rejected.Snapshot())
	}
}

// A socket that is in an epoll set when the container connects or binds it is swapped and
// its registrations go with it: the set reports the new socket with the entry's data, and
// the container's epoll_ctl by its own descriptor number finds the entry.
func TestEpollRegistrationsGoWithTheSwap(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	vip := "10.43.0.77:" + p
	marked := Ports{uint16(port): {}}
	resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	wantEntry := func(t *testing.T, out *syncBuffer, event, events string) {
		t.Helper()
		mod := "mod <nil>"
		if strings.Contains(events, "exclusive") {
			mod = "mod invalid argument"
		}
		for _, want := range []string{"entries 1 others 0\n", "epoll event data 1122334455667788 events " + event, "epoll events 1\n", mod, "del <nil>"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("no %q in:\n%s", want, out)
			}
		}
	}
	for _, order := range []string{"before", "after"} {
		for _, events := range []string{"in,out", "in,out,et", "in,out,et,rdhup"} {
			t.Run("connect/"+order+"/"+events, func(t *testing.T) {
				server, serverOut := runPod(t, podConfig(t, "10.244.5.4", marked, fakeResolver{}), "serve", p)
				serverOut.waitFor(t, "listening")
				client, out := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "epoll", order, "connect", vip, events)
				if err := client.Wait(); err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				if swapped(client) != 1 || len(client.Stats().Fallbacks.Snapshot()) != 0 || !strings.Contains(out.String(), "local 10.244.1.9:") {
					t.Errorf("swapped %d, fallbacks %v: %s", swapped(client), client.Stats().Fallbacks.Snapshot(), out)
				}
				wantEntry(t, out, "4", events) // EPOLLOUT: connected
				server.cmd.Process.Kill()
				server.Wait()
			})
		}
	}
	for _, order := range []string{"before", "after"} {
		for _, events := range []string{"in", "in,et", "in,exclusive", "in,exclusive,et"} {
			t.Run("bind/"+order+"/"+events, func(t *testing.T) {
				requireLoopback(t) // the helper's client dials the pod-side listener on the pod's loopback
				b, bOut := runPod(t, podConfig(t, "10.244.5.4", marked, fakeResolver{}), "epoll", order, "bind", "0.0.0.0:"+p, events)
				if err := b.Wait(); err != nil {
					t.Fatalf("%v\n%s", err, bOut)
				}
				if b.Stats().Binds.Load() != 1 || len(b.Stats().Fallbacks.Snapshot()) != 0 || b.Stats().Accepted.Load() != 1 || !strings.Contains(bOut.String(), "accept <nil>") {
					t.Errorf("binds %d, accepts %d, fallbacks %v: %s", b.Stats().Binds.Load(), b.Stats().Accepted.Load(), b.Stats().Fallbacks.Snapshot(), bOut)
				}
				wantEntry(t, bOut, "1", events) // EPOLLIN: a connection is waiting
			})
		}
	}
}

func BenchmarkInEpoll(b *testing.B) {
	for _, n := range []int{20, 200, 2000} {
		b.Run(fmt.Sprintf("%d descriptors", n), func(b *testing.B) {
			ep, _ := unix.EpollCreate1(0)
			defer unix.Close(ep)
			var fds []int
			for i := 0; i < n; i++ {
				fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
				if err != nil {
					b.Skip(err)
				}
				fds = append(fds, fd)
				if i%2 == 0 {
					unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Events: unix.EPOLLIN})
				}
			}
			defer func() {
				for _, fd := range fds {
					unix.Close(fd)
				}
			}()
			ts := newTargets()
			defer ts.closeAll()
			tid := unix.Gettid()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := ts.epollEntries(tid, fds[len(fds)-1], 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkDirectConnect is the connect of a client pod to a server pod on a node (both
// under a supervisor, the host sockets on the loopback), against the same connect on the
// host's network. The supervisors' timings of the phases are logged (go test -v).
func BenchmarkDirectConnect(b *testing.B) {
	requireNamespaces(b)
	port := freePort(b)
	p := strconv.Itoa(port)
	vip := "10.43.0.77:" + p
	marked := Ports{uint16(port): {}}
	server, serverOut := runPod(b, podConfig(b, "10.244.5.4", marked, fakeResolver{}), "acceptloop", p)
	serverOut.waitFor(b, "listening")
	resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	cfg := podConfig(b, "10.244.1.9", marked, resolver)
	b.ResetTimer()
	client, out := runPod(b, cfg, "dialloop", vip, strconv.Itoa(b.N))
	if err := client.Wait(); err != nil {
		b.Fatalf("client: %v\n%s", err, out)
	}
	b.StopTimer()
	b.Log(strings.TrimSpace(out.String()))
	b.Log("client:\n" + client.timings.String())
	b.Log("server:\n" + server.timings.String())
	server.cmd.Process.Kill()
	server.Wait()

	host := helperCmd("dialloop", false, "127.0.0.1:"+p, "1000")
	// (the host's own listener: the server above is gone, so a plain one takes its place)
	l, err := net.Listen("tcp", "127.0.0.1:"+p)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	hostOut, err := host.CombinedOutput()
	b.Log("host:", strings.TrimSpace(string(hostOut)), err)
}

// A supervisor reports its counters when it starts and once more, ended, with the last, and
// nothing after that.
func TestSupervisorsReportTheirCounters(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	vip := "10.43.0.77:" + p
	marked := Ports{uint16(port): {}}
	type report struct {
		c     api.DirectCounters
		ended bool
	}
	collect := func(cfg *Config) *[]report {
		var mu sync.Mutex
		var reports []report
		cfg.Report = func(c api.DirectCounters, ended bool) {
			mu.Lock()
			defer mu.Unlock()
			reports = append(reports, report{c, ended})
		}
		return &reports
	}
	scfg := podConfig(t, "10.244.5.4", marked, fakeResolver{})
	serverReports := collect(&scfg)
	server, serverOut := runPod(t, scfg, "serve", p)
	serverOut.waitFor(t, "listening")
	resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	ccfg := podConfig(t, "10.244.1.9", marked, resolver)
	clientReports := collect(&ccfg)
	client, clientOut := runPod(t, ccfg, "dial", vip, "1000")
	if err := client.Wait(); err != nil {
		t.Fatalf("client: %v\n%s", err, clientOut)
	}
	if err := server.Wait(); err != nil {
		t.Fatalf("server: %v\n%s", err, serverOut)
	}
	before := len(*clientReports)
	client.push(false) // a late report of a supervisor that has ended is not made
	client.push(true)
	if len(*clientReports) != before {
		t.Errorf("%d reports after the last one", len(*clientReports)-before)
	}
	for name, c := range map[string]struct {
		reports *[]report
		want    api.DirectCounters
	}{
		"client": {clientReports, api.DirectCounters{Connects: 1}},
		"server": {serverReports, api.DirectCounters{Binds: 1, Accepts: 1}},
	} {
		rs := *c.reports
		if len(rs) < 2 || rs[0].ended || !rs[len(rs)-1].ended {
			t.Errorf("%s: %d reports, the first ended %t, the last ended %t", name, len(rs), len(rs) > 0 && rs[0].ended, len(rs) > 0 && rs[len(rs)-1].ended)
			continue
		}
		last := rs[len(rs)-1].c
		if last.Connects != c.want.Connects || last.Binds != c.want.Binds || last.Accepts != c.want.Accepts {
			t.Errorf("%s: the last report is %+v, want %+v", name, last, c.want)
		}
		for _, r := range rs[:len(rs)-1] {
			if r.ended {
				t.Errorf("%s: a report after the last one", name)
			}
		}
	}
}

// The Service's port is not the pod's: the client marks the port it dials (80), the server
// the one it listens on (the Service's targetPort), and each side derives its host port from
// the port of its own end, the backend's for the client and the listener's for the server.
func TestServicePortIsNotTheListenPort(t *testing.T) {
	port, hostPort := freePort(t), freePort(t)
	p := strconv.Itoa(port)
	vip := "10.43.0.77:80"

	scfg := podConfig(t, "10.244.5.4", nil, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(hostPort)}})
	scfg.ListenPorts = Ports{uint16(port): {}}
	server, serverOut := runPod(t, scfg, "serve", p)
	serverOut.waitFor(t, "listening")

	resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", hostPort))}}
	ccfg := podConfig(t, "10.244.1.9", nil, resolver)
	ccfg.ConnectPorts = Ports{80: {}}
	client, clientOut := runPod(t, ccfg, "dial", vip, "1000")
	if err := client.Wait(); err != nil {
		t.Fatalf("client: %v\n%s", err, clientOut)
	}
	if err := server.Wait(); err != nil {
		t.Fatalf("server: %v\n%s", err, serverOut)
	}
	if got := field(t, clientOut.String(), "dialed", "remote"); got != vip {
		t.Errorf("client's peer is %s, want the Service address %s", got, vip)
	}
	if got := field(t, serverOut.String(), "accepted", "local"); got != "10.244.5.4:"+p {
		t.Errorf("server is %s, want its pod address and listen port", got)
	}
	if server.Stats().Binds.Load() != 1 || client.Stats().Connects.Load() != 1 {
		t.Errorf("binds %d, connects %d; want 1, 1", server.Stats().Binds.Load(), client.Stats().Connects.Load())
	}
}

// The two lists are not one: a port a pod listens on is not one it connects to, and the
// other way round.
func TestConnectAndListenPortsAreSeparate(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	marked := Ports{uint16(port): {}}

	// Marked to connect to only: a server on that port stays a socket of the pod network.
	cfg := podConfig(t, "10.244.5.4", nil, fakeResolver{})
	cfg.ConnectPorts = marked
	server, out := runPod(t, cfg, "serve", p)
	out.waitFor(t, "listening")
	server.cmd.Process.Kill()
	server.Wait()
	if server.Stats().Binds.Load() != 0 {
		t.Errorf("a port marked to connect to was bound on the host")
	}

	// Marked to listen on only: a connect to a Service on that port is not held.
	resolver := fakeResolver{"10.43.0.77:" + p: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	cfg = podConfig(t, "10.244.1.9", nil, resolver)
	cfg.ListenPorts = marked
	client, cout := runPod(t, cfg, "rawdial", "10.43.0.77:"+p)
	if err := client.Wait(); err != nil {
		t.Fatalf("client: %v\n%s", err, cout)
	}
	if client.Stats().Connects.Load() != 0 || !strings.Contains(cout.String(), "connect network is unreachable") {
		t.Errorf("a port marked to listen on was connected on the host: %s", cout)
	}
}

// A connect that has to wait is cut short by the signals the Go runtime sends its threads;
// connectWithin goes on waiting for it, and ends with the timeout, not with EINTR.
func TestConnectWithinIsNotCutShortBySignals(t *testing.T) {
	// A listener with no room: its backlog is full, so the next connect's SYN is not
	// answered and the connect waits.
	l, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(l)
	lo := &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}
	if err := unix.Bind(l, lo); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(l, 0); err != nil {
		t.Fatal(err)
	}
	sa, _ := unix.Getsockname(l)
	for i := 0; i < 4; i++ { // fill the queue; these are not waited for
		c, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
		defer unix.Close(c)
		_ = unix.Connect(c, sa)
	}
	sock, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	defer unix.Close(sock)

	result := make(chan error, 1)
	tid := make(chan int, 1)
	go func() {
		runtime.LockOSThread()
		tid <- unix.Gettid()
		result <- connectWithin(sock, sa, 400*time.Millisecond)
	}()
	id := <-tid
	stop := time.After(300 * time.Millisecond)
signals:
	for {
		select {
		case <-stop:
			break signals
		case <-time.After(5 * time.Millisecond):
			_ = unix.Tgkill(unix.Getpid(), id, unix.SIGURG)
		}
	}
	if err := <-result; err != unix.ETIMEDOUT {
		t.Errorf("connectWithin: %v, want a timeout", err)
	}
}
