package vxlan

import (
	"encoding/binary"
	"net"
	"syscall"
	"unsafe"
)

// dropCountOOBLen is the room one SO_RXQ_OVFL control message needs.
var dropCountOOBLen = syscall.CmsgSpace(4)

// enableDropCount makes the kernel attach the socket's drop count to each datagram it
// delivers.
func enableDropCount(conn *net.UDPConn) error {
	rc, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RXQ_OVFL, 1)
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

// parseDropCount finds the SO_RXQ_OVFL message in the control data of a read: the
// number of datagrams the kernel dropped on the socket, as of this one's arrival. It
// walks the messages itself because syscall.ParseSocketControlMessage allocates.
func parseDropCount(oob []byte) (uint32, bool) {
	for len(oob) >= syscall.SizeofCmsghdr {
		h := (*syscall.Cmsghdr)(unsafe.Pointer(&oob[0]))
		if int(h.Len) < syscall.CmsgLen(0) || int(h.Len) > len(oob) {
			return 0, false
		}
		if h.Level == syscall.SOL_SOCKET && h.Type == syscall.SO_RXQ_OVFL && int(h.Len) >= syscall.CmsgLen(4) {
			return binary.NativeEndian.Uint32(oob[syscall.CmsgLen(0):]), true
		}
		next := syscall.CmsgSpace(int(h.Len) - syscall.CmsgLen(0))
		if next > len(oob) {
			return 0, false
		}
		oob = oob[next:]
	}
	return 0, false
}
