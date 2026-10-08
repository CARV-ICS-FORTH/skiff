//go:build !linux

package vxlan

import (
	"errors"
	"net"
)

// Only Linux counts a socket's drops and reports its receive buffer here.
const dropCountOOBLen = 0

func enableDropCount(*net.UDPConn) error { return errors.New("not supported on this platform") }

func rcvBuf(*net.UDPConn) int { return 0 }

func parseDropCount([]byte) (uint32, bool) { return 0, false }
