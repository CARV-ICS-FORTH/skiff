package bridge

import (
	"bytes"
	"net"
	"sync"
	"testing"

	"plaid/pkg/packet"
	"plaid/pkg/packet/packettest"
	"plaid/pkg/svc"
)

// superEndpoint is a mock endpoint that takes frames in batches and, unless plain, TCP
// super-frames whole; it records both.
type superEndpoint struct {
	*mockEndpoint
	plain  bool // its writer takes no super-frames
	mu     sync.Mutex
	adds   [][]byte
	supers []superRecord
}

type superRecord struct {
	h     packet.VnetHeader
	frame []byte
}

func newSuperEndpoint(id, ip, mac string) *superEndpoint {
	return &superEndpoint{mockEndpoint: newMockEndpoint(id, id, ip, mac)}
}

type addWriter struct{ e *superEndpoint }

func (w addWriter) Add(f *packet.EthernetFrame) error {
	b, _ := f.Marshal()
	w.e.mu.Lock()
	w.e.adds = append(w.e.adds, b)
	w.e.mu.Unlock()
	return nil
}
func (addWriter) Flush() error { return nil }
func (addWriter) Release()     {}

type superWriter struct{ addWriter }

func (w superWriter) AddSuper(h packet.VnetHeader, f *packet.EthernetFrame) error {
	b, _ := f.Marshal() // a copy: the payload is the caller's read buffer
	w.e.mu.Lock()
	w.e.supers = append(w.e.supers, superRecord{h, b})
	w.e.mu.Unlock()
	return nil
}

func (e *superEndpoint) NewWriter() FrameWriter {
	if e.plain {
		return addWriter{e}
	}
	return superWriter{addWriter{e}}
}

// untouched reports whether nothing was delivered to the endpoint.
func (e *superEndpoint) untouched() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.adds) == 0 && len(e.supers) == 0 && e.ReceivedCount() == 0
}

const superSegment = 1000

// tsoFrame builds a TCP super-frame of three segments from srcIP to dstIP as a pod's
// stack hands it over, with its virtio header, and the frame as raw bytes.
func tsoFrame(srcMAC, dstMAC net.HardwareAddr, srcIP, dstIP string) (packet.VnetHeader, []byte) {
	ip := packettest.TCP(srcIP, 4000, dstIP, 80, packet.TCPAck, string(bytes.Repeat([]byte{7}, 3*superSegment)))
	f := &packet.EthernetFrame{SrcMAC: srcMAC, DstMAC: dstMAC, EtherType: packet.EtherTypeIPv4, Payload: ip}
	raw, _ := f.Marshal()
	return packet.VnetHeader{
		Flags: packet.VnetNeedsCsum, GSOType: packet.VnetGSOTCPv4, HdrLen: 54, GSOSize: superSegment,
		CsumStart: 34, CsumOffset: 16,
	}, raw
}

// superFixture is a bridge in tap mode with a pod that sends and takes super-frames, a
// pod that takes them, a pod that does not, the bubble, a Service, and an overlay.
type superFixture struct {
	b       *Bridge
	eng     *svc.Engine
	pod     *superEndpoint // the sender
	peer    *superEndpoint
	plain   *superEndpoint // takes frames in batches but no super-frames
	nowrite *mockEndpoint  // takes no batches
	bubble  *superEndpoint
	overlay int
	sent    [][]byte // the frames sent over the overlay
}

func newSuperFixture(t *testing.T) *superFixture {
	t.Helper()
	f := &superFixture{
		pod:     newSuperEndpoint("p1", "10.244.1.4", "02:00:00:00:01:04"),
		peer:    newSuperEndpoint("p2", "10.244.1.5", "02:00:00:00:01:05"),
		plain:   newSuperEndpoint("p3", "10.244.1.6", "02:00:00:00:01:06"),
		nowrite: newMockEndpoint("p4", "p4", "10.244.1.7", "02:00:00:00:01:07"),
		bubble:  newSuperEndpoint("bubble", "10.244.1.1", "02:00:00:00:00:02"),
	}
	f.plain.plain = true
	_, node, _ := net.ParseCIDR("10.244.1.0/24")
	_, cluster, _ := net.ParseCIDR("10.244.0.0/16")
	router, _ := net.ParseMAC("02:00:00:00:00:01")
	f.b = NewBridge(BridgeConfig{GatewayMode: "tap", GatewayIP: net.ParseIP("10.244.1.1"), GatewayMAC: router, NodeCIDR: node, ClusterCIDR: cluster})
	for _, ep := range []Endpoint{f.pod, f.peer, f.plain, f.nowrite} {
		if err := f.b.AddEndpoint(ep); err != nil {
			t.Fatal(err)
		}
	}
	f.b.SetBubbleEndpoint(f.bubble)
	f.b.SetRouteChecker(func(ip net.IP) bool { return net.ParseIP("10.244.2.4").Equal(ip) })
	setOverlayFunc(f.b, func(fr *packet.EthernetFrame, _ net.IP) error {
		f.overlay++
		c := *fr
		c.DstMAC = make(net.HardwareAddr, 6) // the overlay sets it
		b, _ := c.Marshal()
		f.sent = append(f.sent, b)
		return nil
	})
	_, svcCIDR, _ := net.ParseCIDR("10.43.0.0/16")
	eng, err := svc.New(svc.Config{ServiceCIDR: svcCIDR, SNATAddr: net.ParseIP(snatIP)})
	if err != nil {
		t.Fatal(err)
	}
	eng.SetServices([]svc.Service{web(backend("10.244.1.5", 8080))})
	f.b.SetServiceEngine(eng)
	f.eng = eng
	return f
}

// A super-frame to another pod on the node, which the pod sends to the gateway's MAC as
// to any address outside its subnet, is delivered in one piece with the header it had, and
// with the MAC addresses the bridge sets.
func TestSuperFrameGoesWholeToAPod(t *testing.T) {
	f := newSuperFixture(t)
	h, raw := tsoFrame(f.pod.MAC(), f.b.gatewayMAC, "10.244.1.4", "10.244.1.5")
	h.GSOType |= packet.VnetGSOECN // also with the ECN bit
	if !f.b.PassesWhole(f.pod, raw) || !f.peer.untouched() {
		t.Fatalf("PassesWhole is false, or it delivered: %+v", f.peer.supers)
	}
	whole, err := f.b.ProcessSuperFrame(f.pod, h, raw)
	if !whole || err != nil {
		t.Fatalf("whole=%v, %v", whole, err)
	}
	if len(f.peer.supers) != 1 || len(f.peer.adds) != 0 || f.peer.ReceivedCount() != 0 {
		t.Fatalf("the pod got %d super-frames, %d added frames, %d writes", len(f.peer.supers), len(f.peer.adds), f.peer.ReceivedCount())
	}
	got := f.peer.supers[0]
	if got.h != h {
		t.Errorf("header %+v, want %+v", got.h, h)
	}
	want := append([]byte(nil), raw...)
	copy(want[0:6], f.peer.MAC())
	if !bytes.Equal(got.frame, want) {
		t.Error("the frame is not the one sent with the destination MAC of the pod")
	}
	if !f.bubble.untouched() || f.overlay != 0 {
		t.Error("something else was delivered")
	}
}

func TestSuperFrameGoesWholeToTheBubble(t *testing.T) {
	f := newSuperFixture(t)
	for _, dst := range []string{"8.8.8.8", "10.244.1.1"} { // outside, and the gateway's address
		f.bubble.supers = nil
		h, raw := tsoFrame(f.pod.MAC(), f.b.gatewayMAC, "10.244.1.4", dst)
		if whole, err := f.b.ProcessSuperFrame(f.pod, h, raw); !whole || err != nil {
			t.Fatalf("%s: whole=%v, %v", dst, whole, err)
		}
		if len(f.bubble.supers) != 1 || f.bubble.supers[0].h != h {
			t.Fatalf("%s: the bubble got %+v", dst, f.bubble.supers)
		}
		got, _ := packet.ParseEthernet(f.bubble.supers[0].frame)
		if !bytes.Equal(got.DstMAC, f.bubble.MAC()) || !bytes.Equal(got.SrcMAC, f.pod.MAC()) {
			t.Errorf("%s: MACs %s -> %s", dst, got.SrcMAC, got.DstMAC)
		}
	}
}

// Without tap mode a frame addressed to a pod's MAC, or to a pod's IP, goes to it as it is.
func TestSuperFrameGoesWholeInFDBMode(t *testing.T) {
	b, _, _ := setupTestBridge()
	pod := newSuperEndpoint("s1", "10.244.1.8", "02:00:00:00:00:08")
	peer := newSuperEndpoint("s2", "10.244.1.9", "02:00:00:00:00:09")
	_ = b.AddEndpoint(pod)
	_ = b.AddEndpoint(peer)
	h, raw := tsoFrame(pod.MAC(), peer.MAC(), "10.244.1.8", "10.244.1.9")
	if whole, err := b.ProcessSuperFrame(pod, h, raw); !whole || err != nil {
		t.Fatalf("whole=%v, %v", whole, err)
	}
	unknown := net.HardwareAddr{2, 0, 0, 0, 0xee, 1} // an unknown MAC with a known IP: by IP
	h, raw2 := tsoFrame(pod.MAC(), unknown, "10.244.1.8", "10.244.1.9")
	if whole, err := b.ProcessSuperFrame(pod, h, raw2); !whole || err != nil {
		t.Fatalf("by IP: whole=%v, %v", whole, err)
	}
	if len(peer.supers) != 2 || !bytes.Equal(peer.supers[0].frame, raw) || !bytes.Equal(peer.supers[1].frame, raw2) {
		t.Fatalf("the pod got %d super-frames, or not as sent", len(peer.supers))
	}
}

// Whatever the routing does with a super-frame other than hand it to one endpoint that
// takes it whole, or to the Service engine (see service_super_test.go), is left to the segments: nothing is done with the frame, and the segments
// then take the path they always took.
func TestSuperFrameIsLeftToTheSegmentsWhenItCannotGoWhole(t *testing.T) {
	cases := []struct {
		name  string
		dstIP string
		mac   func(f *superFixture) net.HardwareAddr
		after func(t *testing.T, f *superFixture) // after the segments went through
	}{
		{"a remote pod", "10.244.2.4", nil, func(t *testing.T, f *superFixture) {
			if f.overlay != 3 {
				t.Errorf("%d segments went over the overlay, want 3", f.overlay)
			}
		}},
		{"a pod whose writer takes no super-frames", "10.244.1.6", nil, func(t *testing.T, f *superFixture) {
			if len(f.plain.adds) != 3 || len(f.plain.supers) != 0 {
				t.Errorf("%d frames added, %d super-frames", len(f.plain.adds), len(f.plain.supers))
			}
		}},
		{"a pod without a writer", "10.244.1.7", nil, func(t *testing.T, f *superFixture) {
			if f.nowrite.ReceivedCount() != 3 {
				t.Errorf("%d frames written, want 3", f.nowrite.ReceivedCount())
			}
		}},
		{"the sender", "10.244.1.4", nil, func(t *testing.T, f *superFixture) {
			if !f.pod.untouched() {
				t.Error("a frame came back to the sender")
			}
		}},
		{"an unused address of the node", "10.244.1.9", nil, nil},
		{"a broadcast address", "10.244.1.5", func(*superFixture) net.HardwareAddr { return net.HardwareAddr{255, 255, 255, 255, 255, 255} }, nil},
		{"a multicast address", "10.244.1.5", func(*superFixture) net.HardwareAddr { return net.HardwareAddr{1, 0, 0x5e, 0, 0, 1} }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuperFixture(t)
			dstMAC := f.b.gatewayMAC
			if tc.mac != nil {
				dstMAC = tc.mac(f)
			}
			h, raw := tsoFrame(f.pod.MAC(), dstMAC, "10.244.1.4", tc.dstIP)
			before := append([]byte(nil), raw...)
			if f.b.PassesWhole(f.pod, raw) {
				t.Fatal("PassesWhole is true")
			}
			if whole, err := f.b.ProcessSuperFrame(f.pod, h, raw); whole || err != nil {
				t.Fatalf("whole=%v, %v", whole, err)
			}
			flows := f.eng.Stats()
			if f.overlay != 0 || flows.DropsNoFlow != 0 || flows.Forwarded != 0 || !bytes.Equal(raw, before) {
				t.Fatalf("something was done with the frame: %d overlay sends, engine %+v", f.overlay, flows)
			}
			for _, ep := range []*superEndpoint{f.peer, f.plain, f.bubble} {
				if !ep.untouched() {
					t.Fatalf("%s got something", ep.Name())
				}
			}
			if f.nowrite.ReceivedCount() != 0 {
				t.Fatal("a pod got something")
			}

			// The caller cuts it and sends the segments on.
			var sg packet.Segmenter
			segs, err := sg.Segment(h, raw)
			if err != nil || len(segs) != 3 {
				t.Fatalf("%d segments, %v", len(segs), err)
			}
			_ = f.b.ProcessTrustedFrames(f.pod, segs)
			if tc.after != nil {
				tc.after(t, f)
			}
		})
	}
}

// The same decisions without tap mode: a frame for an unknown MAC and IP is flooded, one
// for a slirp MAC or the gateway's MAC goes to the outbound handler, and a remote pod's
// goes over the overlay.
func TestSuperFrameIsLeftToTheSegmentsInFDBMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mac      net.HardwareAddr
		dstIP    string
		outbound bool
	}{
		{"flood", net.HardwareAddr{2, 0, 0, 0, 0xee, 1}, "10.244.1.99", false},
		{"slirp", net.HardwareAddr{0x52, 0x55, 10, 0, 2, 2}, "10.0.2.2", true},
		{"gateway", net.HardwareAddr{2, 0, 0, 0, 0, 1}, "1.1.1.1", true},
		{"overlay", net.HardwareAddr{2, 0, 0, 0, 0xee, 1}, "10.244.2.3", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, _ := setupTestBridge()
			pod := newSuperEndpoint("s1", "10.244.1.8", "02:00:00:00:00:08")
			peer := newSuperEndpoint("s2", "10.244.1.9", "02:00:00:00:00:09")
			_ = b.AddEndpoint(pod)
			_ = b.AddEndpoint(peer)
			var outbound, overlay int
			if tc.outbound {
				b.SetOutboundHandler(func(*packet.EthernetFrame) error { outbound++; return nil })
			}
			setOverlayFunc(b, func(*packet.EthernetFrame, net.IP) error { overlay++; return nil })
			h, raw := tsoFrame(pod.MAC(), tc.mac, "10.244.1.8", tc.dstIP)
			if whole, err := b.ProcessSuperFrame(pod, h, raw); whole || err != nil {
				t.Fatalf("whole=%v, %v", whole, err)
			}
			if outbound != 0 || overlay != 0 || !peer.untouched() || !pod.untouched() {
				t.Fatalf("something was done: outbound %d, overlay %d", outbound, overlay)
			}
			var sg packet.Segmenter
			segs, _ := sg.Segment(h, raw)
			_ = b.ProcessTrustedFrames(pod, segs)
			switch tc.name {
			case "flood":
				if peer.ReceivedCount() != 3 {
					t.Errorf("%d segments flooded to the other pod", peer.ReceivedCount())
				}
			case "slirp", "gateway":
				if outbound != 3 {
					t.Errorf("%d segments to the outbound handler", outbound)
				}
			case "overlay":
				if overlay != 3 {
					t.Errorf("%d segments to the overlay", overlay)
				}
			}
		})
	}
}

// The filter sees a super-frame once, and a frame it drops is dropped whole. A frame that
// is only looked at (PassesWhole) is not filtered, and nothing is learnt from it.
func TestSuperFrameIsFilteredAndLearntOnce(t *testing.T) {
	f := newSuperFixture(t)
	var calls int
	var seen *packet.EthernetFrame
	accept := true
	f.b.SetFilterHandler(func(fr *packet.EthernetFrame, _ Endpoint) bool {
		calls++
		seen = fr
		return accept
	})
	stranger := net.HardwareAddr{2, 0, 0, 0, 0xaa, 1} // a MAC behind the pod that the bridge has not seen
	h, raw := tsoFrame(stranger, f.b.gatewayMAC, "10.244.1.4", "10.244.1.5")

	if !f.b.PassesWhole(f.pod, raw) || calls != 0 || f.b.fdb.Lookup(stranger) != nil {
		t.Fatalf("PassesWhole filtered (%d calls) or learnt", calls)
	}
	if whole, _ := f.b.ProcessSuperFrame(f.pod, h, raw); !whole || calls != 1 || len(f.peer.supers) != 1 {
		t.Fatalf("whole=%v, %d filter calls, %d super-frames delivered", whole, calls, len(f.peer.supers))
	}
	if len(seen.Payload) != len(raw)-packet.EthernetHeaderLen {
		t.Errorf("the filter saw %d bytes, not the whole frame", len(seen.Payload))
	}
	if f.b.fdb.Lookup(stranger) != Endpoint(f.pod) {
		t.Error("the source MAC was not learnt")
	}

	accept, calls = false, 0
	if whole, err := f.b.ProcessSuperFrame(f.pod, h, raw); !whole || err != nil || calls != 1 || len(f.peer.supers) != 1 {
		t.Errorf("a dropped frame: whole=%v, %v, %d filter calls, %d super-frames delivered", whole, err, calls, len(f.peer.supers))
	}
}

// Handling a super-frame costs no more allocations than handling a frame, however many
// segments it stands for.
func TestSuperFrameAllocations(t *testing.T) {
	f := newSuperFixture(t)
	h, raw := tsoFrame(f.pod.MAC(), f.b.gatewayMAC, "10.244.1.4", "10.244.1.5")
	frame := packettest.TCP("10.244.1.4", 4000, "10.244.1.5", 80, packet.TCPAck, "x")
	single := &packet.EthernetFrame{SrcMAC: f.pod.MAC(), DstMAC: f.b.gatewayMAC, EtherType: packet.EtherTypeIPv4, Payload: frame}
	rawSingle, _ := single.Marshal()
	f.peer.mockEndpoint.packets = nil
	one := testing.AllocsPerRun(50, func() {
		_ = f.b.ProcessFrame(f.pod, rawSingle)
		f.peer.adds = f.peer.adds[:0]
	})
	whole := testing.AllocsPerRun(50, func() {
		_ = f.b.PassesWhole(f.pod, raw)
		_, _ = f.b.ProcessSuperFrame(f.pod, h, raw)
		f.peer.supers = f.peer.supers[:0]
	})
	t.Logf("allocations: one frame %.0f, one super-frame of 3 segments, asked and delivered, %.0f", one, whole)
	if whole > 2*one {
		t.Errorf("a super-frame costs %.0f allocations, a frame %.0f", whole, one)
	}
}
