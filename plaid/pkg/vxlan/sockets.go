package vxlan

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
)

// How many sockets an engine has. A node receives on N sockets in one SO_REUSEPORT
// group, each served by a reader and a worker of its own, so that what it receives is not
// limited to one core. It sends from SendPerReceive x N sockets bound to ports of their
// own, so that the datagrams of different flows come from different source ports and the
// receiving kernel spreads them over its group: with only N, a few source ports often fall
// on the same receive socket and leave others idle; with 4 x N almost never.
const (
	DefaultSockets = 4
	MaxSockets     = 16
	SendPerReceive = 4
)

// Role says what an overlay socket is for, and so what SocketFromFile checks.
type Role int

const (
	// Receive is bound to the overlay's address and port, in the SO_REUSEPORT group.
	Receive Role = iota
	// Send is bound to the overlay's address and a port of its own. It only sends: nothing
	// reads it, and datagrams that reach it are none of the overlay's.
	Send
)

// SocketFromFile turns a descriptor received from another process into one of the
// overlay's UDP sockets, after checking that it is what the overlay was configured to
// use: an unconnected IPv4 UDP socket bound to the given address and, for Receive, the
// given port (for Send, any port but 0, so that it has one of its own), and in a
// SO_REUSEPORT group. The file is not consumed; the returned connection
// has its own duplicate of the descriptor.
func SocketFromFile(f *os.File, bind net.IP, port int, role Role) (*net.UDPConn, error) {
	pc, err := net.FilePacketConn(f)
	if err != nil {
		return nil, fmt.Errorf("descriptor is not a datagram socket: %w", err)
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, errors.New("descriptor is not a UDP socket")
	}
	laddr, _ := conn.LocalAddr().(*net.UDPAddr)
	switch {
	case laddr == nil || laddr.IP.To4() == nil:
		_ = conn.Close()
		return nil, errors.New("UDP socket is not IPv4")
	case conn.RemoteAddr() != nil:
		_ = conn.Close()
		return nil, errors.New("UDP socket is connected")
	case role == Send && laddr.Port == 0:
		_ = conn.Close()
		return nil, errors.New("UDP socket is not bound to a port")
	case role == Receive && laddr.Port != port:
		_ = conn.Close()
		return nil, fmt.Errorf("UDP socket is bound to port %d, want %d", laddr.Port, port)
	case !laddr.IP.To4().Equal(bind.To4()):
		_ = conn.Close()
		return nil, fmt.Errorf("UDP socket is bound to %s, want %s", laddr.IP, bind)
	case role == Receive && !hasReusePort(conn):
		_ = conn.Close()
		return nil, errors.New("UDP socket is not in a SO_REUSEPORT group")
	}
	return conn, nil
}

// ListenSockets opens the sockets of an engine with n receive sockets: the n of the
// SO_REUSEPORT group on bind and port (port 0: the first socket picks a free port and the
// others join it), then SendPerReceive x n sockets on bind and ports of their own. The
// caller closes them all; on error none is left open.
func ListenSockets(network string, bind net.IP, port, n int) (rx, tx []*net.UDPConn, err error) {
	if n < 1 || n > MaxSockets {
		return nil, nil, fmt.Errorf("%d overlay sockets: want 1 to %d", n, MaxSockets)
	}
	defer func() {
		if err != nil {
			for _, c := range append(rx, tx...) {
				_ = c.Close()
			}
			rx, tx = nil, nil
		}
	}()
	group := net.ListenConfig{Control: reusePort}
	for len(rx) < n {
		pc, err := group.ListenPacket(context.Background(), network, (&net.UDPAddr{IP: bind, Port: port}).String())
		if err != nil {
			return rx, tx, err
		}
		rx = append(rx, pc.(*net.UDPConn))
		port = rx[0].LocalAddr().(*net.UDPAddr).Port
	}
	for len(tx) < n*SendPerReceive {
		c, err := net.ListenUDP(network, &net.UDPAddr{IP: bind})
		if err != nil {
			return rx, tx, err
		}
		tx = append(tx, c)
	}
	return rx, tx, nil
}
