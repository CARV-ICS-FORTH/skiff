package vxlan

import (
	"encoding/binary"

	"plaid/pkg/packet"
)

// flowSocket picks the send socket, of n, for a frame of the given Ethernet type whose
// payload is ip: by a hash of its inner flow, so that all the datagrams of a flow leave
// from one source port (the receiving kernel then keeps them on one socket, in order) and
// different flows leave from different ones. This is what RFC 7348 (section 5)
// recommends for the outer source port of VXLAN, and what Linux's own VXLAN does.
//
// For IPv4 and IPv6 TCP and UDP that are not fragments the flow is the addresses, the
// protocol and the ports; for fragments, which have no ports but the first, and for other
// protocols it is the addresses and the protocol; anything else (ARP, ...) goes to the
// first socket. The hash is the same for every segment of a flow, whether it was one
// frame or cut from a super-frame, and it allocates nothing.
func flowSocket(etherType uint16, ip []byte, n int) int {
	if n <= 1 {
		return 0
	}
	h, ok := flowHash(etherType, ip)
	if !ok {
		return 0
	}
	return int(h % uint64(n))
}

func flowHash(etherType uint16, ip []byte) (uint64, bool) {
	be := binary.BigEndian
	var h uint64
	var proto uint8
	var ports []byte // the transport header's ports, if the frame has them
	switch {
	case etherType == packet.EtherTypeIPv4 && len(ip) >= packet.IPv4HeaderMinLen && ip[0]>>4 == 4:
		proto = ip[9]
		h = mix(mix(0, be.Uint64(ip[12:])), uint64(proto)) // the source and destination addresses
		hl := int(ip[0]&0xf) * 4
		if hl >= packet.IPv4HeaderMinLen && len(ip) >= hl+4 && be.Uint16(ip[6:])&0x3fff == 0 {
			ports = ip[hl : hl+4]
		}
	case etherType == packet.EtherTypeIPv6 && len(ip) >= 40 && ip[0]>>4 == 6:
		proto = ip[6] // a fragment header, or any other extension header, is the protocol here
		h = mix(0, be.Uint64(ip[8:]))
		h = mix(mix(mix(h, be.Uint64(ip[16:])), be.Uint64(ip[24:])), be.Uint64(ip[32:]))
		h = mix(h, uint64(proto))
		if len(ip) >= 44 {
			ports = ip[40:44]
		}
	default:
		return 0, false
	}
	if ports != nil && (proto == packet.IPProtocolTCP || proto == packet.IPProtocolUDP) {
		h = mix(h, uint64(be.Uint32(ports)))
	}
	return h, true
}

// mix folds v into the hash h, with splitmix64's finalizer so that every input bit
// reaches every output bit.
func mix(h, v uint64) uint64 {
	h ^= v
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h
}
