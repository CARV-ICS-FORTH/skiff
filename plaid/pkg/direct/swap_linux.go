//go:build linux && (amd64 || arm64)

package direct

import (
	"crypto/hmac"
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
	// DefaultPreambleTimeout bounds the wait for a client's preamble, which is held inside
	// the server's accept: a connection that does not send one is not a client of ours, and
	// every idle connection to a host port stalls the accept of a server (an event loop's)
	// for as long. A client sends its preamble as soon as it is connected, so a preamble that
	// has not arrived by then is not coming; one that has is read without waiting.
	DefaultPreambleTimeout = 100 * time.Millisecond
	// preambleSkew is how far a preamble's time may be from the server's clock.
	preambleSkew = time.Minute
	// DefaultAckTimeout is how long a client's supervisor waits for the server's ack.
	DefaultAckTimeout = time.Second

	// What a call that the container gave up has made is kept for the call that is made again
	// (see pending_linux.go), this long, and a listener keeps at most keptAcceptMax connections.
	keptDialAge   = 2 * time.Second
	keptBindAge   = 5 * time.Second
	keptAcceptAge = 5 * time.Second
	keptAcceptMax = 64
)

// fionbio is the ioctl that sets a socket non-blocking or not, in one call, where fcntl takes two
// (x/sys/unix does not name it for Linux; it is the same on amd64 and arm64).
const fionbio = 0x5421

// heldSocket is a container's socket as the supervisor sees it through a duplicate.
type heldSocket struct {
	dup      int // the supervisor's descriptor of the container's socket
	family   int
	nonblock bool
	cloexec  bool
}

var errNotOurs = errors.New("not a TCP socket of IPv4 or IPv6")

// inspect duplicates descriptor fd of the held thread's process and says what kind of socket
// it is; readFlags then says how the container opened it.
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
	return h, nil
}

// readFlags finds out whether the container's descriptor is non-blocking and close-on-exec.
func (h *heldSocket) readFlags(tid, fd int) error {
	flags, err := fdFlags(tid, fd)
	if err != nil {
		return err
	}
	h.nonblock = flags&unix.O_NONBLOCK != 0
	h.cloexec = flags&unix.O_CLOEXEC != 0
	return nil
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
// verdict on the held call: answered, or that the call has been cancelled meanwhile (the
// caller may keep what it made for the call that is made again, see kept).
func (s *Supervisor) put(n *notif, fd int, sock int, cloexec bool) error {
	if !ioctlValid(s.listener, n.ID) {
		return errCancelled
	}
	a := notifAddfd{ID: n.ID, Flags: unix.SECCOMP_ADDFD_FLAG_SETFD, Srcfd: uint32(sock), Newfd: uint32(fd)}
	if cloexec {
		a.NewfdFlags = unix.O_CLOEXEC
	}
	if _, err := ioctlAddfd(s.listener, &a); err != nil {
		// ENOENT: the thread is not in the call any more. ESRCH: it was interrupted while the
		// kernel waited for it to take the file, and the file was not put.
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
			return errCancelled
		}
		return err
	}
	return nil
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
// the client is and which pod it means, and says in the ack that it has the connection.
// Whatever fails before that leaves the container's socket as it was and lets the call
// through: the connect goes to the Service on the pod network.
//
// The connection to the backend's node is started as soon as the socket is known to be a
// TCP one and has its options, and what is left to check about the container's socket (its
// flags, the epoll look, that it is unused) is done while the handshake is on its way. A
// check that fails then closes a connection that has been started: the server's supervisor
// sees one that never sent a preamble. For the epoll look that happens only for a container
// whose sockets are in an epoll set before the connect and that cannot be moved to the host
// socket (see armed), and its supervisor then looks before it connects (epollFirst).
//
// If the container gives the connect up after the server has acked (a signal: the thread
// makes the call again), the connection is kept for the call that is made again, which is
// answered with it and does not make a second one for the server to accept (see connectKey).
func (s *Supervisor) connect(n *notif, tl *timeline) reply {
	tl.mark(phDispatch)
	dst, ok := sockaddrArg(n)
	if !ok || !s.cfg.ConnectPorts.Has(dst.Port()) {
		return letThrough
	}
	backend, ok := s.cfg.Resolver.Resolve(dst)
	if !ok {
		return letThrough
	}
	tl.mark(phResolve)
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
	tl.mark(phInspect)
	valid := func() bool { return ioctlValid(s.listener, n.ID) }
	leave := s.holding.enter(tid, valid) // after the call of this thread that was given up
	if leave == nil {
		return cancelled
	}
	defer leave()
	key := connectKey{tid: tid, fd: fd, cookie: cookieOf(held.dup), dst: dst}
	sock, madeBefore, reused := s.dials.take(key)
	if reused {
		backend = madeBefore
	}
	keep := false // sock is kept for the call that is made again
	defer func() {
		if sock >= 0 && !keep {
			unix.Close(sock)
		}
	}()
	epollFirst := s.epollFirst.Load()
	var entries []epollEntry
	if epollFirst {
		var ok bool
		if entries, ok = s.registrations(tid, fd, held); !ok {
			return s.fallback("epoll", "connect %s: the socket is in an epoll set that it cannot be taken out of", dst)
		}
		tl.mark(phEpoll)
	}

	var dialErr error
	if !reused {
		fresh, err := s.cfg.Factory.Socket(held.family)
		if err != nil {
			s.stats.Errors.Add(1)
			s.logf("connect %s: %v", dst, err)
			return letThrough
		}
		sock = fresh
		tl.mark(phFactory)
		copyOptions(held.dup, sock, held.family)
		tl.mark(phOptions)
		_ = unix.IoctlSetPointerInt(sock, fionbio, 1) // so that the connect starts and the checks go on
		dialErr = unix.Connect(sock, sockaddrFor(backend.Host, held.family))
		if dialErr != nil && dialErr != unix.EINPROGRESS {
			return s.fallback(dialFailure(dialErr), "connect %s: to %s: %v", dst, backend.Host, dialErr)
		}
		tl.mark(phStart)
	}

	if err := held.readFlags(tid, fd); err != nil {
		s.stats.Errors.Add(1)
		s.logf("connect %s: %v", dst, err)
		return letThrough
	}
	tl.mark(phFlags)
	if !epollFirst {
		var ok bool
		if entries, ok = s.registrations(tid, fd, held); !ok {
			s.epollFirst.Store(true)
			return s.fallback("epoll", "connect %s: the socket is in an epoll set that it cannot be taken out of", dst)
		}
		tl.mark(phEpoll)
	}
	if _, err := unix.Getpeername(held.dup); err == nil {
		return letThrough // connected already: the kernel says EISCONN
	}
	if sa, err := unix.Getsockname(held.dup); err == nil && addrPortOf(sa).Port() != 0 {
		return letThrough // the container chose its address: the pod network's to give
	}
	tl.mark(phState)

	if dialErr == unix.EINPROGRESS {
		if dialErr = waitConnected(sock, connectTimeout); dialErr != nil {
			return s.fallback(dialFailure(dialErr), "connect %s: to %s: %v", dst, backend.Host, dialErr)
		}
	}
	tl.mark(phDial)
	sa, err := unix.Getsockname(sock)
	if err != nil {
		return s.fallback("connect-error", "connect %s: %v", dst, err)
	}
	local := netip.AddrPortFrom(s.cfg.PodIP, addrPortOf(sa).Port())
	if !reused {
		pre, err := NewPreamble(PreambleDirect, local, backend.Pod, addrPortOf(sa))
		if err != nil {
			return s.fallback("connect-error", "connect %s: %v", dst, err)
		}
		wire := pre.Marshal(s.cfg.Key)
		if _, err := unix.Write(sock, wire[:]); err != nil {
			return s.fallback("connect-error", "connect %s: %v", dst, err)
		}
		tl.mark(phPreamble)
		var ack [AckSize]byte
		if err := readFull(sock, ack[:], s.cfg.AckTimeout); err != nil {
			return s.fallback("no-ack", "connect %s: %s did not acknowledge: %v", dst, backend.Host, err)
		}
		tl.mark(phAck)
		if want := pre.Ack(s.cfg.Key); !hmac.Equal(ack[:], want[:]) {
			return s.fallback("bad-ack", "connect %s: %s sent a wrong acknowledgement", dst, backend.Host)
		}
	}
	if !held.nonblock {
		_ = unix.IoctlSetPointerInt(sock, fionbio, 0)
	}
	cookie := cookieOf(sock)
	undo, err := s.moveRegistrations(tid, fd, held, sock, entries)
	if err != nil {
		return s.fallback("epoll", "connect %s: %v", dst, err)
	}
	tl.mark(phMove)
	if err := s.put(n, fd, sock, held.cloexec); err != nil {
		undo()
		// The server has the connection (it acked). The call is made again, and takes it.
		keep = errors.Is(err, errCancelled) && s.dials.put(key, sock, backend)
		return s.putFailed("connect", err)
	}
	tl.mark(phAddfd)
	s.table.Add(cookie, Socket{Kind: KindClient, Family: held.family, Local: local, Peer: dst})
	s.stats.Connects.Add(1)
	s.logConn("connect %s: on the host, %s to %s, as %s", dst, addrPortOf(sa), backend.Host, local)
	return succeed()
}

// connectKey says which call a connection that a connect made belongs to, for the call that
// is made again: the thread, which restarts its own call, the descriptor and the socket it
// was (the descriptor may have been closed and made again meanwhile), and the address.
type connectKey struct {
	tid, fd int
	cookie  uint64
	dst     netip.AddrPort
}

// dialFailure is the short word the counts have for why a connection to the backend's node
// could not be made.
func dialFailure(err error) string {
	switch err {
	case unix.ECONNREFUSED:
		return "refused"
	case unix.ETIMEDOUT:
		return "timeout"
	}
	return "connect-error"
}

// waitConnected waits for a connection that was started on a non-blocking socket to be made,
// at most timeout, and returns the error it ended in (ETIMEDOUT if it did not end).
func waitConnected(sock int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		wait := time.Until(deadline)
		if wait <= 0 {
			return unix.ETIMEDOUT
		}
		fds := []unix.PollFd{{Fd: int32(sock), Events: unix.POLLOUT}}
		n, err := unix.Poll(fds, int(wait.Milliseconds())+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return unix.ETIMEDOUT
		}
		soerr, err := unix.GetsockoptInt(sock, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return err
		}
		if soerr != 0 {
			return unix.Errno(soerr)
		}
		return nil
	}
}

// connectWithin connects a host socket to sa and waits for the connection, at most timeout.
// The socket is made non-blocking: a blocking connect in a Go program is cut short with EINTR
// by the runtime's own signals (the ones that preempt a goroutine), and the connection goes
// on in the kernel, so it is the wait that finishes it.
func connectWithin(sock int, sa unix.Sockaddr, timeout time.Duration) error {
	if err := unix.SetNonblock(sock, true); err != nil {
		return err
	}
	if err := unix.Connect(sock, sa); err != nil && err != unix.EINPROGRESS && err != unix.EINTR {
		return err
	}
	return waitConnected(sock, timeout)
}

// connLogFirst is how many connections (made and accepted) the log has a line for; the stats
// have them all. A line for every connection of a container that runs for days is a log of
// its own, and a write for it in the middle of every connect.
const connLogFirst = 20

// logConn logs a connection that was put in the container, the first connLogFirst times.
func (s *Supervisor) logConn(format string, args ...any) {
	switch n := s.connLogged.Add(1); {
	case n <= connLogFirst:
		s.logf(format, args...)
	case n == connLogFirst+1:
		s.logf("the rest of the connections are counted, not logged (stats)")
	}
}

// fallback counts a direct attempt that was given up for the reason, logs it (the first
// time and then at most every few seconds), and lets the call through.
func (s *Supervisor) fallback(reason, format string, args ...any) reply {
	if total, log := s.stats.Fallbacks.Add(reason); log {
		s.logf("falling back to the pod network, %s (%d so far): %s", reason, total, fmt.Sprintf(format, args...))
	}
	return letThrough
}

// reject counts a connection to a swapped listener that was turned away for the reason.
func (s *Supervisor) reject(reason, format string, args ...any) {
	if total, log := s.stats.Rejected.Add(reason); log {
		s.logf("accept: closed a connection, %s (%d so far): %s", reason, total, fmt.Sprintf(format, args...))
	}
}

// registrations finds where the container's socket is registered in epoll sets (see
// targets.epollEntries). Not ok means that swapping the socket is not for the supervisor to
// do: that cannot be told, an entry cannot be made again for another socket (see
// epollEntry.armed), or the descriptor's number is one that the mover's table keeps (see
// runtimeDescriptors). A socket that is in no set has no entries, and is ok.
func (s *Supervisor) registrations(tid, fd int, held *heldSocket) (entries []epollEntry, ok bool) {
	var st unix.Stat_t
	err := unix.Fstat(held.dup, &st)
	if err == nil {
		entries, err = s.targets.epollEntries(tid, fd, st.Ino)
	}
	if err != nil {
		s.stats.Errors.Add(1)
		s.logf("looking for descriptor %d of thread %d in epoll sets: %v", fd, tid, err)
		return nil, false
	}
	for _, e := range entries {
		if !e.armed() {
			return nil, false
		}
	}
	if len(entries) > 0 && s.targets.mover.keeps(fd) {
		s.logf("descriptor %d of thread %d is in epoll sets, and its number is one that the supervisor keeps for itself", fd, tid)
		return nil, false
	}
	return entries, true
}

// moveRegistrations moves the entries from the container's socket to sock (see moveEntries),
// and returns what moves them back, for a swap that fails after it. If it fails nothing has
// moved.
func (s *Supervisor) moveRegistrations(tid, fd int, held *heldSocket, sock int, entries []epollEntry) (undo func(), err error) {
	if len(entries) == 0 {
		return func() {}, nil
	}
	back := func() error { return s.targets.moveEntries(tid, fd, sock, held.dup, entries) }
	if err := s.targets.moveEntries(tid, fd, held.dup, sock, entries); err != nil {
		if berr := back(); berr != nil {
			s.logf("the epoll registrations of descriptor %d could not be moved back: %v", fd, berr)
		}
		s.stats.Errors.Add(1)
		return nil, fmt.Errorf("moving the epoll registrations: %w", err)
	}
	return func() {
		if err := back(); err != nil {
			s.stats.Errors.Add(1)
			s.logf("the epoll registrations of descriptor %d could not be moved back: %v", fd, err)
		}
	}, nil
}

func (s *Supervisor) putFailed(call string, err error) reply {
	if errors.Is(err, errCancelled) {
		return cancelled
	}
	s.stats.Errors.Add(1)
	s.logf("%s: putting the socket in place: %v", call, err)
	return failWith(errnoOf(err))
}

// bind: a marked port is bound on the host, on every address, at the host port that follows
// from the pod's address and the port (HostPort). listen then runs on the host socket. The
// container's own socket stays in the pod's network behind a proxy (see startProxy). If
// the host port cannot be had (a process of the host holds it) the socket stays a socket of
// the pod network: its clients find no ack at the host port and fall back to the overlay.
//
// The container's socket is bound and made to listen in the pod's network before the swap, so
// that a port that the pod's network has taken fails the call with EADDRINUSE. If the call is
// then given up (a signal) the socket stays bound, and the call that is made again, which
// would be told EINVAL, finds a note of it (podBound) and goes on with the swap, after the
// one that was given up has ended (see holding). A call whose answer was given up after the
// swap finds the listener in the table.
func (s *Supervisor) bind(n *notif) (r reply) {
	want, ok := sockaddrArg(n)
	if !ok || !s.cfg.ListenPorts.Has(want.Port()) {
		return letThrough
	}
	if !want.Addr().IsUnspecified() && want.Addr() != s.cfg.PodIP {
		return letThrough // the pod's loopback or another address: the pod network's
	}
	tid, fd := int(n.Pid), int(n.Args[0])
	held, err := s.inspect(tid, fd)
	if err == nil {
		defer held.close()
		err = held.readFlags(tid, fd)
	}
	if err != nil {
		if err != errNotOurs {
			s.stats.Errors.Add(1)
			s.logf("bind %s: %v", want, err)
		}
		return letThrough
	}
	valid := func() bool { return ioctlValid(s.listener, n.ID) }
	leave := s.holding.enter(tid, valid) // after the call of this thread that was given up
	if leave == nil {
		return cancelled
	}
	defer leave()
	cookie := cookieOf(held.dup)
	if sock, ok := s.table.Get(cookie); ok && sock.Kind == KindListener && sock.Local == want {
		// This bind was made, and its answer was given up (a signal came between the socket's
		// being put in place and the answer): the thread makes it again. The kernel would say EINVAL.
		return succeed()
	}
	entries, ok := s.registrations(tid, fd, held)
	if !ok {
		return s.fallback("epoll", "bind %s: the socket is in an epoll set that it cannot be taken out of", want)
	}
	podBound := false // the pod-side socket was bound and listens already, by this call made before
	if sa, err := unix.Getsockname(held.dup); err == nil && addrPortOf(sa).Port() != 0 {
		if _, addr, ok := s.podBound.take(cookie); !ok || addr != want {
			return letThrough // bound already: the kernel says EINVAL
		}
		podBound = true
	}
	if podBound {
		// The pod-side socket is bound where the container asked, so a bind that stays on the
		// pod network has been made: the kernel, asked again, would say EINVAL.
		defer func() {
			if r == letThrough {
				r = succeed()
			}
		}()
	}
	hostPort, ok := s.cfg.Resolver.HostPort(netip.AddrPortFrom(s.cfg.PodIP, want.Port()))
	if !ok {
		return s.fallback("no-host-port", "bind %s: the pod has no host port for it", want)
	}
	sock, err := s.cfg.Factory.Socket(held.family)
	if err != nil {
		s.stats.Errors.Add(1)
		s.logf("bind %s: %v", want, err)
		return letThrough
	}
	defer unix.Close(sock)
	copyOptions(held.dup, sock, held.family)
	host := netip.AddrPortFrom(unspecifiedFor(held.family), hostPort)
	if err := unix.Bind(sock, sockaddrFor(host, held.family)); err != nil {
		reason := "bind-error"
		if err == unix.EADDRINUSE {
			reason = "host-port-in-use"
		}
		return s.fallback(reason, "bind %s: on the host at %s: %v", want, host, err)
	}
	// The registrations of the container's socket in epoll sets go to the host's, and are
	// out of the old one's hands before it is put in the pod's network, so that no set of
	// the container reports a connection to it.
	undo, err := s.moveRegistrations(tid, fd, held, sock, entries)
	if err != nil {
		return s.fallback("epoll", "bind %s: %v", want, err)
	}
	// The container's own socket, which the host's is about to replace, stays in the pod's
	// network at the address it asked for (see the proxy). A port that the pod's network
	// has taken is what the container is told (EADDRINUSE), as the kernel would have. A proxy
	// of an earlier listener of the container's is still there if the container closed its
	// listener and bound again: the host port was free, so it is out of date.
	if !podBound {
		s.dropProxy(want, nil)
		if err := unix.Bind(held.dup, sockaddrFor(want, held.family)); err != nil {
			undo()
			return failWith(errnoOf(err))
		}
		if err := unix.Listen(held.dup, proxyBacklog); err != nil {
			undo()
			return failWith(errnoOf(err))
		}
	}
	if held.nonblock {
		_ = unix.SetNonblock(sock, true)
	}
	hostCookie := cookieOf(sock)
	if err := s.put(n, fd, sock, held.cloexec); err != nil {
		undo()
		if errors.Is(err, errCancelled) {
			// The call was given up (a signal) and is made again, to find the socket bound,
			// which cannot be undone: it is noted, so that the call made again goes on from here.
			s.podBound.put(cookie, -1, want)
		}
		return s.putFailed("bind", err)
	}
	s.table.Add(hostCookie, Socket{Kind: KindListener, Family: held.family, Local: want})
	s.stats.Binds.Add(1)
	if err := s.startProxy(held, want, hostPort, s.ownerOf(tid, fd, hostCookie)); err != nil {
		s.stats.Errors.Add(1)
		s.logf("bind %s: the proxy in the pod's network: %v", want, err)
	}
	s.logf("bind %s: on the host, %s", want, host)
	return succeed()
}

// ownerOf says where the container has the socket that was put in place for descriptor fd of
// thread tid: the process, kept open by a pidfd of its own, and the descriptor.
func (s *Supervisor) ownerOf(tid, fd int, cookie uint64) descriptor {
	d := descriptor{pidfd: -1, fd: fd, cookie: cookie}
	if t, err := s.targets.lookup(tid, false); err == nil {
		if pidfd, err := unix.PidfdOpen(t.tgid, 0); err == nil {
			d.pidfd = pidfd
		}
	}
	return d
}

// accept on a socket that bind put in place: the supervisor accepts, reads who the client
// is, and gives the container the connection with the client's pod address as its peer.
// The listener is the container's open file (the duplicate shares it), so a non-blocking
// listener answers EAGAIN as it would, and the readiness the container's epoll sees is
// the listener's own.
//
// If the container gives the call up after the connection has been taken and the client told
// it is accepted (a signal: the thread makes the call again), the connection is kept for the
// listener's next accept, whichever thread makes it, and for at most keptAcceptAge. The call
// that is made again waits for the one that was given up, so that it finds the connection. A
// non-blocking server that does not make the call again (a signal that its runtime does not
// restart a call after) is given the connection by its next accept, without a new readiness
// event: the connection is no longer in the kernel's queue.
func (s *Supervisor) accept(n *notif, tl *timeline) reply {
	tid, fd := int(n.Pid), int(n.Args[0])
	dup, err := s.targets.getfd(tid, fd)
	if err != nil {
		return letThrough
	}
	defer unix.Close(dup)
	cookie := cookieOf(dup)
	listener, ok := s.table.Get(cookie)
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

	valid := func() bool { return ioctlValid(s.listener, n.ID) }
	var conn int
	var pre Preamble
	var leave func() // the thread's turn, while the call holds a connection (see holding)
	defer func() {
		if leave != nil {
			leave()
		}
	}()
	for {
		if !nonblock {
			// Wait in turns, so that a thread that gave up (a signal) is noticed.
			for !pollReadable(dup, 200) && !s.accepts.has(cookie) {
				if !valid() {
					return cancelled
				}
			}
		}
		if leave == nil {
			if leave = s.holding.enter(tid, valid); leave == nil {
				return cancelled
			}
		}
		queued := false
		if conn, pre, queued = s.accepts.take(cookie); !queued {
			if conn, err = s.acceptReady(dup, cookie, nonblock); err != nil {
				if err == unix.EINTR || (err == unix.EAGAIN && !nonblock) {
					leave() // nothing is held while it waits again
					leave = nil
					continue // another thread took it
				}
				return failWith(errnoOf(err))
			}
		}
		tl.mark(phAccept)
		tl.restart()
		if queued {
			break
		}
		var reason string
		if pre, reason, err = s.admit(conn, listener, tl); err == nil {
			break
		}
		// Not a client of ours, or a wrong or old one: closed, and the next connection is
		// accepted. EAGAIN is for the kernel to say: an edge-triggered epoll would not wake
		// again for the connections that are queued.
		s.reject(reason, "%v", err)
		peer := peerOf(conn, listener.Family)
		unix.Close(conn)
		if !nonblock && !pollReadable(dup, 0) {
			// Nothing else is queued, and a blocking accept cannot be told so. Waiting for
			// the next connection would leave a server that polled the listener and then
			// called accept standing here with whatever else it has to do (iperf3 in the
			// middle of a test), and an error would end a server that does not expect one
			//. The kernel returns the connection that was polled for: a
			// client that went away, or whatever it was. This is one that has.
			if conn, err = s.closedConnection(); err == nil {
				pre = Preamble{Client: peer, Backend: netip.AddrPortFrom(s.cfg.PodIP, listener.Local.Port())}
				s.logf("accept: a closed connection from %s in place of the one turned away", peer)
				break
			}
			s.stats.Errors.Add(1)
			s.logf("accept: no closed connection to hand over: %v", err)
		}
	}
	keep := false
	defer func() {
		if !keep {
			unix.Close(conn)
		}
	}()
	// The call has been given up with the connection in hand, which the client has been told
	// is accepted: it is kept for the next accept (the connection is not the container's yet).
	giveUp := func() reply {
		if flags&unix.SOCK_NONBLOCK != 0 {
			_ = unix.SetNonblock(conn, false)
		}
		if keep = s.accepts.put(cookie, conn, pre); !keep {
			s.reject("queue-full", "from %s, %d are kept for %s", pre.Client, keptAcceptMax, listener.Local)
		}
		return cancelled
	}
	// The address is written, and the call answered, under the task's lock and only if the
	// call is still held (see threadLocks).
	t := s.threads.lock(int(n.Pid))
	defer s.threads.unlock(int(n.Pid), t)
	if !valid() {
		return giveUp()
	}
	if n.Args[1] != 0 && n.Args[2] != 0 {
		if err := writeAddr(int(n.Pid), n.Args[1], n.Args[2], pre.Client, listener.Family); err != nil {
			return failWith(unix.EFAULT)
		}
	}
	tl.mark(phWriteAddr)
	if flags&unix.SOCK_NONBLOCK != 0 {
		_ = unix.SetNonblock(conn, true)
	}
	// The descriptor is added, and then the call is answered with its number. (Adding it with
	// SECCOMP_ADDFD_FLAG_SEND answers in the same step, and does it with one call fewer, but on
	// the 6.8 kernel of the test cluster a Tomcat acceptor that was accepting at about 5000
	// connections a second was now and then given the result 0, once in some 100000 accepts,
	// which it took for its new socket: it called getsockname on its descriptor 0, got ENOTSOCK,
	// and closed it. The connection was lost, and so was the container's stdin, so every
	// descriptor that followed was handed out as 0 until the container ended.)
	a := notifAddfd{ID: n.ID, Srcfd: uint32(conn)}
	if flags&unix.SOCK_CLOEXEC != 0 {
		a.NewfdFlags = unix.O_CLOEXEC
	}
	s.table.Add(cookieOf(conn), Socket{Kind: KindAccepted, Family: listener.Family, Local: pre.Backend, Peer: pre.Client})
	newfd, err := ioctlAddfd(s.listener, &a)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return giveUp()
		}
		return failWith(errnoOf(err))
	}
	if err := ioctlSend(s.listener, &notifResp{ID: n.ID, Val: int64(newfd)}); err != nil {
		// The task was interrupted between the two: it was told nothing, and the descriptor
		// stays in its table, which nothing here can undo.
		s.stats.Errors.Add(1)
		s.logf("accept: the answer of thread %d (descriptor %d): %v", n.Pid, newfd, err)
		return cancelled
	}
	tl.mark(phAcceptFD)
	tl.finish(phAdmit)
	s.stats.Accepted.Add(1)
	s.logConn("accept: fd %d of %s, from %s", newfd, listener.Local, pre.Client)
	return reply{answered: true}
}

// acceptReady accepts a connection from the container's listener. A blocking listener is
// accepted from only when it is readable, and the check and the accept are made under a lock
// of the listener: every accept of a swapped listener is decided here, so no other thread of
// the server can take the connection between them, and the accept does not wait for one
// that is gone. (The listener's file is the container's, and is not made non-blocking for
// the supervisor's sake: the container would see that.) EAGAIN says that nothing is there.
func (s *Supervisor) acceptReady(dup int, cookie uint64, nonblock bool) (int, error) {
	if !nonblock {
		mu := &s.acceptLocks[cookie%uint64(len(s.acceptLocks))]
		mu.Lock()
		defer mu.Unlock()
		if !pollReadable(dup, 0) {
			return -1, unix.EAGAIN
		}
	}
	conn, _, err := unix.Accept4(dup, unix.SOCK_CLOEXEC)
	return conn, err
}

// peerOf is the address the other end of a socket of the family has, or the family's
// unspecified address if the socket cannot say.
func peerOf(fd, family int) netip.AddrPort {
	if sa, err := unix.Getpeername(fd); err == nil {
		if ap := addrPortOf(sa); ap.IsValid() {
			return ap
		}
	}
	return netip.AddrPortFrom(unspecifiedFor(family), 0)
}

// closedConnection is an accepted TCP socket whose other end has closed, one that the
// container reads end of file from and nobody else has: the supervisor connects a host
// socket to a listener of its own on the host's IPv4 loopback and closes it. It is an IPv4
// socket also for an IPv6 listener, because the host may have no ::1 (or no IPv6 on its
// loopback at all); the container is told the addresses of the listener's family, as for
// every connection it is handed (the table, getsockname, the address accept writes).
func (s *Supervisor) closedConnection() (int, error) {
	listener, err := s.cfg.Factory.Socket(unix.AF_INET)
	if err != nil {
		return -1, err
	}
	defer unix.Close(listener)
	if err := unix.Bind(listener, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		return -1, err
	}
	if err := unix.Listen(listener, 1); err != nil {
		return -1, err
	}
	sa, err := unix.Getsockname(listener)
	if err != nil {
		return -1, err
	}
	client, err := s.cfg.Factory.Socket(unix.AF_INET)
	if err != nil {
		return -1, err
	}
	defer unix.Close(client)
	if err := connectWithin(client, sa, connectTimeout); err != nil {
		return -1, err
	}
	conn, _, err := unix.Accept4(listener, unix.SOCK_CLOEXEC)
	return conn, err
}

// admit decides whether the connection that was accepted on the listener is a client
// of ours that the listener's container may have, and if it is acknowledges it. The reason
// it is not is a short word for the counts.
func (s *Supervisor) admit(conn int, listener Socket, tl *timeline) (pre Preamble, reason string, err error) {
	var buf [PreambleSize]byte
	if err := readFull(conn, buf[:], s.cfg.PreambleTimeout); err != nil {
		return pre, "no-preamble", err
	}
	tl.mark(phReadPre)
	if pre, err = ParsePreamble(buf[:], s.cfg.Key); err != nil {
		if err == errBadMAC {
			return pre, "bad-mac", err
		}
		return pre, "not-a-preamble", err
	}
	tl.mark(phVerify)
	if age := time.Since(pre.Time); age > preambleSkew || age < -preambleSkew {
		return pre, "stale", fmt.Errorf("made %s ago", age.Round(time.Second))
	}
	sa, perr := unix.Getpeername(conn)
	peer := addrPortOf(sa)
	if perr != nil || peer != pre.Host {
		return pre, "wrong-host", fmt.Errorf("from %v, not from the %s it says", peer, pre.Host)
	}
	if pre.Backend.Port() != listener.Local.Port() {
		return pre, "wrong-backend", fmt.Errorf("meant for %s, not for port %d of this pod", pre.Backend, listener.Local.Port())
	}
	switch pre.Kind {
	case PreambleDirect:
		if pre.Backend.Addr() != s.cfg.PodIP {
			return pre, "wrong-backend", fmt.Errorf("meant for %s, not for this pod (%s)", pre.Backend, s.cfg.PodIP)
		}
		if host, ok := s.cfg.Resolver.HostOf(pre.Client.Addr()); !ok || host != peer.Addr() {
			return pre, "wrong-client", fmt.Errorf("%s is not a pod of the node %s", pre.Client.Addr(), peer.Addr())
		}
	case PreambleLocal:
		if !peer.Addr().IsLoopback() {
			return pre, "wrong-host", fmt.Errorf("a local connection from %s", peer.Addr())
		}
	default:
		return pre, "not-a-preamble", fmt.Errorf("kind %d", pre.Kind)
	}
	// A direct client waits for the ack before it goes on, and gives up after a while and
	// takes the overlay: it has closed its end, and the container should not be handed that
	// connection as well as the one it makes there. A local connection is the pod-side
	// proxy's, for a client of the pod that does not wait for anything (a readiness probe
	// connects and closes at once), and the kernel hands the container such a connection,
	// which reads end of file: turned away, it would leave a server that had polled its
	// blocking listener and calls accept waiting there for another connection, which in
	// the case of a probe is seconds off.
	tl.mark(phChecks)
	if pre.Kind == PreambleDirect {
		var probe [1]byte
		if m, _, err := unix.Recvfrom(conn, probe[:], unix.MSG_PEEK|unix.MSG_DONTWAIT); err == nil && m == 0 {
			return pre, "client-gone", errors.New("the client closed it while it waited")
		}
	}
	ack := pre.Ack(s.cfg.Key)
	if _, err := unix.Write(conn, ack[:]); err != nil {
		return pre, "client-gone", err
	}
	tl.mark(phAckWrite)
	return pre, "", nil
}

// readFull reads len(buf) bytes from fd, waiting at most timeout for them.
func readFull(fd int, buf []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for got := 0; got < len(buf); {
		wait := time.Until(deadline)
		if wait <= 0 {
			return errors.New("timed out")
		}
		if !pollReadable(fd, int(wait.Milliseconds())+1) {
			continue
		}
		m, err := unix.Read(fd, buf[got:])
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil || m == 0 {
			return errors.New("closed before it was complete")
		}
		got += m
	}
	return nil
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
