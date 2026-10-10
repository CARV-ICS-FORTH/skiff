package packet

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	// ErrNotTCP is returned for a frame that is not a TCP segment that a TSO header can
	// describe (IPv4 or IPv6 without extension headers, not fragmented).
	ErrNotTCP = errors.New("not a TCP segment")
	// ErrTooLarge is returned for a packet larger than the MTU that cannot be made to fit
	// it: it has the Don't Fragment flag or IP options, or it is not IPv4.
	ErrTooLarge = errors.New("packet larger than the MTU")
)

// IPv4 flag bits as they stand in the 16 bits at offset 6 of the header, and the mask of
// the fragment offset.
const (
	ipv4DF       = 0x4000
	ipv4MF       = 0x2000
	ipv4FragMask = 0x1fff
)

// ipLength is the length that the header of the IP packet ip states for it, with its
// header, or -1 if ip is too short to say.
func ipLength(ip []byte, v6 bool) int {
	switch {
	case v6 && len(ip) >= ipv6HeaderLen:
		return ipv6HeaderLen + int(binary.BigEndian.Uint16(ip[4:]))
	case !v6 && len(ip) >= IPv4HeaderMinLen:
		return int(binary.BigEndian.Uint16(ip[2:]))
	}
	return -1
}

// TSOFor prepares frame, which is larger than mtu (the MTU of the pods), to be cut by a
// Segmenter into frames that fit it, and returns the virtio header to cut it with. A TCP
// frame that the kernel handed over as a TSO segment keeps its header, with segments of
// no more than the MSS of mtu. A plain TCP frame (one that a TAP of a larger MTU carries
// whole) is made one: the header says what the kernel's own TSO would, and the checksum
// field holds the sum of the pseudo-header that the cut completes; the checksum is
// checked first unless the kernel has, so that cutting does not make a good checksum of
// a bad one. Any other frame is ErrNotTCP, and a frame whose header does not describe it
// wraps ErrOffload.
func TSOFor(h VnetHeader, frame []byte, mtu int) (VnetHeader, error) {
	if h.GSOType != VnetGSONone {
		if t := h.GSOType &^ VnetGSOECN; t != VnetGSOTCPv4 && t != VnetGSOTCPv6 {
			return h, fmt.Errorf("%w: GSO type %#x", ErrOffload, h.GSOType)
		}
		f, err := checkTSO(h, frame)
		if err != nil {
			return h, err
		}
		h.GSOSize = uint16(min(int(h.GSOSize), mtu-(f.payload-EthernetHeaderLen)))
		return h, nil
	}
	v6 := len(frame) >= EthernetHeaderLen && binary.BigEndian.Uint16(frame[12:]) == EtherTypeIPv6
	f, ok := parseTCPFrame(frame, v6)
	size := mtu - (f.payload - EthernetHeaderLen)
	if !ok || f.payload == len(frame) || size <= 0 || ipLength(frame[EthernetHeaderLen:], v6) != len(frame)-EthernetHeaderLen {
		return h, ErrNotTCP
	}
	tcp := frame[f.l4:]
	src, dst := f.addrs(frame)
	switch {
	case h.Flags&VnetNeedsCsum != 0:
		if int(h.CsumStart) != f.l4 || int(h.CsumOffset) != tcpCsumOffset {
			return h, fmt.Errorf("%w: checksum at %d+%d of a TCP header at %d", ErrOffload, h.CsumStart, h.CsumOffset, f.l4)
		}
	case h.Flags&VnetDataValid == 0 && !tcpValid(src, dst, tcp):
		return h, fmt.Errorf("%w: TCP checksum is wrong", ErrOffload)
	}
	binary.BigEndian.PutUint16(tcp[tcpCsumOffset:], pseudoSum16(src, dst, len(tcp)))

	gso := uint8(VnetGSOTCPv4)
	if v6 {
		gso = VnetGSOTCPv6
	}
	if tcp[13]&tcpCwr != 0 {
		gso |= VnetGSOECN
	}
	return VnetHeader{
		Flags: VnetNeedsCsum, GSOType: gso, HdrLen: uint16(f.payload), GSOSize: uint16(size),
		CsumStart: uint16(f.l4), CsumOffset: tcpCsumOffset,
	}, nil
}

// CompleteSuper makes ip, the IP packet of a TCP super-frame that the virtio header h
// describes (h.IsTSO, and CheckTSO accepts it with the Ethernet header), one ordinary
// packet in place: it states the length of all of it, has a good header checksum (IPv4)
// and the TCP checksum that the sender left to us. The TCP flags are the super-frame's,
// as the one packet carries what the segments would have carried together.
func CompleteSuper(h VnetHeader, ip []byte) error {
	v6 := h.GSOType&^VnetGSOECN == VnetGSOTCPv6
	l4 := int(h.CsumStart) - EthernetHeaderLen
	if !h.IsTSO() || l4 < IPv4HeaderMinLen || l4+tcpHeaderMinLen > len(ip) || len(ip) > 65535 {
		return fmt.Errorf("%w: TSO header %+v for a packet of %d bytes", ErrOffload, h, len(ip))
	}
	var src, dst []byte
	if v6 {
		binary.BigEndian.PutUint16(ip[4:], uint16(len(ip)-ipv6HeaderLen))
		src, dst = ip[8:24], ip[24:40]
	} else {
		binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
		setIPv4Checksum(ip, l4)
		src, dst = ip[12:16], ip[16:20]
	}
	tcp := ip[l4:]
	binary.BigEndian.PutUint16(tcp[tcpCsumOffset:], 0)
	binary.BigEndian.PutUint16(tcp[tcpCsumOffset:], tcpChecksum(src, dst, tcp))
	return nil
}

// Fragment cuts frame, an IPv4 packet larger than mtu that is not for TSO (TSOFor says
// so), into fragments that fit it (RFC 791): the data in pieces of a multiple of eight
// bytes, each with the header of the packet, its own length, offset and checksum, and the
// More Fragments flag on all but the last (or as the packet had it, if it was a fragment
// already). A checksum that the sender left to us is completed first, as the fragments
// carry only parts of it. The fragments are valid until the next call. A packet with the
// Don't Fragment flag or with IP options, or that is not IPv4, is ErrTooLarge: options
// would have to be copied selectively, which no traffic here needs.
func (s *Segmenter) Fragment(h VnetHeader, frame []byte, mtu int) ([][]byte, error) {
	s.frames = s.frames[:0]
	if h.GSOType != VnetGSONone {
		return nil, fmt.Errorf("%w: GSO type %#x", ErrOffload, h.GSOType)
	}
	const hdr = EthernetHeaderLen + IPv4HeaderMinLen
	if len(frame) < hdr || binary.BigEndian.Uint16(frame[12:]) != EtherTypeIPv4 || frame[EthernetHeaderLen] != 0x45 {
		return nil, ErrTooLarge
	}
	ip := frame[EthernetHeaderLen:]
	flagsFrag, total := binary.BigEndian.Uint16(ip[6:]), int(binary.BigEndian.Uint16(ip[2:]))
	if flagsFrag&ipv4DF != 0 || total <= IPv4HeaderMinLen || total > len(ip) {
		return nil, ErrTooLarge
	}
	if h.Flags&VnetNeedsCsum != 0 {
		if err := completeChecksum(h, frame); err != nil {
			return nil, err
		}
	}
	data, per := ip[IPv4HeaderMinLen:total], (mtu-IPv4HeaderMinLen)&^7
	n := (len(data) + per - 1) / per
	out := s.space(len(data) + n*hdr)
	for i, off := 0, 0; i < n; i++ {
		chunk := min(per, len(data)-i*per)
		frag := out[off : off+hdr+chunk : off+hdr+chunk]
		off += hdr + chunk
		copy(frag, frame[:hdr])
		copy(frag[hdr:], data[i*per:])
		fip := frag[EthernetHeaderLen:]
		field := flagsFrag&ipv4FragMask + uint16(i*per/8)
		if i < n-1 || flagsFrag&ipv4MF != 0 {
			field |= ipv4MF
		}
		binary.BigEndian.PutUint16(fip[2:], uint16(IPv4HeaderMinLen+chunk))
		binary.BigEndian.PutUint16(fip[6:], field)
		setIPv4Checksum(fip, IPv4HeaderMinLen)
		s.frames = append(s.frames, frag)
	}
	return s.frames, nil
}
