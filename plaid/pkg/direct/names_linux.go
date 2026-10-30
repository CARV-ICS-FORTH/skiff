//go:build linux && (amd64 || arm64)

package direct

import "golang.org/x/sys/unix"

// getsockname and getpeername of a socket that was put in place: the container is told the
// addresses of the pod network, not of the host socket. A client sees itself as its pod IP
// and its peer as the address it connected to, a Service's or a pod's (as it would behind
// kube-proxy's DNAT); a server sees itself as its pod IP and port and its peer as the client's pod IP
// and port. Any other socket is let through.
func (s *Supervisor) name(n *notif, tl *timeline) reply {
	if s.table.Len() == 0 {
		return letThrough // nothing has been put in place: no lookup to make for every socket
	}
	if n.Args[1] == 0 || n.Args[2] == 0 {
		return letThrough // the kernel says EFAULT
	}
	dup, err := s.targets.getfd(int(n.Pid), int(n.Args[0]))
	if err != nil {
		return letThrough
	}
	cookie := cookieOf(dup)
	unix.Close(dup)
	sock, ok := s.table.Get(cookie)
	if !ok {
		return letThrough
	}
	tl.mark(phLookup)
	addr := sock.Local
	if int(n.Nr) == unix.SYS_GETPEERNAME {
		if sock.Kind == KindListener {
			return letThrough // not connected: the kernel says ENOTCONN
		}
		addr = sock.Peer
	}
	t := s.threads.lock(int(n.Pid)) // see threadLocks
	defer t.unlock()
	if !s.valid(n) {
		return cancelled
	}
	if err := writeAddr(int(n.Pid), n.Args[1], n.Args[2], addr, sock.Family); err != nil {
		return failWith(unix.EFAULT)
	}
	tl.mark(phWriteName)
	s.stats.Names.Add(1)
	return succeed()
}
