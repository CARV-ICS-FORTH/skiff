package packet

import (
	"bytes"
	"encoding/binary"
)

// A Coalescer assembles frames into the single write a TAP with offloads takes: runs of
// TCP segments of one flow become one super-frame with a virtio header that says how to
// cut it again, anything else goes through as it is. It holds one frame at a time, the
// pending one, which later frames may join: Merge offers a frame, and if it does not
// join, the caller takes the pending frame with Take and starts another with Start.
//
// A frame joins only if the receiver would see no difference between it and the
// segments of the super-frame: the same flow and headers, the next bytes of the stream,
// a valid TCP checksum (unless the frame is Trusted, which says that it was checked),
// and nothing but ACK (and PSH on the last). That is the rule of
// the kernel's own receive offload, and with those frames the super-frame is the one the
// kernel's own TSO would have produced from them.
type Coalescer struct {
	pb *Buffer // VnetHeaderLen bytes for the virtio header, then the frame
	n  int     // the length of the frame, 0 if there is none

	tf      tcpFrame
	size    int    // payload of the first segment: no later segment is longer
	nseg    int    // segments in the frame
	next    uint32 // the sequence number that the next segment must have
	psh     bool   // the last segment has PSH
	open    bool   // the frame can take more segments
	checked bool   // the checksum of its first segment is known to be right
}

// Pending reports whether there is a frame to take.
func (c *Coalescer) Pending() bool { return c.n > 0 }

func (c *Coalescer) frame() []byte { return c.pb.B[VnetHeaderLen : VnetHeaderLen+c.n] }

// Held is the pending frame as Start copied it, which the caller may change in place
// (its headers, not its length) until it is taken; it is empty if there is none.
func (c *Coalescer) Held() []byte {
	if c.n == 0 {
		return nil
	}
	return c.frame()
}

// Start makes f the pending frame. There must be none.
func (c *Coalescer) Start(f *EthernetFrame) error {
	c.n, c.nseg, c.open = 0, 1, false
	if c.pb == nil {
		c.pb = GetBuffer()
	}
	buf := c.pb.B[:VnetHeaderLen]
	clear(buf)
	buf, err := f.MarshalTo(buf)
	if err != nil {
		return err
	}
	c.pb.B, c.n = buf, len(buf)-VnetHeaderLen
	c.classify(f.Trusted)
	return nil
}

// classify decides whether the pending frame can take more segments; trusted says that
// its checksum is known to be right.
func (c *Coalescer) classify(trusted bool) {
	frame := c.frame()
	if len(frame) < EthernetHeaderLen {
		return
	}
	etherType := binary.BigEndian.Uint16(frame[12:])
	if etherType != EtherTypeIPv4 && etherType != EtherTypeIPv6 {
		return
	}
	tf, ok := parseTCPFrame(frame, etherType == EtherTypeIPv6)
	if !ok {
		return
	}
	// The IP header states the frame's length exactly: no padding after it, which a
	// super-frame could not carry, and no IPv4 options.
	ip := frame[EthernetHeaderLen:]
	if tf.v6 && ipv6HeaderLen+int(binary.BigEndian.Uint16(ip[4:])) != len(ip) ||
		!tf.v6 && (tf.ihl != IPv4HeaderMinLen || int(binary.BigEndian.Uint16(ip[2:])) != len(ip)) {
		return
	}
	tcp := frame[tf.l4:]
	if tcp[13]&^tcpPsh != TCPAck || tf.payload == len(frame) {
		return
	}
	c.tf, c.size = tf, len(frame)-tf.payload
	c.next = binary.BigEndian.Uint32(tcp[4:]) + uint32(c.size)
	c.psh = tcp[13]&tcpPsh != 0
	c.open, c.checked = !c.psh, trusted
}

// Merge appends f to the pending frame if it continues it, and reports whether it did.
func (c *Coalescer) Merge(f *EthernetFrame) bool {
	if !c.open {
		return false
	}
	frame, tf := c.frame(), c.tf
	head, ip := frame[EthernetHeaderLen:], f.Payload
	if f.EtherType != binary.BigEndian.Uint16(frame[12:]) || !bytes.Equal(f.DstMAC, frame[0:6]) || !bytes.Equal(f.SrcMAC, frame[6:12]) {
		return false
	}
	// The IP headers are equal but for what segmentation changes: length, checksum
	// and, in IPv4, the identification.
	if tf.v6 {
		if len(ip) < ipv6HeaderLen || len(ip) != ipv6HeaderLen+int(binary.BigEndian.Uint16(ip[4:])) ||
			!bytes.Equal(ip[0:4], head[0:4]) || !bytes.Equal(ip[6:ipv6HeaderLen], head[6:ipv6HeaderLen]) {
			return false
		}
	} else if len(ip) < IPv4HeaderMinLen || len(ip) != int(binary.BigEndian.Uint16(ip[2:])) ||
		!bytes.Equal(ip[0:2], head[0:2]) || !bytes.Equal(ip[6:10], head[6:10]) || !bytes.Equal(ip[12:20], head[12:20]) {
		return false
	}
	// The TCP headers too: ports, acknowledgment, window and options.
	tcp, hdrLen := ip[tf.ihl:], tf.payload-tf.l4
	htcp := frame[tf.l4:]
	if len(tcp) <= hdrLen || int(tcp[12]>>4)*4 != hdrLen || tcp[13]&^tcpPsh != TCPAck ||
		!bytes.Equal(tcp[0:4], htcp[0:4]) || !bytes.Equal(tcp[8:12], htcp[8:12]) || !bytes.Equal(tcp[14:16], htcp[14:16]) ||
		!bytes.Equal(tcp[tcpHeaderMinLen:hdrLen], htcp[tcpHeaderMinLen:hdrLen]) {
		return false
	}
	n := len(tcp) - hdrLen
	if binary.BigEndian.Uint32(tcp[4:]) != c.next || n > c.size || c.n-EthernetHeaderLen+n > 65535 {
		return false
	}
	// Nobody checks the checksum once the super-frame is written as one whose sum is
	// still to be done, so it is checked here, for the first segment when a second joins,
	// unless it is known to be right.
	hsrc, hdst := tf.addrs(frame)
	if !c.checked {
		if !tcpValid(hsrc, hdst, htcp) {
			c.open = false
			return false
		}
		c.checked = true
	}
	if !f.Trusted && !tcpValid(hsrc, hdst, tcp) {
		return false
	}

	c.pb.B = append(c.pb.B[:VnetHeaderLen+c.n], tcp[hdrLen:]...)
	c.n += n
	c.nseg++
	c.next += uint32(n)
	if tcp[13]&tcpPsh != 0 {
		c.psh = true
		c.open = false
	}
	if n < c.size {
		c.open = false
	}
	return true
}

// Take returns the pending frame, with the virtio header in front of it, ready to be
// written to the TAP, and the number of segments that were joined in it. It is valid
// until the next Start, or until Detach.
func (c *Coalescer) Take() (b []byte, segments int) {
	b, segments = c.pb.B[:VnetHeaderLen+c.n], c.nseg
	if segments > 1 {
		c.finish(b)
	}
	c.n, c.open = 0, false
	return b, segments
}

// Detach hands the buffer that holds the frame last taken to the caller, with the frame
// as Take returned it in B, so that it can be written later and elsewhere; the caller
// releases it. The next Start takes another from the pool.
func (c *Coalescer) Detach() *Buffer {
	pb := c.pb
	c.pb = nil
	return pb
}

// Discard drops the pending frame, if there is one, so that the Coalescer starts clean.
func (c *Coalescer) Discard() { c.n, c.open = 0, false }

// finish makes the headers of a super-frame what the kernel's own look like: the lengths
// of all of it, the flags of the last segment, and in the TCP checksum field the sum of
// the pseudo-header, which the kernel completes (or, if the frame is cut again, the
// segments' sums) when the frame leaves the pod's stack.
func (c *Coalescer) finish(b []byte) {
	frame, tf := b[VnetHeaderLen:], c.tf
	ip, tcp := frame[EthernetHeaderLen:], frame[tf.l4:]
	if tf.v6 {
		binary.BigEndian.PutUint16(ip[4:], uint16(len(ip)-ipv6HeaderLen))
	} else {
		binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
		setIPv4Checksum(ip, tf.ihl)
	}
	if c.psh {
		tcp[13] |= tcpPsh
	}
	src, dst := tf.addrs(frame)
	binary.BigEndian.PutUint16(tcp[tcpCsumOffset:], pseudoSum16(src, dst, len(tcp)))

	gso := uint8(VnetGSOTCPv4)
	if tf.v6 {
		gso = VnetGSOTCPv6
	}
	VnetHeader{
		Flags: VnetNeedsCsum, GSOType: gso, HdrLen: uint16(tf.payload), GSOSize: uint16(c.size),
		CsumStart: uint16(tf.l4), CsumOffset: tcpCsumOffset,
	}.Put(b)
}

// tcpValid reports whether the checksum of the TCP segment seg, field included, is right.
func tcpValid(src, dst, seg []byte) bool {
	return add16(pseudoSum16(src, dst, len(seg)), sum16(seg)) == 0xffff
}
