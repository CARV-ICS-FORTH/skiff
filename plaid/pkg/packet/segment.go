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

// space returns n bytes of the segmenter's buffer, which grows to the size of the traffic.
func (s *Segmenter) space(n int) []byte {
	if cap(s.buf) < n {
		s.buf = make([]byte, n)
	}
	return s.buf[:n]
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

// checkTSO finds the headers of frame, a TCP segment of up to 64 KiB for which h says how
// to cut it and complete its checksum, and checks that h describes it.
func checkTSO(h VnetHeader, frame []byte) (tcpFrame, error) {
	f, ok := parseTCPFrame(frame, h.GSOType&^VnetGSOECN == VnetGSOTCPv6)
	if !ok {
		return f, fmt.Errorf("%w: GSO type %d frame is not TCP over IP without options or fragments", ErrOffload, h.GSOType)
	}
	if h.Flags&VnetNeedsCsum == 0 || int(h.CsumStart) != f.l4 || int(h.CsumOffset) != tcpCsumOffset ||
		h.GSOSize == 0 || f.payload == len(frame) {
		return f, fmt.Errorf("%w: GSO frame with flags %#x, checksum at %d+%d, segment size %d",
			ErrOffload, h.Flags, h.CsumStart, h.CsumOffset, h.GSOSize)
	}
	return f, nil
}

// IsTSO reports whether h describes a TCP segment of up to 64 KiB (with the ECN bit or
// without) whose checksum is still to be done: what Segment cuts, and what a TAP with
// offloads takes whole.
func (h VnetHeader) IsTSO() bool {
	t := h.GSOType &^ VnetGSOECN
	return (t == VnetGSOTCPv4 || t == VnetGSOTCPv6) && h.Flags&VnetNeedsCsum != 0
}

// CheckTSO checks that h describes frame as Segment does before it cuts a TSO segment.
func CheckTSO(h VnetHeader, frame []byte) error {
	_, err := checkTSO(h, frame)
	return err
}

// The smallest segment size a TSO segment may state, the smallest MSS that TCP allows for
// its IP version (576 and 1280 less the IP and TCP headers), and the most segments it may
// stand for. A pod's stack states a size from its MSS; one that comes from another node
// does not have to, and a tiny one makes whoever cuts the frame (the Segmenter, the
// pod's kernel) produce a huge number of segments.
const (
	MinTSOSizeV4   = 536
	MinTSOSizeV6   = 1220
	MaxTSOSegments = 128
)

// TSOSizeOK reports whether h, the virtio header of a TCP segment with payload bytes of
// data, states a segment size within the limits above.
func (h VnetHeader) TSOSizeOK(payload int) bool {
	least, size := MinTSOSizeV4, int(h.GSOSize)
	if h.GSOType&^VnetGSOECN == VnetGSOTCPv6 {
		least = MinTSOSizeV6
	}
	return size >= least && (payload+size-1)/size <= MaxTSOSegments
}

// CheckTSOSize is CheckTSO and, as a node that takes a super-frame from the overlay
// needs, a check of the segment size h states against the limits above.
func CheckTSOSize(h VnetHeader, frame []byte) error {
	f, err := checkTSO(h, frame)
	if err == nil && !h.TSOSizeOK(len(frame)-f.payload) {
		err = fmt.Errorf("%w: segment size %d for %d bytes of data", ErrOffload, h.GSOSize, len(frame)-f.payload)
	}
	return err
}

// cut is Segment for the TCP segments the kernel sends as one.
func (s *Segmenter) cut(h VnetHeader, frame []byte) ([][]byte, error) {
	f, err := checkTSO(h, frame)
	if err != nil {
		return nil, err
	}
	v6, size := h.GSOType&^VnetGSOECN == VnetGSOTCPv6, int(h.GSOSize)

	hdr, data := f.payload, len(frame)-f.payload
	n := (data + size - 1) / size
	total := len(frame) + (n-1)*hdr
	out := s.space(total)
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
