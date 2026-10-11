package bridge

import (
	"bytes"
	"net"
	"reflect"
	"testing"

	"plaid/pkg/packet"
	"plaid/pkg/packet/packettest"
	"plaid/pkg/svc"
)

// superTo builds a TCP super-frame of three segments as a pod's stack hands it over, for
// the Service engine: in the checksum field the pseudo-header sum only, to be completed.
func superTo(srcMAC, dstMAC net.HardwareAddr, srcIP string, sport uint16, dstIP string, dport uint16, flags byte) (packet.VnetHeader, []byte) {
	data := make([]byte, 3*superSegment)
	for i := range data {
		data[i] = byte(i*13 + 5)
	}
	ip := packettest.TCP(srcIP, sport, dstIP, dport, flags, string(data))
	packettest.Partial(ip)
	f := &packet.EthernetFrame{SrcMAC: srcMAC, DstMAC: dstMAC, EtherType: packet.EtherTypeIPv4, Payload: ip}
	raw, _ := f.Marshal()
	return packet.VnetHeader{
		Flags: packet.VnetNeedsCsum, GSOType: packet.VnetGSOTCPv4, HdrLen: 54, GSOSize: superSegment,
		CsumStart: 34, CsumOffset: 16,
	}, raw
}

// open sends an ordinary SYN from ep (whose address is srcIP) to the Service, which makes
// the flow; the engine keeps the client's port 4000 towards the backend.
func (f *superFixture) open(t *testing.T, ep Endpoint, srcIP string) {
	t.Helper()
	syn := &packet.EthernetFrame{
		SrcMAC: ep.MAC(), DstMAC: f.b.gatewayMAC, EtherType: packet.EtherTypeIPv4,
		Payload: packettest.TCP(srcIP, 4000, vipA, 80, packet.TCPSyn, ""),
	}
	raw, _ := syn.Marshal()
	if err := f.b.ProcessFrame(ep, raw); err != nil {
		t.Fatal(err)
	}
}

// carry sends the super-frame from src: whole, as ProcessSuperFrame does, or, to see
// what the segments would have done, cut by the caller and processed one by one.
func (f *superFixture) carry(t *testing.T, src Endpoint, whole bool, h packet.VnetHeader, raw []byte) {
	t.Helper()
	if whole {
		if !f.b.PassesWhole(src, raw) {
			t.Fatal("PassesWhole is false")
		}
		if got, err := f.b.ProcessSuperFrame(src, h, raw); !got || err != nil {
			t.Fatalf("whole=%v, %v", got, err)
		}
		return
	}
	var sg packet.Segmenter
	segs, err := sg.Segment(h, raw)
	if err != nil || len(segs) != 3 {
		t.Fatalf("%d segments, %v", len(segs), err)
	}
	if err := f.b.ProcessTrustedFrames(src, segs); err != nil {
		t.Fatal(err)
	}
}

// delivered is everything the endpoints and the overlay were given.
type delivered struct {
	overlay, pod, peer, plain, nowrite, bubble [][]byte
	podSupers, peerSupers, bubbleSupers        []superRecord
}

func (f *superFixture) delivered() delivered {
	return delivered{
		overlay: f.sent, pod: f.pod.adds, peer: f.peer.adds, plain: f.plain.adds, nowrite: f.nowrite.packets, bubble: f.bubble.adds,
		podSupers: f.pod.supers, peerSupers: f.peer.supers, bubbleSupers: f.bubble.supers,
	}
}

// completed returns the frame of an Ethernet frame, whose checksums have been completed
// as the stack of its receiver does, and checks them.
func completed(t *testing.T, frame []byte) (*packet.EthernetFrame, packettest.Tuple) {
	t.Helper()
	fr, err := packet.ParseEthernet(append([]byte(nil), frame...))
	if err != nil {
		t.Fatal(err)
	}
	packettest.Complete(fr.Payload)
	if err := packettest.Verify(fr.Payload); err != nil {
		t.Fatal(err)
	}
	tu, _ := packettest.Parse(fr.Payload)
	return fr, tu
}

// A pod's super-frame to a Service whose backend is a pod with offloads on this node is
// translated once and written to the backend whole: the backend's address and port, the
// engine's as the source, the header of the pod's frame unchanged, a right checksum once
// completed; and so is the backend's answer to the engine's address, to the client.
func TestServiceSuperFrameGoesWholeToALocalBackendAndBack(t *testing.T) {
	f := newSuperFixture(t)
	f.open(t, f.pod, "10.244.1.4")

	h, raw := superTo(f.pod.MAC(), f.b.gatewayMAC, "10.244.1.4", 4000, vipA, 80, packet.TCPAck)
	orig := append([]byte(nil), raw...)
	stats, flows := f.eng.Stats(), f.eng.Services()
	if !f.b.PassesWhole(f.pod, raw) {
		t.Fatal("PassesWhole is false")
	}
	// Looking is not doing: the engine's counters and flows, and the frame, are as they were.
	if !reflect.DeepEqual(stats, f.eng.Stats()) || !reflect.DeepEqual(flows, f.eng.Services()) || !bytes.Equal(raw, orig) || len(f.peer.supers) != 0 {
		t.Fatal("PassesWhole changed something")
	}

	if whole, err := f.b.ProcessSuperFrame(f.pod, h, raw); !whole || err != nil {
		t.Fatalf("whole=%v, %v", whole, err)
	}
	if len(f.peer.supers) != 1 || len(f.peer.adds) != 1 { // the SYN, and the super-frame
		t.Fatalf("the backend got %d super-frames and %d frames", len(f.peer.supers), len(f.peer.adds))
	}
	got := f.peer.supers[0]
	if got.h != h {
		t.Errorf("header %+v, want %+v", got.h, h)
	}
	fr, tu := completed(t, got.frame)
	if tu != (packettest.Tuple{Src: snatIP, Sport: 4000, Dst: "10.244.1.5", Dport: 8080}) {
		t.Errorf("the backend got %+v", tu)
	}
	if !bytes.Equal(fr.DstMAC, f.peer.MAC()) || !bytes.Equal(fr.SrcMAC, f.b.gatewayMAC) {
		t.Errorf("MACs %s -> %s", fr.SrcMAC, fr.DstMAC)
	}
	if len(got.frame) != len(raw) || !bytes.Equal(fr.Payload[40:], orig[packet.EthernetHeaderLen+40:]) {
		t.Error("the length or the data changed")
	}
	if st := f.eng.Stats(); st.SuperFrames != 1 || st.Forwarded != 2 || st.DropsNoFlow != 0 {
		t.Errorf("engine %+v", st)
	}
	if !f.plain.untouched() || !f.bubble.untouched() || f.nowrite.ReceivedCount() != 0 || f.overlay != 0 || len(f.pod.supers) != 0 {
		t.Error("something else was delivered")
	}

	// The answer.
	h, raw = superTo(f.peer.MAC(), f.b.gatewayMAC, "10.244.1.5", 8080, snatIP, 4000, packet.TCPAck)
	if whole, err := f.b.ProcessSuperFrame(f.peer, h, raw); !whole || err != nil {
		t.Fatalf("reverse: whole=%v, %v", whole, err)
	}
	if len(f.pod.supers) != 1 || f.pod.supers[0].h != h {
		t.Fatalf("the client got %d super-frames", len(f.pod.supers))
	}
	fr, tu = completed(t, f.pod.supers[0].frame)
	if tu != (packettest.Tuple{Src: vipA, Sport: 80, Dst: "10.244.1.4", Dport: 4000}) || !bytes.Equal(fr.DstMAC, f.pod.MAC()) {
		t.Errorf("the client got %+v to %s", tu, fr.DstMAC)
	}
	if st := f.eng.Stats(); st.SuperFrames != 2 || st.Returned != 1 {
		t.Errorf("engine %+v", st)
	}
}

// Where the translated super-frame cannot go whole it is cut after the translation, and
// the segments are what translating the segments one by one gives: the same frames in the
// same places, for the overlay, a pod whose writer takes no super-frames, a pod without a
// writer, and the client itself.
func TestServiceSuperFrameIsCutAfterTheTranslation(t *testing.T) {
	for _, tc := range []struct {
		name, backend string
		arrived       func(d delivered) [][]byte // where the SYN and the three segments arrive
	}{
		{"a remote pod", "10.244.2.4", func(d delivered) [][]byte { return d.overlay }},
		{"a pod whose writer takes no super-frames", "10.244.1.6", func(d delivered) [][]byte { return d.plain }},
		{"a pod without a writer", "10.244.1.7", func(d delivered) [][]byte { return d.nowrite }},
		{"the client itself", "10.244.1.4", func(d delivered) [][]byte { return d.pod }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var results [2]delivered
			for i, whole := range []bool{true, false} {
				f := newSuperFixture(t)
				f.eng.SetServices([]svc.Service{web(backend(tc.backend, 8080))})
				f.open(t, f.pod, "10.244.1.4")
				h, raw := superTo(f.pod.MAC(), f.b.gatewayMAC, "10.244.1.4", 4000, vipA, 80, packet.TCPAck|0x08)
				f.carry(t, f.pod, whole, h, raw)
				results[i] = f.delivered()
				if st := f.eng.Stats(); whole && (st.SuperFrames != 1 || st.Forwarded != 2) || !whole && (st.SuperFrames != 0 || st.Forwarded != 4) {
					t.Errorf("whole=%v: engine %+v", whole, st)
				}
				if n := len(tc.arrived(results[i])); n != 4 {
					t.Errorf("whole=%v: %d frames arrived, want the SYN and three segments", whole, n)
				}
				if len(results[i].peerSupers)+len(results[i].podSupers)+len(results[i].bubbleSupers) != 0 {
					t.Errorf("whole=%v: a super-frame was written", whole)
				}
			}
			if !reflect.DeepEqual(results[0], results[1]) {
				t.Errorf("translated whole and cut after: %+v\nsegment by segment: %+v", results[0], results[1])
			}
			for _, raw := range tc.arrived(results[0])[1:] { // the segments, translated, with right checksums
				fr, err := packet.ParseEthernet(raw)
				if err != nil {
					t.Fatal(err)
				}
				tu, _ := packettest.Parse(fr.Payload)
				if err := packettest.Verify(fr.Payload); err != nil || tu.Src != snatIP || tu.Dport != 8080 {
					t.Errorf("a segment is %+v: %v", tu, err)
				}
			}
		})
	}
}

// The answer to a client that cannot take it whole is cut after the translation, too: a
// pod without super-frames, and to the bubble, which takes them whole.
func TestServiceSuperFrameAnswersTheClient(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ep    func(f *superFixture) *superEndpoint
		ip    string
		whole bool // the client takes it whole
	}{
		{"a pod without super-frames", func(f *superFixture) *superEndpoint { return f.plain }, "10.244.1.6", false},
		{"the bubble", func(f *superFixture) *superEndpoint { return f.bubble }, "10.244.1.1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var results [2]delivered
			var f *superFixture
			for i, whole := range []bool{true, false} {
				f = newSuperFixture(t)
				client := tc.ep(f)
				f.open(t, client, tc.ip)
				h, raw := superTo(f.peer.MAC(), f.b.gatewayMAC, "10.244.1.5", 8080, snatIP, 4000, packet.TCPAck)
				f.carry(t, f.peer, whole, h, raw)
				results[i] = f.delivered()
				if st := f.eng.Stats(); whole && (st.SuperFrames != 1 || st.Returned != 1) {
					t.Errorf("engine %+v", st)
				}
			}
			if tc.whole {
				d := results[0]
				if len(d.bubbleSupers) != 1 || len(d.bubble) != 0 {
					t.Fatalf("the bubble got %d super-frames, %d frames", len(d.bubbleSupers), len(d.bubble))
				}
				fr, tu := completed(t, d.bubbleSupers[0].frame)
				if tu != (packettest.Tuple{Src: vipA, Sport: 80, Dst: tc.ip, Dport: 4000}) || !bytes.Equal(fr.DstMAC, f.bubble.MAC()) {
					t.Errorf("the bubble got %+v to %s", tu, fr.DstMAC)
				}
				return
			}
			if !reflect.DeepEqual(results[0], results[1]) || len(results[0].plain) != 3 {
				t.Errorf("translated whole and cut after: %d frames to the client; segment by segment: %d", len(results[0].plain), len(results[1].plain))
			}
		})
	}
}

// A super-frame to a Service with no backend that can take a connection is refused as its
// first segment is: one ICMP port unreachable to the client, which is an ordinary packet.
func TestServiceSuperFrameWithoutABackendIsRefused(t *testing.T) {
	f := newSuperFixture(t)
	f.eng.SetServices([]svc.Service{web(svc.Backend{IP: ip4("10.244.1.5"), Port: 8080})}) // not eligible
	h, raw := superTo(f.pod.MAC(), f.b.gatewayMAC, "10.244.1.4", 4000, vipA, 80, packet.TCPSyn)
	if !f.b.PassesWhole(f.pod, raw) {
		t.Fatal("PassesWhole is false")
	}
	if whole, err := f.b.ProcessSuperFrame(f.pod, h, raw); !whole || err != nil {
		t.Fatalf("whole=%v, %v", whole, err)
	}
	if len(f.pod.adds) != 1 || len(f.pod.supers) != 0 || !f.peer.untouched() {
		t.Fatalf("the client got %d frames and %d super-frames", len(f.pod.adds), len(f.pod.supers))
	}
	fr, _ := packet.ParseEthernet(f.pod.adds[0])
	ip := fr.Payload
	if ip[9] != packet.IPProtocolICMP || ip[20] != 3 || ip[21] != packet.ICMPPortUnreachable || packet.Checksum16(ip[20:]) != 0 ||
		!net.IP(ip[12:16]).Equal(net.ParseIP(vipA)) || !net.IP(ip[16:20]).Equal(net.ParseIP("10.244.1.4")) {
		t.Errorf("not an ICMP port unreachable from the Service to the client: % x", ip[:28])
	}
	if st := f.eng.Stats(); st.Rejected != 1 || st.SuperFrames != 0 {
		t.Errorf("engine %+v", st)
	}
}

// A super-frame the engine drops (no flow) is dropped whole, untouched, and counted once;
// one whose IP header does not state its length is left to the segments, and the engine
// is not asked.
func TestServiceSuperFrameDroppedOrLeftToTheSegments(t *testing.T) {
	f := newSuperFixture(t)
	h, raw := superTo(f.pod.MAC(), f.b.gatewayMAC, "10.244.1.4", 4000, vipA, 80, packet.TCPAck)
	if whole, err := f.b.ProcessSuperFrame(f.pod, h, raw); !whole || err != nil {
		t.Fatalf("whole=%v, %v", whole, err)
	}
	if st := f.eng.Stats(); st.DropsNoFlow != 1 || st.SuperFrames != 0 || !f.peer.untouched() || !f.bubble.untouched() || f.overlay != 0 {
		t.Errorf("engine %+v", st)
	}

	raw[packet.EthernetHeaderLen+3]-- // a length that is not the frame's
	if f.b.PassesWhole(f.pod, raw) {
		t.Error("PassesWhole is true")
	}
	if whole, _ := f.b.ProcessSuperFrame(f.pod, h, raw); whole || f.eng.Stats().DropsNoFlow != 1 {
		t.Errorf("whole=%v, engine %+v", whole, f.eng.Stats())
	}
}
