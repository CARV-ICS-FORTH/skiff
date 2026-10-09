package vxlan

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"sync"
	"syscall"

	"plaid/pkg/packet"
)

// What the kernel accepts in one send with UDP_SEGMENT: at most 64 datagrams and 65,507
// bytes (the largest UDP payload).
const (
	maxSegments       = 64
	maxSegmentedBytes = 65507
)

// TxBatch collects the frames sent to remote nodes by one source, so that frames for one
// node leave in one send: consecutive datagrams of one size to one node (the last of the
// run may be shorter) go out with UDP_SEGMENT, which the kernel, or the network card,
// cuts again into the datagrams that were put in, one frame each, as VXLAN says. The
// datagrams on the wire are the same as with one send each, and leave in the same order.
// A TxBatch is used by one goroutine at a time.
type TxBatch struct {
	oe  *OverlayEngine
	buf *packet.Buffer // the datagrams of the run, back to back
	err error          // the first send error since the last Flush

	addr  netip.AddrPort // the run's destination
	seg   int            // the size of its datagrams: the first one's
	n     int            // how many it has
	short bool           // the last one is shorter than seg, which ends the run
	oob   [24]byte       // the UDP_SEGMENT control message, for segmentOOBLen
}

// sendRunSegmented is a variable so that tests can make the call fail.
var sendRunSegmented = sendSegmented

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
	*tb = TxBatch{}
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
	if oe.conn.Load() == nil {
		return fmt.Errorf("overlay engine not started")
	}

	route, err := oe.routes.Lookup(dstIP)
	if err != nil {
		return fmt.Errorf("no overlay route for destination IP %s: %w", dstIP, err)
	}

	// Rewrite destination MAC to remote VTEP MAC if route specifies it
	if len(route.VtepMAC) == 6 {
		frame.DstMAC = route.VtepMAC
	}

	vni := route.VNI
	if vni == 0 {
		vni = oe.cfg.DefaultVNI
	}

	remotePort := route.Port
	if remotePort <= 0 {
		remotePort = oe.cfg.Port
	}

	remoteIP, ok := netip.AddrFromSlice(route.RemoteHostIP)
	if !ok || remotePort > math.MaxUint16 {
		return fmt.Errorf("invalid overlay remote address %s port %d", route.RemoteHostIP, remotePort)
	}
	remoteAddr := netip.AddrPortFrom(remoteIP.Unmap(), uint16(remotePort))

	size := packet.VXLANHeaderLen + packet.EthernetHeaderLen + len(frame.Payload)
	if tb.n > 0 && !tb.joins(remoteAddr, size) {
		tb.sendRun()
	}
	// The header and the frame go into one pooled buffer, behind the datagrams of the run.
	if tb.buf == nil {
		tb.buf = packet.GetBuffer()
	}
	start := len(tb.buf.B)
	tb.buf.B = packet.AppendVXLANHeader(tb.buf.B, vni)
	if tb.buf.B, err = frame.MarshalTo(tb.buf.B); err != nil {
		tb.buf.B = tb.buf.B[:start]
		return fmt.Errorf("failed to marshal inner frame: %w", err)
	}
	if tb.n == 0 {
		tb.addr, tb.seg = remoteAddr, size
	} else if size < tb.seg {
		tb.short = true
	}
	tb.n++
	if !oe.gso.Load() {
		tb.sendRun() // without UDP_SEGMENT there is nothing to gain by waiting: each datagram goes out at once
	}
	return nil
}

// joins reports whether a datagram of size bytes to addr can go in the run.
func (tb *TxBatch) joins(addr netip.AddrPort, size int) bool {
	return tb.oe.gso.Load() && addr == tb.addr && !tb.short && size <= tb.seg &&
		tb.n < maxSegments && len(tb.buf.B)+size <= maxSegmentedBytes
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
	oe, conn := tb.oe, tb.oe.conn.Load()
	data, n, seg := tb.buf.B, tb.n, tb.seg
	tb.buf.B, tb.n, tb.seg, tb.short = data[:0], 0, 0, false

	if n > 1 && oe.gso.Load() {
		putSegmentSize(tb.oob[:], seg)
		err := sendRunSegmented(conn, data, tb.oob[:segmentOOBLen], tb.addr)
		oe.txSends.Add(1)
		if err == nil {
			oe.gsoWorked.Store(true)
			oe.txPackets.Add(uint64(n))
			return
		}
		if !oe.gsoWorked.Load() && (errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOPROTOOPT) || errors.Is(err, syscall.EIO)) {
			if oe.gso.CompareAndSwap(true, false) {
				fmt.Printf("[plaidd/overlay] UDP_SEGMENT is not available (%v): sending one datagram per call\n", err)
			}
		}
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
