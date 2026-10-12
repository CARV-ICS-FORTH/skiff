//go:build !linux

package vxlan

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
)

// Only Linux has UDP_SEGMENT.
const (
	segmentOffload = false
	segmentOOBLen  = 0
)

func putSegmentSize([]byte, int) {}

func sendSegmented(*net.UDPConn, []byte, []byte, netip.AddrPort) error {
	return errors.New("not supported on this platform")
}

func isV6(syscall.RawConn) bool { return false }

// superIO is never used: without UDP_SEGMENT the engine does not send super-frames whole.
type superIO struct{}

func newSuperIO() *superIO { return new(superIO) }

func (*superIO) set(int, []byte) {}

func (*superIO) clear() {}

func (*superIO) send(*txSock, netip.AddrPort, int, []byte, int) error {
	return errors.New("not supported on this platform")
}
