//go:build linux

package vxlan

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// segmentOffload says that this platform has UDP_SEGMENT.
const segmentOffload = true

// segmentOOBLen is the room the UDP_SEGMENT control message needs: a uint16.
var segmentOOBLen = syscall.CmsgSpace(2)

// putSegmentSize writes the UDP_SEGMENT control message for datagrams of size bytes
// into oob, which has room for segmentOOBLen.
func putSegmentSize(oob []byte, size int) {
	clear(oob)
	h := (*syscall.Cmsghdr)(unsafe.Pointer(&oob[0]))
	h.Level, h.Type = unix.SOL_UDP, unix.UDP_SEGMENT
	h.SetLen(syscall.CmsgLen(2))
	binary.NativeEndian.PutUint16(oob[syscall.CmsgLen(0):], uint16(size))
}

// sendSegmented sends b, datagrams of the size in oob back to back (the last may be
// shorter), to addr with one system call; the kernel, or the network card, cuts it.
func sendSegmented(conn *net.UDPConn, b, oob []byte, addr netip.AddrPort) error {
	_, _, err := conn.WriteMsgUDPAddrPort(b, oob, addr)
	return err
}

// isV6 reports whether the socket behind rc is an AF_INET6 one, which a socket bound to
// the wildcard address is: it takes IPv4 addresses as v4-mapped ones.
func isV6(rc syscall.RawConn) bool {
	domain := 0
	_ = rc.Control(func(fd uintptr) { domain, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_DOMAIN) })
	return domain == unix.AF_INET6
}

// superIO is the message of one send of a super-frame's chunks with UDP_SEGMENT: a header
// and a part of the frame for each datagram, in one scatter list, so that the frame is
// read from where it is.
type superIO struct {
	iov   [2 * maxSegments]unix.Iovec
	msg   unix.Msghdr
	sa4   unix.RawSockaddrInet4
	sa6   unix.RawSockaddrInet6
	n     int
	errno syscall.Errno
	do    func(fd uintptr) bool // sendmsg, made once because a closure made per call is an allocation
}

func newSuperIO() *superIO {
	w := new(superIO)
	w.do = w.sendmsg
	return w
}

func (w *superIO) sendmsg(fd uintptr) bool {
	n, _, errno := syscall.Syscall(syscall.SYS_SENDMSG, fd, uintptr(unsafe.Pointer(&w.msg)), 0)
	if errno == syscall.EAGAIN {
		return false // wait until the socket can take the datagrams
	}
	w.n, w.errno = int(n), errno
	return true
}

// set makes b the i'th part of the next send.
func (w *superIO) set(i int, b []byte) {
	w.iov[i].Base = &b[0]
	w.iov[i].SetLen(len(b))
}

// clear lets go of the parts, which are not the scatter list's to keep.
func (w *superIO) clear() { clear(w.iov[:]) }

func setPort(p *uint16, port uint16) {
	b := (*[2]byte)(unsafe.Pointer(p)) // in network byte order
	b[0], b[1] = byte(port>>8), byte(port)
}

// send sends the first parts parts, want bytes, from ts's socket to the IPv4 address to,
// with the control message oob if there is one.
func (w *superIO) send(ts *txSock, to netip.AddrPort, parts int, oob []byte, want int) error {
	m := &w.msg
	*m = unix.Msghdr{Iov: &w.iov[0]}
	m.SetIovlen(parts)
	ip4 := to.Addr().As4()
	if ts.v6 {
		w.sa6 = unix.RawSockaddrInet6{Family: unix.AF_INET6}
		w.sa6.Addr = [16]byte{10: 0xff, 11: 0xff, 12: ip4[0], 13: ip4[1], 14: ip4[2], 15: ip4[3]}
		setPort(&w.sa6.Port, to.Port())
		m.Name, m.Namelen = (*byte)(unsafe.Pointer(&w.sa6)), unix.SizeofSockaddrInet6
	} else {
		w.sa4 = unix.RawSockaddrInet4{Family: unix.AF_INET, Addr: ip4}
		setPort(&w.sa4.Port, to.Port())
		m.Name, m.Namelen = (*byte)(unsafe.Pointer(&w.sa4)), unix.SizeofSockaddrInet4
	}
	if len(oob) > 0 {
		m.Control = &oob[0]
		m.SetControllen(len(oob))
	}
	w.n, w.errno = 0, 0
	err := ts.rc.Write(w.do)
	switch {
	case err != nil:
		return err
	case w.errno != 0:
		return w.errno
	case w.n != want:
		return io.ErrShortWrite
	}
	return nil
}
