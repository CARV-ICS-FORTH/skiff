package svc

import (
	"bytes"
	"encoding/binary"
	"testing"

	"plaid/pkg/packet"
	"plaid/pkg/packet/packettest"
)

const superSize = 1000 // the payload of each segment of a test super-frame

// superPacket is the IP packet of a TCP super-frame of three segments as a sender's stack
// hands it over: the checksum field holds the sum of the pseudo-header only.
func superPacket(src string, sport uint16, dst string, dport uint16, flags byte) []byte {
	data := make([]byte, 3*superSize)
	for i := range data {
		data[i] = byte(i*13 + 5)
	}
	ip := packettest.TCP(src, sport, dst, dport, flags, string(data))
	packettest.Partial(ip)
	return ip
}

// cutSuper cuts the super-frame ip into its segments, as the bridge does, and returns them
// as IP packets with complete checksums.
func cutSuper(t *testing.T, ip []byte) [][]byte {
	t.Helper()
	frame := make([]byte, packet.EthernetHeaderLen, packet.EthernetHeaderLen+len(ip))
	binary.BigEndian.PutUint16(frame[12:], packet.EtherTypeIPv4)
	frame = append(frame, ip...)
	h := packet.VnetHeader{
		Flags: packet.VnetNeedsCsum, GSOType: packet.VnetGSOTCPv4, HdrLen: 54, GSOSize: superSize,
		CsumStart: 34, CsumOffset: 16,
	}
	var sg packet.Segmenter
	segs, err := sg.Segment(h, frame)
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]byte, len(segs))
	for i, s := range segs {
		out[i] = append([]byte(nil), s[packet.EthernetHeaderLen:]...)
	}
	return out
}

// established returns an engine with one flow from client:40000 to the Service, whose
// backend answered, and the source port the backend sees. With collide another client has
// the port 40000 towards the same backend already, so that this flow's port changes.
func established(t *testing.T, collide bool) (*Engine, uint16) {
	t.Helper()
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("a", vip, 80, TCP, be(remote, 8080))})
	if collide {
		must(t, e.Process(packettest.TCP(client2, 40000, vip, 80, packet.TCPSyn, "")))
	}
	out := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")))
	must(t, e.Process(packettest.TCP(remote, 8080, snat, out.Sport, packet.TCPSyn|packet.TCPAck, "")))
	if collide == (out.Sport == 40000) {
		t.Fatalf("source port %d with collide=%v", out.Sport, collide)
	}
	return e, out.Sport
}

// A TCP super-frame is translated as one packet, in place, and what comes out is what the
// segments become one by one: cut afterwards, the same segments; once the stack completes
// its checksum, a right one; and the flow in the state the segments leave it in.
func TestSuperFrameIsTranslatedLikeItsSegments(t *testing.T) {
	flagSets := map[string]byte{
		"ack":      packet.TCPAck,
		"psh":      packet.TCPAck | 0x08,
		"fin":      packet.TCPAck | packet.TCPFin,
		"fin, psh": packet.TCPAck | packet.TCPFin | 0x08,
		"rst":      packet.TCPRst,
	}
	for name, flags := range flagSets {
		for _, collide := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				dir := map[bool]string{false: "forward", true: "reverse"}[reverse]
				port := map[bool]string{false: "same port", true: "port changed"}[collide]
				t.Run(name+"/"+dir+"/"+port, func(t *testing.T) {
					whole, sport := established(t, collide)
					parts, _ := established(t, collide)
					var pkt []byte
					var want packettest.Tuple
					if reverse {
						pkt = superPacket(remote, 8080, snat, sport, flags)
						want = packettest.Tuple{Src: vip, Sport: 80, Dst: client, Dport: 40000}
					} else {
						pkt = superPacket(client, 40000, vip, 80, flags)
						want = packettest.Tuple{Src: snat, Sport: sport, Dst: remote, Dport: 8080}
					}
					before := whole.Stats()
					segments := cutSuper(t, pkt)
					if len(segments) != 3 {
						t.Fatalf("%d segments", len(segments))
					}

					got := whole.ProcessSuper(pkt)
					if got.Verdict != Forward || len(got.Packet) != len(pkt) || &got.Packet[0] != &pkt[0] {
						t.Fatalf("verdict %v, a packet of %d bytes (%d given), or not in place", got.Verdict, len(got.Packet), len(pkt))
					}
					if tu, _ := packettest.Parse(got.Packet); tu != want {
						t.Fatalf("translated to %+v, want %+v", tu, want)
					}
					// The stack completes the checksum later; it is then right.
					packettest.Complete(got.Packet)
					if err := packettest.Verify(got.Packet); err != nil {
						t.Errorf("completed: %v", err)
					}
					if packet.Checksum16(got.Packet[:20]) != 0 {
						t.Error("the IPv4 header checksum is wrong")
					}

					// The segments, translated one by one, are the segments of the result.
					cutAfter := cutSuper(t, got.Packet)
					for i, seg := range segments {
						r := parts.Process(seg)
						if r.Verdict != Forward || r.Dst != got.Dst || !bytes.Equal(r.Packet, cutAfter[i]) {
							t.Fatalf("segment %d: verdict %v, destination %v, or not the cut of the translated super-frame", i, r.Verdict, r.Dst)
						}
					}

					// The flow is where the segments left it, and the counters count a packet.
					fw, fp := whole.flows[fwdKey{TCP, ip(client), 40000, ip(vip), 80}], parts.flows[fwdKey{TCP, ip(client), 40000, ip(vip), 80}]
					if fw == nil || fp == nil || *fw != *fp {
						t.Fatalf("flow after the super-frame %+v, after its segments %+v", fw, fp)
					}
					// The counters count packets: one super-frame, three segments.
					sw, sp := whole.Stats(), parts.Stats()
					moved := func(st Stats) uint64 { return st.Returned + st.Forwarded }
					if sw.SuperFrames != 1 || moved(sw)-moved(before) != 1 || moved(sp)-moved(before) != 3 {
						t.Errorf("counters %+v, after its segments %+v, before %+v", sw, sp, before)
					}
				})
			}
		}
	}
}

// A super-frame with no flow is dropped, untouched, as its segments would be; only a SYN
// makes a flow, and one in a super-frame makes it as the first of its segments does.
func TestSuperFrameWithoutAFlowIsDroppedAndASynMakesOne(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("a", vip, 80, TCP, be(local, 8080))})
	for _, flags := range []byte{packet.TCPAck, packet.TCPAck | packet.TCPFin, packet.TCPRst} {
		pkt := superPacket(client, 40000, vip, 80, flags)
		orig := append([]byte(nil), pkt...)
		if r := e.ProcessSuper(pkt); r.Verdict != Drop || !bytes.Equal(pkt, orig) {
			t.Errorf("flags %#x: %+v, or the packet changed", flags, r)
		}
	}
	// A reply with no flow, too.
	if r := e.ProcessSuper(superPacket(local, 8080, snat, 40000, packet.TCPAck)); r.Verdict != Drop {
		t.Errorf("a reply with no flow: %+v", r)
	}
	if st := e.Stats(); st.DropsNoFlow != 4 || st.SuperFrames != 0 || st.Flows != 0 || st.Forwarded != 0 {
		t.Errorf("stats %+v", st)
	}

	pkt := superPacket(client, 40000, vip, 80, packet.TCPSyn)
	r := e.ProcessSuper(pkt)
	if r.Verdict != Forward || r.Dst != ip(local) {
		t.Fatalf("a SYN: %+v", r)
	}
	packettest.Complete(r.Packet)
	if err := packettest.Verify(r.Packet); err != nil {
		t.Error(err)
	}
	if st := e.Stats(); st.Flows != 1 || st.FlowsCreated != 1 || st.SuperFrames != 1 {
		t.Errorf("stats %+v", st)
	}
	// Refused, a SYN is answered with an ICMP error, a new packet, and the super-frame
	// is as it was.
	e.SetServices([]Service{service("a", vip, 80, TCP, Backend{IP: ip(local), Port: 8080})})
	pkt = superPacket(client2, 40000, vip, 80, packet.TCPSyn)
	orig := append([]byte(nil), pkt...)
	r = e.ProcessSuper(pkt)
	if r.Verdict != Reply || r.Dst != ip(client2) || !bytes.Equal(pkt, orig) || &r.Packet[0] == &pkt[0] {
		t.Errorf("a refused SYN: %+v", r)
	}
}

// Process gives back the packet it was given, translated, and a refusal is a new packet.
func TestProcessTranslatesInPlace(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("a", vip, 80, TCP, be(local, 8080)), service("none", vip2, 80, TCP)})
	pkt := append(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "x"), make([]byte, 6)...) // with padding
	r := e.Process(pkt)
	if r.Verdict != Forward || &r.Packet[0] != &pkt[0] || len(r.Packet) != len(pkt)-6 {
		t.Fatalf("%+v", r)
	}
	must(t, r)
	pkt = packettest.TCP(client, 40001, vip2, 80, packet.TCPSyn, "")
	orig := append([]byte(nil), pkt...)
	if r := e.Process(pkt); r.Verdict != Reply || &r.Packet[0] == &pkt[0] || !bytes.Equal(pkt, orig) {
		t.Errorf("%+v", r)
	}
}

// Once a flow is established, translating a packet or a super-frame, either way, allocates
// nothing.
func TestTranslationAllocatesNothing(t *testing.T) {
	e, sport := established(t, false)
	for _, tc := range []struct {
		name  string
		super bool
		pkt   []byte
	}{
		{"forward", false, packettest.TCP(client, 40000, vip, 80, packet.TCPAck, "request")},
		{"reverse", false, packettest.TCP(remote, 8080, snat, sport, packet.TCPAck, "response")},
		{"forward super-frame", true, superPacket(client, 40000, vip, 80, packet.TCPAck)},
		{"reverse super-frame", true, superPacket(remote, 8080, snat, sport, packet.TCPAck)},
	} {
		buf := make([]byte, len(tc.pkt))
		allocs := testing.AllocsPerRun(100, func() {
			copy(buf, tc.pkt)
			var r Result
			if tc.super {
				r = e.ProcessSuper(buf)
			} else {
				r = e.Process(buf)
			}
			if r.Verdict != Forward {
				t.Fatal("dropped")
			}
		})
		if allocs != 0 {
			t.Errorf("%s: %.0f allocations per packet", tc.name, allocs)
		}
	}
	e.SetServices([]Service{service("dns", vip, 53, UDP, be(local, 5353))})
	udp := packettest.UDP(client, 50000, vip, 53, "query", false)
	buf := make([]byte, len(udp))
	if allocs := testing.AllocsPerRun(100, func() { copy(buf, udp); e.Process(buf) }); allocs != 0 {
		t.Errorf("UDP: %.0f allocations per packet", allocs)
	}
}
