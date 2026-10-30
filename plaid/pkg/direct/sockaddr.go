package direct

import (
	"encoding/binary"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

// Sizes of the kernel's socket address structures.
const (
	sockaddrInetSize  = 16 // struct sockaddr_in
	sockaddrInet6Size = 28 // struct sockaddr_in6
)

// parseSockaddr decodes a sockaddr_in or sockaddr_in6 as the kernel lays it out. An
// IPv4-mapped IPv6 address is returned as the IPv4 one it stands for.
func parseSockaddr(b []byte) (netip.AddrPort, bool) {
	if len(b) < 2 {
		return netip.AddrPort{}, false
	}
	switch binary.NativeEndian.Uint16(b) {
	case syscall.AF_INET:
		if len(b) < 8 {
			return netip.AddrPort{}, false
		}
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), binary.BigEndian.Uint16(b[2:4])), true
	case syscall.AF_INET6:
		if len(b) < 24 {
			return netip.AddrPort{}, false
		}
		return netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[8:24])).Unmap(), binary.BigEndian.Uint16(b[2:4])), true
	}
	return netip.AddrPort{}, false
}

// encodeSockaddr lays ap out as a socket of the given family reports an address: AF_INET,
// or AF_INET6 where an IPv4 address is written mapped.
func encodeSockaddr(ap netip.AddrPort, family int) []byte {
	if family == syscall.AF_INET6 {
		b := make([]byte, sockaddrInet6Size)
		binary.NativeEndian.PutUint16(b, syscall.AF_INET6)
		binary.BigEndian.PutUint16(b[2:], ap.Port())
		a := ap.Addr().As16()
		copy(b[8:], a[:])
		return b
	}
	b := make([]byte, sockaddrInetSize)
	binary.NativeEndian.PutUint16(b, syscall.AF_INET)
	binary.BigEndian.PutUint16(b[2:], ap.Port())
	a := ap.Addr().Unmap().As4()
	copy(b[4:], a[:])
	return b
}

// sockaddrFor is ap as an address to connect or bind a socket of the family to.
func sockaddrFor(ap netip.AddrPort, family int) unix.Sockaddr {
	if family == syscall.AF_INET6 {
		return &unix.SockaddrInet6{Port: int(ap.Port()), Addr: ap.Addr().As16()}
	}
	return &unix.SockaddrInet4{Port: int(ap.Port()), Addr: ap.Addr().Unmap().As4()}
}

// addrPortOf is the address a socket reports, IPv4-mapped ones unmapped.
func addrPortOf(sa unix.Sockaddr) netip.AddrPort {
	switch s := sa.(type) {
	case *unix.SockaddrInet4:
		return netip.AddrPortFrom(netip.AddrFrom4(s.Addr), uint16(s.Port))
	case *unix.SockaddrInet6:
		return netip.AddrPortFrom(netip.AddrFrom16(s.Addr).Unmap(), uint16(s.Port))
	}
	return netip.AddrPort{}
}
