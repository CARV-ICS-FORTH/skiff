//go:build linux && (amd64 || arm64)

package direct

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func init() {
	// sigacceptown PORT: with a marked port bound, an accept on a listener of its own (see
	// TestAnInterruptedAcceptOfItsOwnListenerKeepsItsConnection).
	helpers["sigacceptown"] = func(args []string) int {
		port, _ := strconv.Atoi(args[0])
		if err := loUp(); err != nil {
			fmt.Println("error lo", err)
			return 2
		}
		marked, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err := unix.Bind(marked, &unix.SockaddrInet4{Port: port}); err != nil {
			fmt.Println("error bind", err)
			return 2
		}
		fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		_ = unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}})
		_ = unix.Listen(fd, 8)
		sa, _ := unix.Getsockname(fd)
		cl, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err := unix.Connect(cl, sa); err != nil {
			fmt.Println("error connect", err)
			return 2
		}
		_, _ = unix.Write(cl, []byte("hello"))
		runtime.LockOSThread()
		before := openFds()
		free := 0
		for slices.Contains(before, free) {
			free++
		}
		nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		after := openFds()
		if err != nil {
			fmt.Println("error accept", err)
			return 2
		}
		var buf [5]byte
		n, _ := unix.Read(nfd, buf[:])
		if nfd == free && len(after) == len(before)+1 {
			fmt.Println("the descriptor that was free, one more,", string(buf[:n]))
		} else {
			fmt.Println("descriptor", nfd, "where", free, "was free, descriptors", len(before), len(after), string(buf[:n]))
		}
		return 0
	}
	// raceguard ADDR HOST_ADDR: with a socket of the host's that has been reset, one thread keeps
	// changing what a descriptor is while another keeps connecting it to a service of the host's.
	helpers["raceguard"] = func(args []string) int {
		ap := netip.MustParseAddrPort(args[0])
		out := netip.MustParseAddrPort(args[1])
		hostSock, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err := unix.Connect(hostSock, sockaddrFor(ap, unix.AF_INET)); err != nil {
			fmt.Println("marked connect failed", err)
			return 2
		}
		_, _ = unix.Write(hostSock, []byte("reset me"))
		time.Sleep(100 * time.Millisecond)
		own, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		const fd = 200
		_ = unix.Dup2(own, fd)
		done := make(chan struct{})
		go func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				_ = unix.Dup2(hostSock, fd)
				_ = unix.Dup2(own, fd)
			}
		}()
		for start := time.Now(); time.Since(start) < 1500*time.Millisecond; {
			_ = unix.Connect(fd, sockaddrFor(out, unix.AF_INET))
		}
		close(done)
		fmt.Println("finished")
		return 0
	}
	// calls DIR ADDR: with a socket of the host's in the container, what is run for it of a process's
	// other sockets: its Unix sockets by absolute and relative paths and abstract names, a
	// listener on its own network that it accepts from, names and interfaces.
	helpers["calls"] = func(args []string) int {
		dir, ap := args[0], netip.MustParseAddrPort(args[1])
		if code := helpers["loup"](nil); code != 0 {
			return code
		}
		hostSock, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if err := unix.Connect(hostSock, sockaddrFor(ap, unix.AF_INET)); err != nil {
			fmt.Println("marked connect failed", err)
			return 2
		}
		unix.Umask(0o027)
		pair := func(name string, sa *unix.SockaddrUnix) {
			l, _ := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			defer unix.Close(l)
			if err := unix.Bind(l, sa); err != nil {
				fmt.Println(name, "bind:", err)
				return
			}
			unix.Listen(l, 1)
			c, _ := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			defer unix.Close(c)
			fmt.Println(name, "connect:", unix.Connect(c, sa))
			nfd, _, err := unix.Accept4(l, unix.SOCK_CLOEXEC)
			fmt.Println(name, "accept:", err)
			if err == nil {
				unix.Close(nfd)
			}
		}
		pair("absolute", &unix.SockaddrUnix{Name: filepath.Join(dir, "abs.sock")})
		if info, err := os.Stat(filepath.Join(dir, "abs.sock")); err == nil {
			fmt.Printf("its mode: %o\n", info.Mode().Perm())
		}
		named, _ := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		_ = unix.Bind(named, &unix.SockaddrUnix{Name: filepath.Join(dir, "named.sock")})
		if sa, err := unix.Getsockname(named); err == nil {
			fmt.Println("its own address:", sa.(*unix.SockaddrUnix).Name == filepath.Join(dir, "named.sock"))
		}
		_ = os.Chdir(dir)
		pair("relative", &unix.SockaddrUnix{Name: "rel.sock"})
		pair("abstract", &unix.SockaddrUnix{Name: "@skiff-test-" + strconv.Itoa(os.Getpid())})
		_ = os.Symlink(filepath.Join(dir, "abs.sock"), filepath.Join(dir, "link.sock"))
		c, _ := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		fmt.Println("nothing there:", unix.Connect(c, &unix.SockaddrUnix{Name: filepath.Join(dir, "none.sock")}))
		_ = os.Mkdir("below", 0o755)
		_ = os.Chdir("below")
		up, _ := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		lu, _ := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		_ = unix.Bind(lu, &unix.SockaddrUnix{Name: "../up.sock"})
		_ = unix.Listen(lu, 1)
		fmt.Println("up from the working directory:", unix.Connect(up, &unix.SockaddrUnix{Name: "../up.sock"}))
		s, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		fmt.Println("its own bind:", unix.Bind(s, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}))
		fmt.Println("its own listen:", unix.Listen(s, 4))
		sa, err := unix.Getsockname(s)
		fmt.Println("its own name:", err)
		cl, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		fmt.Println("its own connect:", unix.Connect(cl, sa))
		nfd, peer, err := unix.Accept4(s, unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		fmt.Println("its own accept:", err)
		if err == nil {
			flags, _ := unix.FcntlInt(uintptr(nfd), unix.F_GETFL, 0)
			fmt.Println("with its flags:", flags&unix.O_NONBLOCK != 0, "and the peer:", peer != nil)
		}
		fmt.Println("an ioctl that asks:", unix.IoctlSetInt(s, unix.SIOCSIFMTU, 1400) != nil)
		_, ierr := unix.IoctlGetInt(s, unix.SIOCGIFINDEX)
		fmt.Println("an interface name it does not have:", ierr != nil)
		_, atmark := unix.IoctlGetInt(cl, unix.SIOCATMARK)
		fmt.Println("an ioctl of the socket itself:", atmark)
		dgram, _ := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
		_, _, e := unix.Accept4(dgram, 0)
		fmt.Println("accept of what is not a listener:", e)
		_, _, unknown := unix.Syscall6(unix.SYS_ACCEPT4, uintptr(s), 0, 0, 1, 0, 0)
		fmt.Println("accept with flags that do not exist:", unknown)
		lt, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		_ = unix.Bind(lt, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}})
		_ = unix.Listen(lt, 1)
		_ = unix.SetsockoptTimeval(lt, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 300000})
		_, _, e = unix.Accept4(lt, 0)
		fmt.Println("accept past its timeout:", e)
		var fo [1]byte
		fmt.Println("a send that connects:", unix.Sendto(cl, fo[:], unix.MSG_FASTOPEN, sa))
		return 0
	}
}

// Changing what a descriptor is, from another thread, while it is being connected does not get a
// socket of the host's connected anywhere: what the supervisor looked at is what it runs.
// An accept on a listener of the container's own, once it has a socket of the host's, that is given
// up between the descriptor's being added and the answer: the call made again has that descriptor.
func TestAnInterruptedAcceptOfItsOwnListenerKeepsItsConnection(t *testing.T) {
	requireLoopback(t)
	port := freePort(t)
	var fired atomic.Int32
	cfg := podConfig(t, "10.244.5.4", Ports{uint16(port): {}}, fakeResolver{})
	cfg.beforeSend = interruptOnce(t, unix.SYS_ACCEPT4, &fired)
	server, out := runPod(t, cfg, "sigacceptown", strconv.Itoa(port))
	if err := server.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "the descriptor that was free, one more, hello") {
		t.Errorf("the container says:\n%s", out)
	}
	if fired.Load() != 1 || server.Stats().Accepted.Load() != 0 {
		t.Errorf("interrupted %d times, accepts on swapped listeners %d", fired.Load(), server.Stats().Accepted.Load())
	}
}

func TestARacingThreadDoesNotConnectAHostSocket(t *testing.T) {
	requireLoopback(t)
	svc, reached := hostService(t)
	port, hostPort := freePort(t), freePort(t)
	p := strconv.Itoa(port)
	marked := Ports{uint16(port): {}}
	server, serverOut := runPod(t, podConfig(t, "10.244.5.4", marked, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(hostPort)}}), "reset", p)
	serverOut.waitFor(t, "listening")
	resolver := fakeResolver{"10.43.0.77:" + p: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(hostPort))}}
	c, out := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "raceguard", "10.43.0.77:"+p, svc.Addr().String())
	c.Wait()
	server.Wait()
	wantLines(t, out.String(), "finished")
	notReached(t, reached, 0)
}

// With a socket of the host's in the container, the calls on its other sockets still do what they do.
func TestCallsRunByTheSupervisor(t *testing.T) {
	requireLoopback(t)
	port, hostPort := freePort(t), freePort(t)
	p := strconv.Itoa(port)
	marked := Ports{uint16(port): {}}
	server, serverOut := runPod(t, podConfig(t, "10.244.5.4", marked, hostPorts{fakeResolver{}, map[uint16]uint16{uint16(port): uint16(hostPort)}}), "acceptloop", p)
	serverOut.waitFor(t, "listening")
	defer func() {
		server.cmd.Process.Kill()
		server.Wait()
	}()
	resolver := fakeResolver{"10.43.0.77:" + p: {Pod: netip.MustParseAddrPort("10.244.5.4:" + p), Host: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(hostPort))}}
	dir := t.TempDir()
	c, out := runPod(t, podConfig(t, "10.244.1.9", marked, resolver), "calls", dir, "10.43.0.77:"+p)
	c.Wait()
	wantLines(t, out.String(),
		"absolute connect: <nil>", "absolute accept: <nil>", "its mode: 750",
		"relative connect: <nil>", "relative accept: <nil>",
		"abstract connect: <nil>", "abstract accept: <nil>",
		"nothing there: no such file or directory",
		"up from the working directory: <nil>",
		"its own address: true",
		"an ioctl of the socket itself: <nil>",
		"accept of what is not a listener: operation not supported",
		"accept with flags that do not exist: invalid argument",
		"accept past its timeout: resource temporarily unavailable",
		"its own bind: <nil>", "its own listen: <nil>", "its own name: <nil>", "its own connect: <nil>", "its own accept: <nil>",
		"with its flags: true and the peer: true",
		"an ioctl that asks: true",
		"an interface name it does not have: true",
		"a send that connects: operation not supported")
	if _, err := net.DialTimeout("unix", filepath.Join(dir, "abs.sock"), time.Second); err == nil {
		t.Error("nothing should be listening any more")
	}
}
