package vxlan

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"plaid/pkg/packet"
)

// What the kernel accepts in one send with UDP_SEGMENT: at most 64 datagrams and 65,507
// bytes (the largest UDP payload).
const (
	maxSegments       = 64
	maxSegmentedBytes = 65507
	udpHeaderLen      = 8
)

// TxBatch collects the frames sent to remote nodes by one source, so that frames for one
// node leave in one send: consecutive datagrams of one size to one node (the last of the
// run may be shorter) go out with UDP_SEGMENT, which the kernel, or the network card,
// cuts again into the datagrams that were put in, one frame each, as VXLAN says. The
// datagrams on the wire are the same as with one send each, and leave in the same order.
// A run is also for one send socket (frames of different flows leave from different
// ports, see packet.FlowIndex): a frame for another socket ends it, as one for another node
// does. A TxBatch is used by one goroutine at a time.
//
// A TCP super-frame does not go through the batch: SendSuper sends it whole, after what
// the batch holds.
//
// To a node whose RDMA link is up, a run is one RDMA message instead (see
// rdma_transport.go), with or without UDP_SEGMENT, and a super-frame is sent whole in one
// message.
type TxBatch struct {
	oe  *OverlayEngine
	buf *packet.Buffer // the datagrams of the run, back to back
	err error          // the first send error since the last Flush

	addr  netip.AddrPort // the run's destination
	sock  int            // and its send socket, an index into the engine's send sockets
	link  *rdmaLink      // the RDMA link the run goes over, nil for UDP
	seg   int            // the size of its datagrams: the first one's
	n     int            // how many it has
	short bool           // the last one is shorter than seg, which ends the run
	oob   [24]byte       // the UDP_SEGMENT control message, for segmentOOBLen
	io    *superIO       // the scatter list of SendSuper, made when first needed and kept with the batch
}

// sendRunSegmented and superSend are variables so that tests can make the calls fail.
var (
	sendRunSegmented = sendSegmented
	superSend        = (*superIO).send
)

var txBatchPool = sync.Pool{New: func() any { return new(TxBatch) }}

// NewBatch returns an empty TxBatch; Release it when done.
func (oe *OverlayEngine) NewBatch() *TxBatch {
	tb := txBatchPool.Get().(*TxBatch)
	tb.oe = oe
	return tb
}

// Release returns the batch for reuse. What was not flushed is dropped.
func (tb *TxBatch) Release() {
	if tb.buf != nil {
		tb.buf.Release()
	}
	io := tb.io
	*tb = TxBatch{io: io}
	txBatchPool.Put(tb)
}

// Send encapsulates an Ethernet frame into a VXLAN packet for the remote node and holds
// it for Flush. The frame is not used after Send returns. Routing and encoding errors
// are returned here; errors from the socket, at Flush.
func (tb *TxBatch) Send(frame *packet.EthernetFrame, dstIP net.IP) (err error) {
	oe := tb.oe
	defer func() {
		if err != nil {
			oe.txErrors.Add(1)
		}
	}()
	socks := oe.socks.Load()
	if socks == nil {
		return fmt.Errorf("overlay engine not started")
	}

	route, vni, remoteAddr, err := tb.resolve(dstIP)
	if err != nil {
		return err
	}
	// Rewrite destination MAC to remote VTEP MAC if route specifies it
	if len(route.VtepMAC) == 6 {
		frame.DstMAC = route.VtepMAC
	}

	size := packet.VXLANHeaderLen + packet.EthernetHeaderLen + len(frame.Payload)
	sock := packet.FlowIndex(frame.EtherType, frame.Payload, len(socks.tx))
	link := oe.rdmaLink(remoteAddr.Addr())
	if tb.n > 0 && !tb.joins(remoteAddr, sock, size, link) {
		tb.sendRun()
	}
	// The header and the frame go into one pooled buffer, behind the datagrams of the run.
	if tb.buf == nil {
		tb.buf = packet.GetBuffer()
	}
	start := len(tb.buf.B)
	tb.buf.B = packet.AppendVXLANHeader(tb.buf.B, oe.sendFlags, vni)
	if tb.buf.B, err = frame.MarshalTo(tb.buf.B); err != nil {
		tb.buf.B = tb.buf.B[:start]
		return fmt.Errorf("failed to marshal inner frame: %w", err)
	}
	if tb.n == 0 {
		tb.addr, tb.sock, tb.seg, tb.link = remoteAddr, sock, size, link
	} else if size < tb.seg {
		tb.short = true
	}
	tb.n++
	if !oe.gso.Load() && link == nil {
		tb.sendRun() // without UDP_SEGMENT there is nothing to gain by waiting: each datagram goes out at once
	}
	return nil
}

// rdmaLink is the RDMA link to the node at a that is up, or nil.
func (oe *OverlayEngine) rdmaLink(a netip.Addr) *rdmaLink {
	if t := oe.rd.Load(); t != nil {
		return t.upLink(a)
	}
	return nil
}

// resolve finds the route for dstIP and the VNI and the address that its datagrams use.
func (tb *TxBatch) resolve(dstIP net.IP) (route *RouteEntry, vni uint32, addr netip.AddrPort, err error) {
	oe := tb.oe
	route, err = oe.routes.Lookup(dstIP)
	if err != nil {
		return nil, 0, addr, fmt.Errorf("no overlay route for destination IP %s: %w", dstIP, err)
	}
	vni = route.VNI
	if vni == 0 {
		vni = oe.cfg.DefaultVNI
	}
	port := route.Port
	if port <= 0 {
		port = oe.cfg.Port
	}
	ip, ok := netip.AddrFromSlice(route.RemoteHostIP)
	if !ok || port > math.MaxUint16 {
		return nil, 0, addr, fmt.Errorf("invalid overlay remote address %s port %d", route.RemoteHostIP, port)
	}
	return route, vni, netip.AddrPortFrom(ip.Unmap(), uint16(port)), nil
}

// joins reports whether a datagram of size bytes to addr, from send socket sock, over link
// (nil: UDP), can go in the run.
func (tb *TxBatch) joins(addr netip.AddrPort, sock, size int, link *rdmaLink) bool {
	return (tb.oe.gso.Load() || link != nil) && link == tb.link && addr == tb.addr && sock == tb.sock && !tb.short &&
		size <= tb.seg && tb.n < maxSegments && len(tb.buf.B)+size <= maxSegmentedBytes
}

// Flush sends what is held and returns the first send error since the last Flush.
func (tb *TxBatch) Flush() error {
	tb.sendRun()
	err := tb.err
	tb.err = nil
	return err
}

// sendRun sends the run: with one call if it has several datagrams and UDP_SEGMENT
// works, otherwise one by one.
func (tb *TxBatch) sendRun() {
	if tb.n == 0 {
		return
	}
	oe := tb.oe
	conn := oe.socks.Load().tx[tb.sock].conn
	data, n, seg, link := tb.buf.B, tb.n, tb.seg, tb.link
	tb.buf.B, tb.n, tb.seg, tb.short, tb.link = data[:0], 0, 0, false, nil

	if link != nil && oe.rdmaLink(tb.addr.Addr()) == link {
		oe.rd.Load().sendRun(link, tb.sock, data, seg)
		return
	} // a link that went down since: the run goes over UDP

	if n > 1 && oe.gso.Load() {
		putSegmentSize(tb.oob[:], seg)
		err := sendRunSegmented(conn, data, tb.oob[:segmentOOBLen], tb.addr)
		oe.txSends.Add(1)
		if err == nil {
			oe.gsoWorked.Store(true)
			oe.txPackets.Add(uint64(n))
			return
		}
		oe.segmentFailed(err)
		// Nothing was sent. Whatever the reason (a path whose MTU is below the datagram
		// size is one, and the datagrams can still be sent, in fragments), the
		// datagrams go out one by one.
	}
	for rest := data; len(rest) > 0; rest = rest[min(seg, len(rest)):] {
		_, err := conn.WriteToUDPAddrPort(rest[:min(seg, len(rest))], tb.addr)
		oe.txSends.Add(1)
		if err != nil {
			oe.txErrors.Add(1)
			if tb.err == nil {
				tb.err = err
			}
		} else {
			oe.txPackets.Add(1)
		}
	}
}

// segmentFailed gives UDP_SEGMENT up for good, and says so once, if err says that the kernel
// or the device cannot do it and no send with it has worked yet.
func (oe *OverlayEngine) segmentFailed(err error) {
	if !oe.gsoWorked.Load() && (errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOPROTOOPT) || errors.Is(err, syscall.EIO)) {
		if oe.gso.CompareAndSwap(true, false) {
			fmt.Printf("[plaidd/overlay] UDP_SEGMENT is not available (%v): sending one datagram per call\n", err)
		}
	}
}

// superHoldOff is how long the engine sends no super-frame whole after a send of one
// failed for a reason that does not give UDP_SEGMENT up: the frames are cut, as they are
// for a node that does not take them, instead of every super-frame trying, and failing,
// again.
const superHoldOff = int64(time.Second)

// canSendSuper reports whether the engine may send a super-frame whole at all: it is
// meant to, UDP_SEGMENT works, and no send of one failed within the last second.
func (oe *OverlayEngine) canSendSuper() bool {
	return oe.cfg.Super && oe.gso.Load() && oe.clock() >= oe.superHold.Load()
}

// AcceptsSuper reports whether the node that dstIP is routed to takes super-frames whole:
// it is on a route to an IPv4 address that has sent the mark within the last two seconds.
func (tb *TxBatch) AcceptsSuper(dstIP net.IP) bool {
	oe := tb.oe
	if oe.socks.Load() == nil {
		return false
	}
	_, _, addr, err := tb.resolve(dstIP)
	if err == nil && oe.rdmaLink(addr.Addr()) != nil {
		return true // a super-frame goes whole over an RDMA link, in one message
	}
	return oe.canSendSuper() && err == nil && addr.Addr().Is4() && oe.peers.fresh(addr.Addr(), oe.clock())
}

// superDatagram is the size of the datagrams that carry the super-frame whose IP packet is
// ip, which the virtio header h describes: that of the datagrams of its segments, which
// are the largest that the overlay sends, so no datagram is larger than it would be if the
// frame were cut. It is not ok for a frame that the receiving node would reject for its
// segment size, so that two nodes of this version never disagree.
func superDatagram(h packet.VnetHeader, ip []byte) (int, bool) {
	l4 := int(h.CsumStart) - packet.EthernetHeaderLen
	if !h.IsTSO() || h.GSOSize == 0 || l4 < packet.IPv4HeaderMinLen || l4+13 >= len(ip) {
		return 0, false
	}
	hdr := l4 + int(ip[l4+12]>>4)*4 // the IP and TCP headers
	d := packet.VXLANHeaderLen + packet.EthernetHeaderLen + hdr + int(h.GSOSize)
	return d, hdr < len(ip) && h.TSOSizeOK(len(ip)-hdr) && d > packet.VXLANHeaderLen+chunkHdrLen+packet.VnetHeaderLen+packet.EthernetHeaderLen && d <= maxSegmentedBytes
}

// linkChunks reports whether the chunks are sized by OverlayConfig.LinkMTU: it is set, and no
// send of chunks of that size has failed.
func (oe *OverlayEngine) linkChunks() bool {
	return oe.cfg.LinkMTU > 0 && !oe.linkChunksOff.Load()
}

// chunkBytes is the size of the datagrams that carry a super-frame whose segments'
// datagrams are d (see superDatagram): the largest that the link allows, the IP and UDP
// headers taken off its MTU, if that is more than d, and never more than one send with
// UDP_SEGMENT takes.
func (oe *OverlayEngine) chunkBytes(d int) int {
	if oe.linkChunks() {
		d = max(d, min(oe.cfg.LinkMTU-packet.IPv4HeaderMinLen-udpHeaderLen, maxSegmentedBytes))
	}
	return d
}

// linkChunksFailed turns the chunks of the link's size off for good, and says so once, if err
// says that datagrams that large do not fit the route: EINVAL for a send with UDP_SEGMENT
// whose segments exceed the route's MTU, or EMSGSIZE. It reports whether it did. UDP_SEGMENT
// itself stays on.
func (oe *OverlayEngine) linkChunksFailed(err error) bool {
	if !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.EMSGSIZE) {
		return false
	}
	if oe.linkChunksOff.CompareAndSwap(false, true) {
		fmt.Printf("[plaidd/overlay] chunks of the link MTU (%d) failed (%v): sending chunks of the segment size\n", oe.cfg.LinkMTU, err)
	}
	return true
}

// SendSuper sends frame, a TCP super-frame that h describes (packet.CheckTSO accepts
// them), whole to the node that dstIP is routed to, as chunks (see the package comment).
// The frames this batch holds go first, so that a flow keeps its order; then the chunks
// leave from the send socket that the frame's flow selects, in as few sends with
// UDP_SEGMENT as the limits allow, and the frame's bytes are not copied: the kernel reads
// them where they are. The socket is locked from the first chunk to the last, so that
// the chunks of two frames do not mix.
//
// sent is false, and nothing has been sent, if the node has not said that it takes
// super-frames, or the frame's segment size is one that a node would not take (see
// packet.CheckTSOSize), or the first send failed (UDP_SEGMENT is given up for good if that
// is what the error says, and any other error holds the super-frames back for a second):
// it is for the caller to cut the frame and send the segments. An error after the first
// send is the frame's loss.
func (tb *TxBatch) SendSuper(h packet.VnetHeader, frame *packet.EthernetFrame, dstIP net.IP) (sent bool, err error) {
	oe := tb.oe
	socks := oe.socks.Load()
	if socks == nil {
		return false, nil
	}
	if sent := tb.sendSuperRDMA(h, frame, dstIP); sent {
		return true, nil
	}
	if !oe.canSendSuper() {
		return false, nil
	}
	route, vni, addr, rerr := tb.resolve(dstIP)
	d, ok := superDatagram(h, frame.Payload)
	if rerr != nil || !ok || !addr.Addr().Is4() || !oe.peers.fresh(addr.Addr(), oe.clock()) {
		return false, nil
	}
	if len(route.VtepMAC) == 6 {
		frame.DstMAC = route.VtepMAC
	}
	sock := packet.FlowIndex(frame.EtherType, frame.Payload, len(socks.tx))
	ts := socks.tx[sock]
	if len(frame.DstMAC) != 6 || len(frame.SrcMAC) != 6 || ts.rc == nil {
		return false, nil
	}
	tb.sendRun()
	if tb.buf == nil {
		tb.buf = packet.GetBuffer()
	}
	if tb.io == nil {
		tb.io = newSuperIO()
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return tb.sendChunks(ts, vni, addr, h, frame, d, oe.chunkBytes(d))
}

// sendSuperRDMA sends a super-frame whole over the RDMA link to dstIP's node, after the
// run the batch holds, if the link is up.
func (tb *TxBatch) sendSuperRDMA(h packet.VnetHeader, frame *packet.EthernetFrame, dstIP net.IP) bool {
	oe := tb.oe
	route, vni, addr, err := tb.resolve(dstIP)
	if err != nil {
		return false
	}
	link := oe.rdmaLink(addr.Addr())
	if link == nil {
		return false
	}
	if len(route.VtepMAC) == 6 {
		frame.DstMAC = route.VtepMAC
	}
	if len(frame.DstMAC) != 6 || len(frame.SrcMAC) != 6 {
		return false
	}
	tb.sendRun()
	sock := packet.FlowIndex(frame.EtherType, frame.Payload, len(oe.socks.Load().tx))
	oe.rd.Load().sendWhole(link, sock, vni, h, frame)
	oe.superTx.Add(1)
	return true
}

// sendChunks is SendSuper once the socket is locked. The segments' datagrams are d bytes,
// the chunks' are c (see chunkBytes). If the first send fails because c does not fit the
// route, the frame goes again with chunks of d.
func (tb *TxBatch) sendChunks(ts *txSock, vni uint32, addr netip.AddrPort, h packet.VnetHeader, frame *packet.EthernetFrame, d, c int) (bool, error) {
	oe, ip := tb.oe, frame.Payload
	total := packet.EthernetHeaderLen + len(ip)
	per := c - packet.VXLANHeaderLen - chunkHdrLen // frame bytes in a chunk; the first has the virtio header too
	first := per - packet.VnetHeaderLen
	chunks := 1
	if total > first {
		chunks += (total - first + per - 1) / per
	}
	perSend := min(maxSegments, maxSegmentedBytes/c)
	id := ts.id
	ts.id++
	flags := oe.sendFlags | packet.VXLANFlagChunk

	for k := 0; k < chunks; {
		n := min(perSend, chunks-k)
		buf, want := tb.buf.B[:0], 0
		for j := 0; j < n; j++ {
			start, off, end := len(buf), 0, first
			if k+j > 0 {
				off = first + (k+j-1)*per
				end = off + per
			}
			end = min(end, total)
			buf = packet.AppendVXLANHeader(buf, flags, vni)
			buf = appendChunkHeader(buf, id, total, off, h)
			var data []byte
			if off == 0 {
				buf = append(buf, frame.DstMAC...)
				buf = append(buf, frame.SrcMAC...)
				buf = binary.BigEndian.AppendUint16(buf, frame.EtherType)
				data = ip[:end-packet.EthernetHeaderLen]
			} else {
				data = ip[off-packet.EthernetHeaderLen : end-packet.EthernetHeaderLen]
			}
			tb.io.set(2*j, buf[start:])
			tb.io.set(2*j+1, data)
			want += len(buf) - start + len(data)
		}
		var oob []byte
		if n > 1 {
			putSegmentSize(tb.oob[:], c)
			oob = tb.oob[:segmentOOBLen]
		}
		err := superSend(tb.io, ts, addr, 2*n, oob, want)
		tb.io.clear()
		oe.txSends.Add(1)
		if err == nil {
			if n > 1 {
				oe.gsoWorked.Store(true)
			}
			oe.txPackets.Add(uint64(n))
			k += n
			continue
		}
		if k > 0 {
			// The start of the frame is on the wire, and the node will drop it.
			oe.txErrors.Add(uint64(chunks - k))
			return true, err
		}
		if c > d && oe.linkChunksFailed(err) {
			return tb.sendChunks(ts, vni, addr, h, frame, d, d)
		}
		if n > 1 {
			oe.segmentFailed(err)
		}
		if oe.gso.Load() {
			oe.superTxFailed.Add(1)
			oe.superHold.Store(oe.clock() + superHoldOff)
		}
		return false, nil // nothing was sent; the segments will tell if the node can be reached
	}
	oe.superTx.Add(1)
	oe.superTxChunks.Add(uint64(chunks))
	return true, nil
}
