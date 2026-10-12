//go:build linux

package vxlan

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"plaid/pkg/packet"
)

// newSuperFixture is a fixture for the feature, bound to bind, that skips the test where the
// kernel cannot segment if the engine is to.
func newSuperFixture(t *testing.T, bind string, cfg OverlayConfig) *fixture {
	t.Helper()
	cfg.BindAddress = bind
	f := newFixture(t, cfg, 1)
	if cfg.Offload {
		skipWithoutSegmentation(t, f.oe)
	}
	return f
}

// sendSuper sends the super-frame whole through a batch of oe's own.
func sendSuper(oe *OverlayEngine, h packet.VnetHeader, wire []byte, dst net.IP) (bool, error) {
	tb := oe.NewBatch()
	defer tb.Release()
	sent, err := tb.SendSuper(h, ethernet(wire), dst)
	if ferr := tb.Flush(); err == nil {
		err = ferr
	}
	return sent, err
}

// send sends the super-frame whole to the sink's node.
func (f *fixture) send(h packet.VnetHeader, wire []byte) (bool, error) {
	return sendSuper(f.oe, h, wire, f.dst(0))
}

// The chunks leave as the package comment says, byte for byte, within the datagram size of
// the segments, in as few sends as the limits allow, from the socket of the flow, with the
// route's VNI, the MAC address it sets, the mark, and the frame untouched.
func TestSendSuperPutsTheChunksOnTheWire(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "0.0.0.0"} { // an AF_INET and an AF_INET6 socket
		f := newSuperFixture(t, bind, OverlayConfig{Offload: true, Super: true})
		f.announce()
		ids := map[int]uint32{} // the next id of each send socket
		for _, v6 := range []bool{false, true} {
			for _, mtu := range []int{1450, 8950} {
				for _, n := range []int{1, 2000, 9000, 20000, 65400} {
					name := fmt.Sprintf("bind=%s v6=%v mtu=%d payload=%d", bind, v6, mtu, n)
					h, frame := tsoFrame(v6, n, mtu, byte(n))
					orig := append([]byte(nil), frame...)
					d := packet.VXLANHeaderLen + packet.EthernetHeaderLen + mtu
					wire := append([]byte(nil), frame...)
					copy(wire[0:6], fixtureVTEP)
					sock := flowSocket(binary.BigEndian.Uint16(frame[12:]), frame[14:], len(f.oe.socks.Load().tx))
					want := refChunks(packet.VXLANFlagSuperOK, 7, ids[sock], h, wire, d)
					before := f.oe.Stats()

					if sent, err := f.send(h, frame); !sent || err != nil {
						t.Fatalf("%s: sent=%v, %v", name, sent, err)
					}
					got, ports := f.receiveFrom(t, 0, len(want))
					for i := range want {
						if !bytes.Equal(got[i], want[i]) {
							t.Fatalf("%s: datagram %d of %d (%d bytes) is not what the format says (%d bytes)", name, i, len(want), len(got[i]), len(want[i]))
						}
						if ports[i] != ports[0] {
							t.Errorf("%s: the datagrams come from ports %d and %d", name, ports[0], ports[i])
						}
					}
					for i, dg := range got {
						if len(dg) > d || i < len(got)-1 && len(dg) != d {
							t.Errorf("%s: datagram %d of %d is %d bytes, want %d", name, i, len(got), len(dg), d)
						}
					}
					if !bytes.Equal(frame, orig) {
						t.Errorf("%s: the frame was changed", name)
					}
					st := f.oe.Stats()
					sends := (len(want) + min(maxSegments, maxSegmentedBytes/d) - 1) / min(maxSegments, maxSegmentedBytes/d)
					if st.SuperTx-before.SuperTx != 1 || int(st.SuperTxChunks-before.SuperTxChunks) != len(want) ||
						int(st.TxPackets-before.TxPackets) != len(want) || int(st.TxSends-before.TxSends) != sends || st.TxErrors != 0 {
						t.Errorf("%s: counters %+v, want %d datagrams in %d sends", name, st, len(want), sends)
					}
					ids[sock]++
				}
			}
		}
		// The chunks of a flow come from the socket that its plain frames use.
		h, frame := tsoFrame(false, 3000, 1450, 1)
		ef := ethernet(frame)
		want := f.oe.socks.Load().tx[flowSocket(ef.EtherType, ef.Payload, len(f.oe.socks.Load().tx))].conn.LocalAddr().(*net.UDPAddr).Port
		if sent, _ := f.send(h, frame); !sent {
			t.Fatal("not sent")
		}
		if _, ports := f.receiveFrom(t, 0, 3); ports[0] != want {
			t.Errorf("chunks came from port %d, the flow's socket has %d", ports[0], want)
		}
	}
}

// What the chunks hold is not copied in user space: the engine's buffer holds the headers
// of one send and the frame is read where it lies.
func TestSendSuperDoesNotCopyTheFrame(t *testing.T) {
	f := newSuperFixture(t, "127.0.0.1", OverlayConfig{Offload: true, Super: true})
	f.announce()
	h, frame := tsoFrame(false, 60000, 1450, 1)
	var parts [][]byte
	var hdr []byte
	old := superSend
	superSend = func(w *superIO, ts *txSock, to netip.AddrPort, n int, oob []byte, want int) error {
		parts = parts[:0]
		for i := 0; i < n; i++ {
			parts = append(parts, unsafe.Slice(w.iov[i].Base, int(w.iov[i].Len)))
		}
		return old(w, ts, to, n, oob, want)
	}
	defer func() { superSend = old }()
	if sent, err := f.send(h, frame); !sent || err != nil {
		t.Fatal(sent, err)
	}
	f.receive(t, 0, 1)
	in := 0
	for i, p := range parts {
		if i%2 == 1 && len(p) > 0 { // a part of the frame
			if uintptr(unsafe.Pointer(&p[0])) < uintptr(unsafe.Pointer(&frame[0])) || uintptr(unsafe.Pointer(&p[0])) >= uintptr(unsafe.Pointer(&frame[0]))+uintptr(len(frame)) {
				t.Fatalf("part %d is a copy: it lies outside the frame", i)
			}
			in += len(p)
		} else {
			hdr = p
		}
	}
	if in == 0 || len(hdr) == 0 {
		t.Errorf("%d parts, %d bytes of the frame", len(parts), in)
	}
}

// Chunks of two frames do not mix on the wire, even when the frames are sent by different
// goroutines from the same socket and need more than one send.
func TestSuperFramesFromManyGoroutinesDoNotInterleave(t *testing.T) {
	f := newSuperFixture(t, "127.0.0.1", OverlayConfig{Offload: true, Super: true})
	f.announce()
	type call struct{ ids, offsets []uint32 } // the chunks of one send
	var mu sync.Mutex
	var calls []call
	old := superSend
	superSend = func(w *superIO, ts *txSock, to netip.AddrPort, n int, oob []byte, want int) error {
		var c call
		for i := 0; i < n/2; i++ { // n parts: a header and a piece of the frame for each datagram
			hdr := unsafe.Slice(w.iov[2*i].Base, int(w.iov[2*i].Len))
			c.ids = append(c.ids, binary.BigEndian.Uint32(hdr[8+4:]))
			c.offsets = append(c.offsets, binary.BigEndian.Uint32(hdr[8+12:]))
		}
		runtime.Gosched() // give the others their chance
		time.Sleep(50 * time.Microsecond)
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		return nil // sent, as far as the engine can tell
	}
	defer func() { superSend = old }()

	const goroutines, each = 6, 15
	h, frame := tsoFrame(false, 65000, 1450, 1) // two sends of 44 and 2
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if sent, err := f.send(h, append([]byte(nil), frame...)); !sent || err != nil {
					t.Errorf("sent=%v, %v", sent, err)
				}
			}
		}()
	}
	wg.Wait()

	// Flatten the sends into the order of the chunks on the wire: each frame's chunks are
	// in one run, from offset 0 on, and the runs follow each other.
	var ids, offsets []uint32
	for _, c := range calls {
		ids, offsets = append(ids, c.ids...), append(offsets, c.offsets...)
	}
	runs, prev := 0, ^uint32(0)
	seen := map[uint32]bool{}
	for i, id := range ids {
		if id != prev {
			if seen[id] || offsets[i] != 0 {
				t.Fatalf("chunk %d: frame %d continues after another's (offset %d)", i, id, offsets[i])
			}
			seen[id], prev = true, id
			runs++
		} else if offsets[i] <= offsets[i-1] {
			t.Fatalf("chunk %d of frame %d is out of order", i, id)
		}
	}
	if runs != goroutines*each {
		t.Errorf("%d frames on the wire, want %d", runs, goroutines*each)
	}
}

// A frame that the batch holds before the super-frame, of the same flow, is sent first,
// and what comes after goes after.
func TestSendSuperSendsTheHeldFramesFirst(t *testing.T) {
	f := newSuperFixture(t, "127.0.0.1", OverlayConfig{Offload: true, Super: true})
	f.announce()
	h, frame := tsoFrame(false, 5000, 1450, 1)
	plain := func(tag byte) *packet.EthernetFrame {
		_, p := tsoFrame(false, 100, 1450, tag)
		return ethernet(p)
	}
	tb := f.oe.NewBatch()
	defer tb.Release()
	for _, e := range []error{tb.Send(plain(1), f.dst(0)), tb.Send(plain(2), f.dst(0))} {
		if e != nil {
			t.Fatal(e)
		}
	}
	ef := ethernet(frame)
	if sent, err := tb.SendSuper(h, ef, f.dst(0)); !sent || err != nil {
		t.Fatal(sent, err)
	}
	if err := tb.Send(plain(3), f.dst(0)); err != nil {
		t.Fatal(err)
	}
	if err := tb.Flush(); err != nil {
		t.Fatal(err)
	}
	chunks := len(refChunks(0, 7, 0, h, frame, chunkDatagram))
	got := f.receive(t, 0, 2+chunks+1)
	kind := func(d []byte) string { // the last payload byte of tsoFrame is byte(99*7) + tag
		if d[0]&packet.VXLANFlagChunk != 0 {
			return "chunk"
		}
		return fmt.Sprint(d[len(d)-1] - byte(99*7%256))
	}
	var order []string
	for _, d := range got {
		order = append(order, kind(d))
	}
	want := append(append([]string{"1", "2"}, slices.Repeat([]string{"chunk"}, chunks)...), "3")
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("order %v, want %v", order, want)
	}
}

// Super-frames go only to a node that has said it takes them, in the last two seconds, and
// only if the engine is meant to send them and can.
func TestSuperFramesGoOnlyToNodesThatAnnounced(t *testing.T) {
	h, frame := tsoFrame(false, 5000, 1450, 1)
	f := newSuperFixture(t, "127.0.0.1", OverlayConfig{Offload: true, Super: true})
	tb := f.oe.NewBatch()
	defer tb.Release()
	if tb.AcceptsSuper(f.dst(0)) {
		t.Error("accepts before the node has said anything")
	}
	if sent, err := f.send(h, frame); sent || err != nil {
		t.Errorf("sent=%v, %v to a node that has not announced", sent, err)
	}
	f.nothing(t, 0)
	if st := f.oe.Stats(); st.SuperTx != 0 || st.TxPackets != 0 || st.TxErrors != 0 {
		t.Errorf("counters %+v", st)
	}

	f.announce()
	if !tb.AcceptsSuper(f.dst(0)) || tb.AcceptsSuper(net.IPv4(10, 244, 9, 9)) {
		t.Error("the node that announced is not accepted, or a destination without a route is")
	}
	if sent, _ := f.send(h, frame); !sent {
		t.Error("not sent to the node that announced")
	}
	f.receive(t, 0, 1)

	f.now.Add(int64(1990 * time.Millisecond))
	if !tb.AcceptsSuper(f.dst(0)) {
		t.Error("the mark expired before two seconds")
	}
	f.now.Add(int64(20 * time.Millisecond))
	if tb.AcceptsSuper(f.dst(0)) {
		t.Error("accepts after two seconds without the mark")
	}
	if sent, _ := f.send(h, frame); sent {
		t.Error("sent after two seconds without the mark")
	}
	f.announce()
	if sent, _ := f.send(h, frame); !sent {
		t.Error("not sent after the mark came again")
	}

	// UDP_SEGMENT gone: nothing whole.
	f.oe.gso.Store(false)
	if tb.AcceptsSuper(f.dst(0)) {
		t.Error("accepts without UDP_SEGMENT")
	}
	if sent, _ := f.send(h, frame); sent {
		t.Error("sent without UDP_SEGMENT")
	}

	// And an engine that is not to, or cannot, does not.
	for name, cfg := range map[string]OverlayConfig{
		"feature off": {Offload: true},
		"no offload":  {Super: true},
	} {
		g := newSuperFixture(t, "127.0.0.1", cfg)
		g.announce()
		gb := g.oe.NewBatch()
		if gb.AcceptsSuper(g.dst(0)) {
			t.Errorf("%s: accepts", name)
		}
		if sent, err := g.send(h, frame); sent || err != nil {
			t.Errorf("%s: sent=%v, %v", name, sent, err)
		}
		gb.Release()
	}
}

// The mark is in the flags of every datagram an engine with the feature sends, plain or
// chunk, and in none that another engine sends.
func TestEveryDatagramCarriesTheMarkWhenTheFeatureIsOn(t *testing.T) {
	_, frame := tsoFrame(false, 100, 1450, 1)
	plain := ethernet(frame)
	for _, tc := range []struct {
		name string
		cfg  OverlayConfig
		want byte
	}{
		{"on", OverlayConfig{Offload: true, Super: true}, packet.VXLANFlags | packet.VXLANFlagSuperOK},
		{"off", OverlayConfig{Offload: true}, packet.VXLANFlags},
		{"without the offload, which it needs", OverlayConfig{Super: true}, packet.VXLANFlags},
		{"nothing", OverlayConfig{}, packet.VXLANFlags},
	} {
		f := newSuperFixture(t, "127.0.0.1", tc.cfg)
		f.announce()
		// One alone, and a run of equal ones with UDP_SEGMENT.
		for i := 0; i < 3; i++ {
			if err := f.oe.Send(plain, f.dst(0)); err != nil {
				t.Fatal(err)
			}
		}
		tb := f.oe.NewBatch()
		for i := 0; i < 5; i++ {
			_ = tb.Send(plain, f.dst(0))
		}
		_ = tb.Flush()
		tb.Release()
		got := f.receive(t, 0, 8)
		if tc.cfg.Super && tc.cfg.Offload {
			h, wire := tsoFrame(false, 9000, 1450, 1)
			if sent, _ := f.send(h, wire); !sent {
				t.Fatal("not sent")
			}
			more := f.receive(t, 0, len(refChunks(0, 7, 0, h, wire, chunkDatagram)))
			for _, d := range more {
				if d[0] != tc.want|packet.VXLANFlagChunk {
					t.Errorf("%s: chunk with flags %#x, want %#x", tc.name, d[0], tc.want|packet.VXLANFlagChunk)
				}
			}
		}
		for i, d := range got {
			if d[0] != tc.want {
				t.Errorf("%s: datagram %d has flags %#x, want %#x", tc.name, i, d[0], tc.want)
			}
		}
	}
}

// node is an engine of the end-to-end tests, on its own loopback address.
type node struct {
	oe   *OverlayEngine
	sink *superSink
	addr string
	ip   net.IP // the pod network behind it
}

func newNode(t *testing.T, n int, cfg OverlayConfig) *node {
	t.Helper()
	nd := &node{sink: &superSink{}, addr: fmt.Sprintf("127.0.0.%d", n), ip: net.IPv4(10, 244, byte(n), 10)}
	cfg.BindAddress = nd.addr
	nd.oe = NewOverlayEngine(cfg, NewRouteTable(), nd.sink)
	if err := nd.oe.Start(context.Background()); err != nil {
		t.Skipf("cannot start an engine on %s: %v", nd.addr, err)
	}
	t.Cleanup(func() { nd.oe.Close() })
	return nd
}

// routeTo adds a route to the other node's pods.
func (n *node) routeTo(o *node) {
	_, subnet, _ := net.ParseCIDR(fmt.Sprintf("%s/24", o.ip.Mask(net.CIDRMask(24, 32))))
	n.oe.routes.AddRoute(&RouteEntry{Subnet: subnet, RemoteHostIP: net.ParseIP(o.addr), VtepMAC: net.HardwareAddr{2, 0, 0, 0, 0, n.ip.To4()[2]}, VNI: 3, Port: o.oe.LocalPort()})
}

func (n *node) sendSuper(h packet.VnetHeader, wire []byte, to *node) (bool, error) {
	return sendSuper(n.oe, h, wire, to.ip)
}

func (n *node) sendPlain(t *testing.T, to *node) {
	t.Helper()
	d := testFrame(100)
	if err := n.oe.Send(d, to.ip); err != nil {
		t.Fatal(err)
	}
}

func waitCount(t *testing.T, what string, n int, get func() int) {
	t.Helper()
	waitFor(t, what, func() bool { return get() >= n })
}

// Two engines with real sockets, UDP_SEGMENT at one end and UDP_GRO at the other: the
// first learns from a plain datagram that the second takes super-frames, and sends them
// whole, and they come out intact, in both directions.
func TestSuperFramesBetweenTwoEngines(t *testing.T) {
	cfg := OverlayConfig{Offload: true, Super: true, RcvBuf: 4 << 20}
	a, b := newNode(t, 1, cfg), newNode(t, 2, cfg)
	a.routeTo(b)
	b.routeTo(a)
	skipWithoutSegmentation(t, a.oe)

	h, wire := tsoFrame(false, 1000, 1450, 1)
	if sent, _ := a.sendSuper(h, wire, b); sent {
		t.Fatal("sent before B said it takes them")
	}
	b.sendPlain(t, a) // A learns that B takes them
	waitCount(t, "A to see the mark", 1, func() int { return a.oe.Stats().SuperPeers })
	b.sendPlain(t, a)

	var want [][]byte
	for i, n := range []int{1, 1398, 1399, 30000, 65400, 100, 64000} {
		for _, v6 := range []bool{false, true} {
			h, wire := tsoFrame(v6, n, 1450, byte(i))
			sent, err := a.sendSuper(h, wire, b)
			if !sent || err != nil {
				t.Fatalf("payload %d: sent=%v, %v", n, sent, err)
			}
			want = append(want, wire)
			// One at a time: the socket's buffer is smaller than a burst of 64 KiB frames.
			waitCount(t, fmt.Sprintf("frame %d to arrive", len(want)), len(want), func() int { _, s := b.sink.counts(); return s })
			b.sink.mu.Lock()
			got := b.sink.supers[len(want)-1]
			b.sink.mu.Unlock()
			// The route sets the destination MAC.
			exp := append([]byte(nil), wire...)
			copy(exp[0:6], net.HardwareAddr{2, 0, 0, 0, 0, 1})
			if got.h != h || !bytes.Equal(got.frame, exp) {
				t.Fatalf("payload %d v6=%v: the frame that arrived is not the one sent: %+v vs %+v, % x vs % x", n, v6, got.h, h, got.frame[:20], exp[:20])
			}
		}
	}
	sa, sb := a.oe.Stats(), b.oe.Stats()
	if sa.SuperTx != uint64(len(want)) || sb.SuperRx != uint64(len(want)) || sb.SuperPartial != 0 || sb.SuperInvalid != 0 || sb.RxInvalid != 0 {
		t.Errorf("A %+v, B %+v", sa, sb)
	}
	if sb.RxReads >= sa.TxPackets && sb.ReceiveOffload {
		t.Logf("B read %d times for %d datagrams: no coalescing on this path", sb.RxReads, sa.TxPackets)
	}
	t.Logf("A sent %d super-frames in %d datagrams and %d sends; B read them in %d reads", sa.SuperTx, sa.SuperTxChunks, sa.TxSends, sb.RxReads)
	if sa.SuperPeers != 1 {
		t.Errorf("A knows %d peers", sa.SuperPeers)
	}

	// The other way, once A has told B.
	a.sendPlain(t, b)
	waitCount(t, "B to see the mark", 1, func() int { return b.oe.Stats().SuperPeers })
	h, wire = tsoFrame(false, 40000, 1450, 9)
	if sent, err := b.sendSuper(h, wire, a); !sent || err != nil {
		t.Fatalf("B to A: sent=%v, %v", sent, err)
	}
	waitCount(t, "the frame at A", 1, func() int { _, s := a.sink.counts(); return s })
}

// A node without the feature never announces, so it is never sent chunks, and it handles
// the plain datagrams that carry the marks of the others like any other.
func TestNodeWithoutTheFeatureIsNeverSentChunks(t *testing.T) {
	on := OverlayConfig{Offload: true, Super: true}
	for name, other := range map[string]OverlayConfig{"feature off": {Offload: true}, "no offload": {}, "no offload but the feature": {Super: true}} {
		t.Run(name, func(t *testing.T) {
			a, b := newNode(t, 1, on), newNode(t, 2, other)
			a.routeTo(b)
			b.routeTo(a)
			skipWithoutSegmentation(t, a.oe)
			h, wire := tsoFrame(false, 20000, 1450, 1)
			for i := 0; i < 3; i++ {
				b.sendPlain(t, a)
				a.sendPlain(t, b) // marked
			}
			waitCount(t, "the frames at B", 3, func() int { n, _ := b.sink.counts(); return n })
			waitCount(t, "the frames at A", 3, func() int { n, _ := a.sink.counts(); return n })
			if sent, err := a.sendSuper(h, wire, b); sent || err != nil {
				t.Errorf("sent=%v, %v to a node that did not announce", sent, err)
			}
			if st := b.oe.Stats(); st.SuperRx != 0 || st.SuperInvalid != 0 || st.RxInvalid != 0 || st.RxPackets != 3 {
				t.Errorf("B %+v", st)
			}
			if st := a.oe.Stats(); st.SuperPeers != 0 || st.SuperTx != 0 {
				t.Errorf("A %+v", st)
			}
			// What B received is the frame, as sent: marks or none.
			b.sink.mu.Lock()
			defer b.sink.mu.Unlock()
			for _, f := range b.sink.frames {
				if len(f.Payload) != 100 {
					t.Errorf("a frame of %d bytes", len(f.Payload))
				}
			}
		})
	}
}

// A node that has the feature receives chunks whatever its own switch says: a peer sends
// them only to a node that announced, and accepting them is harmless.
func TestChunksAreAcceptedWhateverTheSwitch(t *testing.T) {
	h, wire := tsoFrame(false, 4000, 1450, 1)
	for _, cfg := range []OverlayConfig{{}, {Offload: true}, {Offload: true, Super: true}} {
		n := newNode(t, 3, cfg)
		c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(n.addr), Port: n.oe.LocalPort()})
		if err != nil {
			t.Fatal(err)
		}
		for _, dg := range refChunks(0, 1, 1, h, wire, chunkDatagram) {
			if _, err := c.Write(dg); err != nil {
				t.Fatal(err)
			}
		}
		c.Close()
		waitCount(t, "the super-frame", 1, func() int { _, s := n.sink.counts(); return s })
		if st := n.oe.Stats(); st.SuperRx != 1 {
			t.Errorf("%+v: %+v", cfg, st)
		}
		n.oe.Close()
	}
}

// failSuperSends makes the calls of superSend that are numbered in nths (from 1) fail with
// err, and returns the function that puts it back.
func failSuperSends(err error, nths ...int) func() {
	old := superSend
	calls := 0
	superSend = func(w *superIO, ts *txSock, to netip.AddrPort, n int, oob []byte, want int) error {
		calls++
		if slices.Contains(nths, calls) {
			return err
		}
		return old(w, ts, to, n, oob, want)
	}
	return func() { superSend = old }
}

// The failures of a send: one that fails before anything is on the wire leaves the frame
// to be cut (and gives UDP_SEGMENT up if that is what failed, as it does for a run); one
// that fails after the start of the frame is its loss.
func TestSuperSendErrors(t *testing.T) {
	h, frame := tsoFrame(false, 65000, 1450, 1) // 45 chunks: sends of 44 and 1
	failing := func(nth int, err error) func() { return failSuperSends(err, nth) }

	t.Run("segmentation not available", func(t *testing.T) {
		f := newSuperFixture(t, "127.0.0.1", OverlayConfig{Offload: true, Super: true})
		f.announce()
		defer failing(1, syscall.EIO)()
		if sent, err := f.send(h, frame); sent || err != nil {
			t.Fatalf("sent=%v, %v", sent, err)
		}
		st := f.oe.Stats()
		if st.SegmentOffload || st.SuperTx != 0 || st.TxErrors != 0 || st.TxPackets != 0 || st.SuperTxFailed != 0 {
			t.Errorf("%+v", st)
		}
		tb := f.oe.NewBatch()
		defer tb.Release()
		if tb.AcceptsSuper(f.dst(0)) {
			t.Error("still accepts super-frames")
		}
	})
	t.Run("another failure", func(t *testing.T) {
		f := newSuperFixture(t, "127.0.0.1", OverlayConfig{Offload: true, Super: true})
		f.announce()
		defer failing(1, syscall.ENOBUFS)()
		if sent, err := f.send(h, frame); sent || err != nil {
			t.Fatalf("sent=%v, %v", sent, err)
		}
		if st := f.oe.Stats(); !st.SegmentOffload || st.SuperTx != 0 || st.TxErrors != 0 || st.SuperTxFailed != 1 {
			t.Errorf("%+v", st)
		}
	})
	t.Run("after the start", func(t *testing.T) {
		f := newSuperFixture(t, "127.0.0.1", OverlayConfig{Offload: true, Super: true})
		f.announce()
		defer failing(2, syscall.ENOBUFS)()
		sent, err := f.send(h, frame)
		if !sent || !errors.Is(err, syscall.ENOBUFS) {
			t.Fatalf("sent=%v, %v", sent, err)
		}
		st := f.oe.Stats()
		if !st.SegmentOffload || st.SuperTx != 0 || st.TxErrors != 1 || st.TxPackets != 44 || st.TxSends != 2 || st.SuperTxFailed != 0 { // 45 chunks
			t.Errorf("%+v", st)
		}
	})
	t.Run("a socket that is closed", func(t *testing.T) {
		f := newSuperFixture(t, "127.0.0.1", OverlayConfig{Offload: true, Super: true})
		f.announce()
		for _, ts := range f.oe.socks.Load().tx {
			ts.conn.Close()
		}
		if sent, err := f.send(h, frame); sent || err != nil {
			t.Fatalf("sent=%v, %v", sent, err)
		}
	})
}

// A send that fails before anything is on the wire, for a reason other than UDP_SEGMENT,
// makes the engine cut the super-frames for a second, instead of every one of them trying
// again; then it tries again, and a second failure holds them back for another second.
func TestFailedSuperSendHoldsSuperFramesBackForASecond(t *testing.T) {
	h, frame := tsoFrame(false, 3000, 1450, 1) // 3 chunks, one send
	f := newSuperFixture(t, "127.0.0.1", OverlayConfig{Offload: true, Super: true})
	f.announce()
	defer failSuperSends(syscall.ENOBUFS, 1, 2)()
	tb := f.oe.NewBatch()
	defer tb.Release()

	// try sends the frame and reports whether it went whole, with the engine's counters.
	try := func(what string, wantSent bool) Stats {
		t.Helper()
		before := f.oe.Stats()
		sent, err := f.send(h, frame)
		if sent != wantSent || err != nil {
			t.Fatalf("%s: sent=%v (want %v), %v", what, sent, wantSent, err)
		}
		if st := f.oe.Stats(); wantSent {
			f.receive(t, 0, 3)
			if st.SuperTx-before.SuperTx != 1 {
				t.Errorf("%s: %+v", what, st)
			}
		} else if st.TxPackets != before.TxPackets {
			t.Errorf("%s: datagrams went out: %+v", what, st)
		}
		return f.oe.Stats()
	}

	if st := try("the failing send", false); st.SuperTxFailed != 1 || !st.SegmentOffload {
		t.Errorf("after the failure: %+v", st)
	}
	f.nothing(t, 0)
	failed := f.now.Load()
	for _, after := range []time.Duration{0, 400 * time.Millisecond, 999 * time.Millisecond} {
		f.now.Store(failed + int64(after))
		sends := f.oe.Stats().TxSends
		if st := try(fmt.Sprintf("%v after the failure", after), false); st.SuperTxFailed != 1 || st.TxSends != sends {
			t.Errorf("held back at %v: %+v", after, st)
		}
		if tb.AcceptsSuper(f.dst(0)) {
			t.Errorf("accepts super-frames %v after the failure", after)
		}
	}
	// A plain frame is not held back: it is what the caller sends instead.
	ef := ethernet(frame[:14+1000])
	if err := f.oe.Send(ef, f.dst(0)); err != nil {
		t.Fatal(err)
	}
	f.receive(t, 0, 1)

	f.now.Store(failed + int64(time.Second))
	if !tb.AcceptsSuper(f.dst(0)) {
		t.Error("does not accept super-frames a second after the failure")
	}
	if st := try("the retry, which fails", false); st.SuperTxFailed != 2 || !st.SegmentOffload {
		t.Errorf("after the second failure: %+v", st)
	}
	f.now.Add(int64(999 * time.Millisecond))
	try("within a second of the second failure", false)
	f.now.Add(int64(time.Millisecond))
	if st := try("a second after the second failure", true); st.SuperTx != 1 || st.SuperTxFailed != 2 {
		t.Errorf("after the retry: %+v", st)
	}
	try("and the next", true)
}

// A frame that the receiving node would reject for its segment size (see
// packet.CheckTSOSize) is never sent whole, and is left to be cut; one it would take is.
func TestSuperFramesTheReceiverWouldRejectAreNotSentWhole(t *testing.T) {
	f := newSuperFixture(t, "127.0.0.1", OverlayConfig{Offload: true, Super: true})
	f.announce()
	for _, v6 := range []bool{false, true} {
		for _, size := range []int{1, 100, 535, 536, 537, 1000, 1219, 1220, 1221, 1400, 8000} {
			for _, n := range []int{size + 1, 3000, 60000} {
				name := fmt.Sprintf("v6=%v size=%d payload=%d", v6, size, n)
				h, frame := tsoFrame(v6, n, 1450, 1)
				h.GSOSize = uint16(size)
				before := f.oe.Stats()
				sent, err := f.send(h, frame)
				want := packet.CheckTSOSize(h, frame) == nil // the receiver takes it
				if sent != want || err != nil {
					t.Fatalf("%s: sent=%v, %v; the receiver takes it: %v", name, sent, err, want)
				}
				st := f.oe.Stats()
				if !want {
					if st.TxSends != before.TxSends || st.SuperTxFailed != 0 {
						t.Errorf("%s: something went out or failed: %+v", name, st)
					}
					continue
				}
				f.receive(t, 0, int(st.SuperTxChunks-before.SuperTxChunks))
			}
		}
	}
	f.nothing(t, 0)
}

// Sending a super-frame whole allocates nothing, whatever its size.
func TestSendingASuperFrameAllocatesNothing(t *testing.T) {
	f := newSuperFixture(t, "127.0.0.1", OverlayConfig{Offload: true, Super: true})
	f.announce()
	for _, n := range []int{3000, 65000} {
		h, frame := tsoFrame(false, n, 1450, 1)
		ef, dst := ethernet(frame), f.dst(0)
		run := func() {
			tb := f.oe.NewBatch()
			if sent, err := tb.SendSuper(h, ef, dst); !sent || err != nil {
				t.Fatal(sent, err)
			}
			_ = tb.Flush()
			tb.Release()
		}
		run()
		allocs := testing.AllocsPerRun(50, run)
		t.Logf("payload %d: %.1f allocations", n, allocs)
		if allocs != 0 && !raceEnabled {
			t.Errorf("payload %d: %.1f allocations for a super-frame, want 0", n, allocs)
		}
	}
}
