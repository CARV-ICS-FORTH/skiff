package packet

import (
	"encoding/binary"
	"errors"
	"net"
)

// TCP flag bits.
const (
	TCPFin = 0x01
	TCPSyn = 0x02
	TCPRst = 0x04
	TCPAck = 0x10
)

const (
	udpHeaderLen    = 8
	tcpHeaderMinLen = 20
)

var (
	ErrNotIPv4        = errors.New("not an IPv4 packet")
	ErrBadIPv4Length  = errors.New("bad IPv4 length")
	ErrTransportTrunc = errors.New("truncated transport header")
)

// L3 is a parsed view of an IPv4 packet. It does not copy: Pkt is the packet itself,
// trimmed to the length its header states (an Ethernet frame may carry padding).
type L3 struct {
	Pkt      []byte
	HdrLen   int
	Proto    uint8
	Src, Dst [4]byte
	// Fragment is set for any fragment (more fragments, or a non-zero offset): only the
	// first carries the transport header, so a translator that keys on ports cannot
	// handle it.
	Fragment bool
	// PartialSum says that the TCP checksum field holds only the folded sum of the
	// pseudo-header, not its complement: the sender left the rest to the stack, as for a
	// TSO segment (NEEDS_CSUM). Set by the caller that knows; ParseL3 cannot tell.
	PartialSum bool
}

// ParseL3 validates the IPv4 header of pkt (version, header and total length) and returns
// a view of it.
func ParseL3(pkt []byte) (L3, error) {
	var l L3
	if len(pkt) < IPv4HeaderMinLen || pkt[0]>>4 != 4 {
		return l, ErrNotIPv4
	}
	hdr := int(pkt[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(pkt[2:4]))
	if hdr < IPv4HeaderMinLen || total < hdr || total > len(pkt) {
		return l, ErrBadIPv4Length
	}
	l.Pkt = pkt[:total]
	l.HdrLen = hdr
	l.Proto = pkt[9]
	copy(l.Src[:], pkt[12:16])
	copy(l.Dst[:], pkt[16:20])
	flagsFrag := binary.BigEndian.Uint16(pkt[6:8])
	l.Fragment = flagsFrag&0x2000 != 0 || flagsFrag&0x1fff != 0
	return l, nil
}

// Transport returns the bytes after the IP header.
func (l L3) Transport() []byte { return l.Pkt[l.HdrLen:] }

// HasPorts reports whether the packet is an unfragmented TCP or UDP packet whose
// transport header is complete, which is what the ports and flags accessors need.
func (l L3) HasPorts() error {
	if l.Fragment {
		return ErrTransportTrunc
	}
	t := l.Transport()
	switch l.Proto {
	case IPProtocolTCP:
		if len(t) < tcpHeaderMinLen {
			return ErrTransportTrunc
		}
		if off := int(t[12]>>4) * 4; off < tcpHeaderMinLen || off > len(t) {
			return ErrTransportTrunc
		}
	case IPProtocolUDP:
		if len(t) < udpHeaderLen {
			return ErrTransportTrunc
		}
	default:
		return ErrTransportTrunc
	}
	return nil
}

func (l L3) SrcPort() uint16 { return binary.BigEndian.Uint16(l.Transport()[0:2]) }
func (l L3) DstPort() uint16 { return binary.BigEndian.Uint16(l.Transport()[2:4]) }

// TCPFlags returns the flag byte of a TCP packet.
func (l L3) TCPFlags() uint8 { return l.Transport()[13] }

// Rewrite changes the addresses and ports of the packet in place, updating the IPv4
// header checksum and the TCP or UDP checksum incrementally (RFC 1624). A nil argument
// leaves that field alone. A UDP datagram without a checksum (zero) stays without one.
// With PartialSum a change of address changes the field as a change of the pseudo-header
// sum, and a change of port leaves it alone: the ports are in the data the stack will
// still sum (the same as Linux's inet_proto_csum_replace4 for CHECKSUM_PARTIAL). The
// packet must satisfy HasPorts. The view's Src and Dst copies are not refreshed:
// parse the packet again to read the new addresses.
func (l L3) Rewrite(src, dst *[4]byte, sport, dport *uint16) {
	p := l.Pkt
	t := l.Transport()
	var csumOff int
	switch l.Proto {
	case IPProtocolTCP:
		csumOff = 16
	default:
		csumOff = 6
	}
	transportSum := binary.BigEndian.Uint16(t[csumOff : csumOff+2])
	ipSum := binary.BigEndian.Uint16(p[10:12])
	hasTransportSum := l.Proto == IPProtocolTCP || transportSum != 0
	partial := l.PartialSum && l.Proto == IPProtocolTCP
	if partial {
		transportSum = ^transportSum // as the checksum of the pseudo-header alone
	}

	replace32 := func(off int, new *[4]byte) {
		var old [4]byte
		copy(old[:], p[off:off+4])
		copy(p[off:off+4], new[:])
		ipSum = csumReplace(ipSum, old[:], new[:])
		if hasTransportSum {
			transportSum = csumReplace(transportSum, old[:], new[:]) // pseudo-header address
		}
	}
	replace16 := func(off int, new uint16) {
		var old, nw [2]byte
		copy(old[:], t[off:off+2])
		binary.BigEndian.PutUint16(nw[:], new)
		copy(t[off:off+2], nw[:])
		if hasTransportSum && !partial {
			transportSum = csumReplace(transportSum, old[:], nw[:])
		}
	}
	if src != nil {
		replace32(12, src)
	}
	if dst != nil {
		replace32(16, dst)
	}
	if sport != nil {
		replace16(0, *sport)
	}
	if dport != nil {
		replace16(2, *dport)
	}
	binary.BigEndian.PutUint16(p[10:12], ipSum)
	if partial {
		transportSum = ^transportSum
	}
	if hasTransportSum {
		if l.Proto == IPProtocolUDP && transportSum == 0 {
			transportSum = 0xffff // zero means "no checksum" on the wire
		}
		binary.BigEndian.PutUint16(t[csumOff:csumOff+2], transportSum)
	}
}

// csumReplace updates a ones-complement checksum for a change of old to new bytes
// (equal, even lengths): HC' = ~(~HC + ~m + m') over 16-bit words.
func csumReplace(sum uint16, old, new []byte) uint16 {
	acc := uint32(^sum)
	for i := 0; i+1 < len(old); i += 2 {
		acc += uint32(^binary.BigEndian.Uint16(old[i:])) & 0xffff
		acc += uint32(binary.BigEndian.Uint16(new[i:]))
	}
	for acc > 0xffff {
		acc = (acc & 0xffff) + (acc >> 16)
	}
	return ^uint16(acc)
}

// ICMP destination unreachable codes.
const (
	ICMPPortUnreachable = 3
)

// ICMPDestUnreachable builds the packet a host sends when nothing listens: an ICMP
// destination unreachable of the given code, from `from` to the sender of orig, quoting
// orig's IP header and the first eight bytes of its payload.
func ICMPDestUnreachable(orig L3, from net.IP, code uint8) []byte {
	quote := orig.HdrLen + 8
	if quote > len(orig.Pkt) {
		quote = len(orig.Pkt)
	}
	icmpLen := 8 + quote
	out := make([]byte, IPv4HeaderMinLen+icmpLen)
	out[0] = 0x45
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)))
	out[8] = 64
	out[9] = IPProtocolICMP
	copy(out[12:16], from.To4())
	copy(out[16:20], orig.Src[:])
	binary.BigEndian.PutUint16(out[10:12], Checksum16(out[:IPv4HeaderMinLen]))
	icmp := out[IPv4HeaderMinLen:]
	icmp[0] = 3 // destination unreachable
	icmp[1] = code
	copy(icmp[8:], orig.Pkt[:quote])
	binary.BigEndian.PutUint16(icmp[2:4], Checksum16(icmp))
	return out
}
