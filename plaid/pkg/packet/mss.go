package packet

import "encoding/binary"

// TCP option kinds that the walk over a header's options needs.
const (
	tcpOptEnd = 0
	tcpOptNOP = 1
	tcpOptMSS = 2
)

// RaiseSYNMSS makes the MSS option of frame, an Ethernet frame that carries a TCP SYN
// (or SYN-ACK) over IPv4 or IPv6, the MSS that a link of mtu allows (mtu less the IP
// header and 20 bytes of TCP) if it is lower, and reports whether it changed it. The
// frame's TCP checksum, which must be complete, is updated for the change (RFC 1624). A
// frame that is not a SYN, has no MSS option, or has options that do not parse is left as
// it is.
func RaiseSYNMSS(frame []byte, mtu int) bool {
	_, changed := synMSS(frame, mtu, true, false)
	return changed
}

// LowerSYNMSS is RaiseSYNMSS for an MSS that is higher than the one mtu allows.
func LowerSYNMSS(frame []byte, mtu int) bool {
	_, changed := synMSS(frame, mtu, false, false)
	return changed
}

// RaiseSYNMSSCopy is RaiseSYNMSS for a frame that is not the caller's to change: if the MSS
// is to change it returns a changed copy, and otherwise frame itself.
func RaiseSYNMSSCopy(frame []byte, mtu int) (out []byte, changed bool) {
	return synMSS(frame, mtu, true, true)
}

// synMSS is the walk of RaiseSYNMSS and LowerSYNMSS. With cow the change is made in a copy
// of frame, which is returned; otherwise in frame, which is returned.
func synMSS(frame []byte, mtu int, raise, cow bool) ([]byte, bool) {
	if len(frame) < EthernetHeaderLen+IPv4HeaderMinLen+tcpHeaderMinLen {
		return frame, false
	}
	f, ok := parseTCPFrame(frame, binary.BigEndian.Uint16(frame[12:]) == EtherTypeIPv6)
	if !ok || frame[f.l4+13]&TCPSyn == 0 {
		return frame, false
	}
	target := mtu - f.ihl - tcpHeaderMinLen
	if target <= 0 || target > 0xffff {
		return frame, false
	}
	// The first MSS option, found as a receiver would: an end of the list stops the walk,
	// and an option whose length does not fit ends it as well.
	tcp := frame[f.l4:f.payload]
	for i := tcpHeaderMinLen; i < len(tcp); {
		switch tcp[i] {
		case tcpOptEnd:
			return frame, false
		case tcpOptNOP:
			i++
			continue
		}
		if i+1 >= len(tcp) || tcp[i+1] < 2 || i+int(tcp[i+1]) > len(tcp) {
			return frame, false
		}
		if tcp[i] != tcpOptMSS {
			i += int(tcp[i+1])
			continue
		}
		old := int(binary.BigEndian.Uint16(tcp[i+2:]))
		if tcp[i+1] != 4 || raise && old >= target || !raise && old <= target {
			return frame, false
		}
		// The two bytes may straddle two 16-bit words of the checksummed data (after an odd
		// number of NOPs): replace the words they touch.
		lo := (i + 2) &^ 1
		n := (i + 4 - lo + 1) &^ 1
		var before, after [4]byte
		copy(before[:], tcp[lo:lo+n])
		if cow {
			frame = append([]byte(nil), frame...)
			tcp = frame[f.l4:f.payload]
		}
		binary.BigEndian.PutUint16(tcp[i+2:], uint16(target))
		copy(after[:], tcp[lo:lo+n])
		binary.BigEndian.PutUint16(tcp[tcpCsumOffset:], csumReplace(binary.BigEndian.Uint16(tcp[tcpCsumOffset:]), before[:n], after[:n]))
		return frame, true
	}
	return frame, false
}
