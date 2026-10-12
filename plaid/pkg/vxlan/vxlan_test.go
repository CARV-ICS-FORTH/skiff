package vxlan

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"plaid/pkg/packet"
)

type mockInjector struct {
	mu     sync.Mutex
	frames []*packet.EthernetFrame
	notify chan struct{}
}

func newMockInjector() *mockInjector {
	return &mockInjector{
		notify: make(chan struct{}, 10),
	}
}

func (m *mockInjector) InjectFrame(frame *packet.EthernetFrame) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.frames = append(m.frames, frame)
	select {
	case m.notify <- struct{}{}:
	default:
	}
	return nil
}

func (m *mockInjector) LastFrame() *packet.EthernetFrame {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.frames) == 0 {
		return nil
	}
	return m.frames[len(m.frames)-1]
}

// Send encapsulates one frame into a VXLAN packet and sends it to the remote node, through
// a batch of its own: what the tests that send one frame at a time use.
func (oe *OverlayEngine) Send(frame *packet.EthernetFrame, dstIP net.IP) error {
	tb := oe.NewBatch()
	defer tb.Release()
	if err := tb.Send(frame, dstIP); err != nil {
		return err
	}
	return tb.Flush()
}

func TestRouteTable(t *testing.T) {
	rt := NewRouteTable()

	_, sn1, _ := net.ParseCIDR("10.244.2.0/24")
	_, sn2, _ := net.ParseCIDR("10.244.3.0/24")

	vtepMAC, _ := net.ParseMAC("02:00:00:00:02:01")

	rt.AddRoute(&RouteEntry{
		Subnet:       sn1,
		RemoteHostIP: net.ParseIP("192.168.1.10"),
		VtepMAC:      vtepMAC,
		VNI:          1,
		Port:         8472,
	})

	rt.AddRoute(&RouteEntry{
		Subnet:       sn2,
		RemoteHostIP: net.ParseIP("192.168.1.11"),
		VNI:          1,
	})

	r, err := rt.Lookup(net.ParseIP("10.244.2.45"))
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if !r.RemoteHostIP.Equal(net.ParseIP("192.168.1.10")) {
		t.Errorf("RemoteHostIP mismatch: %s", r.RemoteHostIP)
	}

	r2, err := rt.Lookup(net.ParseIP("10.244.3.100"))
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if !r2.RemoteHostIP.Equal(net.ParseIP("192.168.1.11")) {
		t.Errorf("RemoteHostIP mismatch")
	}

	_, err = rt.Lookup(net.ParseIP("10.244.99.1"))
	if err == nil {
		t.Errorf("Expected route lookup error for unmapped IP")
	}

	deleted := rt.RemoveRoute("10.244.2.0/24")
	if !deleted {
		t.Errorf("Expected route to be deleted")
	}
	_, err = rt.Lookup(net.ParseIP("10.244.2.45"))
	if err == nil {
		t.Errorf("Expected lookup error after deletion")
	}
}

func TestOverlayEngineEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Node B setup (port 0 selects random free port)
	injB := newMockInjector()
	engineB := NewOverlayEngine(OverlayConfig{BindAddress: "127.0.0.1", Port: 0, DefaultVNI: 1}, NewRouteTable(), injB)
	if err := engineB.Start(ctx); err != nil {
		t.Fatalf("Failed to start engine B: %v", err)
	}
	defer engineB.Close()

	// Node A setup, with a route to Node B's subnet 10.244.2.0/24
	engineA := NewOverlayEngine(OverlayConfig{BindAddress: "127.0.0.1", Port: 0, DefaultVNI: 1}, routeTo(engineB.LocalPort()), newMockInjector())
	if err := engineA.Start(ctx); err != nil {
		t.Fatalf("Failed to start engine A: %v", err)
	}
	defer engineA.Close()

	// Construct inner Ethernet frame on Node A
	srcMAC, _ := net.ParseMAC("02:00:00:00:01:05")
	dstMAC, _ := net.ParseMAC("02:00:00:00:02:10")
	innerFrame := &packet.EthernetFrame{
		DstMAC:    dstMAC,
		SrcMAC:    srcMAC,
		EtherType: packet.EtherTypeIPv4,
		Payload:   []byte("cross-node overlay test payload"),
	}

	// Send overlay frame from A to an IP in Node B's subnet
	targetIP := net.ParseIP("10.244.2.10")
	if err := engineA.Send(innerFrame, targetIP); err != nil {
		t.Fatalf("engineA.Send failed: %v", err)
	}

	// Wait for Node B to receive and decapsulate
	select {
	case <-injB.notify:
	case <-time.After(2 * time.Second):
		t.Fatalf("Timed out waiting for Node B to receive VXLAN packet")
	}

	receivedFrame := injB.LastFrame()
	if receivedFrame == nil {
		t.Fatalf("Node B received nil frame")
	}

	if !bytes.Equal(receivedFrame.SrcMAC, srcMAC) {
		t.Errorf("SrcMAC mismatch: %s vs %s", receivedFrame.SrcMAC, srcMAC)
	}
	if !bytes.Equal(receivedFrame.Payload, innerFrame.Payload) {
		t.Errorf("Payload mismatch: %s vs %s", receivedFrame.Payload, innerFrame.Payload)
	}
}

// routeTo is the route table of an engine whose only route, 10.244.2.0/24, leads to the
// overlay socket on the loopback address at port.
func routeTo(port int) *RouteTable {
	rt := NewRouteTable()
	_, subnet, _ := net.ParseCIDR("10.244.2.0/24")
	rt.AddRoute(&RouteEntry{Subnet: subnet, RemoteHostIP: loopback, VNI: 1, Port: port})
	return rt
}

// fixture is an engine whose routes lead to loopback sockets the test owns, one per route:
// 10.244.N.0/24 goes to sinks[N-2], with the VNI 7 and the VTEP MAC 02:00:00:00:00:02. The
// engine binds to 127.0.0.1 unless cfg says otherwise.
type fixture struct {
	oe    *OverlayEngine
	sinks []*net.UDPConn
	now   atomic.Int64 // the engine's clock, which a test moves; it starts at an hour
}

// fixtureVTEP is the VTEP MAC address of the routes of a fixture.
var fixtureVTEP = net.HardwareAddr{2, 0, 0, 0, 0, 2}

// peerAddr is the address of the fixture's sinks, and so of the node that "announces".
var peerAddr = netip.MustParseAddr("127.0.0.1")

func newFixture(tb testing.TB, cfg OverlayConfig, sinks int) *fixture {
	tb.Helper()
	routes := NewRouteTable()
	f := &fixture{}
	f.now.Store(int64(time.Hour))
	for i := 0; i < sinks; i++ {
		sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
		if err != nil {
			tb.Skipf("cannot open a loopback UDP socket: %v", err)
		}
		_ = sink.SetReadBuffer(8 << 20)
		tb.Cleanup(func() { sink.Close() })
		f.sinks = append(f.sinks, sink)
		_, subnet, _ := net.ParseCIDR(fmt.Sprintf("10.244.%d.0/24", i+2))
		routes.AddRoute(&RouteEntry{Subnet: subnet, RemoteHostIP: loopback, VtepMAC: fixtureVTEP, VNI: 7, Port: sink.LocalAddr().(*net.UDPAddr).Port})
	}
	if cfg.BindAddress == "" {
		cfg.BindAddress = "127.0.0.1"
	}
	f.oe = NewOverlayEngine(cfg, routes, nil)
	f.oe.clock = f.now.Load
	if err := f.oe.Start(context.Background()); err != nil {
		tb.Fatalf("Failed to start engine: %v", err)
	}
	tb.Cleanup(func() { f.oe.Close() })
	return f
}

// dst is an address behind sinks[sink].
func (f *fixture) dst(sink int) net.IP { return net.IPv4(10, 244, byte(sink+2), 10) }

// receiveFrom reads n datagrams from sinks[sink] and the ports they came from.
func (f *fixture) receiveFrom(t *testing.T, sink, n int) (out [][]byte, ports []int) {
	t.Helper()
	buf := make([]byte, 70000)
	for len(out) < n {
		_ = f.sinks[sink].SetReadDeadline(time.Now().Add(3 * time.Second))
		m, from, err := f.sinks[sink].ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("sink %d: got %d of %d datagrams: %v", sink, len(out), n, err)
		}
		out = append(out, append([]byte(nil), buf[:m]...))
		ports = append(ports, from.Port)
	}
	return out, ports
}

// receive reads n datagrams from sinks[sink].
func (f *fixture) receive(t *testing.T, sink, n int) [][]byte {
	t.Helper()
	out, _ := f.receiveFrom(t, sink, n)
	return out
}

// nothing checks that no datagram arrives at sinks[sink].
func (f *fixture) nothing(t *testing.T, sink int) {
	t.Helper()
	_ = f.sinks[sink].SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if m, _, err := f.sinks[sink].ReadFromUDP(make([]byte, 70000)); err == nil {
		t.Fatalf("a datagram of %d bytes arrived", m)
	}
}

// announce makes the sinks' node a peer, as a datagram from it with the mark does.
func (f *fixture) announce() { f.oe.peers.note(peerAddr, f.now.Load()) }

// ethernet is the frame in wire, whose payload shares its bytes.
func ethernet(wire []byte) *packet.EthernetFrame {
	f, err := packet.ParseEthernet(wire)
	if err != nil {
		panic(err)
	}
	return f
}

func BenchmarkSend(b *testing.B) {
	for _, size := range []int{1400, 8900} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			engine := newFixture(b, OverlayConfig{}, 1).oe
			srcMAC, _ := net.ParseMAC("02:00:00:00:01:05")
			dstMAC, _ := net.ParseMAC("02:00:00:00:02:10")
			frame := &packet.EthernetFrame{
				DstMAC:    dstMAC,
				SrcMAC:    srcMAC,
				EtherType: packet.EtherTypeIPv4,
				Payload:   make([]byte, size),
			}
			target := net.ParseIP("10.244.2.10")

			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := engine.Send(frame, target); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// referenceDatagram is the datagram Send must produce, laid out byte by byte: the
// VXLAN header, then the inner frame addressed to the route's VTEP.
func referenceDatagram(vni uint32, dst, src net.HardwareAddr, payload []byte) []byte {
	out := []byte{0x08, 0, 0, 0, byte(vni >> 16), byte(vni >> 8), byte(vni), 0}
	out = append(out, dst...)
	out = append(out, src...)
	out = append(out, 0x08, 0x00)
	return append(out, payload...)
}

func testFrame(size int) *packet.EthernetFrame {
	srcMAC, _ := net.ParseMAC("02:00:00:00:01:05")
	dstMAC, _ := net.ParseMAC("02:00:00:00:02:10")
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*13 + 5)
	}
	return &packet.EthernetFrame{DstMAC: dstMAC, SrcMAC: srcMAC, EtherType: packet.EtherTypeIPv4, Payload: payload}
}

func TestSendDatagramEncoding(t *testing.T) {
	f := newFixture(t, OverlayConfig{}, 1)
	engine, sink := f.oe, f.sinks[0]
	vtepMAC, _ := net.ParseMAC("02:00:00:00:00:02")
	buf := make([]byte, 65535)

	for i, size := range []int{0, 1400, 8900} {
		frame := testFrame(size)
		want := referenceDatagram(7, vtepMAC, frame.SrcMAC, frame.Payload)

		if err := engine.Send(frame, net.ParseIP("10.244.2.10")); err != nil {
			t.Fatalf("Send(%d) failed: %v", size, err)
		}
		sink.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := sink.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("no datagram for payload %d: %v", size, err)
		}
		if !bytes.Equal(buf[:n], want) {
			t.Errorf("payload %d: datagram differs from the reference encoding (got %d bytes, want %d)", size, n, len(want))
		}
		if got := engine.Stats().TxPackets; got != uint64(i+1) {
			t.Errorf("TxPackets = %d after %d sends", got, i+1)
		}
	}
	if got := engine.Stats().TxErrors; got != 0 {
		t.Errorf("TxErrors = %d, want 0", got)
	}
}

func TestSendErrorsAreCounted(t *testing.T) {
	engine := newFixture(t, OverlayConfig{}, 1).oe
	_, badIP, _ := net.ParseCIDR("10.244.3.0/24")
	engine.routes.AddRoute(&RouteEntry{Subnet: badIP, RemoteHostIP: net.IP{1, 2, 3}, VNI: 1, Port: 8472})
	_, badPort, _ := net.ParseCIDR("10.244.4.0/24")
	engine.routes.AddRoute(&RouteEntry{Subnet: badPort, RemoteHostIP: loopback, VNI: 1, Port: 70000})
	_, noMAC, _ := net.ParseCIDR("10.244.5.0/24")
	engine.routes.AddRoute(&RouteEntry{Subnet: noMAC, RemoteHostIP: loopback, VNI: 1, Port: 8472})

	badFrame := testFrame(10)
	badFrame.SrcMAC = badFrame.SrcMAC[:4]

	for i, tc := range []struct {
		name  string
		frame *packet.EthernetFrame
		dst   string
	}{
		{"no route", testFrame(10), "10.9.9.9"},
		{"bad remote address", testFrame(10), "10.244.3.1"},
		{"bad remote port", testFrame(10), "10.244.4.1"},
		{"bad inner MAC", badFrame, "10.244.5.1"},
	} {
		if err := engine.Send(tc.frame, net.ParseIP(tc.dst)); err == nil {
			t.Errorf("%s: Send succeeded", tc.name)
		}
		if got := engine.Stats().TxErrors; got != uint64(i+1) {
			t.Errorf("%s: TxErrors = %d, want %d", tc.name, got, i+1)
		}
	}
	if got := engine.Stats().TxPackets; got != 0 {
		t.Errorf("TxPackets = %d, want 0", got)
	}
}

// TestSendDoesNotAllocatePerByte checks that Send allocates nothing that grows with
// the frame: it encodes into a pooled buffer and writes from it.
func TestSendDoesNotAllocatePerByte(t *testing.T) {
	engine := newFixture(t, OverlayConfig{}, 1).oe
	target := net.ParseIP("10.244.2.10")

	measure := func(size int) (allocs float64, bytesPerOp uint64) {
		frame := testFrame(size)
		send := func() {
			if err := engine.Send(frame, target); err != nil {
				t.Fatal(err)
			}
		}
		allocs = testing.AllocsPerRun(100, send)

		const runs = 5000
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for i := 0; i < runs; i++ {
			send()
		}
		runtime.ReadMemStats(&after)
		return allocs, (after.TotalAlloc - before.TotalAlloc) / runs
	}

	smallAllocs, smallBytes := measure(100)
	bigAllocs, bigBytes := measure(8900)
	t.Logf("100-byte payload: %v allocs, %d B per Send; 8900-byte payload: %v allocs, %d B per Send",
		smallAllocs, smallBytes, bigAllocs, bigBytes)

	if smallAllocs != bigAllocs || bigAllocs > 2 {
		t.Errorf("allocs per Send: %v for 100 bytes, %v for 8900, want equal and at most 2", smallAllocs, bigAllocs)
	}
	// A copy of the frame would add 8800 bytes or more. The race detector makes the
	// pool drop buffers at random, which costs the same for both sizes but is noisy.
	if bigBytes > smallBytes+4096 {
		t.Errorf("bytes per Send grow with the frame: %d for 100 bytes, %d for 8900", smallBytes, bigBytes)
	}
}
