//go:build linux

package vxlan

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"plaid/pkg/packet"
)

// flowFrame is a UDP packet of flow (source port 10000+flow) that carries its sequence number.
func flowFrame(flow int, seq uint32) *packet.EthernetFrame {
	f := testFrame(0)
	f.Payload = ipv4Packet(packet.IPProtocolUDP, podA, podB, uint16(10000+flow), 5001, 8+8, 0)
	binary.BigEndian.PutUint16(f.Payload[28:], uint16(flow))
	binary.BigEndian.PutUint32(f.Payload[30:], seq)
	return f
}

// flowInjector records the sequence numbers of the frames of each flow in the order they
// are delivered. The overlay's workers call it at the same time.
type flowInjector struct {
	mu    sync.Mutex
	flows map[int][]uint32
}

func (fi *flowInjector) InjectFrame(f *packet.EthernetFrame) error {
	flow, seq := int(binary.BigEndian.Uint16(f.Payload[28:])), binary.BigEndian.Uint32(f.Payload[30:])
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if fi.flows == nil {
		fi.flows = map[int][]uint32{}
	}
	fi.flows[flow] = append(fi.flows[flow], seq)
	return nil
}

func (fi *flowInjector) total() (n int) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	for _, s := range fi.flows {
		n += len(s)
	}
	return n
}

// newEngine starts an engine on loopback with sockets of its own.
func newEngine(t *testing.T, cfg OverlayConfig, inj FrameInjector, routes *RouteTable) *OverlayEngine {
	t.Helper()
	cfg.BindAddress = "127.0.0.1"
	if routes == nil {
		routes = NewRouteTable()
	}
	oe := NewOverlayEngine(cfg, routes, inj)
	if err := oe.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { oe.Close() })
	return oe
}

// The engine's sockets: the receive sockets share the port, the send sockets have ports
// of their own, and closing the engine closes them all and ends every goroutine.
func TestStartOpensTheGroupAndTheSendSockets(t *testing.T) {
	for _, n := range []int{1, 4, 16} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			runtime.GC()
			before := runtime.NumGoroutine()
			oe := newEngine(t, OverlayConfig{Sockets: n}, newMockInjector(), nil)
			st := oe.Stats()
			if st.RxSockets != n || st.TxSockets != n*SendPerReceive || len(st.RxPerSocket) != n {
				t.Fatalf("sockets: %d rx, %d tx, %d counters", st.RxSockets, st.TxSockets, len(st.RxPerSocket))
			}
			s := oe.socks.Load()
			port := oe.LocalPort()
			seen := map[int]bool{port: true}
			for _, l := range s.rx {
				if got := l.conn.LocalAddr().(*net.UDPAddr).Port; got != port {
					t.Errorf("receive socket on port %d, want %d", got, port)
				}
				if !hasReusePort(l.conn) {
					t.Error("a receive socket is not in the group")
				}
			}
			for _, c := range s.tx {
				p := c.LocalAddr().(*net.UDPAddr).Port
				if seen[p] {
					t.Errorf("send socket on port %d, which another socket has", p)
				}
				seen[p] = true
			}

			if err := oe.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			settle(t, before)
			for _, c := range append([]*net.UDPConn{s.rx[0].conn}, s.tx...) {
				if _, err := c.WriteToUDP([]byte("x"), &net.UDPAddr{IP: loopback, Port: 9}); err == nil {
					t.Error("a socket is open after Close")
				}
			}
		})
	}
}

func settle(t *testing.T, n int) {
	t.Helper()
	waitFor(t, "the goroutines to end", func() bool { return runtime.NumGoroutine() <= n })
}

func TestAdoptNeedsReceiveAndSendSockets(t *testing.T) {
	oe := NewOverlayEngine(OverlayConfig{}, NewRouteTable(), nil)
	if err := oe.Adopt(context.Background(), nil, []*net.UDPConn{plainSocket(t)}); err == nil {
		t.Error("an engine without a receive socket was attached")
	}
	if err := oe.Adopt(context.Background(), []*net.UDPConn{plainSocket(t)}, nil); err == nil {
		t.Error("an engine without a send socket was attached")
	}
}

// pair is a sender and a receiver on loopback, each with its sockets as asked.
type pair struct {
	rx, tx *OverlayEngine
	inj    *flowInjector
}

func newPair(t *testing.T, rxCfg, txCfg OverlayConfig) pair {
	t.Helper()
	p := pair{inj: &flowInjector{}}
	rxCfg.RcvBuf = 4 << 20
	p.rx = newEngine(t, rxCfg, p.inj, nil)
	p.tx = newEngine(t, txCfg, nil, routeTo(p.rx.LocalPort()))
	return p
}

// sendFlows sends rounds x flows numbered datagrams, a round at a time, each waiting for
// the receiver to have taken the round (so that its buffers hold it).
func (p pair) sendFlows(t *testing.T, flows, rounds int) {
	t.Helper()
	dst := net.ParseIP("10.244.2.10")
	seq := make([]uint32, flows)
	for r := 0; r < rounds; r++ {
		for f := 0; f < flows; f++ {
			if err := p.tx.Send(flowFrame(f, seq[f]), dst); err != nil {
				t.Fatal(err)
			}
			seq[f]++
		}
		want := (r + 1) * flows
		waitFor(t, fmt.Sprintf("round %d", r), func() bool { return p.inj.total() == want })
	}
}

func (p pair) check(t *testing.T, flows, rounds int) {
	t.Helper()
	p.inj.mu.Lock()
	defer p.inj.mu.Unlock()
	if len(p.inj.flows) != flows {
		t.Errorf("%d flows arrived, want %d", len(p.inj.flows), flows)
	}
	for f, seqs := range p.inj.flows {
		if len(seqs) != rounds {
			t.Errorf("flow %d: %d datagrams, want %d", f, len(seqs), rounds)
		}
		for i, s := range seqs {
			if s != uint32(i) {
				t.Errorf("flow %d: datagram %d carries sequence %d: order was lost", f, i, s)
				break
			}
		}
	}
	if st := p.tx.Stats(); st.TxErrors != 0 || st.TxPackets != uint64(flows*rounds) {
		t.Errorf("sender: %d datagrams, %d errors", st.TxPackets, st.TxErrors)
	}
	if st := p.rx.Stats(); st.RxPackets != uint64(flows*rounds) || st.RxInvalid != 0 || st.RxDropped != 0 || st.RxQueueDrops != 0 || st.SocketDrops != 0 {
		t.Errorf("receiver: %+v", st)
	}
}

// Two engines with several sockets: every datagram of 64 flows arrives, each flow in
// order, and the kernel spread the flows over the receiver's sockets.
func TestFlowsArriveInOrderAndSpreadOverTheReceiveSockets(t *testing.T) {
	const flows, rounds = 64, 20
	p := newPair(t, OverlayConfig{Sockets: 4}, OverlayConfig{Sockets: 4})
	p.sendFlows(t, flows, rounds)
	p.check(t, flows, rounds)

	st := p.rx.Stats()
	t.Logf("receive sockets delivered %v", st.RxPerSocket)
	used := 0
	for _, n := range st.RxPerSocket {
		if n > 0 {
			used++
		}
		if n%rounds != 0 {
			t.Errorf("a socket delivered %d datagrams: a flow of %d was split over sockets", n, rounds)
		}
	}
	if used < 3 {
		t.Errorf("only %d of 4 receive sockets delivered datagrams: %v", used, st.RxPerSocket)
	}
}

// Engines with other numbers of sockets talk to each other.
func TestEnginesWithAnyNumberOfSocketsInteroperate(t *testing.T) {
	const flows, rounds = 64, 8
	for _, tc := range []struct {
		name           string
		rx, tx         OverlayConfig
		wantRx, wantTx int
	}{
		{"one socket group to four", OverlayConfig{Sockets: 1}, OverlayConfig{Sockets: 4}, 1, 16},
		{"four to one socket group", OverlayConfig{Sockets: 4}, OverlayConfig{Sockets: 1}, 4, 4},
		{"sixteen to four", OverlayConfig{Sockets: 16}, OverlayConfig{Sockets: 4}, 16, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPair(t, tc.rx, tc.tx)
			if st := p.rx.Stats(); st.RxSockets != tc.wantRx {
				t.Errorf("receiver has %d sockets, want %d", st.RxSockets, tc.wantRx)
			}
			if st := p.tx.Stats(); st.TxSockets != tc.wantTx {
				t.Errorf("sender has %d send sockets, want %d", st.TxSockets, tc.wantTx)
			}
			p.sendFlows(t, flows, rounds)
			p.check(t, flows, rounds)
		})
	}
}

// What leaves the engine is plain VXLAN to the route's port from several source ports,
// each flow from its own one.
func TestSendSocketsAreChosenByFlow(t *testing.T) {
	const flows, rounds = 64, 3
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Skipf("cannot open a UDP socket: %v", err)
	}
	defer sink.Close()
	_ = sink.SetReadBuffer(4 << 20)
	tx := newEngine(t, OverlayConfig{Sockets: 4}, nil, routeTo(sink.LocalAddr().(*net.UDPAddr).Port))
	for r := 0; r < rounds; r++ {
		for f := 0; f < flows; f++ {
			if err := tx.Send(flowFrame(f, uint32(r)), net.ParseIP("10.244.2.10")); err != nil {
				t.Fatal(err)
			}
		}
	}
	ports := map[int]uint16{} // flow -> source port
	all := map[uint16]bool{}
	buf := make([]byte, 2000)
	for i := 0; i < flows*rounds; i++ {
		_ = sink.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, from, err := sink.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.Fatalf("datagram %d: %v", i, err)
		}
		vx, err := packet.ParseVXLAN(buf[:n])
		if err != nil || vx.Header.VNI != 1 {
			t.Fatalf("not VXLAN: %v", err)
		}
		flow := int(binary.BigEndian.Uint16(vx.InnerFrame.Payload[28:]))
		if p, ok := ports[flow]; ok && p != from.Port() {
			t.Fatalf("flow %d left from ports %d and %d", flow, p, from.Port())
		}
		ports[flow], all[from.Port()] = from.Port(), true
	}
	t.Logf("%d source ports for %d flows", len(all), flows)
	if len(all) < 4 {
		t.Errorf("%d source ports", len(all))
	}
	for _, p := range ports {
		if int(p) == tx.LocalPort() {
			t.Error("a datagram left from the receive socket's port")
		}
	}
}

// A run of datagrams is for one send socket: another flow ends it, the same flow's
// datagrams are sent together.
func TestTxBatchRunsBreakOnASocketChange(t *testing.T) {
	f := newFixture(t, OverlayConfig{Offload: true}, 1)
	skipWithoutSegmentation(t, f.oe)
	// Find two flows on different sockets, and one more on the first one's.
	n := len(f.oe.socks.Load().tx)
	sockOf := func(flow int) int { return flowSocket(packet.EtherTypeIPv4, flowFrame(flow, 0).Payload, n) }
	a, b, a2 := 0, 1, 1
	for ; sockOf(b) == sockOf(a); b++ {
		if b > 1000 {
			t.Fatal("no two flows on different sockets")
		}
	}
	for ; sockOf(a2) != sockOf(a); a2++ {
		if a2 > 1000 {
			t.Fatal("no second flow on the first one's socket")
		}
	}
	frame := func(flow int, tag byte) *packet.EthernetFrame {
		fr := flowFrame(flow, uint32(tag))
		fr.Payload = append(fr.Payload, make([]byte, 1400)...) // all of one size
		return fr
	}

	for _, tc := range []struct {
		name  string
		flows []int
		sends uint64
	}{
		{"one flow", []int{a, a, a, a}, 1},
		{"two flows on one socket", []int{a, a2, a, a2}, 1},
		{"two flows on two sockets, in turn", []int{a, a, b, b}, 2},
		{"two flows on two sockets, alternating", []int{a, b, a, b}, 4},
		{"back to the first", []int{a, a, b, b, a, a}, 3},
	} {
		sends0 := f.oe.Stats().TxSends
		tb := f.oe.NewBatch()
		for i, flow := range tc.flows {
			if err := tb.Send(frame(flow, byte(i)), f.dst(0)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tb.Flush(); err != nil {
			t.Fatal(err)
		}
		tb.Release()
		if got := f.oe.Stats().TxSends - sends0; got != tc.sends {
			t.Errorf("%s: %d sends, want %d", tc.name, got, tc.sends)
		}
		// Every datagram arrives, from the port of its flow's socket.
		sink := f.sinks[0]
		buf := make([]byte, 70000)
		for i, flow := range tc.flows {
			_ = sink.SetReadDeadline(time.Now().Add(3 * time.Second))
			m, from, err := sink.ReadFromUDPAddrPort(buf)
			if err != nil {
				t.Fatalf("%s: datagram %d: %v", tc.name, i, err)
			}
			vx, _ := packet.ParseVXLAN(buf[:m])
			if got := int(binary.BigEndian.Uint16(vx.InnerFrame.Payload[28:])); got != flow {
				t.Errorf("%s: datagram %d is of flow %d, want %d: order was lost", tc.name, i, got, flow)
			}
			want := f.oe.socks.Load().tx[sockOf(flow)].LocalAddr().(*net.UDPAddr).Port
			if int(from.Port()) != want {
				t.Errorf("%s: datagram %d left from port %d, want its socket's %d", tc.name, i, from.Port(), want)
			}
		}
	}
}

func TestListenSocketsRefusesWhatIsOutOfRange(t *testing.T) {
	for _, n := range []int{-1, 0, MaxSockets + 1} {
		if rx, tx, err := ListenSockets("udp4", loopback, 0, n); err == nil {
			t.Errorf("%d sockets: opened %d and %d", n, len(rx), len(tx))
		}
	}
	// A port that is taken leaves nothing open.
	taken := plainSocket(t)
	before := openFDs(t)
	if _, _, err := ListenSockets("udp4", loopback, taken.LocalAddr().(*net.UDPAddr).Port, 4); err == nil {
		t.Error("a port that another socket (outside the group) holds was opened")
	}
	if after := openFDs(t); after != before {
		t.Errorf("%d descriptors before a failed ListenSockets, %d after", before, after)
	}
}

// openFDs counts this process's open descriptors by probing the range.
func openFDs(t *testing.T) int {
	t.Helper()
	n := 0
	for fd := 0; fd < 4096; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			n++
		}
	}
	return n
}
