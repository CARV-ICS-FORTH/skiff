//go:build linux && (amd64 || arm64)

package direct

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// A listener that bind put on the host is out of the pod's network: the kubelet-style probes
// Skifflet runs at the pod's address, the pod's other containers and `kubectl exec` on
// localhost, and the clients that do not take the direct path (unmarked containers, or ones
// that fell back to the overlay) would find nothing at the port. So the supervisor keeps
// the container's own socket, the one bind replaced: it binds it to the address the
// container asked for, in the pod's network, and accepts on it. For each connection it makes
// a connection of its own to the container's listener on the host's loopback, tells it in a
// preamble of kind PreambleLocal who the client really is, and copies bytes between the two.
// The container's accept then returns the connection with the client's real address, as
// for a direct one. It is the low-volume path (probes, a shell, a client that fell back),
// not the one the direct path is for.

const (
	// proxyBacklog is the backlog of the pod-side socket (the container's own listen is
	// made on the host socket, and its backlog is the host socket's).
	proxyBacklog = 128
	// proxyWatchEvery is how often a proxy asks whether the container's listener is still there.
	proxyWatchEvery = time.Second
)

// localProxy is one swapped listener's pod-side socket.
type localProxy struct {
	s        *Supervisor
	podPort  uint16
	hostPort uint16
	family   int // of the container's socket, and so of the host's
	ln       *net.TCPListener
	once     sync.Once
	done     chan struct{}
}

// startProxy takes over the container's own socket (held.dup, already bound to the address
// the container asked for and listening) and accepts on it, and watches for the container's
// listener to go.
func (s *Supervisor) startProxy(held *heldSocket, podPort, hostPort uint16) error {
	f := os.NewFile(uintptr(held.dup), "pod-side listener")
	ln, err := net.FileListener(f)
	f.Close() // FileListener has its own descriptor; this one is the held duplicate
	held.dup = -1
	if err != nil {
		return err
	}
	p := &localProxy{s: s, podPort: podPort, hostPort: hostPort, family: held.family, ln: ln.(*net.TCPListener), done: make(chan struct{})}
	s.proxyMu.Lock()
	if s.proxies == nil {
		s.proxies = make(map[uint16]*localProxy)
	}
	s.proxies[podPort] = p
	s.proxyMu.Unlock()
	go p.accept()
	go p.watch()
	return nil
}

// dropProxy closes the proxy of the pod port, if there is one.
func (s *Supervisor) dropProxy(podPort uint16) {
	s.proxyMu.Lock()
	p := s.proxies[podPort]
	delete(s.proxies, podPort)
	s.proxyMu.Unlock()
	if p != nil {
		p.close()
	}
}

func (s *Supervisor) closeProxies() {
	s.proxyMu.Lock()
	proxies := s.proxies
	s.proxies = nil
	s.proxyMu.Unlock()
	for _, p := range proxies {
		p.close()
	}
}

// close ends the proxy: the pod-side socket goes, and with it the port in the pod's network.
// The connections in flight run on.
func (p *localProxy) close() {
	p.once.Do(func() {
		close(p.done)
		p.ln.Close()
	})
}

func (p *localProxy) accept() {
	for {
		c, err := p.ln.AcceptTCP()
		if err != nil {
			return // closed
		}
		go p.forward(c)
	}
}

// watch ends the proxy when the container's listener has gone. The container's close of
// its listener is not a call the filter holds, so the supervisor asks the host: a bind of
// the host port (the container's own bind needs it free to rebind, as a listener's does)
// that succeeds twice in a row, with nobody listening, says it is gone. Connections that
// the listener accepted and that are still open do not keep the port from this bind: it
// sets SO_REUSEADDR, as the container's socket mostly did. A container that did not set it
// keeps its proxy until those connections end.
func (p *localProxy) watch() {
	tick := time.NewTicker(proxyWatchEvery)
	defer tick.Stop()
	free := 0
	for {
		select {
		case <-p.done:
			return
		case <-tick.C:
		}
		if !p.s.hostPortFree(p.family, p.hostPort) {
			free = 0
			continue
		}
		if free++; free == 2 {
			p.s.logf("the listener of port %d is gone: the proxy at the pod's port is closed", p.podPort)
			p.s.dropProxy(p.podPort)
			return
		}
	}
}

// hostPortFree says whether nothing on the host holds the port: a bind of it to every
// address, not kept, succeeds.
func (s *Supervisor) hostPortFree(family int, port uint16) bool {
	sock, err := s.cfg.Factory.Socket(family)
	if err != nil {
		return false
	}
	defer unix.Close(sock)
	_ = unix.SetsockoptInt(sock, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	return unix.Bind(sock, sockaddrFor(netip.AddrPortFrom(unspecifiedFor(family), port), family)) == nil
}

// forward carries one connection that came to the pod-side socket to the container's
// listener on the host and back.
func (p *localProxy) forward(c *net.TCPConn) {
	defer c.Close()
	peer := c.RemoteAddr().(*net.TCPAddr).AddrPort()
	local := c.LocalAddr().(*net.TCPAddr).AddrPort()
	h, hostAddr, err := p.dial()
	if err != nil {
		p.s.stats.ProxyFailed.Add(1)
		p.s.logf("proxy: port %d: %v", p.podPort, err)
		return
	}
	defer h.Close()
	pre, err := NewPreamble(PreambleLocal, netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port()), netip.AddrPortFrom(local.Addr().Unmap(), local.Port()), hostAddr)
	if err != nil {
		return
	}
	wire := pre.Marshal(p.s.cfg.Key)
	if _, err := h.Write(wire[:]); err != nil {
		p.s.stats.ProxyFailed.Add(1)
		return
	}
	p.s.stats.Proxied.Add(1)

	// The ack is the first thing the host side says, when the container accepts; the client
	// of the pod network waits for the container as it would for its backlog.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var ack [AckSize]byte
		if _, err := io.ReadFull(h, ack[:]); err != nil {
			c.Close()
			return
		}
		if want := pre.Ack(p.s.cfg.Key); !hmac.Equal(ack[:], want[:]) {
			p.s.logf("proxy: port %d: a wrong acknowledgement", p.podPort)
			c.Close()
			return
		}
		_, _ = io.Copy(c, h)
		_ = c.CloseWrite()
	}()
	_, _ = io.Copy(h, c)
	_ = h.CloseWrite()
	wg.Wait()
}

// dial connects to the container's listener on the host's loopback: its IPv4 address for a
// socket of the family, and for an IPv6 one its IPv6 address first (a listener that is only
// IPv6 has no other).
func (p *localProxy) dial() (*net.TCPConn, netip.AddrPort, error) {
	candidates := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	if p.family == unix.AF_INET6 {
		candidates = []netip.Addr{netip.IPv6Loopback(), candidates[0]}
	}
	var last error
	for _, addr := range candidates {
		family := unix.AF_INET
		if addr.Is6() {
			family = unix.AF_INET6
		}
		sock, err := p.s.cfg.Factory.Socket(family)
		if err != nil {
			last = err
			continue
		}
		timeout := unix.NsecToTimeval(int64(connectTimeout))
		_ = unix.SetsockoptTimeval(sock, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &timeout)
		if err := unix.Connect(sock, sockaddrFor(netip.AddrPortFrom(addr, p.hostPort), family)); err != nil {
			unix.Close(sock)
			last = fmt.Errorf("connect to %s: %w", netip.AddrPortFrom(addr, p.hostPort), err)
			continue
		}
		_ = unix.SetsockoptTimeval(sock, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &unix.Timeval{})
		sa, err := unix.Getsockname(sock)
		if err != nil {
			unix.Close(sock)
			last = err
			continue
		}
		f := os.NewFile(uintptr(sock), "host side")
		conn, err := net.FileConn(f)
		f.Close()
		if err != nil {
			last = err
			continue
		}
		return conn.(*net.TCPConn), addrPortOf(sa), nil
	}
	return nil, netip.AddrPort{}, errors.Join(errors.New("the container's listener is not reachable on the host"), last)
}
