//go:build linux && (amd64 || arm64)

package direct

import (
	"errors"
	"fmt"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

// The calls that put a socket of the host's network namespace in place of the container's.
// The container made its socket in its pod's namespace and has not used it yet; the
// supervisor takes one of the host's from the factory, does with it what the call asked,
// and gives it the descriptor number the container used (SECCOMP_IOCTL_NOTIF_ADDFD with
// SETFD, which closes the container's socket). Where the call is decided here its answer
// is a value, and the kernel never runs it: connect has been made, bind has been made.

const (
	// connectTimeout bounds the supervisor's connect. A connect that the container made
	// non-blocking would not have waited for it, but it could not be answered before.
	connectTimeout = 3 * time.Second
	// preambleTimeout bounds the wait for a client's preamble: a connection that does not
	// send one is not a client of ours.
	preambleTimeout = time.Second
)

// heldSocket is a container's socket as the supervisor sees it through a duplicate.
type heldSocket struct {
	dup      int // the supervisor's descriptor of the container's socket
	family   int
	nonblock bool
	cloexec  bool
}

var errNotOurs = errors.New("not a TCP socket of IPv4 or IPv6")

// inspect duplicates descriptor fd of the held thread's process and says what it is.
func (s *Supervisor) inspect(tid, fd int) (*heldSocket, error) {
	dup, err := s.targets.getfd(tid, fd)
	if err != nil {
		return nil, err
	}
	h := &heldSocket{dup: dup}
	if h.family, err = unix.GetsockoptInt(dup, unix.SOL_SOCKET, unix.SO_DOMAIN); err != nil {
		unix.Close(dup)
		return nil, err
	}
	typ, err := unix.GetsockoptInt(dup, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil || typ != unix.SOCK_STREAM || (h.family != unix.AF_INET && h.family != unix.AF_INET6) {
		unix.Close(dup)
		return nil, errNotOurs
	}
	flags, err := unix.FcntlInt(uintptr(dup), unix.F_GETFL, 0)
	if err != nil {
		unix.Close(dup)
		return nil, err
	}
	h.nonblock = flags&unix.O_NONBLOCK != 0
	if h.cloexec, err = fdCloexec(tid, fd); err != nil {
		unix.Close(dup)
		return nil, err
	}
	return h, nil
}

func (h *heldSocket) close() { unix.Close(h.dup) }

// sockaddrArg reads the address a held connect or bind was given.
func sockaddrArg(n *notif) (netip.AddrPort, bool) {
	length := int(n.Args[2])
	if length < 8 {
		return netip.AddrPort{}, false
	}
	buf := make([]byte, min(length, sockaddrInet6Size))
	if err := readRemote(int(n.Pid), n.Args[1], buf); err != nil {
		return netip.AddrPort{}, false
	}
	return parseSockaddr(buf)
}

// copyOptions gives the host socket the options the container set on its own. What the
// container sets after the swap is set on the host socket itself.
func copyOptions(from, to, family int) {
	copyInt := func(level, opt int, halve bool) {
		v, err := unix.GetsockoptInt(from, level, opt)
		if err != nil {
			return
		}
		if halve { // the kernel doubles a buffer size when it is set: set it only if it was set
			if def, err := unix.GetsockoptInt(to, level, opt); err != nil || def == v {
				return
			}
			v /= 2
		}
		_ = unix.SetsockoptInt(to, level, opt, v)
	}
	copyInt(unix.SOL_SOCKET, unix.SO_REUSEADDR, false)
	copyInt(unix.SOL_SOCKET, unix.SO_REUSEPORT, false)
	copyInt(unix.SOL_SOCKET, unix.SO_KEEPALIVE, false)
	copyInt(unix.SOL_SOCKET, unix.SO_SNDBUF, true)
	copyInt(unix.SOL_SOCKET, unix.SO_RCVBUF, true)
	copyInt(unix.IPPROTO_TCP, unix.TCP_NODELAY, false)
	if family == unix.AF_INET6 {
		copyInt(unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, false) // the sysctl of the host is not the pod's
	}
	if cc, err := unix.GetsockoptString(from, unix.IPPROTO_TCP, unix.TCP_CONGESTION); err == nil && cc != "" {
		_ = unix.SetsockoptString(to, unix.IPPROTO_TCP, unix.TCP_CONGESTION, cc)
	}
}

// put gives the container's descriptor fd the socket sock, and returns the kernel's
// verdict on the held call: answered, or that the call has been cancelled meanwhile.
func (s *Supervisor) put(n *notif, fd int, sock int, cloexec bool) error {
	if !ioctlValid(s.listener, n.ID) {
		return errCancelled
	}
	a := notifAddfd{ID: n.ID, Flags: unix.SECCOMP_ADDFD_FLAG_SETFD, Srcfd: uint32(sock), Newfd: uint32(fd)}
	if cloexec {
		a.NewfdFlags = unix.O_CLOEXEC
	}
	_, err := ioctlAddfd(s.listener, &a)
	return err
}

// errCancelled says that the container's thread gave up the call (a signal, or it died):
// there is nobody to answer.
var errCancelled = errors.New("the call was cancelled")

var cancelled = reply{answered: true}

func cookieOf(fd int) uint64 {
	c, _ := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_COOKIE)
	return c
}

func unspecifiedFor(family int) netip.Addr {
	if family == unix.AF_INET6 {
		return netip.IPv6Unspecified()
	}
	return netip.IPv4Unspecified()
}

// connect: a marked Service port goes to its backend on the host network. The container
// is told the connection is made; the backend's supervisor is told, in the preamble, who
// the client is and which pod it means.
func (s *Supervisor) connect(n *notif) reply {
	dst, ok := sockaddrArg(n)
	if !ok || !s.cfg.Ports.Has(dst.Port()) {
		return letThrough
	}
	backend, ok := s.cfg.Resolver.Resolve(dst)
	if !ok {
		return letThrough
	}
	tid, fd := int(n.Pid), int(n.Args[0])
	held, err := s.inspect(tid, fd)
	if err != nil {
		if err != errNotOurs {
			s.stats.Errors.Add(1)
			s.logf("connect %s: %v", dst, err)
		}
		return letThrough
	}
	defer held.close()
	if _, err := unix.Getpeername(held.dup); err == nil {
		return letThrough // connected already: the kernel says EISCONN
	}
	if sa, err := unix.Getsockname(held.dup); err == nil && addrPortOf(sa).Port() != 0 {
		return letThrough // the container chose its address: the pod network's to give
	}

	sock, err := s.cfg.Factory.Socket(held.family)
	if err != nil {
		s.stats.Errors.Add(1)
		s.logf("connect %s: %v", dst, err)
		return letThrough
	}
	defer unix.Close(sock)
	copyOptions(held.dup, sock, held.family)

	timeout := unix.NsecToTimeval(int64(connectTimeout))
	_ = unix.SetsockoptTimeval(sock, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &timeout)
	if err := unix.Connect(sock, sockaddrFor(backend.Host, held.family)); err != nil {
		if err == unix.EINPROGRESS {
			err = unix.ETIMEDOUT // the timeout of a blocking connect
		}
		s.logf("connect %s: to %s: %v", dst, backend.Host, err)
		return failWith(errnoOf(err))
	}
	_ = unix.SetsockoptTimeval(sock, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &unix.Timeval{})
	sa, err := unix.Getsockname(sock)
	if err != nil {
		return failWith(errnoOf(err))
	}
	local := netip.AddrPortFrom(s.cfg.PodIP, addrPortOf(sa).Port())
	pre, err := Preamble{Client: local, Backend: backend.Pod}.Marshal()
	if err != nil {
		s.logf("connect %s: %v", dst, err)
		return failWith(unix.EAFNOSUPPORT)
	}
	if _, err := unix.Write(sock, pre[:]); err != nil {
		return failWith(errnoOf(err))
	}
	if held.nonblock {
		_ = unix.SetNonblock(sock, true)
	}
	cookie := cookieOf(sock)
	if err := s.put(n, fd, sock, held.cloexec); err != nil {
		return s.putFailed("connect", err)
	}
	s.table.Add(cookie, Socket{Kind: KindClient, Family: held.family, Local: local, Peer: dst})
	s.stats.Swapped.Add(1)
	s.logf("connect %s: on the host, %s to %s, as %s", dst, addrPortOf(sa), backend.Host, local)
	return succeed()
}

func (s *Supervisor) putFailed(call string, err error) reply {
	if errors.Is(err, errCancelled) {
		return cancelled
	}
	s.stats.Errors.Add(1)
	s.logf("%s: putting the socket in place: %v", call, err)
	return failWith(errnoOf(err))
}

// bind: a marked port is bound on the host, on every address, as the pod's own port.
// listen then runs on the host socket.
func (s *Supervisor) bind(n *notif) reply {
	want, ok := sockaddrArg(n)
	if !ok || !s.cfg.Ports.Has(want.Port()) {
		return letThrough
	}
	if !want.Addr().IsUnspecified() && want.Addr() != s.cfg.PodIP {
		return letThrough // the pod's loopback or another address: the pod network's
	}
	tid, fd := int(n.Pid), int(n.Args[0])
	held, err := s.inspect(tid, fd)
	if err != nil {
		if err != errNotOurs {
			s.stats.Errors.Add(1)
			s.logf("bind %s: %v", want, err)
		}
		return letThrough
	}
	defer held.close()
	if sa, err := unix.Getsockname(held.dup); err == nil && addrPortOf(sa).Port() != 0 {
		return letThrough // bound already: the kernel says EINVAL
	}
	sock, err := s.cfg.Factory.Socket(held.family)
	if err != nil {
		s.stats.Errors.Add(1)
		s.logf("bind %s: %v", want, err)
		return letThrough
	}
	defer unix.Close(sock)
	copyOptions(held.dup, sock, held.family)
	host := netip.AddrPortFrom(unspecifiedFor(held.family), want.Port())
	if err := unix.Bind(sock, sockaddrFor(host, held.family)); err != nil {
		s.logf("bind %s: on the host: %v", want, err)
		return failWith(errnoOf(err))
	}
	if held.nonblock {
		_ = unix.SetNonblock(sock, true)
	}
	cookie := cookieOf(sock)
	if err := s.put(n, fd, sock, held.cloexec); err != nil {
		return s.putFailed("bind", err)
	}
	s.table.Add(cookie, Socket{Kind: KindListener, Family: held.family, Local: want})
	s.stats.Swapped.Add(1)
	s.logf("bind %s: on the host, %s", want, host)
	return succeed()
}

// accept on a socket that bind put in place: the supervisor accepts, reads who the client
// is, and gives the container the connection with the client's pod address as its peer.
// The listener is the container's open file (the duplicate shares it), so a non-blocking
// listener answers EAGAIN as it would, and the readiness the container's epoll sees is
// the listener's own.
func (s *Supervisor) accept(n *notif) reply {
	tid, fd := int(n.Pid), int(n.Args[0])
	dup, err := s.targets.getfd(tid, fd)
	if err != nil {
		return letThrough
	}
	defer unix.Close(dup)
	listener, ok := s.table.Get(cookieOf(dup))
	if !ok || listener.Kind != KindListener {
		return letThrough
	}
	flags := 0
	if int(n.Nr) == unix.SYS_ACCEPT4 {
		flags = int(n.Args[3])
	}
	fl, err := unix.FcntlInt(uintptr(dup), unix.F_GETFL, 0)
	if err != nil {
		return letThrough
	}
	nonblock := fl&unix.O_NONBLOCK != 0

	var conn int
	var pre Preamble
	for {
		if !nonblock {
			// Wait in turns, so that a thread that gave up (a signal) is noticed.
			for !pollReadable(dup, 200) {
				if !ioctlValid(s.listener, n.ID) {
					return cancelled
				}
			}
		}
		conn, _, err = unix.Accept4(dup, unix.SOCK_CLOEXEC)
		if err != nil {
			if err == unix.EINTR || (err == unix.EAGAIN && !nonblock) {
				continue // another thread took it
			}
			return failWith(errnoOf(err))
		}
		if pre, err = s.readPreamble(conn); err == nil {
			if pre.Backend.Addr() == s.cfg.PodIP && pre.Backend.Port() == listener.Local.Port() {
				break
			}
			err = fmt.Errorf("meant for %s, not for this pod (%s:%d)", pre.Backend, s.cfg.PodIP, listener.Local.Port())
		}
		// Not a client of ours, or one that was sent to the wrong pod (an out-of-date
		// Service table): closed, and the next connection is accepted. EAGAIN is for the
		// kernel to say: an edge-triggered epoll would not wake again for the connections
		// that are queued.
		s.logf("accept: closed a connection: %v", err)
		unix.Close(conn)
	}
	defer unix.Close(conn)
	if flags&unix.SOCK_NONBLOCK != 0 {
		_ = unix.SetNonblock(conn, true)
	}
	if n.Args[1] != 0 && n.Args[2] != 0 {
		if err := writeAddr(int(n.Pid), n.Args[1], n.Args[2], pre.Client, listener.Family); err != nil {
			return failWith(unix.EFAULT)
		}
	}
	cookie := cookieOf(conn)
	if !ioctlValid(s.listener, n.ID) {
		return cancelled
	}
	// The descriptor is added and the call answered with its number in one step.
	a := notifAddfd{ID: n.ID, Flags: unix.SECCOMP_ADDFD_FLAG_SEND, Srcfd: uint32(conn)}
	if flags&unix.SOCK_CLOEXEC != 0 {
		a.NewfdFlags = unix.O_CLOEXEC
	}
	s.table.Add(cookie, Socket{Kind: KindAccepted, Family: listener.Family, Local: pre.Backend, Peer: pre.Client})
	newfd, err := ioctlAddfd(s.listener, &a)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return cancelled
		}
		return failWith(errnoOf(err))
	}
	s.stats.Accepted.Add(1)
	s.logf("accept: fd %d of %s, from %s", newfd, listener.Local, pre.Client)
	return reply{answered: true}
}

// readPreamble reads the preamble a client's supervisor wrote first.
func (s *Supervisor) readPreamble(conn int) (Preamble, error) {
	var buf [PreambleSize]byte
	deadline := time.Now().Add(preambleTimeout)
	for got := 0; got < len(buf); {
		wait := time.Until(deadline)
		if wait <= 0 {
			return Preamble{}, errors.New("no preamble")
		}
		if !pollReadable(conn, int(wait.Milliseconds())+1) {
			continue
		}
		m, err := unix.Read(conn, buf[got:])
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil || m == 0 {
			return Preamble{}, errors.New("closed before a preamble")
		}
		got += m
	}
	return ParsePreamble(buf[:])
}

func pollReadable(fd, timeoutMs int) bool {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, timeoutMs)
	return err == nil && n > 0
}

// writeAddr writes ap, as a socket of the family reports it, to the container's address
// buffer and length, truncated to the buffer as the kernel does, with the full length in
// the length.
func writeAddr(tid int, addrPtr, lenPtr uint64, ap netip.AddrPort, family int) error {
	var lb [4]byte
	if err := readRemote(tid, lenPtr, lb[:]); err != nil {
		return err
	}
	have := int(int32(nativeUint32(lb[:])))
	sa := encodeSockaddr(ap, family)
	if have > 0 {
		if err := writeRemote(tid, addrPtr, sa[:min(have, len(sa))]); err != nil {
			return err
		}
	}
	putNativeUint32(lb[:], uint32(len(sa)))
	return writeRemote(tid, lenPtr, lb[:])
}
