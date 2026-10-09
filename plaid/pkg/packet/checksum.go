package packet

import (
	"encoding/binary"
	"math/bits"
)

// sum16 is the ones-complement sum (RFC 1071), folded to 16 bits, of data read as
// big-endian 16-bit words; an odd last byte counts as the high byte of a word. It adds
// eight bytes at a time, which is the same sum in any byte order once the result is
// swapped back, and what keeps the checksums of whole super-frames cheap.
func sum16(data []byte) uint16 {
	var sum, carry uint64
	for len(data) >= 32 {
		sum, carry = bits.Add64(sum, binary.LittleEndian.Uint64(data[0:]), carry)
		sum, carry = bits.Add64(sum, binary.LittleEndian.Uint64(data[8:]), carry)
		sum, carry = bits.Add64(sum, binary.LittleEndian.Uint64(data[16:]), carry)
		sum, carry = bits.Add64(sum, binary.LittleEndian.Uint64(data[24:]), carry)
		data = data[32:]
	}
	for len(data) >= 8 {
		sum, carry = bits.Add64(sum, binary.LittleEndian.Uint64(data), carry)
		data = data[8:]
	}
	var tail uint64
	for i := len(data) - 1; i >= 0; i-- {
		tail = tail<<8 | uint64(data[i])
	}
	sum, carry = bits.Add64(sum, tail, carry)
	sum += carry
	sum = sum&0xffffffff + sum>>32
	sum = sum&0xffffffff + sum>>32
	sum = sum&0xffff + sum>>16
	sum = sum&0xffff + sum>>16
	return bits.ReverseBytes16(uint16(sum))
}

// add16 adds two folded ones-complement sums.
func add16(a, b uint16) uint16 {
	s := uint32(a) + uint32(b)
	return uint16(s + s>>16)
}

// Checksum16 is the Internet checksum (RFC 1071) of data.
func Checksum16(data []byte) uint16 { return ^sum16(data) }

// pseudoSum16 is the folded sum of the pseudo-header of a TCP segment of length n: the
// addresses (4 or 16 bytes each), the protocol and the length.
func pseudoSum16(src, dst []byte, n int) uint16 {
	return add16(add16(sum16(src), sum16(dst)), add16(IPProtocolTCP, uint16(n)))
}

// setIPv4Checksum sets the header checksum of the IPv4 packet ip, whose header is ihl bytes.
func setIPv4Checksum(ip []byte, ihl int) {
	binary.BigEndian.PutUint16(ip[10:], 0)
	binary.BigEndian.PutUint16(ip[10:], ^sum16(ip[:ihl]))
}

// tcpChecksum is the checksum of the TCP segment seg whose checksum field holds zero.
func tcpChecksum(src, dst, seg []byte) uint16 {
	return ^add16(pseudoSum16(src, dst, len(seg)), sum16(seg))
}
