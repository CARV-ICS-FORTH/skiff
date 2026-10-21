//go:build linux && (amd64 || arm64)

package direct

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// loUp brings up the loopback interface of the network namespace the "container" is in,
// which is down in a new one.
func loUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	ifr.SetUint16(unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// requireLoopback skips the test where a user and network namespace of an unprivileged
// user cannot bring up its loopback interface: it is made but has no capabilities in it,
// as under Ubuntu's AppArmor restriction of unprivileged user namespaces.
func requireLoopback(t testing.TB) {
	t.Helper()
	requireNamespaces(t)
	loopbackOnce.Do(func() {
		out, err := helperCmd("loup", true).CombinedOutput()
		if err != nil {
			loopbackErr = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
	})
	if loopbackErr != nil {
		t.Skipf("the loopback of an unprivileged network namespace cannot be brought up: %v", loopbackErr)
	}
}

var (
	loopbackOnce sync.Once
	loopbackErr  error
)

// localDial connects to the listener of the "container" from inside its network namespace,
// writes a word and closes.
func localDial(port string) {
	c, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Println("error local dial", err)
		return
	}
	fmt.Println("local dialed", describe(nil, c.(*net.TCPConn)))
	c.Write([]byte("hello"))
	c.Close()
}

// acceptAndSay accepts a connection and prints its addresses and what it carries.
func acceptAndSay(l *net.TCPListener, word string) bool {
	c, err := l.Accept()
	if err != nil {
		fmt.Println("error accept", err)
		return false
	}
	defer c.Close()
	buf := make([]byte, 5)
	n, _ := c.Read(buf)
	fmt.Println(word, describe(nil, c.(*net.TCPConn)), "data="+string(buf[:n]))
	return true
}

func waitForFile(path string) {
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func init() {
	helpers["loup"] = func([]string) int {
		if err := loUp(); err != nil {
			fmt.Println(err)
			return 1
		}
		return 0
	}
	// serveboth PORT: a server that takes a connection from a client of another pod and one
	// from a client in its own pod, on the loopback.
	helpers["serveboth"] = func(args []string) int {
		if err := loUp(); err != nil {
			fmt.Println("error lo", err)
			return 2
		}
		l, err := net.Listen("tcp", ":"+args[0])
		if err != nil {
			fmt.Println("error", err)
			return 2
		}
		fmt.Println("listening")
		go localDial(args[0])
		for i := 0; i < 2; i++ {
			if !acceptAndSay(l.(*net.TCPListener), "accepted") {
				return 2
			}
		}
		return 0
	}
	// twolisteners PORT: an IPv4 and an IPv6-only listener at one port, as a server that
	// listens on every address of the host often has (Python's asyncio).
	helpers["twolisteners"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		a, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		b, _ := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM, 0)
		unix.SetsockoptInt(b, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 1)
		if err := unix.Bind(a, &unix.SockaddrInet4{Port: port}); err != nil {
			fmt.Println("error bind 4", err)
			return 2
		}
		if err := unix.Bind(b, &unix.SockaddrInet6{Port: port}); err != nil {
			fmt.Println("error bind 6", err)
			return 2
		}
		unix.Listen(a, 8)
		unix.Listen(b, 8)
		fmt.Println("listening")
		time.Sleep(10 * time.Second)
		return 0
	}
	// latelisten PORT LISTEN [DIAL]: a server that binds its port (with SO_REUSEADDR, as most
	// do) and listens only when the file LISTEN appears; if DIAL, a client of its pod connects
	// to the port while it is bound and does not listen (it is refused, as on any host), and
	// another after it has listened and takes the connection.
	helpers["latelisten"] = func(args []string) int {
		if err := loUp(); err != nil {
			return 2
		}
		port, _ := strconv.Atoi(args[0])
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		if err := unix.Bind(fd, &unix.SockaddrInet4{Port: port}); err != nil {
			fmt.Println("error bind", err)
			return 2
		}
		fmt.Println("bound")
		if len(args) > 2 {
			c, err := net.Dial("tcp", "127.0.0.1:"+args[0])
			if err == nil {
				c.Close()
			}
			fmt.Println("dialed while bound", err)
		}
		waitForFile(args[1])
		if err := unix.Listen(fd, 8); err != nil {
			fmt.Println("error listen", err)
			return 2
		}
		f := os.NewFile(uintptr(fd), "listener")
		l, err := net.FileListener(f)
		if err != nil {
			fmt.Println("error", err)
			return 2
		}
		fmt.Println("listening")
		go localDial(args[0])
		if !acceptAndSay(l.(*net.TCPListener), "accepted") {
			return 2
		}
		return 0
	}
	// relisten PORT CLOSE AGAIN [DIAL]: a server that closes its listener when the file CLOSE
	// appears (and a client of its pod then connects, if DIAL) and listens at the same port
	// again when AGAIN appears, then takes a connection of its own pod.
	helpers["relisten"] = func(args []string) int {
		if err := loUp(); err != nil {
			return 2
		}
		l, err := net.Listen("tcp", ":"+args[0])
		if err != nil {
			fmt.Println("error", err)
			return 2
		}
		fmt.Println("listening")
		waitForFile(args[1])
		l.Close()
		fmt.Println("closed")
		if len(args) > 3 {
			localDial(args[0]) // the pod-side socket is still there: it takes the connection and drops it
		}
		waitForFile(args[2])
		l, err = net.Listen("tcp", ":"+args[0])
		if err != nil {
			fmt.Println("error relisten", err)
			return 2
		}
		fmt.Println("listening again")
		go localDial(args[0])
		if !acceptAndSay(l.(*net.TCPListener), "accepted") {
			return 2
		}
		return 0
	}
}

func (s *Supervisor) proxyCount() int {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	return len(s.proxies)
}

// A listener that the supervisor put on the host stays reachable inside the pod: a client of
// the pod on the loopback is carried to it with its real address, next to a client of
// another pod that came the direct way.
func TestSwappedListenerIsReachableInThePod(t *testing.T) {
	requireLoopback(t)
	port := freePort(t)
	p := strconv.Itoa(port)
	marked := Ports{uint16(port): {}}
	hostPort := freePort(t)
	cfg := podConfig(t, "10.244.5.4", marked, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(hostPort)}})
	server, serverOut := runPod(t, cfg, "serveboth", p)
	serverOut.waitFor(t, "listening")

	resolver := fakeResolver{"10.43.0.77:" + p: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", hostPort))}}
	client, clientOut := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "dial", "10.43.0.77:"+p, "10")
	if err := client.Wait(); err != nil {
		t.Fatalf("client: %v\n%s", err, clientOut)
	}
	if err := server.Wait(); err != nil {
		t.Fatalf("server: %v\n%s", err, serverOut)
	}
	out := serverOut.String()
	var local, remote string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "accepted ") {
			continue
		}
		if strings.Contains(line, "remote=127.0.0.1:") {
			local = line
		} else if strings.Contains(line, "remote=10.244.1.9:") {
			remote = line
		}
	}
	if remote == "" || local == "" {
		t.Fatalf("the server did not get both clients:\n%s", out)
	}
	// The client of the pod: its real address and the address it dialed, as the server's own
	// names for the socket say it, through Go and through the system calls.
	clientAddr := field(t, out, "local", "local") // "local dialed local=..."
	for _, key := range []string{"remote", "rawremote"} {
		if got := tokenOf(local, key); got != clientAddr {
			t.Errorf("accepted %s = %s, want the client's own %s", key, got, clientAddr)
		}
	}
	for _, key := range []string{"local", "rawlocal"} {
		if got := tokenOf(local, key); got != "127.0.0.1:"+p {
			t.Errorf("accepted %s = %s, want 127.0.0.1:%s", key, got, p)
		}
	}
	if !strings.Contains(local, "data=hello") {
		t.Errorf("the proxied connection carried %q", local)
	}
	if server.Stats().Proxied.Load() != 1 || server.Stats().Accepted.Load() != 2 || server.Stats().ProxyFailed.Load() != 0 {
		t.Errorf("proxied %d, accepted %d, failed %d", server.Stats().Proxied.Load(), server.Stats().Accepted.Load(), server.Stats().ProxyFailed.Load())
	}
}

// tokenOf is the value of key= in a line of key=value words.
func tokenOf(line, key string) string {
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v
		}
	}
	return ""
}

// The pod-side socket lives as long as the container's listener: when the container closes
// it the proxy goes (it is noticed within a few seconds), and a container that binds the
// port again at once, before it is noticed, is not told the port is taken.
func TestProxyEndsWithTheListener(t *testing.T) {
	requireLoopback(t)
	for _, tc := range []struct {
		name    string
		prompt  bool // bind again before the proxy has been noticed to be out of date
		dial    bool // a client of the pod connects after the listener has closed
		timeout time.Duration
	}{{"noticed", false, false, 4 * time.Second}, {"noticed by a client", false, true, 800 * time.Millisecond}, {"rebound at once", true, false, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			port := freePort(t)
			p := strconv.Itoa(port)
			cfg := podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(freePort(t))}})
			dir := t.TempDir()
			closeFile, againFile := filepath.Join(dir, "close"), filepath.Join(dir, "again")
			args := []string{p, closeFile, againFile}
			if tc.dial {
				args = append(args, "dial")
			}
			server, out := runPod(t, cfg, "relisten", args...)
			out.waitFor(t, "listening")
			if server.proxyCount() != 1 {
				t.Fatalf("%d proxies after the bind", server.proxyCount())
			}
			os.WriteFile(closeFile, nil, 0o600)
			out.waitFor(t, "closed")
			closedAt := time.Now()
			if !tc.prompt {
				deadline := closedAt.Add(tc.timeout)
				for server.proxyCount() != 0 {
					if time.Now().After(deadline) {
						t.Fatal("the proxy of a closed listener is still there")
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			os.WriteFile(againFile, nil, 0o600)
			if err := server.Wait(); err != nil {
				t.Fatalf("server: %v\n%s", err, out)
			}
			if !strings.Contains(out.String(), "listening again") || !strings.Contains(out.String(), "accepted") {
				t.Errorf("the container could not listen again, or the new listener is not reachable in the pod:\n%s", out)
			}
		})
	}
}

// A server that listens at one port on an IPv4 and an IPv6 socket has a pod-side socket for
// each, and the second does not replace the first.
func TestEachListenerOfAPortHasItsProxy(t *testing.T) {
	port := freePort(t)
	cfg := podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(freePort(t))}})
	server, out := runPod(t, cfg, "twolisteners", strconv.Itoa(port))
	out.waitFor(t, "listening")
	defer func() { server.cmd.Process.Kill(); server.Wait() }()
	if server.Stats().Swapped() != 2 || server.proxyCount() != 2 {
		t.Errorf("swapped %d, %d proxies; want 2 and 2\n%s", server.Stats().Swapped(), server.proxyCount(), out)
	}
}

// A container that binds its port and listens later (a socket bound with SO_REUSEADDR does not
// keep another such socket from the port, and a connection to it is refused) has not closed
// its listener: the pod-side socket stays, however long it takes and whoever tries the port
// meanwhile, and carries the connection that comes after the listen.
func TestProxyStaysWhileTheContainerIsBoundAndNotListening(t *testing.T) {
	requireLoopback(t)
	for _, tc := range []struct {
		name string
		dial bool // a client of the pod finds the port refused while it is bound
	}{{"nobody tries", false}, {"a client tries", true}} {
		t.Run(tc.name, func(t *testing.T) {
			port := freePort(t)
			p := strconv.Itoa(port)
			cfg := podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(freePort(t))}})
			listenFile := filepath.Join(t.TempDir(), "listen")
			args := []string{p, listenFile}
			if tc.dial {
				args = append(args, "dial")
			}
			server, out := runPod(t, cfg, "latelisten", args...)
			out.waitFor(t, "bound")
			if tc.dial {
				out.waitFor(t, "dialed while bound")
			}
			time.Sleep(3 * time.Second) // more than the proxy's looks at the host port take to decide
			if server.proxyCount() != 1 {
				t.Errorf("the pod-side socket was closed while the container was bound and not yet listening (%d proxies)", server.proxyCount())
			}
			os.WriteFile(listenFile, nil, 0o600)
			if err := server.Wait(); err != nil {
				t.Fatalf("server: %v\n%s", err, out)
			}
			if o := out.String(); !strings.Contains(o, "accepted") || !strings.Contains(o, "data=hello") {
				t.Errorf("the connection of the pod's client after the listen was not carried:\n%s", o)
			}
		})
	}
}

// A stream is carried whole, in both directions, through the copy of the proxy, and its end
// (a half-close) is passed on.
func TestCopyConnCarriesAStream(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		accepted <- c
	}()
	a, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b := <-accepted
	defer b.Close()

	want := make([]byte, 3<<20+17) // more than a buffer, and not a multiple of it
	for i := range want {
		want[i] = byte(i * 7)
	}
	go func() {
		a.Write(want)
		a.(*net.TCPConn).CloseWrite()
	}()
	var got bytes.Buffer
	done := make(chan int64, 1)
	pr, pw := net.Pipe() // the other side of the copy: anything that is a connection
	go func() {
		n, _ := copyConn(pw, b)
		pw.Close()
		done <- n
	}()
	io.Copy(&got, pr)
	if n := <-done; n != int64(len(want)) || !bytes.Equal(got.Bytes(), want) {
		t.Errorf("carried %d bytes, intact %t; want %d", n, bytes.Equal(got.Bytes(), want), len(want))
	}
}
