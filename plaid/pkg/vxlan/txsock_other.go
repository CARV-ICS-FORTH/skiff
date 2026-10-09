//go:build !linux

package vxlan

import (
	"errors"
	"net"
	"net/netip"
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
