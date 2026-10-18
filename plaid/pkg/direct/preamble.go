// Package direct is the direct path: TCP connections that a pod marks bypass Plaid's
// overlay and run on sockets of the physical host's network namespace, while the pod keeps
// the addresses Kubernetes gave it.
//
// A container that declares direct ports runs under a supervisor (Run) that holds the
// listening end of a seccomp user notification. The container's connect, bind, accept,
// getsockname and getpeername calls are held in the kernel and decided by the supervisor:
//
//   - connect to a Service port that is marked: the supervisor resolves the Service's
//     backend (the Service table and the routes of plaidd), takes a socket of the host
//     network namespace from the socket factory (the host-side program, see ServeFactory),
//     connects it to the node of the backend, tells the backend who the client is (the
//     preamble) and puts the socket in place of the container's own, with pidfd_getfd and
//     SECCOMP_IOCTL_NOTIF_ADDFD. The container's connect returns success.
//   - bind of a marked port: the same, for a socket bound to that port on the host.
//     listen then runs on the host's socket.
//   - accept on such a listener: the supervisor accepts, reads the preamble and returns
//     the new socket together with the client's pod address.
//   - getsockname and getpeername of a socket that was put in place: answered with the
//     pod's addresses (the pod's own address and the Service's, or the client's) and not
//     with the host's.
//
// Everything else is let through (SECCOMP_USER_NOTIF_FLAG_CONTINUE) and runs as if there
// were no supervisor. Nothing here is a security boundary: see the preamble.
package direct

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// PreambleSize is the length of the preamble.
const PreambleSize = 16

var preambleMagic = [4]byte{'D', 'P', 'v', '1'}

// Preamble is what the client's supervisor writes first on a connection on the host
// network, and the server's supervisor reads before it hands the connection to the
// server: who the client is in the pod network and which pod it meant to reach. The
// connection is between nodes, so the sockets themselves say nothing of either. It is
// not authenticated: whoever can reach the port can claim any pod address.
type Preamble struct {
	Client  netip.AddrPort // the client pod's address and the port of its host socket
	Backend netip.AddrPort // the pod and port the client's Service address stands for
}

// Marshal is the preamble as it goes on the wire: the magic "DPv1", the client's IPv4
// address and port, the backend's, the ports in network order.
func (p Preamble) Marshal() ([PreambleSize]byte, error) {
	var b [PreambleSize]byte
	if !p.Client.Addr().Is4() || !p.Backend.Addr().Is4() {
		return b, errors.New("the preamble carries IPv4 addresses only")
	}
	copy(b[:4], preambleMagic[:])
	c, s := p.Client.Addr().As4(), p.Backend.Addr().As4()
	copy(b[4:8], c[:])
	binary.BigEndian.PutUint16(b[8:10], p.Client.Port())
	copy(b[10:14], s[:])
	binary.BigEndian.PutUint16(b[14:16], p.Backend.Port())
	return b, nil
}

// ParsePreamble reads a preamble; it refuses anything that does not start with the magic.
func ParsePreamble(b []byte) (Preamble, error) {
	if len(b) < PreambleSize {
		return Preamble{}, fmt.Errorf("a preamble is %d bytes, got %d", PreambleSize, len(b))
	}
	if [4]byte(b[:4]) != preambleMagic {
		return Preamble{}, errors.New("not a preamble")
	}
	return Preamble{
		Client:  netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), binary.BigEndian.Uint16(b[8:10])),
		Backend: netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[10:14])), binary.BigEndian.Uint16(b[14:16])),
	}, nil
}
