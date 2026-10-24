//go:build linux && (amd64 || arm64)

package direct

import (
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Calls that the container gives up (a signal interrupts the thread, and the kernel makes the
// call again), and calls that wait for something that may not come.

// signals sends SIGURG, which the Go runtime handles and which restarts the calls it
// interrupts, to thread tid at random moments, a few hundred microseconds apart, until the
// returned function is called.
func signals(tid int) (stop func()) {
	var stopped atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stopped.Load() {
			_ = unix.Tgkill(os.Getpid(), tid, unix.SIGURG)
			time.Sleep(time.Duration(150+rand.IntN(300)) * time.Microsecond)
		}
	}()
	return func() { stopped.Store(true); <-done }
}

func init() {
	// sigdial ADDR [quiet]: a blocking connect on a thread that is signalled while the call is
	// held (not by the helper if it is quiet: the test signals it).
	helpers["sigdial"] = func(args []string) int {
		ap := netip.MustParseAddrPort(args[0])
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		tids := make(chan int)
		done := make(chan struct{})
		go func() {
			runtime.LockOSThread()
			tids <- unix.Gettid()
			err := unix.Connect(fd, sockaddrFor(ap, unix.AF_INET))
			local, _ := unix.Getsockname(fd)
			peer, _ := unix.Getpeername(fd)
			fmt.Println("connect", err, "local", addrPortOf(local), "peer", addrPortOf(peer))
			close(done)
		}()
		tid := <-tids
		if len(args) < 2 {
			time.Sleep(100 * time.Millisecond)
			unix.Tgkill(os.Getpid(), tid, unix.SIGURG)
		}
		<-done
		return 0
	}
	// sigaccept PORT: a blocking accept on a listener, which the test signals once the
	// connection is in the table (the test's hook), with the descriptors the process has
	// before and after it, and the lowest number that was free before.
	helpers["sigaccept"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err := unix.Bind(fd, &unix.SockaddrInet4{Port: port}); err != nil {
			fmt.Println("error bind", err)
			return 2
		}
		unix.Listen(fd, 8)
		fmt.Println("listening")
		runtime.LockOSThread()
		before := openFds()
		free := 0
		for slices.Contains(before, free) {
			free++
		}
		nfd, from, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		after := openFds()
		if err != nil {
			fmt.Println("error accept", err)
			return 2
		}
		var buf [5]byte
		n, _ := unix.Read(nfd, buf[:])
		fmt.Println("accepted fd", nfd, "free", free, "fds", len(before), len(after), "from", addrPortOf(from), "read", string(buf[:n]))
		return 0
	}
	// sigbind PORT N: N binds of a marked port, each listener closed after, on a thread that
	// is signalled all the while; then one bind and listen without, and a connection to it
	// from the pod's own network, which the proxy has to carry to the listener.
	helpers["sigbind"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		n, _ := strconv.Atoi(args[1])
		if err := loUp(); err != nil {
			fmt.Println("error lo", err)
			return 2
		}
		failed := map[string]int{}
		tids := make(chan int)
		done := make(chan struct{})
		bind := func() (int, error) {
			fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
			unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			err := unix.Bind(fd, &unix.SockaddrInet4{Port: port})
			return fd, err
		}
		go func() {
			defer close(done)
			runtime.LockOSThread()
			tids <- unix.Gettid()
			for i := 0; i < n; i++ {
				fd, err := bind()
				if err != nil {
					failed[err.Error()]++
				}
				unix.Close(fd)
			}
		}()
		stop := signals(<-tids)
		<-done
		stop()
		fmt.Println("binds failed", len(failed), failed)
		fd, err := bind()
		if err != nil {
			fmt.Println("error bind", err)
			return 2
		}
		unix.Listen(fd, 8)
		c, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		err = unix.Connect(c, sockaddrFor(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)), unix.AF_INET))
		fmt.Println("pod-side connect", err)
		if pollReadable(fd, 3000) {
			nfd, _, err := unix.Accept4(fd, 0)
			fmt.Println("accept", err, nfd > 0)
		}
		return 0
	}
	// sigbindone PORT: a bind on a thread that is signalled while the call is held, then
	// the listening and a connection to it from the pod's own network (see sigbind).
	helpers["sigbindone"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		if err := loUp(); err != nil {
			fmt.Println("error lo", err)
			return 2
		}
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		tids := make(chan int)
		done := make(chan struct{})
		go func() {
			runtime.LockOSThread()
			tids <- unix.Gettid()
			fmt.Println("bind", unix.Bind(fd, &unix.SockaddrInet4{Port: port}))
			close(done)
		}()
		tid := <-tids
		time.Sleep(100 * time.Millisecond)
		unix.Tgkill(os.Getpid(), tid, unix.SIGURG)
		<-done
		unix.Listen(fd, 8)
		c, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		fmt.Println("pod-side connect", unix.Connect(c, sockaddrFor(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)), unix.AF_INET)))
		if pollReadable(fd, 3000) {
			nfd, _, err := unix.Accept4(fd, 0)
			fmt.Println("accept", err, nfd > 0)
		}
		return 0
	}
	// bindtaken PORT: a bind of a marked port that a listener of the pod's loopback (which the
	// supervisor leaves to the pod network) holds, and then of a port that nothing holds.
	helpers["bindtaken"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		if err := loUp(); err != nil {
			fmt.Println("error lo", err)
			return 2
		}
		taker, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err := unix.Bind(taker, &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
			fmt.Println("error taker", err)
			return 2
		}
		unix.Listen(taker, 8)
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		fmt.Println("bind taken", unix.Bind(fd, &unix.SockaddrInet4{Port: port}))
		fd, _ = unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		fmt.Println("bind free", unix.Bind(fd, &unix.SockaddrInet4{Port: port + 1}))
		return 0
	}
	// twoaccept PORT: two threads in accept on one blocking listener; it ends when one has a
	// connection, with the other still in its call.
	helpers["twoaccept"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err := unix.Bind(fd, &unix.SockaddrInet4{Port: port}); err != nil {
			fmt.Println("error bind", err)
			return 2
		}
		unix.Listen(fd, 8)
		fmt.Println("listening")
		got := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() {
				runtime.LockOSThread()
				_, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
				got <- err
			}()
		}
		fmt.Println("accepted", <-got)
		time.Sleep(300 * time.Millisecond)
		return 0
	}
}

// openFds is the descriptors this process has, from /proc/self/fd (the one it reads them with
// is not among them).
func openFds() []int {
	d, err := unix.Open("/proc/self/fd", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	defer unix.Close(d)
	nums, _, _ := listDescriptors(d)
	return slices.DeleteFunc(nums, func(n int) bool { return n == d })
}

// interruptOnce is a Config.beforeSend that interrupts the first call of system call nr that is
// answered, with SIGURG (which the Go runtime handles, and the kernel then makes the call
// again), and returns when the call is no longer held: the answer that follows finds it gone.
func interruptOnce(t *testing.T, nr int, fired *atomic.Int32) func(tid, nr int, valid func() bool) {
	return func(tid, got int, valid func() bool) {
		if got != nr || !fired.CompareAndSwap(0, 1) {
			return
		}
		tgid, err := tgidOf(tid)
		if err == nil {
			err = unix.Tgkill(tgid, tid, unix.SIGURG)
		}
		if err != nil {
			t.Errorf("signalling thread %d: %v", tid, err)
			return
		}
		for i := 0; i < 2000 && valid(); i++ {
			time.Sleep(time.Millisecond)
		}
		if valid() {
			t.Errorf("the call of thread %d is held after the signal", tid)
		}
	}
}

// A connect that the container gives up after the server has acked it is answered, when the
// thread makes it again, with the connection that the server has, not a second one for the
// server to accept: the supervisor's connect waits for the ack, and the thread is interrupted
// while it does (this server is slow with it).
func TestAnInterruptedConnectDoesNotDialTwice(t *testing.T) {
	requireNamespaces(t)
	port := freePort(t)
	p := strconv.Itoa(port)
	l, err := net.Listen("tcp", "127.0.0.1:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer c.Close()
				var buf [PreambleSize]byte
				if _, err := io.ReadFull(c, buf[:]); err != nil {
					return
				}
				pre, err := ParsePreamble(buf[:], testKey)
				if err != nil {
					return
				}
				time.Sleep(400 * time.Millisecond)
				ack := pre.Ack(testKey)
				c.Write(ack[:])
				io.Copy(io.Discard, c)
			}()
		}
	}()
	vip := "10.43.0.77:" + p
	resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	client, out := runPod(t, podConfig(t, "10.244.1.9", Ports{uint16(port): {}}, resolver), "sigdial", vip)
	if err := client.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "connect <nil> local 10.244.1.9:") || !strings.Contains(out.String(), "peer "+vip) {
		t.Errorf("client: %s", out)
	}
	time.Sleep(200 * time.Millisecond)
	if got := accepted.Load(); got != 1 || client.Stats().Connects.Load() != 1 || len(client.Stats().Fallbacks.Snapshot()) != 0 {
		t.Errorf("the server accepted %d connections, connects %d, fallbacks %v; want one connection", got, client.Stats().Connects.Load(), client.Stats().Fallbacks.Snapshot())
	}
}

// slowHostPort is a resolver whose first answer to a bind is slow, so that the bind is held
// for a while: the call is given up meanwhile.
type slowHostPort struct {
	fakeResolver
	delay time.Duration
	calls atomic.Int32
}

func (r *slowHostPort) HostPort(pod netip.AddrPort) (uint16, bool) {
	if r.calls.Add(1) == 1 {
		time.Sleep(r.delay)
	}
	return r.fakeResolver.HostPort(pod)
}

// A bind that the container gives up before the supervisor has put the host socket in place
// is made again: the pod-side socket is bound by then and cannot be unbound, so the call that
// is made again goes on with it (see podBound), and the proxy at the pod's address works.
func TestAnInterruptedBindIsMadeAgain(t *testing.T) {
	requireLoopback(t)
	port := freePort(t)
	p := strconv.Itoa(port)
	server, out := runPod(t, podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, &slowHostPort{delay: 400 * time.Millisecond}), "sigbindone", p)
	if err := server.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"bind <nil>", "pod-side connect <nil>", "accept <nil> true"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if server.Stats().Binds.Load() != 1 || server.Stats().Accepted.Load() != 1 || len(server.Stats().Fallbacks.Snapshot()) != 0 {
		t.Errorf("binds %d, accepts %d, fallbacks %v", server.Stats().Binds.Load(), server.Stats().Accepted.Load(), server.Stats().Fallbacks.Snapshot())
	}
}

// Binds that are interrupted by signals, whenever they are, all succeed: a call that is given
// up is made again, and goes on from where it was. The pod-side proxy of the last one works.
func TestInterruptedBindsAreMadeAgain(t *testing.T) {
	requireLoopback(t)
	port := freePort(t)
	p := strconv.Itoa(port)
	server, out := runPod(t, podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, fakeResolver{}), "sigbind", p, "300")
	if err := server.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"binds failed 0 map[]", "pod-side connect <nil>", "accept <nil> true"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if server.Stats().Binds.Load() < 100 {
		t.Errorf("%d binds were swapped of 301: %v", server.Stats().Binds.Load(), server.Stats().Fallbacks.Snapshot())
	}
}

// A port that the pod's network has taken (another process of the pod listens on it) fails
// the bind with EADDRINUSE, as it would without the supervisor, and nothing is swapped.
func TestABindOfAPortTakenInThePodFailsWithAddrInUse(t *testing.T) {
	requireLoopback(t)
	port := freePort(t)
	free := port + 1
	if l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(free)); err != nil {
		t.Skipf("port %d is not free: %v", free, err)
	} else {
		l.Close()
	}
	marked := Ports{uint16(port): {}, uint16(free): {}}
	server, out := runPod(t, podConfig(t, "10.244.5.4", marked, fakeResolver{}), "bindtaken", strconv.Itoa(port))
	if err := server.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"bind taken address already in use", "bind free <nil>"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if n := server.Stats().Binds.Load(); n != 1 {
		t.Errorf("%d binds were swapped, want the free port's only", n)
	}
}

// A blocking listener that two threads of the server accept on, with one connection: the
// thread that does not get it is still in its call, and the supervisor's accept for it does
// not stand in the kernel's accept, which would not end with the container.
func TestTwoThreadsAcceptOnOneBlockingListener(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	server, out := runPod(t, podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, fakeResolver{}), "twoaccept", p)
	out.waitFor(t, "listening")
	time.Sleep(300 * time.Millisecond) // both calls are held
	c := forged(t, p, func(*Preamble, *[]byte) {})
	defer c.Close()
	ack := make([]byte, AckSize)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, ack); err != nil {
		t.Fatalf("no ack: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- server.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the supervisor does not end: an accept of it is stuck\n%s", out)
	}
	if !strings.Contains(out.String(), "accepted <nil>") || server.Stats().Accepted.Load() != 1 {
		t.Errorf("accepted %d: %s", server.Stats().Accepted.Load(), out)
	}
}

// An idle connection to a host port, one that sends no preamble, holds the server's accept
// for no longer than the preamble timeout, and the real client behind it is acked then.
func TestAnIdleConnectionDelaysAcceptByTheTimeoutAtMost(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	server, out := runPod(t, podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, fakeResolver{}), "serve", p)
	out.waitFor(t, "listening")
	start := time.Now()
	idle, err := net.Dial("tcp", "127.0.0.1:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	c := forged(t, p, func(*Preamble, *[]byte) {})
	ack := make([]byte, AckSize)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, ack); err != nil {
		t.Fatalf("no ack: %v", err)
	}
	if took := time.Since(start); took > 400*time.Millisecond { // the wait is 100 ms; it was a second
		t.Errorf("the real client was acked after %v behind an idle connection, want 100 ms and a little", took)
	}
	c.Close()
	if err := server.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if r := server.Stats().Rejected.Snapshot(); r["no-preamble"] != 1 {
		t.Errorf("rejections: %v", r)
	}
}

// Two ports of a pod that are served at one host port are swapped neither: both stay
// listeners of the pod network, which would have refused the second, and a port of its own
// is swapped.
func TestPortsThatShareAHostPortAreNotSwapped(t *testing.T) {
	var shared, own uint16
	for {
		shared = uint16(freePort(t))
		if shared < 65000 && shared%PodPortsPerPod != 0 {
			break
		}
	}
	own = shared + 1 // not equal modulo 16 to either
	marked := Ports{shared: {}, shared + PodPortsPerPod: {}, own: {}}
	cfg := podConfig(t, "10.244.5.4", marked, fakeResolver{})
	var mu sync.Mutex
	var logs []string
	cfg.Logf = func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	for _, port := range []uint16{shared, own} {
		server, out := runPod(t, cfg, "serve", strconv.Itoa(int(port)))
		out.waitFor(t, "listening")
		server.cmd.Process.Kill()
		server.Wait()
		want := uint64(0)
		if port == own {
			want = 1
		}
		if got := server.Stats().Binds.Load(); got != want {
			t.Errorf("port %d: %d binds, want %d", port, got, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	said := 0
	for _, l := range logs {
		if strings.Contains(l, fmt.Sprintf("direct-listen-ports [%d %d]", shared, shared+PodPortsPerPod)) {
			said++
		}
	}
	if said != 2 { // once for each supervisor, which logs it at its start
		t.Errorf("the ports were named %d times in %q", said, logs)
	}
}

// A connect that the container gives up after the socket was put in place, before it was
// answered (the signal comes between the two), is made again with a descriptor that is the
// connected host socket: it is answered with success, and does not make a second connection
// for the server to accept, nor is it told that it is connected.
func TestAConnectInterruptedAfterTheSwapIsAnsweredWhenMadeAgain(t *testing.T) {
	requireNamespaces(t)
	port := freePort(t)
	p := strconv.Itoa(port)
	l, err := net.Listen("tcp", "127.0.0.1:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer c.Close()
				var buf [PreambleSize]byte
				if _, err := io.ReadFull(c, buf[:]); err != nil {
					return
				}
				pre, err := ParsePreamble(buf[:], testKey)
				if err != nil {
					return
				}
				ack := pre.Ack(testKey)
				c.Write(ack[:])
				io.Copy(io.Discard, c)
			}()
		}
	}()
	vip := "10.43.0.77:" + p
	resolver := fakeResolver{vip: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort("127.0.0.1:" + p)}}
	var fired atomic.Int32
	cfg := podConfig(t, "10.244.1.9", Ports{uint16(port): {}}, resolver)
	cfg.beforeSend = interruptOnce(t, unix.SYS_CONNECT, &fired)
	client, out := runPod(t, cfg, "sigdial", vip, "quiet")
	if err := client.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "connect <nil> local 10.244.1.9:") || !strings.Contains(out.String(), "peer "+vip) {
		t.Errorf("client: %s", out)
	}
	time.Sleep(200 * time.Millisecond)
	if got := accepted.Load(); fired.Load() != 1 || got != 1 || client.Stats().Connects.Load() != 1 || len(client.Stats().Fallbacks.Snapshot()) != 0 {
		t.Errorf("interrupted %d times; the server accepted %d connections, connects %d, fallbacks %v; want one connection", fired.Load(), got, client.Stats().Connects.Load(), client.Stats().Fallbacks.Snapshot())
	}
}

// An accept that the container gives up after the connection was added to its table, before it
// was answered with the number, is made again and is answered with the same number: the
// connection is not added a second time (the container has no descriptor more than the
// connection), and the container reads what the client sent.
func TestAnAcceptInterruptedAfterTheDescriptorWasAddedIsAnsweredWhenMadeAgain(t *testing.T) {
	port := freePort(t)
	p := strconv.Itoa(port)
	var fired atomic.Int32
	cfg := podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, fakeResolver{})
	cfg.beforeSend = interruptOnce(t, unix.SYS_ACCEPT4, &fired)
	server, out := runPod(t, cfg, "sigaccept", p)
	out.waitFor(t, "listening")
	c := forged(t, p, func(*Preamble, *[]byte) {})
	defer c.Close()
	c.Write([]byte("hello"))
	if err := server.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	line := ""
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "accepted ") {
			line = l
		}
	}
	var fd, free, before, after int
	var from, read string
	if _, err := fmt.Sscanf(line, "accepted fd %d free %d fds %d %d from %s read %s", &fd, &free, &before, &after, &from, &read); err != nil {
		t.Fatalf("the container's line %q: %v\n%s", line, err, out)
	}
	if fd != free || after != before+1 || read != "hello" || !strings.HasPrefix(from, "10.244.1.9:") {
		t.Errorf("accepted descriptor %d where %d was free, descriptors %d before and %d after, from %s, read %q", fd, free, before, after, from, read)
	}
	if fired.Load() != 1 || server.Stats().Accepted.Load() != 1 || len(server.Stats().Rejected.Snapshot()) != 0 {
		t.Errorf("interrupted %d times, accepts %d, rejected %v", fired.Load(), server.Stats().Accepted.Load(), server.Stats().Rejected.Snapshot())
	}
}

// A connection that a given-up accept installed is taken for the restarted call only where the
// process still has it at its number: the same process, the same flags, the same socket.
func TestAnInstalledConnectionIsRecognised(t *testing.T) {
	if b, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope"); err == nil && strings.TrimSpace(string(b)) != "0" && strings.TrimSpace(string(b)) != "1" {
		t.Skip("the supervisor cannot look into its child under this Yama ptrace_scope")
	}
	theirs, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	mine, err := unix.Dup(theirs) // the same socket, as the supervisor's descriptor of the container's
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(mine)
	other, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(other)
	otherDup, err := unix.Dup(other)
	if err != nil {
		t.Fatal(err)
	}
	theirFiles := []*os.File{os.NewFile(uintptr(theirs), "theirs"), os.NewFile(uintptr(otherDup), "other")} // its 3 and 4
	cmd := exec.Command("sleep", "30")
	cmd.ExtraFiles = theirFiles
	err = cmd.Start()
	for _, f := range theirFiles {
		f.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	s := &Supervisor{targets: newTargets()}
	defer s.targets.closeAll()
	pid := cmd.Process.Pid
	at := func(k keptAccept, conn, flags int) bool { return s.installedAt(pid, k, conn, flags) }
	if !at(keptAccept{tgid: pid, fd: 3, flags: 7}, mine, 7) {
		t.Error("the connection is at its number and was not recognised")
	}
	for name, ok := range map[string]bool{
		"not installed":   at(keptAccept{fd: 3, flags: 7}, mine, 7),
		"another process": at(keptAccept{tgid: pid + 1, fd: 3, flags: 7}, mine, 7),
		"another number":  at(keptAccept{tgid: pid, fd: 4, flags: 7}, mine, 7),
		"no such number":  at(keptAccept{tgid: pid, fd: 9, flags: 7}, mine, 7),
		"another socket":  at(keptAccept{tgid: pid, fd: 3, flags: 7}, other, 7),
		"different flags": at(keptAccept{tgid: pid, fd: 3, flags: 7}, mine, 5),
	} {
		if ok {
			t.Errorf("a connection that is not there (%s) was recognised", name)
		}
	}
}
