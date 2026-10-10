// Package packettest builds IPv4 TCP and UDP packets and checks them independently of
// the code that rewrites them: it recomputes the checksums from scratch.
package packettest

import (
	"encoding/binary"
	"fmt"
	"net"

	"plaid/pkg/packet"
)

func ip4(s string) [4]byte {
	var a [4]byte
	copy(a[:], net.ParseIP(s).To4())
	return a
}

// IPHeader returns a 20-byte IPv4 header for a packet of the given total length.
func ipHeader(proto byte, src, dst string, total int) []byte {
	h := make([]byte, 20)
	h[0] = 0x45
	binary.BigEndian.PutUint16(h[2:4], uint16(total))
	binary.BigEndian.PutUint16(h[4:6], 0x1234)
	h[8] = 64
	h[9] = proto
	s, d := ip4(src), ip4(dst)
	copy(h[12:16], s[:])
	copy(h[16:20], d[:])
	binary.BigEndian.PutUint16(h[10:12], packet.Checksum16(h))
	return h
}

func pseudoSum(proto byte, src, dst []byte, segment []byte) uint16 {
	ph := make([]byte, 12, 12+len(segment))
	copy(ph[0:4], src)
	copy(ph[4:8], dst)
	ph[9] = proto
	binary.BigEndian.PutUint16(ph[10:12], uint16(len(segment)))
	return packet.Checksum16(append(ph, segment...))
}

// TCP builds a TCP packet with a valid checksum.
func TCP(src string, sport uint16, dst string, dport uint16, flags byte, payload string) []byte {
	seg := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(seg[0:2], sport)
	binary.BigEndian.PutUint16(seg[2:4], dport)
	binary.BigEndian.PutUint32(seg[4:8], 1000)
	seg[12] = 5 << 4
	seg[13] = flags
	binary.BigEndian.PutUint16(seg[14:16], 65535)
	copy(seg[20:], payload)
	s, d := ip4(src), ip4(dst)
	binary.BigEndian.PutUint16(seg[16:18], pseudoSum(packet.IPProtocolTCP, s[:], d[:], seg))
	return append(ipHeader(packet.IPProtocolTCP, src, dst, 20+len(seg)), seg...)
}

// Partial leaves the TCP or UDP checksum of pkt as a sender that offloads it does: the folded
// sum of the pseudo-header alone, not complemented, for the stack to complete.
func Partial(pkt []byte) {
	l, _ := packet.ParseL3(pkt)
	t := l.Transport()
	ph := make([]byte, 12)
	copy(ph[0:4], l.Src[:])
	copy(ph[4:8], l.Dst[:])
	ph[9] = l.Proto
	binary.BigEndian.PutUint16(ph[10:12], uint16(len(t)))
	off := 16 // the checksum field of TCP, of UDP at 6
	if l.Proto == packet.IPProtocolUDP {
		off = 6
	}
	binary.BigEndian.PutUint16(t[off:off+2], ^packet.Checksum16(ph))
}

// UDP builds a UDP datagram with a valid checksum, or with none (zero) when noChecksum is set.
func UDP(src string, sport uint16, dst string, dport uint16, payload string, noChecksum bool) []byte {
	seg := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(seg[0:2], sport)
	binary.BigEndian.PutUint16(seg[2:4], dport)
	binary.BigEndian.PutUint16(seg[4:6], uint16(len(seg)))
	copy(seg[8:], payload)
	if !noChecksum {
		s, d := ip4(src), ip4(dst)
		sum := pseudoSum(packet.IPProtocolUDP, s[:], d[:], seg)
		if sum == 0 {
			sum = 0xffff
		}
		binary.BigEndian.PutUint16(seg[6:8], sum)
	}
	return append(ipHeader(packet.IPProtocolUDP, src, dst, 20+len(seg)), seg...)
}

// Verify recomputes the IPv4 header checksum and, for TCP and for UDP that carries one,
// the transport checksum, and reports the first that is wrong.
func Verify(pkt []byte) error {
	l, err := packet.ParseL3(pkt)
	if err != nil {
		return err
	}
	if packet.Checksum16(l.Pkt[:l.HdrLen]) != 0 {
		return fmt.Errorf("IPv4 header checksum is wrong")
	}
	switch l.Proto {
	case packet.IPProtocolTCP:
		if pseudoSum(l.Proto, l.Src[:], l.Dst[:], l.Transport()) != 0 {
			return fmt.Errorf("TCP checksum is wrong")
		}
	case packet.IPProtocolUDP:
		t := l.Transport()
		if binary.BigEndian.Uint16(t[6:8]) == 0 {
			return nil
		}
		if pseudoSum(l.Proto, l.Src[:], l.Dst[:], t) != 0 {
			return fmt.Errorf("UDP checksum is wrong")
		}
	}
	return nil
}

// Tuple describes a packet's addresses and ports for assertions.
type Tuple struct {
	Src   string
	Sport uint16
	Dst   string
	Dport uint16
}

// Parse returns the tuple of a TCP or UDP packet.
func Parse(pkt []byte) (Tuple, error) {
	l, err := packet.ParseL3(pkt)
	if err != nil {
		return Tuple{}, err
	}
	if err := l.HasPorts(); err != nil {
		return Tuple{}, err
	}
	return Tuple{
		Src: net.IP(l.Src[:]).String(), Sport: l.SrcPort(),
		Dst: net.IP(l.Dst[:]).String(), Dport: l.DstPort(),
	}, nil
}
