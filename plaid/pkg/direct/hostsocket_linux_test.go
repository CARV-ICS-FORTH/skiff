//go:build linux && (amd64 || arm64)

package direct

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// System calls on a socket of the host's network. Each line of output is what the kernel or the supervisor said.
func init() {
	// misuse MARKED_PORT HOST_ADDR ANY_PORT: with a socket bound as a marked port.
	helpers["misuselistener"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		out := netip.MustParseAddrPort(args[1])
		any, _ := strconv.Atoi(args[2])
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		fmt.Println("the pod's interfaces:", interfaceList(fd))
		if err := unix.Bind(fd, &unix.SockaddrInet4{Port: port}); err != nil {
			fmt.Println("bind failed", err)
			return 2
		}
		fmt.Println("connect to the host:", unix.Connect(fd, sockaddrFor(out, unix.AF_INET)))
		fmt.Println("the socket's interfaces:", interfaceList(fd))
		var ifr [40]byte
		copy(ifr[:], "lo")
		_, _, wireless := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0x8B01, uintptr(unsafe.Pointer(&ifr[0]))) // SIOCGIWNAME
		fmt.Println("the socket's wireless:", error(wireless))
		fmt.Println("disconnect:", disconnect(fd))
		fmt.Println("bind elsewhere:", unix.Bind(fd, &unix.SockaddrInet4{Port: any}))
		fmt.Println("its listen:", unix.Listen(fd, 8))
		return 0
	}
	// misuseclient ADDR HOST_ADDR ANY_PORT: with a connection to a marked port, and again after
	// the peer has reset it.
	helpers["misuseclient"] = func(args []string) int {
		ap := netip.MustParseAddrPort(args[0])
		out := netip.MustParseAddrPort(args[1])
		any, _ := strconv.Atoi(args[2])
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err := unix.Connect(fd, sockaddrFor(ap, unix.AF_INET)); err != nil {
			fmt.Println("marked connect failed", err)
			return 2
		}
		fmt.Println("marked connect: ok")
		for _, when := range []string{"connected", "reset"} {
			fmt.Printf("%s: connect to the host: %v\n", when, unix.Connect(fd, sockaddrFor(out, unix.AF_INET)))
			fmt.Printf("%s: disconnect: %v\n", when, disconnect(fd))
			fmt.Printf("%s: bind elsewhere: %v\n", when, unix.Bind(fd, &unix.SockaddrInet4{Port: any}))
			fmt.Printf("%s: listen: %v\n", when, unix.Listen(fd, 8))
			fmt.Printf("%s: fast open: %v\n", when, unix.Sendto(fd, []byte("x"), unix.MSG_FASTOPEN, sockaddrFor(out, unix.AF_INET)))
			fmt.Printf("%s: interfaces: %v\n", when, interfaceList(fd))
			if when == "connected" {
				// The peer resets the connection when it has read this.
				if _, err := unix.Write(fd, []byte("reset me")); err != nil {
					fmt.Println("write", err)
				}
				buf := make([]byte, 16)
				_, err := unix.Read(fd, buf)
				fmt.Println("the peer's answer:", err)
			}
		}
		return 0
	}
	// misuseundumpable ADDR HOST_ADDR: as misuseclient, from a process that has made itself hard
	// for the supervisor to look at.
	helpers["misuseundumpable"] = func(args []string) int {
		ap := netip.MustParseAddrPort(args[0])
		out := netip.MustParseAddrPort(args[1])
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err := unix.Connect(fd, sockaddrFor(ap, unix.AF_INET)); err != nil {
			fmt.Println("marked connect failed", err)
			return 2
		}
		if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
			fmt.Println("prctl failed", err)
			return 2
		}
		fmt.Println("out of reach")
		time.Sleep(300 * time.Millisecond)
		fmt.Println("disconnect allowed:", disconnect(fd) == nil)
		fmt.Println("connect to the host allowed:", unix.Connect(fd, sockaddrFor(out, unix.AF_INET)) == nil)
		_, _ = unix.Write(fd, []byte("reset me"))
		return 0
	}
	// undumpableaccept PORT: a server that listens on a marked port, sets itself not dumpable,
	// and says hello to the first connection that accept gives it.
	helpers["undumpableaccept"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err := unix.Bind(fd, &unix.SockaddrInet4{Port: port}); err != nil {
			fmt.Println("bind", err)
			return 2
		}
		unix.Listen(fd, 8)
		if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
			fmt.Println("prctl failed", err)
			return 2
		}
		fmt.Println("out of reach")
		time.Sleep(300 * time.Millisecond)
		for i := 0; i < 2; i++ {
			c, _, err := unix.Accept4(fd, 0)
			fmt.Println("accept:", err)
			if err == nil {
				unix.Write(c, []byte("hello"))
				unix.Close(c)
			}
			time.Sleep(200 * time.Millisecond)
		}
		return 0
	}
	// reset PORT: a server that resets the first connection when something comes on it.
	helpers["reset"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			fmt.Println("listen", err)
			return 2
		}
		fmt.Println("listening")
		c, err := l.Accept()
		if err != nil {
			fmt.Println("accept", err)
			return 2
		}
		buf := make([]byte, 16)
		c.Read(buf)
		c.(*net.TCPConn).SetLinger(0)
		c.Close()
		time.Sleep(500 * time.Millisecond)
		return 0
	}
	// ownsockets: what is not a socket of the host's goes on as it did, and what cannot be
	// watched does not run at all.
	helpers["ownsockets"] = func(args []string) int {
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		fmt.Println("interfaces:", interfaceList(fd))
		out := netip.MustParseAddrPort("192.0.2.1:9")
		fmt.Println("fast open:", unix.Sendto(fd, []byte("x"), unix.MSG_FASTOPEN, sockaddrFor(out, unix.AF_INET)))
		other, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		fmt.Println("connect:", unix.Connect(other, sockaddrFor(out, unix.AF_INET)))
		sp, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		fmt.Println("bind:", unix.Bind(sp, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}))
		fmt.Println("listen:", unix.Listen(sp, 1))
		_, _, e := unix.Syscall(unix.SYS_IO_URING_SETUP, 8, uintptr(unsafe.Pointer(new([120]byte))), 0)
		fmt.Println("io_uring:", e)
		if abiBit != 0 {
			_, _, e = unix.Syscall(abiBit|unix.SYS_GETPID, 0, 0, 0)
			fmt.Println("the other ABI:", e)
		}
		return 0
	}
}

func disconnect(fd int) error {
	var unspec [16]byte // AF_UNSPEC
	if _, _, e := unix.Syscall(unix.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&unspec)), 16); e != 0 {
		return e
	}
	return nil
}

// interfaceList is what SIOCGIFCONF says of the socket's network.
func interfaceList(fd int) string {
	type ifreq struct {
		Name [16]byte
		_    [24]byte
	}
	var reqs [32]ifreq
	conf := struct {
		Len int32
		_   int32
		Buf uintptr
	}{Len: int32(unsafe.Sizeof(reqs)), Buf: uintptr(unsafe.Pointer(&reqs))}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCGIFCONF, uintptr(unsafe.Pointer(&conf))); e != 0 {
		return e.Error()
	}
	return fmt.Sprintf("%d", int(conf.Len)/int(unsafe.Sizeof(reqs[0])))
}

func hostService(t *testing.T) (*net.TCPListener, chan struct{}) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	reached := make(chan struct{}, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		c.Close()
		reached <- struct{}{}
	}()
	return l.(*net.TCPListener), reached
}

func wantLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("no line %q in:\n%s", line, out)
		}
	}
}

func notReached(t *testing.T, reached chan struct{}, any int) {
	t.Helper()
	select {
	case <-reached:
		t.Error("a service only the host's namespace can reach was reached")
	case <-time.After(100 * time.Millisecond):
	}
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", any), 200*time.Millisecond); err == nil {
		c.Close()
		t.Errorf("something listens on the host's port %d", any)
	}
}

func TestAListenerOfTheHostIsOnlyAListener(t *testing.T) {
	requireLoopback(t)
	svc, reached := hostService(t)
	port, any := freePort(t), freePort(t)
	cfg := podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, fakeResolver{})
	s, out := runPod(t, cfg, "misuselistener", strconv.Itoa(port), svc.Addr().String(), strconv.Itoa(any))
	s.Wait()
	wantLines(t, out.String(),
		"connect to the host: transport endpoint is already connected",
		"the socket's interfaces: operation not permitted",
		"the socket's wireless: operation not permitted",
		"disconnect: transport endpoint is already connected",
		"bind elsewhere: invalid argument",
		"its listen: <nil>")
	notReached(t, reached, any)
}

func TestAConnectionOfTheHostIsOnlyThatConnection(t *testing.T) {
	requireLoopback(t)
	svc, reached := hostService(t)
	port, hostPort, any := freePort(t), freePort(t), freePort(t)
	p := strconv.Itoa(port)
	marked := Ports{uint16(port): {}}
	server, serverOut := runPod(t, podConfig(t, "10.244.5.4", marked, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(hostPort)}}), "reset", p)
	serverOut.waitFor(t, "listening")
	resolver := fakeResolver{"10.43.0.77:" + p: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(hostPort))}}
	c, out := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "misuseclient", "10.43.0.77:"+p, svc.Addr().String(), strconv.Itoa(any))
	c.Wait()
	server.Wait()
	var want []string
	wantLines(t, out.String(), "marked connect: ok", "the peer's answer: connection reset by peer")
	for _, when := range []string{"connected", "reset"} {
		want = append(want,
			when+": connect to the host: transport endpoint is already connected",
			when+": disconnect: transport endpoint is already connected",
			when+": bind elsewhere: invalid argument",
			when+": listen: invalid argument",
			when+": fast open: operation not supported",
			when+": interfaces: operation not permitted")
	}
	wantLines(t, out.String(), want...)
	notReached(t, reached, any)
}

func TestWhatIsNotTheHostsGoesOnAndWhatCannotBeWatchedDoesNotRun(t *testing.T) {
	requireLoopback(t)
	cfg := podConfig(t, "10.244.5.4", Ports{}, fakeResolver{})
	s, out := runPod(t, cfg, "ownsockets")
	s.Wait()
	lines := []string{"bind: <nil>", "listen: <nil>", "io_uring: function not implemented"}
	if abiBit != 0 {
		lines = append(lines, "the other ABI: function not implemented")
	}
	wantLines(t, out.String(), lines...)
	for _, refused := range []string{"interfaces: operation not permitted", "fast open: operation not supported", "connect: transport endpoint is already connected"} {
		if strings.Contains(out.String(), refused) {
			t.Errorf("a socket of the pod's own got %q:\n%s", refused, out)
		}
	}
}

// A process the supervisor cannot look at gets nothing by it.
func TestAProcessOutOfReachDoesNotGetPastTheGuard(t *testing.T) {
	requireLoopback(t)
	svc, reached := hostService(t)
	port, hostPort := freePort(t), freePort(t)
	p := strconv.Itoa(port)
	marked := Ports{uint16(port): {}}
	server, serverOut := runPod(t, podConfig(t, "10.244.5.4", marked, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(hostPort)}}), "reset", p)
	serverOut.waitFor(t, "listening")
	resolver := fakeResolver{"10.43.0.77:" + p: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(hostPort))}}
	c, out := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "misuseundumpable", "10.43.0.77:"+p, svc.Addr().String())
	c.Wait()
	server.Wait()
	wantLines(t, out.String(), "disconnect allowed: false", "connect to the host allowed: false")
	notReached(t, reached, 0)
}

// A process that the supervisor cannot look at does not get a connection from a listener of the
// host's that nothing has checked: whoever connects to the host port without a preamble.
func TestAProcessOutOfReachIsNotHandedAnUncheckedConnection(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	var away atomic.Bool
	cfg := podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, fakeResolver{})
	cfg.outOfReach = func(int) bool { return away.Load() }
	server, out := runPod(t, cfg, "undumpableaccept", p)
	out.waitFor(t, "out of reach")
	away.Store(true)
	c, err := net.Dial("tcp", "127.0.0.1:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("no preamble of any kind"))
	c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	buf := make([]byte, 5)
	if n, _ := c.Read(buf); n > 0 {
		t.Errorf("the server was handed the connection, and said %q\n%s", buf[:n], out)
	}
	server.Wait()
	if !strings.Contains(out.String(), "accept: operation not permitted") {
		t.Errorf("the accept was not refused:\n%s", out)
	}
}

// Nor does it get past the guard by asking for a port that is marked: the supervisor cannot see
// that the socket is one of the host's, and the kernel must not be left to connect it.
func TestAProcessOutOfReachDoesNotConnectAHostSocketToAMarkedPort(t *testing.T) {
	requireLoopback(t)
	svc, reached := hostService(t)
	svcPort := svc.Addr().(*net.TCPAddr).Port
	port, hostPort := freePort(t), freePort(t)
	p := strconv.Itoa(port)
	server, serverOut := runPod(t, podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(hostPort)}}), "reset", p)
	serverOut.waitFor(t, "listening")
	resolver := fakeResolver{
		"10.43.0.77:" + p:   {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(hostPort))},
		svc.Addr().String(): {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(hostPort))},
	}
	marked := Ports{uint16(port): {}, uint16(svcPort): {}}
	var away atomic.Bool
	cfg := podConfig(t, "10.244.1.9", marked, resolver)
	cfg.outOfReach = func(int) bool { return away.Load() }
	c, out := runPod(t, cfg, "misuseundumpable", "10.43.0.77:"+p, svc.Addr().String())
	out.waitFor(t, "out of reach")
	away.Store(true)
	c.Wait()
	server.Wait()
	wantLines(t, out.String(), "disconnect allowed: false", "connect to the host allowed: false")
	notReached(t, reached, 0)
}

// A socket of the host's network is known by that when the table has forgotten it, which it does.
func TestAHostSocketTheTableForgotIsStillOne(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	here := netnsCookieOf(fd)
	if here == 0 {
		t.Skip("no SO_NETNS_COOKIE before Linux 5.14")
	}
	s := &Supervisor{table: NewTable(0, 0)}
	if _, ok := s.hostSocket(fd); ok {
		t.Error("before anything was put in place")
	}
	s.podNet.Store(here + 1)
	s.hostNet.Store(here)
	if sock, ok := s.hostSocket(fd); !ok || sock.Kind == KindListener {
		t.Errorf("in the host's network, not in the table: %v, %v", sock, ok)
	}
	s.hostNet.Store(here + 2)
	if _, ok := s.hostSocket(fd); ok {
		t.Error("in another network")
	}
	s.podNet.Store(here)
	s.hostNet.Store(here)
	if _, ok := s.hostSocket(fd); ok {
		t.Error("where the host and the pod are one network, only the table can say")
	}
}

// nodeAt is a resolver whose node is at an address of its own.
type nodeAt struct {
	fakeResolver
	node netip.Addr
}

func (n nodeAt) HostOf(netip.Addr) (netip.Addr, bool) { return n.node, true }

// A marked port is served on the node's address, not on the host's others.
func TestAMarkedPortIsOnTheNodesAddress(t *testing.T) {
	requireLoopback(t)
	port := freePort(t)
	p := strconv.Itoa(port)
	node := netip.MustParseAddr("127.0.0.2")
	server, out := runPod(t, podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, nodeAt{fakeResolver{}, node}), "acceptloop", p)
	out.waitFor(t, "listening")
	defer func() {
		server.cmd.Process.Kill()
		server.Wait()
	}()
	c, err := net.DialTimeout("tcp", "127.0.0.2:"+p, time.Second)
	if err != nil {
		t.Fatalf("on the node's address: %v", err)
	}
	c.Close()
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+p, 200*time.Millisecond); err == nil {
		c.Close()
		t.Error("the port is on the host's other addresses too")
	}
}
