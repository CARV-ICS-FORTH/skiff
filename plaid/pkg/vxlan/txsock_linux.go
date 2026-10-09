//go:build linux

package vxlan

import (
	"encoding/binary"
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
