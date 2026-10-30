// Package direct is the direct path: TCP connections that a pod marks bypass Plaid's
// overlay and run on sockets of the physical host's network namespace, while the pod keeps
// the addresses Kubernetes gave it.
//
// A container that declares direct ports runs under a supervisor (Start) that holds the
// listening end of a seccomp user notification. The container's connect, bind, accept,
// getsockname and getpeername calls are held in the kernel and decided by the supervisor:
//
//   - connect to a Service port that is marked: the supervisor resolves the Service's
//     backend (the Service table and the routes of plaidd), takes a socket of the host
//     network namespace from the socket factory (the host-side program, see ServeFactory),
//     connects it to the node of the backend, tells the backend who the client is (the
//     preamble), waits for the backend's supervisor to say that it has the connection (the
//     ack), and puts the socket in place of the container's with pidfd_getfd and
//     SECCOMP_IOCTL_NOTIF_ADDFD. The container's connect returns success. If any of that
//     fails the connect is let through, and runs on the pod network as it would have.
//   - bind of a marked port: the same, for a socket bound to that port on the host.
//     listen then runs on the host's socket.
//   - accept on such a listener: the supervisor accepts, checks the preamble and returns
//     the new socket together with the client's pod address.
//   - getsockname and getpeername of a socket that was put in place: answered with the
//     pod's addresses (the pod's own address and the Service's, or the client's) and not
//     with the host's.
//
// Everything else is let through (SECCOMP_USER_NOTIF_FLAG_CONTINUE) and runs as if there
// were no supervisor. The preamble and the ack are authenticated with a key that every node
// of the cluster has and a pod does not (see DeriveKey); that is all the security the
// direct path has, and the pod network's packet filter does not see its traffic.
package direct

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

const (
	// PreambleSize is the length of the preamble.
	PreambleSize = 4 + 1 + 3*addrSize + 8 + nonceSize + macSize
	// AckSize is the length of the ack.
	AckSize = macSize

	addrSize  = 16 + 2 // an address as 16 bytes (IPv4 mapped) and a port
	nonceSize = 16
	macSize   = sha256.Size
)

var preambleMagic = [4]byte{'D', 'P', 'v', '2'}

// PreambleKind says how the connection came to the listener's host socket.
type PreambleKind byte

const (
	// PreambleDirect is a connection from another pod's container: the client's supervisor
	// connected to this node over the host network.
	PreambleDirect PreambleKind = 1
	// PreambleLocal is a connection from this pod's own supervisor, on the host's
	// loopback, for a client that reached the listener's pod socket (see the proxy): the
	// preamble carries the client's real address.
	PreambleLocal PreambleKind = 2
)

// Preamble is what the client's supervisor writes first on a connection on the host
// network, and the server's supervisor reads before it hands the connection to the
// server: who the client is in the pod network, which pod it meant to reach, and which
// host socket it is writing from, authenticated. The connection is between nodes, so the
// sockets themselves say nothing of the pods.
type Preamble struct {
	Kind    PreambleKind
	Client  netip.AddrPort // the client in the pod network (for a PreambleDirect its pod and the port of its host socket)
	Backend netip.AddrPort // the pod and port the client meant (for a PreambleLocal the address the client connected to)
	Host    netip.AddrPort // the client's host socket as the server sees it, so that the MAC cannot be replayed from elsewhere
	Time    time.Time      // when the client made it, to the second
	Nonce   [nonceSize]byte
}

// NewPreamble is a preamble made now, with a nonce of its own.
func NewPreamble(kind PreambleKind, client, backend, host netip.AddrPort) (Preamble, error) {
	p := Preamble{Kind: kind, Client: client, Backend: backend, Host: host, Time: time.Now()}
	if _, err := rand.Read(p.Nonce[:]); err != nil {
		return Preamble{}, err
	}
	return p, nil
}

// Marshal is the preamble as it goes on the wire: the magic "DPv2", the kind, the three
// addresses (16 bytes, an IPv4 address mapped, and the port in network order), the time
// in seconds, the nonce, and the HMAC-SHA256 of all that under key.
func (p Preamble) Marshal(key []byte) [PreambleSize]byte {
	var b [PreambleSize]byte
	copy(b[:4], preambleMagic[:])
	b[4] = byte(p.Kind)
	off := 5
	for _, ap := range []netip.AddrPort{p.Client, p.Backend, p.Host} {
		a := ap.Addr().As16()
		copy(b[off:], a[:])
		binary.BigEndian.PutUint16(b[off+16:], ap.Port())
		off += addrSize
	}
	binary.BigEndian.PutUint64(b[off:], uint64(p.Time.Unix()))
	off += 8
	copy(b[off:], p.Nonce[:])
	off += nonceSize
	copy(b[off:], mac(key, b[:off]))
	return b
}

var errBadMAC = errors.New("the MAC is wrong")

// ParsePreamble reads a preamble and checks its MAC; it refuses anything that does not
// start with the magic. Whether the preamble is fresh, and whether it is true of the
// connection it came on, is for the caller.
func ParsePreamble(b, key []byte) (Preamble, error) {
	if len(b) < PreambleSize {
		return Preamble{}, fmt.Errorf("a preamble is %d bytes, got %d", PreambleSize, len(b))
	}
	if [4]byte(b[:4]) != preambleMagic {
		return Preamble{}, errors.New("not a preamble")
	}
	body := PreambleSize - macSize
	if !hmac.Equal(mac(key, b[:body]), b[body:PreambleSize]) {
		return Preamble{}, errBadMAC
	}
	p := Preamble{Kind: PreambleKind(b[4])}
	off := 5
	for _, dst := range []*netip.AddrPort{&p.Client, &p.Backend, &p.Host} {
		*dst = netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[off:off+16])).Unmap(), binary.BigEndian.Uint16(b[off+16:]))
		off += addrSize
	}
	p.Time = time.Unix(int64(binary.BigEndian.Uint64(b[off:])), 0)
	off += 8
	copy(p.Nonce[:], b[off:])
	return p, nil
}

// Ack is what the server's supervisor answers when it hands the connection to the
// server: the HMAC of the preamble's nonce, which only a holder of the key can make.
func (p Preamble) Ack(key []byte) [AckSize]byte {
	return [AckSize]byte(mac(key, append([]byte("DPv2 ack"), p.Nonce[:]...)))
}

func mac(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
