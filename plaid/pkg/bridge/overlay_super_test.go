package bridge

import (
	"bytes"
	"errors"
	"net"
	"reflect"
	"testing"

	"plaid/pkg/packet"
	"plaid/pkg/packet/packettest"
	"plaid/pkg/svc"
)

// superOverlay is an overlay batch that takes super-frames whole, from the nodes it
// says accept them, and records what it is given.
type superOverlay struct {
	accept  bool  // AcceptsSuper
	refuse  bool  // SendSuper sends nothing and says so
	failErr error // SendSuper sends, and fails
	asks    int
	sends   [][]byte // the frames sent one by one, with the MAC addresses they had
	supers  []overlaySuper
	flushes int
}

type overlaySuper struct {
	h     packet.VnetHeader
	frame []byte
	dst   string
}

// wire encodes a frame as the overlay would send it, with a zero destination MAC where the
// bridge has set none (the overlay does).
func wire(f *packet.EthernetFrame) []byte {
	c := *f
	if len(c.DstMAC) != 6 {
		c.DstMAC = make(net.HardwareAddr, 6)
	}
	b, _ := c.Marshal()
	return b
}

func (o *superOverlay) Send(f *packet.EthernetFrame, _ net.IP) error {
	o.sends = append(o.sends, wire(f))
	return nil
}
func (o *superOverlay) Flush() error { o.flushes++; return nil }
func (o *superOverlay) Release()     {}

func (o *superOverlay) AcceptsSuper(net.IP) bool {
	o.asks++
	return o.accept
}

func (o *superOverlay) SendSuper(h packet.VnetHeader, f *packet.EthernetFrame, dst net.IP) (bool, error) {
	if !o.accept || o.refuse {
		return false, nil
	}
	o.supers = append(o.supers, overlaySuper{h, wire(f), dst.String()})
	return true, o.failErr
}

// plainOverlay hides the methods for super-frames: an overlay that has none.
type plainOverlay struct{ o *superOverlay }

func (p plainOverlay) Send(f *packet.EthernetFrame, d net.IP) error { return p.o.Send(f, d) }
func (p plainOverlay) Flush() error                                 { return p.o.Flush() }
func (p plainOverlay) Release()                                     {}

func (f *superFixture) overlayOf(o OverlayBatch) {
	f.b.SetOverlayBatcher(func() OverlayBatch { return o })
}

// A pod's super-frame for a pod on another node that takes super-frames goes to the
// overlay in one piece, with the virtio header it had, and nothing is cut. Asking whether
// it would is not doing it: nothing is filtered, learnt or sent. The filter and the
// learning see the frame once.
func TestSuperFrameGoesWholeOverTheOverlay(t *testing.T) {
	f := newSuperFixture(t)
	ov := &superOverlay{accept: true}
	f.overlayOf(ov)
	var filtered int
	f.b.SetFilterHandler(func(*packet.EthernetFrame, Endpoint) bool { filtered++; return true })
	stranger := net.HardwareAddr{2, 0, 0, 0, 0xaa, 1}
	h, raw := tsoFrame(stranger, f.b.gatewayMAC, "10.244.1.4", "10.244.2.4")
	h.GSOType |= packet.VnetGSOECN
	orig := append([]byte(nil), raw...)

	if !f.b.PassesWhole(f.pod, raw) {
		t.Fatal("PassesWhole is false")
	}
	if ov.asks == 0 || len(ov.supers) != 0 || len(ov.sends) != 0 || filtered != 0 || f.b.fdb.Lookup(stranger) != nil || !bytes.Equal(raw, orig) {
		t.Fatalf("PassesWhole did something: %d asks, %+v, %d filtered", ov.asks, ov, filtered)
	}

	whole, err := f.b.ProcessSuperFrame(f.pod, h, raw)
	if !whole || err != nil {
		t.Fatalf("whole=%v, %v", whole, err)
	}
	if len(ov.supers) != 1 || len(ov.sends) != 0 || f.overlay != 0 {
		t.Fatalf("%d super-frames and %d frames sent to the overlay", len(ov.supers), len(ov.sends))
	}
	if got := ov.supers[0]; got.h != h || got.dst != "10.244.2.4" || !bytes.Equal(got.frame, raw) {
		t.Errorf("the overlay got %+v to %s, or not the frame as it was", got.h, got.dst)
	}
	if filtered != 1 || f.b.fdb.Lookup(stranger) != Endpoint(f.pod) {
		t.Errorf("filtered %d times, or the MAC was not learnt", filtered)
	}
	if !f.peer.untouched() || !f.bubble.untouched() {
		t.Error("something else was delivered")
	}
}

// A node that does not take super-frames, or an overlay that cannot send them, gets what
// it always got: the frame is left to be cut and the segments go one by one.
func TestSuperFrameToANodeThatDoesNotTakeThemIsCut(t *testing.T) {
	var results [][][]byte
	for _, tc := range []struct {
		name string
		// set connects the overlay to the fixture and returns what it was sent, one by one.
		set func(f *superFixture) func() [][]byte
	}{
		{"the node did not announce", func(f *superFixture) func() [][]byte {
			o := &superOverlay{}
			f.overlayOf(o)
			return func() [][]byte { return o.sends }
		}},
		{"the overlay has no super-frames", func(f *superFixture) func() [][]byte {
			o := &superOverlay{accept: true}
			f.overlayOf(plainOverlay{o})
			return func() [][]byte { return o.sends }
		}},
		{"the send says it sent nothing", func(f *superFixture) func() [][]byte {
			o := &superOverlay{accept: true, refuse: true}
			f.overlayOf(o)
			return func() [][]byte { return o.sends }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuperFixture(t)
			sent := tc.set(f)
			h, raw := tsoFrame(f.pod.MAC(), f.b.gatewayMAC, "10.244.1.4", "10.244.2.4")
			// The read stage asks first. (The last overlay accepts, and then does not send.)
			if asked := f.b.PassesWhole(f.pod, raw); asked != (tc.name == "the send says it sent nothing") {
				t.Fatalf("PassesWhole is %v", asked)
			}
			if whole, err := f.b.ProcessSuperFrame(f.pod, h, raw); whole || err != nil {
				t.Fatalf("whole=%v, %v", whole, err)
			}
			var sg packet.Segmenter
			segs, _ := sg.Segment(h, raw)
			if err := f.b.ProcessTrustedFrames(f.pod, segs); err != nil {
				t.Fatal(err)
			}
			if len(sent()) != 3 {
				t.Fatalf("%d frames sent, want the 3 segments", len(sent()))
			}
			for i, s := range sent() {
				fr, _ := packet.ParseEthernet(s)
				if err := packettest.Verify(fr.Payload); err != nil {
					t.Errorf("segment %d: %v", i, err)
				}
			}
			results = append(results, sent())
		})
	}
	for i := 1; i < len(results); i++ {
		if !reflect.DeepEqual(results[0], results[i]) {
			t.Errorf("the frames sent differ between the cases 0 and %d", i)
		}
	}
}

// A failed send of the whole frame is its loss and is reported; the frame is not cut.
func TestSuperFrameSendErrorIsReported(t *testing.T) {
	f := newSuperFixture(t)
	ov := &superOverlay{accept: true, failErr: errors.New("socket")}
	f.overlayOf(ov)
	h, raw := tsoFrame(f.pod.MAC(), f.b.gatewayMAC, "10.244.1.4", "10.244.2.4")
	whole, err := f.b.ProcessSuperFrame(f.pod, h, raw)
	if !whole || err == nil || len(ov.supers) != 1 || len(ov.sends) != 0 {
		t.Fatalf("whole=%v, %v, %d super-frames, %d frames", whole, err, len(ov.supers), len(ov.sends))
	}
}

// A super-frame for a Service whose backend is on another node is translated once and
// sent whole if that node takes it, and cut after the translation, as the segments would
// be translated one by one, if not.
func TestServiceSuperFrameToARemoteBackend(t *testing.T) {
	run := func(t *testing.T, ov *superOverlay, whole bool) *superFixture {
		f := newSuperFixture(t)
		f.eng.SetServices([]svc.Service{web(backend("10.244.2.4", 8080))})
		f.overlayOf(ov)
		f.open(t, f.pod, "10.244.1.4")
		h, raw := superTo(f.pod.MAC(), f.b.gatewayMAC, "10.244.1.4", 4000, vipA, 80, packet.TCPAck)
		f.carry(t, f.pod, whole, h, raw)
		return f
	}
	t.Run("whole", func(t *testing.T) {
		ov := &superOverlay{accept: true}
		f := run(t, ov, true)
		if len(ov.supers) != 1 || len(ov.sends) != 1 { // the SYN and the super-frame
			t.Fatalf("%d super-frames and %d frames sent", len(ov.supers), len(ov.sends))
		}
		got := ov.supers[0]
		h, _ := superTo(f.pod.MAC(), f.b.gatewayMAC, "10.244.1.4", 4000, vipA, 80, packet.TCPAck)
		fr, tu := completed(t, got.frame)
		if got.h != h || got.dst != "10.244.2.4" || tu != (packettest.Tuple{Src: snatIP, Sport: 4000, Dst: "10.244.2.4", Dport: 8080}) {
			t.Errorf("the overlay got %+v to %s: %+v", got.h, got.dst, tu)
		}
		if len(fr.Payload) != 40+3*superSegment {
			t.Errorf("a frame of %d bytes", len(fr.Payload))
		}
		if st := f.eng.Stats(); st.SuperFrames != 1 || st.Forwarded != 2 {
			t.Errorf("engine %+v", st)
		}
	})
	t.Run("cut after the translation", func(t *testing.T) {
		whole, cut := &superOverlay{}, &superOverlay{}
		run(t, whole, true)
		run(t, cut, false)
		if len(whole.supers) != 0 || len(whole.sends) != 4 || !reflect.DeepEqual(whole.sends, cut.sends) {
			t.Errorf("%d frames sent, or not those of the segments: %d super-frames", len(whole.sends), len(whole.supers))
		}
	})
}

// superBuffer is raw in a buffer after the room for a virtio header, as the overlay hands a
// super-frame over.
func superBuffer(raw []byte) *packet.Buffer {
	buf := packet.GetBuffer()
	buf.B = append(append(buf.B[:0], make([]byte, packet.VnetHeaderLen)...), raw...)
	return buf
}

// inject delivers raw, a super-frame that came through the overlay, to the bridge.
func (f *superFixture) inject(t *testing.T, h packet.VnetHeader, raw []byte) {
	t.Helper()
	x := f.b.NewBatch()
	defer x.Release()
	if err := x.InjectSuper(h, superBuffer(raw)); err != nil {
		t.Fatal(err)
	}
	if err := x.Flush(); err != nil {
		t.Fatal(err)
	}
}

// injectSegments delivers the segments of raw one by one, as an overlay without
// super-frames does.
func (f *superFixture) injectSegments(t *testing.T, h packet.VnetHeader, raw []byte) {
	t.Helper()
	var sg packet.Segmenter
	segs, err := sg.Segment(h, raw)
	if err != nil {
		t.Fatal(err)
	}
	x := f.b.NewBatch()
	defer x.Release()
	for _, s := range segs {
		fr, _ := packet.ParseEthernet(s)
		fr.Trusted = true
		if err := x.InjectFrame(fr); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.Flush(); err != nil {
		t.Fatal(err)
	}
}

// A super-frame from the overlay is delivered whole to a pod with offloads, with the MAC
// addresses that frames from the overlay get; and cut, with right checksums, where that
// is not possible, the segments being what the overlay's segments would have been.
func TestInjectedSuperFrameIsDeliveredWholeOrCut(t *testing.T) {
	remote := net.HardwareAddr{2, 0, 0, 0, 2, 4}
	t.Run("a pod with offloads", func(t *testing.T) {
		f := newSuperFixture(t)
		h, raw := tsoFrame(remote, f.b.gatewayMAC, "10.244.2.4", "10.244.1.5")
		h.GSOType |= packet.VnetGSOECN
		f.inject(t, h, raw)
		if len(f.peer.supers) != 1 || len(f.peer.adds) != 0 || f.peer.ReceivedCount() != 0 {
			t.Fatalf("the pod got %d super-frames, %d added frames, %d writes", len(f.peer.supers), len(f.peer.adds), f.peer.ReceivedCount())
		}
		got := f.peer.supers[0]
		want := append([]byte(nil), raw...)
		copy(want[0:6], f.peer.MAC())
		copy(want[6:12], f.b.gatewayMAC)
		if got.h != h || !bytes.Equal(got.frame, want) {
			t.Errorf("the pod got %+v, or not the frame with the MAC addresses set", got.h)
		}
	})
	cut := []struct {
		name string
		dst  string
		set  func(f *superFixture)
		got  func(f *superFixture) int // the frames that arrived
	}{
		{"a pod whose writer takes no super-frames", "10.244.1.6", nil, func(f *superFixture) int { return len(f.plain.adds) }},
		{"a pod without a writer", "10.244.1.7", nil, func(f *superFixture) int { return f.nowrite.ReceivedCount() }},
		{"the bubble without super-frames", "10.244.1.1", func(f *superFixture) { f.bubble.plain = true }, func(f *superFixture) int { return len(f.bubble.adds) }},
	}
	for _, tc := range cut {
		t.Run(tc.name, func(t *testing.T) {
			var results [2]delivered
			for i, whole := range []bool{true, false} {
				f := newSuperFixture(t)
				if tc.set != nil {
					tc.set(f)
				}
				h, raw := tsoFrame(remote, f.b.gatewayMAC, "10.244.2.4", tc.dst)
				if whole {
					f.inject(t, h, raw)
				} else {
					f.injectSegments(t, h, raw)
				}
				if n := tc.got(f); n != 3 {
					t.Fatalf("whole=%v: %d frames arrived, want the 3 segments", whole, n)
				}
				results[i] = f.delivered()
				if len(results[i].peerSupers)+len(results[i].podSupers)+len(results[i].bubbleSupers) != 0 || f.overlay != 0 {
					t.Errorf("whole=%v: a super-frame was written", whole)
				}
			}
			if !reflect.DeepEqual(results[0], results[1]) {
				t.Errorf("injected whole: %+v\nsegment by segment: %+v", results[0], results[1])
			}
		})
	}
	t.Run("checksums of the segments", func(t *testing.T) {
		f := newSuperFixture(t)
		h, raw := superTo(remote, f.b.gatewayMAC, "10.244.2.4", 4000, "10.244.1.6", 80, packet.TCPAck)
		f.inject(t, h, raw)
		if len(f.plain.adds) != 3 {
			t.Fatalf("%d segments", len(f.plain.adds))
		}
		for i, s := range f.plain.adds {
			fr, _ := packet.ParseEthernet(s)
			if err := packettest.Verify(fr.Payload); err != nil {
				t.Errorf("segment %d: %v", i, err)
			}
		}
	})
}

// The overlay never gets a super-frame that came from it: one for a pod on another node is
// dropped, as its segments are.
func TestInjectedSuperFrameNeverGoesBackToTheOverlay(t *testing.T) {
	f := newSuperFixture(t)
	ov := &superOverlay{accept: true}
	f.overlayOf(ov)
	for _, dst := range []string{"10.244.2.4", "10.244.9.9", "8.8.8.8"} {
		h, raw := tsoFrame(net.HardwareAddr{2, 0, 0, 0, 2, 4}, f.b.gatewayMAC, "10.244.2.5", dst)
		f.inject(t, h, raw)
	}
	if len(ov.supers) != 0 || len(ov.sends) != 0 || f.overlay != 0 || ov.asks != 0 {
		t.Errorf("the overlay was given something: %d super-frames, %d frames, %d asks", len(ov.supers), len(ov.sends), ov.asks)
	}
	for _, ep := range []*superEndpoint{f.pod, f.peer, f.plain, f.bubble} {
		if !ep.untouched() {
			t.Errorf("%s got something", ep.Name())
		}
	}
}

// A super-frame from the overlay to the bubble: whole to the gateway's address, where the
// bubble takes it.
func TestInjectedSuperFrameGoesWholeToTheBubble(t *testing.T) {
	f := newSuperFixture(t)
	h, raw := tsoFrame(net.HardwareAddr{2, 0, 0, 0, 2, 4}, f.b.gatewayMAC, "10.244.2.4", "10.244.1.1")
	f.inject(t, h, raw)
	if len(f.bubble.supers) != 1 || f.bubble.supers[0].h != h {
		t.Fatalf("the bubble got %d super-frames", len(f.bubble.supers))
	}
	got, _ := packet.ParseEthernet(f.bubble.supers[0].frame)
	if !bytes.Equal(got.DstMAC, f.bubble.MAC()) || !bytes.Equal(got.SrcMAC, f.b.gatewayMAC) {
		t.Errorf("MACs %s -> %s", got.SrcMAC, got.DstMAC)
	}
}

// The answer of a backend on another node to the Service engine's address arrives as a
// super-frame: the engine translates it once, and the client gets it whole or cut.
func TestInjectedSuperFrameForTheServiceEngine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client func(f *superFixture) *superEndpoint
		ip     string
		whole  bool
	}{
		{"a client with offloads", func(f *superFixture) *superEndpoint { return f.pod }, "10.244.1.4", true},
		{"a client without super-frames", func(f *superFixture) *superEndpoint { return f.plain }, "10.244.1.6", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuperFixture(t)
			f.eng.SetServices([]svc.Service{web(backend("10.244.2.4", 8080))})
			client := tc.client(f)
			f.open(t, client, tc.ip)
			f.sent, f.overlay = nil, 0
			h, raw := superTo(net.HardwareAddr{2, 0, 0, 0, 2, 4}, f.b.gatewayMAC, "10.244.2.4", 8080, snatIP, 4000, packet.TCPAck)
			f.inject(t, h, raw)
			if st := f.eng.Stats(); st.SuperFrames != 1 || st.Returned != 1 {
				t.Errorf("engine %+v", st)
			}
			if tc.whole {
				if len(client.supers) != 1 || client.supers[0].h != h {
					t.Fatalf("the client got %d super-frames", len(client.supers))
				}
				_, tu := completed(t, client.supers[0].frame)
				if tu != (packettest.Tuple{Src: vipA, Sport: 80, Dst: tc.ip, Dport: 4000}) {
					t.Errorf("the client got %+v", tu)
				}
			} else {
				if len(client.supers) != 0 || len(client.adds) != 3 {
					t.Fatalf("the client got %d super-frames and %d segments", len(client.supers), len(client.adds))
				}
				for _, s := range client.adds {
					fr, _ := packet.ParseEthernet(s)
					tu, _ := packettest.Parse(fr.Payload)
					if err := packettest.Verify(fr.Payload); err != nil || tu.Src != vipA || tu.Dport != 4000 {
						t.Errorf("a segment is %+v: %v", tu, err)
					}
				}
			}
			if f.overlay != 0 {
				t.Error("something went to the overlay")
			}
		})
	}
}

// Without tap mode: a frame for a known pod goes whole, one for an unknown MAC and IP is
// flooded cut, and none goes back to the overlay.
func TestInjectedSuperFrameInFDBMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mac   net.HardwareAddr
		dstIP string
		whole bool
	}{
		{"by MAC", net.HardwareAddr{2, 0, 0, 0, 0, 9}, "10.244.1.9", true},
		{"by IP", net.HardwareAddr{2, 0, 0, 0, 0xee, 1}, "10.244.1.9", true},
		{"flood", net.HardwareAddr{2, 0, 0, 0, 0xee, 1}, "10.244.1.99", false},
		{"broadcast", net.HardwareAddr{255, 255, 255, 255, 255, 255}, "10.244.1.99", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, _ := setupTestBridge()
			peer := newSuperEndpoint("s2", "10.244.1.9", "02:00:00:00:00:09")
			other := newSuperEndpoint("s3", "10.244.1.8", "02:00:00:00:00:08")
			_ = b.AddEndpoint(peer)
			_ = b.AddEndpoint(other)
			ov := &superOverlay{accept: true}
			b.SetOverlayBatcher(func() OverlayBatch { return ov })
			h, raw := tsoFrame(net.HardwareAddr{2, 0, 0, 0, 2, 4}, tc.mac, "10.244.2.4", tc.dstIP)
			x := b.NewBatch()
			if err := x.InjectSuper(h, superBuffer(raw)); err != nil {
				t.Fatal(err)
			}
			_ = x.Flush()
			x.Release()
			if tc.whole {
				if len(peer.supers) != 1 || peer.supers[0].h != h {
					t.Errorf("the pod got %d super-frames", len(peer.supers))
				}
			} else if len(peer.supers)+len(other.supers) != 0 {
				t.Errorf("a super-frame was written to everyone")
			}
			if len(ov.supers)+len(ov.sends) != 0 {
				t.Error("the overlay was given something")
			}
		})
	}
}

// bufEndpoint is a pod whose writer can take the buffer of a super-frame, or refuse it.
type bufEndpoint struct {
	*superEndpoint
	mode   string // "take": owns the buffer; "decline": leaves it; "fail": errors without taking it
	taken  []*packet.Buffer
	copies [][]byte // what the buffer held when it was taken
}

type bufWriter struct {
	superWriter
	e *bufEndpoint
}

func (e *bufEndpoint) NewWriter() FrameWriter {
	return bufWriter{superWriter{addWriter{e.superEndpoint}}, e}
}

func (w bufWriter) AddSuperBuffer(h packet.VnetHeader, f *packet.EthernetFrame, buf *packet.Buffer) (bool, error) {
	switch w.e.mode {
	case "take":
		w.e.taken = append(w.e.taken, buf)
		w.e.copies = append(w.e.copies, append([]byte(nil), buf.B...))
		return true, nil
	case "fail":
		return false, errors.New("closed")
	}
	return false, nil
}

// A frame that lies in a buffer is given to a writer that takes buffers, which owns it from
// then on (the batch does not release it), or, where the writer declines or fails, written
// as any super-frame is and released by the batch. Where the writer takes no buffers, the
// same.
func TestInjectedSuperFrameBufferBelongsToTheWriterThatTakesIt(t *testing.T) {
	for _, mode := range []string{"take", "decline", "fail", "plain"} {
		t.Run(mode, func(t *testing.T) {
			f := newSuperFixture(t)
			var dst *superEndpoint
			var be *bufEndpoint
			if mode == "plain" {
				dst = f.peer
			} else {
				be = &bufEndpoint{superEndpoint: newSuperEndpoint("p9", "10.244.1.9", "02:00:00:00:01:09"), mode: mode}
				if err := f.b.AddEndpoint(be); err != nil {
					t.Fatal(err)
				}
				dst = be.superEndpoint
			}
			h, raw := tsoFrame(net.HardwareAddr{2, 0, 0, 0, 2, 4}, f.b.gatewayMAC, "10.244.2.4", dst.IP().String())
			buf := superBuffer(raw)
			x := f.b.NewBatch()
			err := x.InjectSuper(h, buf)
			_ = x.Flush()
			x.Release()

			switch mode {
			case "take":
				if err != nil || len(be.taken) != 1 || be.taken[0] != buf || len(dst.supers) != 0 {
					t.Fatalf("err %v, %d buffers taken, %d written in place", err, len(be.taken), len(dst.supers))
				}
				// The batch did not release it: other buffers do not reuse it.
				for i := 0; i < 8; i++ {
					other := packet.GetBuffer()
					other.B = append(other.B, bytes.Repeat([]byte{0xee}, 3000)...)
					defer other.Release()
				}
				if !bytes.Equal(buf.B, be.copies[0]) {
					t.Error("the buffer was changed after it was taken")
				}
				buf.Release()
			case "decline", "plain":
				if err != nil || len(dst.supers) != 1 || dst.supers[0].h != h {
					t.Fatalf("err %v, %d super-frames written in place", err, len(dst.supers))
				}
			case "fail":
				if err == nil || len(dst.supers) != 0 {
					t.Fatalf("err %v, %d super-frames written in place", err, len(dst.supers))
				}
			}
		})
	}
}
