package vxlan

import (
	"encoding/binary"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// rxOOBLen is the room the control messages of one read need: SO_RXQ_OVFL and UDP_GRO,
// an int each.
var rxOOBLen = 2 * syscall.CmsgSpace(4)

// enableDropCount makes the kernel attach the socket's drop count to each datagram it
// delivers.
func enableDropCount(conn *net.UDPConn) error {
	return setsockoptInt(conn, syscall.SOL_SOCKET, syscall.SO_RXQ_OVFL, 1)
}

// enableGRO lets the kernel hand over datagrams that arrived together from one peer, of
// one size but for the last, as one read, with their size in a control message.
func enableGRO(conn *net.UDPConn) error {
	return setsockoptInt(conn, unix.SOL_UDP, unix.UDP_GRO, 1)
}

func setsockoptInt(conn *net.UDPConn, level, opt, value int) error {
	rc, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), level, opt, value)
	}); err != nil {
		return err
	}
	return serr
}

// rcvBuf is the socket's effective receive buffer: what the kernel granted, which is
// capped at net.core.rmem_max and counts the doubling Linux applies for bookkeeping.
func rcvBuf(conn *net.UDPConn) int {
	n := 0
	rc, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	_ = rc.Control(func(fd uintptr) {
		n, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	})
	return n
}

// findCmsg returns the data of the first control message in oob of the given level and
// type. It walks the messages itself because syscall.ParseSocketControlMessage allocates.
func findCmsg(oob []byte, level, typ int) ([]byte, bool) {
	for len(oob) >= syscall.SizeofCmsghdr {
		h := (*syscall.Cmsghdr)(unsafe.Pointer(&oob[0]))
		if int(h.Len) < syscall.CmsgLen(0) || int(h.Len) > len(oob) {
			return nil, false
		}
		if int(h.Level) == level && int(h.Type) == typ {
			return oob[syscall.CmsgLen(0):h.Len], true
		}
		next := syscall.CmsgSpace(int(h.Len) - syscall.CmsgLen(0))
		if next > len(oob) {
			return nil, false
		}
		oob = oob[next:]
	}
	return nil, false
}

// parseDropCount finds the SO_RXQ_OVFL message in the control data of a read: the
// number of datagrams the kernel dropped on the socket, as of this one's arrival.
func parseDropCount(oob []byte) (uint32, bool) {
	data, ok := findCmsg(oob, syscall.SOL_SOCKET, syscall.SO_RXQ_OVFL)
	if !ok || len(data) < 4 {
		return 0, false
	}
	return binary.NativeEndian.Uint32(data), true
}

// parseGROSize finds the UDP_GRO message in the control data of a read: the size of the
// datagrams that the read holds back to back, or 0 if it is one datagram.
func parseGROSize(oob []byte) int {
	data, ok := findCmsg(oob, unix.SOL_UDP, unix.UDP_GRO)
	switch {
	case !ok:
		return 0
	case len(data) >= 4:
		return int(binary.NativeEndian.Uint32(data))
	case len(data) >= 2:
		return int(binary.NativeEndian.Uint16(data))
	}
	return 0
}
