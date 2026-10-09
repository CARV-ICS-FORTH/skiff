package packet

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrOffload is wrapped by the errors for a frame whose virtio header does not describe it.
var ErrOffload = errors.New("malformed offloaded frame")

// TCP header flags (the byte at offset 13) that segmentation and coalescing treat
// specially.
const (
	tcpPsh = 0x08
	tcpCwr = 0x80
)

const (
	ipv6HeaderLen = 40
	tcpCsumOffset = 16 // of the checksum field in a TCP header
)

// A Segmenter turns what a TAP with offloads returns into ordinary frames: complete,
// MTU-sized, with full checksums, which is all the rest of the datapath handles. It keeps
// the segments it makes in one buffer that it reuses, so that segmenting allocates
// nothing once the buffer has grown to the size of the traffic.
type Segmenter struct {
	buf    []byte
	frames [][]byte
}

// Segment returns the frames that frame, read from a TAP with the virtio header h, stands
// for. A frame the kernel has fully prepared is returned as it is; one whose checksum is
// not done is completed in place; a TCP segment with a GSO type, with the ECN bit or without, is
// cut into segments of h.GSOSize bytes of payload, each with its own headers, and the CWR
// flag on the first only, as the kernel's own segmentation does it. The result is valid until the
// next call and is empty on error.
func (s *Segmenter) Segment(h VnetHeader, frame []byte) ([][]byte, error) {
	s.frames = s.frames[:0]
	switch tcp := h.GSOType &^ VnetGSOECN; {
	case h.GSOType == VnetGSONone:
		if h.Flags&VnetNeedsCsum != 0 {
			if err := completeChecksum(h, frame); err != nil {
				return nil, err
			}
		}
		s.frames = append(s.frames, frame)
		return s.frames, nil
	case tcp == VnetGSOTCPv4 || tcp == VnetGSOTCPv6:
		return s.cut(h, frame)
	}
	return nil, fmt.Errorf("%w: GSO type %#x", ErrOffload, h.GSOType)
}

// completeChecksum writes the checksum the sender left to the kernel: the field holds
// the sum of the pseudo-header, and the checksum of everything from the transport header
// on, field included, is what completes it.
func completeChecksum(h VnetHeader, frame []byte) error {
	start, field := int(h.CsumStart), int(h.CsumStart)+int(h.CsumOffset)
	if start < EthernetHeaderLen || field+2 > len(frame) {
		return fmt.Errorf("%w: checksum at %d in a frame of %d", ErrOffload, field, len(frame))
	}
	sum := ^sum16(frame[start:])
	if sum == 0 {
		sum = 0xffff // as the kernel writes it: zero means "none" to UDP
	}
	binary.BigEndian.PutUint16(frame[field:], sum)
	return nil
}

// tcpFrame locates the headers of an Ethernet frame that carries TCP over IPv4 or IPv6
// without extension headers, options (IPv4) or fragmentation: the offsets of the IP and
// TCP headers and the end of the TCP header.
type tcpFrame struct {
	v6               bool
	ihl, l4, payload int // the length of the IP header, the offsets of the TCP header and its payload
}

// addrs are the source and destination addresses in frame.
func (f tcpFrame) addrs(frame []byte) (src, dst []byte) {
	ip := frame[EthernetHeaderLen:]
	if f.v6 {
		return ip[8:24], ip[24:40]
	}
	return ip[12:16], ip[16:20]
}

// parseTCPFrame finds the headers of frame, which is IPv6 if v6 and IPv4 otherwise; ok is
// false if it is not TCP of that shape.
func parseTCPFrame(frame []byte, v6 bool) (f tcpFrame, ok bool) {
	f.v6 = v6
	if len(frame) < EthernetHeaderLen+IPv4HeaderMinLen {
		return f, false
	}
	ip := frame[EthernetHeaderLen:]
	if etherType := binary.BigEndian.Uint16(frame[12:]); !v6 {
		if etherType != EtherTypeIPv4 || ip[0]>>4 != 4 || ip[9] != IPProtocolTCP {
			return f, false
		}
		f.ihl = int(ip[0]&0x0f) * 4
		if f.ihl < IPv4HeaderMinLen || binary.BigEndian.Uint16(ip[6:])&0x3fff != 0 {
			return f, false
		}
	} else {
		if etherType != EtherTypeIPv6 || len(ip) < ipv6HeaderLen || ip[0]>>4 != 6 || ip[6] != IPProtocolTCP {
			return f, false
		}
		f.ihl = ipv6HeaderLen
	}
	f.l4 = EthernetHeaderLen + f.ihl
	if len(frame) < f.l4+tcpHeaderMinLen {
		return f, false
	}
	f.payload = f.l4 + int(frame[f.l4+12]>>4)*4
	return f, f.payload >= f.l4+tcpHeaderMinLen && f.payload <= len(frame)
}

// cut is Segment for the TCP segments the kernel sends as one.
func (s *Segmenter) cut(h VnetHeader, frame []byte) ([][]byte, error) {
	v6 := h.GSOType&^VnetGSOECN == VnetGSOTCPv6
	f, ok := parseTCPFrame(frame, v6)
	if !ok {
		return nil, fmt.Errorf("%w: GSO type %d frame is not TCP over IP without options or fragments", ErrOffload, h.GSOType)
	}
	size := int(h.GSOSize)
	if h.Flags&VnetNeedsCsum == 0 || int(h.CsumStart) != f.l4 || int(h.CsumOffset) != tcpCsumOffset ||
		size == 0 || f.payload == len(frame) {
		return nil, fmt.Errorf("%w: GSO frame with flags %#x, checksum at %d+%d, segment size %d",
			ErrOffload, h.Flags, h.CsumStart, h.CsumOffset, size)
	}

	hdr, data := f.payload, len(frame)-f.payload
	n := (data + size - 1) / size
	total := len(frame) + (n-1)*hdr
	if cap(s.buf) < total {
		s.buf = make([]byte, total)
	}
	out := s.buf[:total]
	tcpLen := f.payload - f.l4
	src, dst := f.addrs(frame)
	seq := binary.BigEndian.Uint32(frame[f.l4+4:])
	id := binary.BigEndian.Uint16(frame[EthernetHeaderLen+4:])

	for i, off := 0, 0; i < n; i++ {
		chunk := min(size, data-i*size)
		seg := out[off : off+hdr+chunk : off+hdr+chunk]
		off += hdr + chunk
		copy(seg, frame[:hdr])
		copy(seg[hdr:], frame[hdr+i*size:])

		ip, tcp := seg[EthernetHeaderLen:], seg[f.l4:]
		if v6 {
			binary.BigEndian.PutUint16(ip[4:], uint16(tcpLen+chunk))
		} else {
			binary.BigEndian.PutUint16(ip[2:], uint16(f.ihl+tcpLen+chunk))
			binary.BigEndian.PutUint16(ip[4:], id+uint16(i))
			setIPv4Checksum(ip, f.ihl)
		}
		binary.BigEndian.PutUint32(tcp[4:], seq+uint32(i*size))
		if i < n-1 {
			tcp[13] &^= TCPFin | tcpPsh
		}
		if i > 0 {
			tcp[13] &^= tcpCwr
		}
		binary.BigEndian.PutUint16(tcp[tcpCsumOffset:], 0)
		binary.BigEndian.PutUint16(tcp[tcpCsumOffset:], tcpChecksum(src, dst, tcp))
		s.frames = append(s.frames, seg)
	}
	return s.frames, nil
}
