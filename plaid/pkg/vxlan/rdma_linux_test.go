//go:build linux

package vxlan

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"plaid/pkg/packet"
	"plaid/pkg/rdma"
)

// Two engines, A on 127.0.0.1 and B on 127.0.0.2, each with a route to the other (A's pods
// are 10.244.1.0/24, B's 10.244.2.0/24) and a device on one fake fabric (see fakeFabric):
// the transport's links, messages, failures and events, with real UDP sockets for the
// control datagrams and the fall back.

// rdmaRecorder records what an engine delivers: frames by flow and sequence (flowFrame),
// and super-frames whole, which it releases at once unless told to hold them.
type rdmaRecorder struct {
	flowInjector
	mu     sync.Mutex
	supers [][]byte
	hs     []packet.VnetHeader
	hold   bool
	held   []*packet.Buffer
}

func (r *rdmaRecorder) InjectSuper(h packet.VnetHeader, buf *packet.Buffer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hs = append(r.hs, h)
	r.supers = append(r.supers, append([]byte(nil), buf.B[packet.VnetHeaderLen:]...))
	if r.hold {
		r.held = append(r.held, buf)
	} else {
		buf.Release()
	}
	return nil
}

func (r *rdmaRecorder) superCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.supers)
}

func (r *rdmaRecorder) releaseHeld() {
	r.mu.Lock()
	held := r.held
	r.held, r.hold = nil, false
	r.mu.Unlock()
	for _, b := range held {
		b.Release()
	}
}

type rdmaNodes struct {
	fabric     *fakeFabric
	devA, devB *fakeDev
	a, b       *OverlayEngine
	recA, recB *rdmaRecorder
}

// newRDMANodes starts A and B; rdmaA and rdmaB are their transports' settings (nil: none).
func newRDMANodes(t *testing.T, rdmaA, rdmaB *RDMAConfig) *rdmaNodes {
	t.Helper()
	n := &rdmaNodes{fabric: newFakeFabric(), recA: &rdmaRecorder{}, recB: &rdmaRecorder{}}
	n.devA, n.devB = n.fabric.add("fa", 10), n.fabric.add("fb", 14)
	old := openRDMA
	openRDMA = n.fabric.open
	t.Cleanup(func() { // after the engines' (cleanups run last first): everything is closed by now
		openRDMA = old
		for _, v := range n.fabric.checkOrder() {
			t.Errorf("verbs order: %s", v)
		}
	})
	start := func(ip string, rc *RDMAConfig, rec *rdmaRecorder) *OverlayEngine {
		oe := NewOverlayEngine(OverlayConfig{BindAddress: ip, Port: 0, Sockets: 2, Offload: true, Super: true, RDMA: rc}, NewRouteTable(), rec)
		if err := oe.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { oe.Close() })
		return oe
	}
	n.a = start("127.0.0.1", rdmaA, n.recA)
	n.b = start("127.0.0.2", rdmaB, n.recB)
	_, netA, _ := net.ParseCIDR("10.244.1.0/24")
	_, netB, _ := net.ParseCIDR("10.244.2.0/24")
	n.a.routes.AddRoute(&RouteEntry{Subnet: netB, RemoteHostIP: net.ParseIP("127.0.0.2").To4(), VNI: 1, Port: n.b.LocalPort()})
	n.b.routes.AddRoute(&RouteEntry{Subnet: netA, RemoteHostIP: net.ParseIP("127.0.0.1").To4(), VNI: 1, Port: n.a.LocalPort()})
	return n
}

func smallRDMA(dev string) *RDMAConfig {
	return &RDMAConfig{Port: rdma.Port{Device: dev, Num: 1}, RecvSlots: 16, SendSlots: 32, MaxInFlight: 8}
}

// peerOf is what the engine's status says of its one peer.
func peerOf(oe *OverlayEngine) RDMAPeer {
	st := oe.Stats().RDMA
	if st == nil || len(st.Peers) == 0 {
		return RDMAPeer{}
	}
	return st.Peers[0]
}

func waitState(t *testing.T, oe *OverlayEngine, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for peerOf(oe).State != state {
		if time.Now().After(deadline) {
			t.Fatalf("the peer is %+v, not %q, after 10 s", peerOf(oe), state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sendFlows sends rounds frames of each of flows flows from A to B, in one batch per round.
func (n *rdmaNodes) sendFlows(t *testing.T, flows, rounds int, first uint32) {
	t.Helper()
	for r := 0; r < rounds; r++ {
		tb := n.a.NewBatch()
		for f := 0; f < flows; f++ {
			if err := tb.Send(flowFrame(f, first+uint32(r)), net.ParseIP("10.244.2.9")); err != nil {
				t.Fatal(err)
			}
		}
		if err := tb.Flush(); err != nil {
			t.Fatal(err)
		}
		tb.Release()
	}
}

func (n *rdmaNodes) waitFrames(t *testing.T, want int) {
	t.Helper()
	waitFor(t, "frames at B", func() bool { return n.recB.total() >= want })
}

func checkOrder(t *testing.T, rec *rdmaRecorder, flows, rounds int) {
	t.Helper()
	rec.flowInjector.mu.Lock()
	defer rec.flowInjector.mu.Unlock()
	for f := 0; f < flows; f++ {
		seqs := rec.flows[f]
		if len(seqs) != rounds {
			t.Errorf("flow %d: %d frames, want %d", f, len(seqs), rounds)
			continue
		}
		for i, s := range seqs {
			if s != uint32(i) {
				t.Errorf("flow %d: frame %d is %d: out of order", f, i, s)
				break
			}
		}
	}
}

// Both offer: the link comes up at both ends, and what A sends to B goes over it, in order
// per flow, and not over the UDP sockets.
func TestRDMALinkCarriesTheOverlay(t *testing.T) {
	a, b := smallRDMA("fa"), smallRDMA("fb")
	a.MaxInFlight, a.SendSlots, b.RecvSlots = rdmaDefaultInFlight, 256, rdmaDefaultRecv // room for the burst: no drops
	n := newRDMANodes(t, a, b)
	waitState(t, n.a, "up")
	waitState(t, n.b, "up")
	n.sendFlows(t, 6, 40, 0)
	if st := n.a.Stats().RDMA; st.TxFull != 0 || st.TxNoSlot != 0 {
		t.Fatalf("A dropped %d at the in-flight cap and %d for want of a slot", st.TxFull, st.TxNoSlot)
	}
	n.waitFrames(t, 6*40)
	checkOrder(t, n.recB, 6, 40)
	st := n.a.Stats()
	if st.TxPackets != 0 || st.RDMA.TxMessages == 0 {
		t.Errorf("A sent %d datagrams over UDP and %d RDMA messages: all should be RDMA", st.TxPackets, st.RDMA.TxMessages)
	}
	if p := peerOf(n.a); p.QPs != 2 || p.Failures != 0 {
		t.Errorf("A's peer: %+v", p)
	}
	// The receive slots are all posted again once the frames have been handled.
	waitFor(t, "B's receives posted again", func() bool {
		for _, n := range n.b.Stats().RDMA.RecvPosted {
			if n != int64(b.RecvSlots) {
				return false
			}
		}
		return true
	})
}

// A super-frame goes whole, in one message, and arrives whole with its virtio header.
func TestRDMASuperFrameGoesWhole(t *testing.T) {
	n := newRDMANodes(t, smallRDMA("fa"), smallRDMA("fb"))
	waitState(t, n.a, "up")
	waitState(t, n.b, "up")
	h, raw := tsoFrame(false, 60000, 9000, 3)
	frame, err := packet.ParseEthernet(raw)
	if err != nil {
		t.Fatal(err)
	}
	tb := n.a.NewBatch()
	defer tb.Release()
	if !tb.AcceptsSuper(net.ParseIP("10.244.2.4")) {
		t.Fatal("AcceptsSuper is false over an RDMA link that is up")
	}
	sent, err := tb.SendSuper(h, frame, net.ParseIP("10.244.2.4"))
	if !sent || err != nil {
		t.Fatalf("SendSuper: %v %v", sent, err)
	}
	waitFor(t, "the super-frame at B", func() bool { return n.recB.superCount() == 1 })
	n.recB.mu.Lock()
	defer n.recB.mu.Unlock()
	if n.recB.hs[0] != h {
		t.Errorf("virtio header %+v, want %+v", n.recB.hs[0], h)
	}
	if !bytes.Equal(n.recB.supers[0][packet.EthernetHeaderLen:], raw[packet.EthernetHeaderLen:]) {
		t.Error("the frame's bytes differ")
	}
	if st := n.a.Stats(); st.SuperTxChunks != 0 || st.TxPackets != 0 {
		t.Errorf("chunks %d, datagrams %d: nothing should go over UDP", st.SuperTxChunks, st.TxPackets)
	}
}

// A link whose sends are not acknowledged fails, recoverably: A is on UDP at once (what it
// sends arrives), and a new link comes up. The third failure leaves the peer on UDP, and B
// is told to stop offering.
func TestRDMARecoverableFailures(t *testing.T) {
	n := newRDMANodes(t, smallRDMA("fa"), smallRDMA("fb"))
	sent := 0
	for i := 1; i <= rdmaMaxFailures; i++ {
		waitState(t, n.a, "up")
		waitState(t, n.b, "up")
		n.devA.breakQPs()
		n.sendFlows(t, 1, 1, uint32(sent)) // fails the link: this one is lost
		sent++
		waitFor(t, "the failure", func() bool { return peerOf(n.a).Failures == i })
		if p := peerOf(n.a); p.Why != rdma.WCRetryExcErr.String() || p.State == "up" {
			t.Fatalf("failure %d: %+v", i, p)
		}
		n.sendFlows(t, 1, 1, uint32(sent)) // over UDP
		sent++
		waitFor(t, "a frame over UDP", func() bool { return n.a.Stats().TxPackets > 0 })
	}
	waitState(t, n.a, "given up")
	waitState(t, n.b, "not offered by the peer")
	if st := n.b.Stats().RDMA; st.LinkFailures != 0 {
		t.Errorf("B counted %d failures: A's are A's", st.LinkFailures)
	}
}

// Any other error of a completion is taken for a bug: the peer stays on UDP at once.
func TestRDMAFatalErrorGivesTheLinkUp(t *testing.T) {
	n := newRDMANodes(t, smallRDMA("fa"), smallRDMA("fb"))
	waitState(t, n.a, "up")
	waitState(t, n.b, "up")
	n.devA.failNextSend(rdma.WCLocProtErr)
	n.sendFlows(t, 1, 1, 0)
	waitState(t, n.a, "given up")
	if p := peerOf(n.a); p.Failures != 1 || p.Why != rdma.WCLocProtErr.String() {
		t.Errorf("A's peer: %+v", p)
	}
	n.sendFlows(t, 1, 3, 1)
	waitFor(t, "frames over UDP", func() bool { return n.recB.total() >= 3 })
}

// A node without the transport answers NONE: the other stays on UDP and says why.
func TestRDMANotOfferedByAPeerWithoutIt(t *testing.T) {
	n := newRDMANodes(t, smallRDMA("fa"), nil)
	waitState(t, n.a, "not offered by the peer")
	if n.b.Stats().RDMA != nil {
		t.Error("B, without the transport, reports one")
	}
	n.sendFlows(t, 2, 10, 0)
	n.waitFrames(t, 20)
	checkOrder(t, n.recB, 2, 10)
	if n.a.Stats().RDMA.TxMessages != 0 {
		t.Error("A sent RDMA messages to a node without the transport")
	}
}

// The port going down takes the link down at once, without it counting as a failure at
// either end, and its coming back brings a new link.
func TestRDMAPortDownAndUp(t *testing.T) {
	n := newRDMANodes(t, smallRDMA("fa"), smallRDMA("fb"))
	waitState(t, n.a, "up")
	waitState(t, n.b, "up")
	n.devA.events <- rdma.Event{Type: rdma.EventPortError, Port: 1}
	waitState(t, n.a, "down, offered again")
	waitFor(t, "B to see the link go", func() bool { return peerOf(n.b).State != "up" })
	n.sendFlows(t, 1, 2, 0)
	waitFor(t, "frames over UDP", func() bool { return n.recB.total() >= 2 })
	n.devA.events <- rdma.Event{Type: rdma.EventPortActive, Port: 1}
	waitState(t, n.a, "up")
	waitState(t, n.b, "up")
	if a, b := peerOf(n.a), peerOf(n.b); a.Failures != 0 || b.Failures != 0 {
		t.Errorf("failures counted for a port that went down: A %+v, B %+v", a, b)
	}
	n.sendFlows(t, 1, 2, 2)
	n.waitFrames(t, 4)
	checkOrder(t, n.recB, 1, 4)
}

// A receiver that holds its buffers runs out of posted receives (its lane says it ran low);
// the sender's QP then holds MaxInFlight sends and drops the rest, counted, without waiting.
// When the receiver lets the buffers go, what was in flight arrives.
func TestRDMAInFlightCapDropsWithoutWaiting(t *testing.T) {
	a, b := smallRDMA("fa"), smallRDMA("fb")
	b.RecvSlots = 4
	n := newRDMANodes(t, a, b)
	waitState(t, n.a, "up")
	waitState(t, n.b, "up")
	n.recB.mu.Lock()
	n.recB.hold = true
	n.recB.mu.Unlock()
	h, raw := tsoFrame(false, 30000, 9000, 1)
	tb := n.a.NewBatch()
	defer tb.Release()
	start := time.Now()
	for i := 0; i < 40; i++ {
		frame, _ := packet.ParseEthernet(append([]byte(nil), raw...))
		if sent, err := tb.SendSuper(h, frame, net.ParseIP("10.244.2.4")); !sent || err != nil {
			t.Fatalf("SendSuper %d: %v %v", i, sent, err)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("40 sends took %v: the sender waited for the receiver", d)
	}
	waitFor(t, "B to hold its 4 buffers", func() bool { return n.recB.superCount() == 4 })
	if st := n.a.Stats().RDMA; st.TxFull == 0 {
		t.Errorf("no drop counted at the in-flight cap: %+v", st)
	}
	waitFor(t, "B's lane to say it ran low", func() bool { return n.b.Stats().RDMA.RxLow > 0 })
	n.recB.releaseHeld()
	// Every send arrives or is counted as dropped (what counts against the cap includes sends
	// delivered but not yet completed, see rdmaSignalEvery), and no send slot is lost.
	ta := n.a.rd.Load()
	waitFor(t, "every send to arrive or be counted as dropped", func() bool {
		st := n.a.Stats().RDMA
		return n.recB.superCount()+int(st.TxFull+st.TxNoSlot) == 40 && len(ta.sendFree) == cap(ta.sendFree)
	})
}

// Closing an engine destroys the transport in the verbs' order (the fake reports any other,
// see newRDMANodes) while traffic flows and a TAP writer still holds received buffers, which
// it releases afterwards: their slots are not posted to the destroyed SRQ.
func TestRDMACloseWithBuffersHeld(t *testing.T) {
	n := newRDMANodes(t, smallRDMA("fa"), smallRDMA("fb"))
	waitState(t, n.a, "up")
	waitState(t, n.b, "up")
	n.recB.mu.Lock()
	n.recB.hold = true
	n.recB.mu.Unlock()
	h, raw := tsoFrame(false, 20000, 9000, 1)
	tb := n.a.NewBatch()
	for i := 0; i < 3; i++ {
		frame, _ := packet.ParseEthernet(append([]byte(nil), raw...))
		if sent, err := tb.SendSuper(h, frame, net.ParseIP("10.244.2.4")); !sent || err != nil {
			t.Fatalf("SendSuper: %v %v", sent, err)
		}
	}
	tb.Release()
	waitFor(t, "B to hold 3 buffers", func() bool { return n.recB.superCount() == 3 })
	stop := make(chan struct{})
	go func() { // traffic while the engines close
		for {
			select {
			case <-stop:
				return
			default:
			}
			tb := n.a.NewBatch() // errors are expected once the engine is closing
			_ = tb.Send(flowFrame(0, 0), net.ParseIP("10.244.2.9"))
			_ = tb.Flush()
			tb.Release()
		}
	}()
	n.b.Close()
	n.a.Close()
	close(stop)
	n.recB.releaseHeld()
	if v := n.fabric.checkOrder(); len(v) > 0 {
		t.Errorf("verbs order: %v", v)
	}
}

// A device that is slow to open does not hold up the engine's start (which may be answering
// the handoff of its sockets, under a deadline): the transport starts afterwards, and a HELLO
// that arrives meanwhile is not refused, so the link comes up as soon as the device is open,
// not after the 30 s that a NONE would make the peer wait.
func TestRDMASlowDeviceDoesNotDelayTheStart(t *testing.T) {
	fabric := newFakeFabric()
	fabric.add("fa", 10)
	fabric.add("fb", 14)
	fabric.openDelay = 2 * time.Second
	old := openRDMA
	openRDMA = fabric.open
	t.Cleanup(func() { openRDMA = old })
	start := time.Now()
	a := NewOverlayEngine(OverlayConfig{BindAddress: "127.0.0.1", Sockets: 2, Offload: true, Super: true, RDMA: smallRDMA("fa")}, NewRouteTable(), &rdmaRecorder{})
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if d := time.Since(start); d > time.Second {
		t.Errorf("Start took %v with a device that takes 2 s to open", d)
	}
	fabric.mu.Lock()
	fabric.openDelay = 0
	fabric.mu.Unlock()
	b := NewOverlayEngine(OverlayConfig{BindAddress: "127.0.0.2", Sockets: 2, Offload: true, Super: true, RDMA: smallRDMA("fb")}, NewRouteTable(), &rdmaRecorder{})
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, netA, _ := net.ParseCIDR("10.244.1.0/24")
	_, netB, _ := net.ParseCIDR("10.244.2.0/24")
	a.routes.AddRoute(&RouteEntry{Subnet: netB, RemoteHostIP: net.ParseIP("127.0.0.2").To4(), VNI: 1, Port: b.LocalPort()})
	b.routes.AddRoute(&RouteEntry{Subnet: netA, RemoteHostIP: net.ParseIP("127.0.0.1").To4(), VNI: 1, Port: a.LocalPort()})
	waitState(t, a, "up") // within 10 s: B was not told NONE while A's device opened
	waitState(t, b, "up")
}

// Only one send in rdmaSignalEvery is signaled, but no send slot is held once a burst is over:
// the lane completes a burst's unsignaled tail with a NOP when its CQ goes idle, whatever the
// burst's length, and the peer gives the NOP's receive back.
func TestRDMAUnsignaledTailsAreCompleted(t *testing.T) {
	a, b := smallRDMA("fa"), smallRDMA("fb")
	a.MaxInFlight, a.SendSlots = rdmaDefaultInFlight, 128 // the real cap: bursts that fit it are not dropped
	n := newRDMANodes(t, a, b)
	waitState(t, n.a, "up")
	waitState(t, n.b, "up")
	ta := n.a.rd.Load()
	sent := 0
	for _, burst := range []int{1, 5, 8, 13, 3} {
		n.sendFlows(t, 1, burst, uint32(sent))
		sent += burst
		n.waitFrames(t, sent)
		waitFor(t, "every send slot of A back", func() bool { return len(ta.sendFree) == cap(ta.sendFree) })
	}
	checkOrder(t, n.recB, 1, sent)
	waitFor(t, "B's receives all posted again", func() bool {
		for _, p := range n.b.Stats().RDMA.RecvPosted {
			if p != 16 {
				return false
			}
		}
		return true
	})
	if st := n.b.Stats().RDMA; st.RxMessages != n.a.Stats().RDMA.TxMessages {
		t.Errorf("B counted %d messages, A sent %d: NOPs are not messages", st.RxMessages, n.a.Stats().RDMA.TxMessages)
	}
}
