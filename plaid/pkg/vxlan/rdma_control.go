package vxlan

import (
	"encoding/binary"
	"errors"

	"plaid/pkg/packet"
)

// The control datagrams of the RDMA transport go over the UDP overlay, to a node's VXLAN
// port, with the VXLANFlagControl bit and without the I flag, so that a plaidd without the
// transport counts them as invalid and drops them (packet.ParseVXLAN wants the I flag).
// After the VXLAN header, all numbers big-endian:
//
//	byte 0      version, 1
//	byte 1      type: ctlHello, ctlDown or ctlNone, with ctlUp set when the sender's link to the
//	            receiver is up (it sends over it)
//	bytes 2-3   the sender's VXLAN port, where answers go (the datagram leaves from a send socket)
//	bytes 4-7   the sender's session: a random number of its transport, new when it starts
//	bytes 8-11  the generation of the sender's link to this node: its QPs (0: none)
//	bytes 12-15 the session and
//	bytes 16-19 the generation of the receiver's link that the sender's QPs are connected to (0: none)
//	bytes 20-21 the LID of the sender's port
//	bytes 22-23 its IB MTU in bytes
//	bytes 24-31 the subnet prefix of its GID 0: nodes with the same are in one subnet
//	bytes 32-33 n, how many QPs follow
//	n x 8 bytes the QP's number and first packet sequence number, one per lane
//
// HELLO offers a link and says how far it is; DOWN says that the sender has taken its link
// (session, generation) down; NONE says that the sender does not take RDMA from this node
// (no transport, a subnet it cannot reach, or it has given the link up), and the receiver
// stops offering it for a while.
const (
	ctlVersion = 1
	ctlHello   = 1
	ctlDown    = 2
	ctlNone    = 3
	ctlUp      = 0x80

	ctlFixedLen = 34
	ctlMaxQPs   = MaxSockets
)

// control is a control datagram's content.
type control struct {
	typ       byte
	up        bool // the sender's link to the receiver is up
	port      uint16
	session   uint32
	gen       uint32
	peerSess  uint32 // the receiver's session the sender is connected to
	peerGen   uint32
	lid       uint16
	mtu       uint16
	gidPrefix uint64
	qps       []qpAddr
}

type qpAddr struct{ qpn, psn uint32 }

var errControl = errors.New("invalid control datagram")

func (c *control) appendTo(dst []byte, vni uint32) []byte {
	dst = packet.AppendVXLANHeader(dst, packet.VXLANFlagControl, vni)
	typ := c.typ
	if c.up {
		typ |= ctlUp
	}
	dst = append(dst, ctlVersion, typ)
	dst = binary.BigEndian.AppendUint16(dst, c.port)
	dst = binary.BigEndian.AppendUint32(dst, c.session)
	dst = binary.BigEndian.AppendUint32(dst, c.gen)
	dst = binary.BigEndian.AppendUint32(dst, c.peerSess)
	dst = binary.BigEndian.AppendUint32(dst, c.peerGen)
	dst = binary.BigEndian.AppendUint16(dst, c.lid)
	dst = binary.BigEndian.AppendUint16(dst, c.mtu)
	dst = binary.BigEndian.AppendUint64(dst, c.gidPrefix)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(c.qps)))
	for _, q := range c.qps {
		dst = binary.BigEndian.AppendUint32(dst, q.qpn)
		dst = binary.BigEndian.AppendUint32(dst, q.psn&0xffffff)
	}
	return dst
}

// parseControl reads a control datagram's payload (after the VXLAN header).
func parseControl(b []byte) (control, error) {
	var c control
	if len(b) < ctlFixedLen || b[0] != ctlVersion {
		return c, errControl
	}
	c.typ, c.up = b[1]&^ctlUp, b[1]&ctlUp != 0
	if c.typ != ctlHello && c.typ != ctlDown && c.typ != ctlNone {
		return c, errControl
	}
	be := binary.BigEndian
	c.port = be.Uint16(b[2:])
	c.session = be.Uint32(b[4:])
	c.gen = be.Uint32(b[8:])
	c.peerSess = be.Uint32(b[12:])
	c.peerGen = be.Uint32(b[16:])
	c.lid = be.Uint16(b[20:])
	c.mtu = be.Uint16(b[22:])
	c.gidPrefix = be.Uint64(b[24:])
	n := int(be.Uint16(b[32:]))
	if n > ctlMaxQPs || len(b) != ctlFixedLen+8*n || c.port == 0 || c.session == 0 {
		return c, errControl
	}
	if c.typ == ctlHello && (n == 0 || c.gen == 0 || c.lid == 0) {
		return c, errControl
	}
	for i := 0; i < n; i++ {
		o := ctlFixedLen + 8*i
		c.qps = append(c.qps, qpAddr{qpn: be.Uint32(b[o:]), psn: be.Uint32(b[o+4:]) & 0xffffff})
	}
	return c, nil
}
