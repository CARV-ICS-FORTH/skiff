//go:build !linux

package vxlan

import (
	"errors"
	"net"
	"syscall"
)

// Only Linux spreads the datagrams for a port over a group of sockets: elsewhere an
// engine can have one receive socket.
func reusePort(string, string, syscall.RawConn) error { return nil }

func hasReusePort(*net.UDPConn) bool { return true }

// Only Linux counts a socket's drops, reports its receive buffer and coalesces what it
// receives here.
const rxOOBLen = 0

func enableDropCount(*net.UDPConn) error { return errors.New("not supported on this platform") }

func enableGRO(*net.UDPConn) error { return errors.New("not supported on this platform") }

func rcvBuf(*net.UDPConn) int { return 0 }

func parseDropCount([]byte) (uint32, bool) { return 0, false }

func parseGROSize([]byte) int { return 0 }
