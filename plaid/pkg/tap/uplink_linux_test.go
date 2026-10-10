//go:build linux

package tap

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"sort"
	"sync"
	"testing"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
	"plaid/pkg/packet/packettest"
	"plaid/pkg/svc"
)

const (
	testPodMTU    = 1450
	testUplinkMTU = 65520
	testSegment   = testPodMTU - 40 // payload of a full pod segment
	testVIP       = "10.43.0.50"
	testSNAT      = "10.244.1.2"
	testRemote    = "10.244.2.4"
	testOutside   = "203.0.113.9"
)

// uplinkRig is a bridge in tap mode with the bubble's TAP at an uplink MTU, a pod with
// offloads that passthrough serves ("a", 10.244.1.4, and "b", 10.244.1.5, the backend of
// a Service), a pod without offloads ("c", .6), a remote pod, and a Service. The files are
// the pods' and the bubble's ends of the TAPs, as the bubble's kernel and the pods' stacks
// see them.
type uplinkRig struct {
	br                         *bridge.Bridge
	bubble                     *TapEndpoint
	bubblePod, aPod, bPod, cPd *os.File
	eng                        *svc.Engine

	mu       sync.Mutex
	overlay  [][]byte // the frames sent to the remote pod
	maxFiltr int      // the largest IP packet the filter saw from the bubble
}

func newUplinkRig(t *testing.T, uplinkMTU int) *uplinkRig {
	t.Helper()
	r := &uplinkRig{br: bridge.NewBridge(tapModeCfg)}
	a, aPod := pipeTap(t, "a", 4, false, 0)
	b, bPod := pipeTap(t, "b", 5, false, 0)
	c, cPod := socketTap(t, tapOpts{name: "c", id: 6, vnet: true})
	bubble, bubblePod := pipeTap(t, "bubble", 9, false, 0)
	r.bubble, r.bubblePod, r.aPod, r.bPod, r.cPd = bubble, bubblePod, aPod, bPod, cPod
	for _, ep := range []*TapEndpoint{a, b, c} {
		ep.SetPassthrough()
		if err := r.br.AddEndpoint(ep); err != nil {
			t.Fatal(err)
		}
	}
	bubble.SetPassthrough()
	bubble.SetUplinkMTU(testPodMTU, uplinkMTU)
	r.br.SetBubbleEndpoint(bubble)

	_, cidr, _ := net.ParseCIDR("10.43.0.0/16")
	var err error
	if r.eng, err = svc.New(svc.Config{ServiceCIDR: cidr, SNATAddr: net.ParseIP(testSNAT)}); err != nil {
		t.Fatal(err)
	}
	var vip, backend [4]byte
	copy(vip[:], net.ParseIP(testVIP).To4())
	copy(backend[:], net.ParseIP("10.244.1.5").To4())
	r.eng.SetServices([]svc.Service{{ID: "web", VIP: vip, Port: 80, Proto: svc.TCP, Backends: []svc.Backend{{IP: backend, Port: 8080, Eligible: true}}}})
	r.br.SetServiceEngine(r.eng)
	r.br.SetRouteChecker(func(ip net.IP) bool { return ip.Equal(net.ParseIP(testRemote)) })
	setOverlayFunc(r.br, func(f *packet.EthernetFrame, _ net.IP) error {
		r.mu.Lock()
		r.overlay = append(r.overlay, mustMarshal(f))
		r.mu.Unlock()
		return nil
	})
	r.br.SetFilterHandler(func(f *packet.EthernetFrame, src bridge.Endpoint) bool {
		if src == bridge.Endpoint(bubble) {
			r.mu.Lock()
			r.maxFiltr = max(r.maxFiltr, len(f.Payload))
			r.mu.Unlock()
		}
		return true
	})
	t.Cleanup(func() {
		for _, ep := range []*TapEndpoint{a, b, c, bubble} {
			_ = ep.Close()
		}
	})
	for _, ep := range []*TapEndpoint{a, b, c, bubble} {
		ep.StartReadLoop(context.Background(), r.br)
	}
	return r
}

func (r *uplinkRig) overlayFrames() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.overlay...)
}

// pattern bytes: the payload of the streams of these tests, so that a segment out of place
// or a lost byte shows.
func payloadOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = pattern(i)
	}
	return b
}

// bubbleRecord is what the bubble's kernel writes to plaid0 for a plain packet: an empty
// virtio header and the frame, addressed to the MAC of dst (any, as the bridge routes by
// the IP address).
func (r *uplinkRig) bubbleRecord(ip []byte) []byte {
	f := &packet.EthernetFrame{
		DstMAC: net.HardwareAddr{2, 0, 0, 0, 0, 5}, SrcMAC: r.bubble.MAC(),
		EtherType: packet.EtherTypeIPv4, Payload: ip,
	}
	return append(make([]byte, packet.VnetHeaderLen), mustMarshal(f)...)
}

// tsoRecord is what a pod's stack writes for a TSO segment of data bytes in segments of
// size bytes from src to dst with the TCP flags: the virtio header, the frame, and in the
// checksum field the pseudo-header sum that the kernel leaves to be completed.
func tsoRecord(srcMAC, dstMAC net.HardwareAddr, src, dst string, size, data int, flags byte) []byte {
	ip := packettest.TCP(src, 4000, dst, 80, flags, string(payloadOf(data)))
	packettest.Partial(ip)
	gso := uint8(packet.VnetGSOTCPv4)
	if flags&0x80 != 0 {
		gso |= packet.VnetGSOECN
	}
	rec := make([]byte, packet.VnetHeaderLen)
	packet.VnetHeader{Flags: packet.VnetNeedsCsum, GSOType: gso, HdrLen: 54, GSOSize: uint16(size), CsumStart: 34, CsumOffset: 16}.Put(rec)
	f := &packet.EthernetFrame{DstMAC: dstMAC, SrcMAC: srcMAC, EtherType: packet.EtherTypeIPv4, Payload: ip}
	return append(rec, mustMarshal(f)...)
}

// checkStream checks that segs are the consecutive TCP segments of the stream that
// payloadOf describes, from sequence number 1000: each at most the pod MTU, with right
// checksums (recomputed by packettest), carrying the bytes in order, and that dst and
// dport are where they are going. It returns the flags of the first and of the last.
func checkStream(t *testing.T, segs [][]byte, dst string, dport uint16, total int) (first, last byte) {
	t.Helper()
	next, got := uint32(1000), 0
	for i, f := range segs {
		fr, err := packet.ParseEthernet(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(fr.Payload) > testPodMTU {
			t.Fatalf("segment %d has an IP packet of %d bytes, above the pod MTU", i, len(fr.Payload))
		}
		if err := packettest.Verify(fr.Payload); err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		tu, _ := packettest.Parse(fr.Payload)
		if tu.Dst != dst || tu.Dport != dport {
			t.Fatalf("segment %d goes to %s:%d", i, tu.Dst, tu.Dport)
		}
		tcp := fr.Payload[20:]
		if seq := binary.BigEndian.Uint32(tcp[4:]); seq != next {
			t.Fatalf("segment %d has sequence %d, want %d", i, seq, next)
		}
		data := tcp[20:]
		if !bytes.Equal(data, payloadOf(total)[got:got+len(data)]) {
			t.Fatalf("segment %d carries other bytes", i)
		}
		next += uint32(len(data))
		got += len(data)
		if i == 0 {
			first = tcp[13]
		}
		last = tcp[13]
	}
	if got != total {
		t.Fatalf("%d bytes arrived, want %d", got, total)
	}
	return first, last
}

// A big TCP frame from the bubble to a pod with offloads becomes one TSO segment for the
// pod's TAP, with the header of the kernel's own TSO: the pod's MSS, the offsets of the TCP
// header, the pseudo-header sum in the checksum field.
func TestBubbleFrameGoesWholeToAPod(t *testing.T) {
	r := newUplinkRig(t, testUplinkMTU)
	const total = 64000
	ip := packettest.TCP(testOutside, 443, "10.244.1.5", 80, packet.TCPAck, string(payloadOf(total)))
	before := Offloads()
	if _, err := r.bubblePod.Write(r.bubbleRecord(ip)); err != nil {
		t.Fatal(err)
	}
	recs := readQuiet(r.bPod)
	if len(recs) != 1 {
		t.Fatalf("the pod got %d records, want the one TSO segment", len(recs))
	}
	h, frame := packet.ParseVnetHeader(recs[0]), recs[0][packet.VnetHeaderLen:]
	want := packet.VnetHeader{
		Flags: packet.VnetNeedsCsum, GSOType: packet.VnetGSOTCPv4, HdrLen: 54, GSOSize: testSegment,
		CsumStart: 34, CsumOffset: 16,
	}
	if h != want {
		t.Errorf("header %+v, want %+v", h, want)
	}
	if err := packet.CheckTSO(h, frame); err != nil {
		t.Errorf("the kernel's checks of the header: %v", err)
	}
	partial := append([]byte(nil), frame[14:]...)
	packettest.Partial(partial)
	if got, want := frame[14+20+16:14+20+18], partial[20+16:20+18]; !bytes.Equal(got, want) {
		t.Errorf("the checksum field holds %x, want the pseudo-header sum %x", got, want)
	}
	_, segs := recordFrames(t, recs[0])
	if len(segs) != (total+testSegment-1)/testSegment {
		t.Errorf("%d segments", len(segs))
	}
	checkStream(t, segs, "10.244.1.5", 80, total)
	d := delta(Offloads(), before)
	if d.UplinkSuperFrames != 1 || d.PassedSuperFrames != 1 || d.ReadSuperFrames != 0 || d.OversizeDropped != 0 || d.ReadDropped != 0 {
		t.Errorf("counters %+v", d)
	}
}

// A TSO segment from the bubble's kernel, whose segment size is that of the uplink's MSS,
// is cut to the pod MTU too; a big frame with a wrong checksum is dropped and counted, not
// repaired by the cut.
func TestBubbleFrameWithOtherSizesOrBadChecksums(t *testing.T) {
	r := newUplinkRig(t, testUplinkMTU)
	const total = 60000
	rec := tsoRecord(r.bubble.MAC(), net.HardwareAddr{2, 0, 0, 0, 0, 5}, testOutside, "10.244.1.5", 30000, total, packet.TCPAck)
	if _, err := r.bubblePod.Write(rec); err != nil {
		t.Fatal(err)
	}
	recs := readQuiet(r.bPod)
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	if h, segs := recordFrames(t, recs[0]); h.GSOSize != testSegment {
		t.Errorf("segment size %d, want %d", h.GSOSize, testSegment)
	} else {
		checkStream(t, segs, "10.244.1.5", 80, total)
	}

	ip := packettest.TCP(testOutside, 443, "10.244.1.5", 80, packet.TCPAck, string(payloadOf(total)))
	ip[len(ip)-1] ^= 1
	before := Offloads()
	if _, err := r.bubblePod.Write(r.bubbleRecord(ip)); err != nil {
		t.Fatal(err)
	}
	if recs := readQuiet(r.bPod); len(recs) != 0 {
		t.Errorf("a frame with a wrong checksum reached the pod: %d records", len(recs))
	}
	if d := delta(Offloads(), before); d.ReadDropped != 1 || d.UplinkSuperFrames != 0 {
		t.Errorf("counters %+v", d)
	}
}

// CWR and the other flags of a big frame survive: the ECN type of the TSO segment says that
// the first segment has CWR, and the flags of the last (PSH, FIN) are the frame's.
func TestBubbleFrameKeepsItsFlags(t *testing.T) {
	r := newUplinkRig(t, testUplinkMTU)
	const total = 5 * testSegment
	for _, flags := range []byte{packet.TCPAck | 0x80, packet.TCPAck | 0x08 | packet.TCPFin} {
		ip := packettest.TCP(testOutside, 443, "10.244.1.5", 80, flags, string(payloadOf(total)))
		if _, err := r.bubblePod.Write(r.bubbleRecord(ip)); err != nil {
			t.Fatal(err)
		}
		recs := readQuiet(r.bPod)
		if len(recs) != 1 {
			t.Fatalf("flags %#x: %d records", flags, len(recs))
		}
		h, segs := recordFrames(t, recs[0])
		if got := h.GSOType&packet.VnetGSOECN != 0; got != (flags&0x80 != 0) {
			t.Errorf("flags %#x: GSO type %#x", flags, h.GSOType)
		}
		first, last := checkStream(t, segs, "10.244.1.5", 80, total)
		// As the kernel's segmentation does it: CWR on the first only, PSH and FIN on the last.
		if flags&0x80 != 0 && (first&0x80 == 0 || last&0x80 != 0) {
			t.Errorf("CWR: first %#x, last %#x", first, last)
		}
		if flags&0x08 != 0 && (first&0x09 != 0 || last&0x09 != 0x09) {
			t.Errorf("PSH|FIN: first %#x, last %#x", first, last)
		}
	}
}

// To a pod without offloads, to a pod on another node and to the Service engine, the frame
// is cut: what they get is segments of the pod MTU, in order, with right checksums.
func TestBubbleFrameIsCutWhereItCannotGoWhole(t *testing.T) {
	const total = 64000
	n := (total + testSegment - 1) / testSegment
	t.Run("a pod without offloads", func(t *testing.T) {
		r := newUplinkRig(t, testUplinkMTU)
		ip := packettest.TCP(testOutside, 443, "10.244.1.6", 80, packet.TCPAck, string(payloadOf(total)))
		before := Offloads()
		if _, err := r.bubblePod.Write(r.bubbleRecord(ip)); err != nil {
			t.Fatal(err)
		}
		recs := readQuiet(r.cPd)
		var segs [][]byte
		for _, rec := range recs {
			_, s := recordFrames(t, rec)
			segs = append(segs, s...)
		}
		if len(recs) != n || len(segs) != n {
			t.Fatalf("%d records, %d segments, want %d plain frames", len(recs), len(segs), n)
		}
		checkStream(t, segs, "10.244.1.6", 80, total)
		if d := delta(Offloads(), before); d.UplinkSuperFrames != 1 || d.ReadSuperFrames != 1 || d.ReadSegments != uint64(n) || d.PassedSuperFrames != 0 {
			t.Errorf("counters %+v", d)
		}
	})
	t.Run("a remote pod", func(t *testing.T) {
		r := newUplinkRig(t, testUplinkMTU)
		ip := packettest.TCP(testOutside, 443, testRemote, 80, packet.TCPAck, string(payloadOf(total)))
		if _, err := r.bubblePod.Write(r.bubbleRecord(ip)); err != nil {
			t.Fatal(err)
		}
		readQuiet(r.bPod)
		segs := r.overlayFrames()
		if len(segs) != n {
			t.Fatalf("%d frames over the overlay, want %d", len(segs), n)
		}
		checkStream(t, segs, testRemote, 80, total)
	})
	t.Run("a Service", func(t *testing.T) {
		r := newUplinkRig(t, testUplinkMTU)
		syn := packettest.TCP(testOutside, 5555, testVIP, 80, packet.TCPSyn, "")
		if _, err := r.bubblePod.Write(r.bubbleRecord(syn)); err != nil {
			t.Fatal(err)
		}
		ip := packettest.TCP(testOutside, 5555, testVIP, 80, packet.TCPAck, string(payloadOf(total)))
		if _, err := r.bubblePod.Write(r.bubbleRecord(ip)); err != nil {
			t.Fatal(err)
		}
		var segs [][]byte
		for _, rec := range readQuiet(r.bPod) {
			if _, s := recordFrames(t, rec); len(s) > 0 {
				segs = append(segs, s...)
			}
		}
		st := r.eng.Stats()
		if st.Forwarded != uint64(1+n) || st.DropsNoFlow != 0 || st.DropsUnsupported != 0 {
			t.Errorf("the engine translated %d packets (want %d), dropped %d without a flow and %d as unsupported", st.Forwarded, 1+n, st.DropsNoFlow, st.DropsUnsupported)
		}
		r.mu.Lock()
		seen := r.maxFiltr
		r.mu.Unlock()
		if seen > testPodMTU {
			t.Errorf("the bridge handled a packet of %d bytes from the bubble", seen)
		}
		if len(segs) != 1+n {
			t.Fatalf("the backend got %d segments, want the SYN and %d", len(segs), n)
		}
		// Every segment was translated: to the backend, from the engine's address.
		for i, f := range segs[1:] {
			fr, _ := packet.ParseEthernet(f)
			tu, _ := packettest.Parse(fr.Payload)
			if tu.Src != testSNAT || tu.Dst != "10.244.1.5" || tu.Dport != 8080 || len(fr.Payload) > testPodMTU || packettest.Verify(fr.Payload) != nil {
				t.Fatalf("segment %d: %+v, %d bytes", i, tu, len(fr.Payload))
			}
			if !bytes.Equal(fr.Payload[40:], payloadOf(total)[i*testSegment:min((i+1)*testSegment, total)]) {
				t.Fatalf("segment %d carries other bytes", i)
			}
		}
	})
}

// A UDP datagram of 20 KB from the bubble, with the Don't Fragment flag clear, reaches the
// pod as fragments of the pod MTU that reassemble to it; with the flag set it is dropped.
func TestBubbleDatagramIsFragmentedOrDropped(t *testing.T) {
	// udp is a datagram of 20 KB: with the Don't Fragment flag if df, and, if partial, with
	// the pseudo-header sum in the checksum field as the bubble's kernel hands it over. It
	// returns the datagram complete and as sent.
	udp := func(df, partial bool) (whole, sent []byte) {
		ip := packettest.UDP(testOutside, 5353, "10.244.1.6", 53, string(payloadOf(20000)), false)
		if df {
			ip[6] |= 0x40
			sumFrame(ip)
		}
		whole = append([]byte(nil), ip...)
		if partial {
			packettest.Partial(ip)
		}
		return whole, ip
	}
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete checksum", true: "checksum to complete"}[partial], func(t *testing.T) {
			r := newUplinkRig(t, testUplinkMTU)
			whole, sent := udp(false, partial)
			rec := r.bubbleRecord(sent)
			if partial {
				packet.VnetHeader{Flags: packet.VnetNeedsCsum, CsumStart: 34, CsumOffset: 6}.Put(rec)
			}
			before := Offloads()
			if _, err := r.bubblePod.Write(rec); err != nil {
				t.Fatal(err)
			}
			recs := readQuiet(r.cPd)
			type frag struct {
				off  int
				data []byte
				more bool
			}
			var frags []frag
			for _, rec := range recs {
				fr, err := packet.ParseEthernet(rec[packet.VnetHeaderLen:])
				if err != nil {
					t.Fatal(err)
				}
				ip := fr.Payload
				if len(ip) > testPodMTU || packet.Checksum16(ip[:20]) != 0 || int(binary.BigEndian.Uint16(ip[2:])) != len(ip) {
					t.Fatalf("fragment of %d bytes with a bad header", len(ip))
				}
				ff := binary.BigEndian.Uint16(ip[6:])
				if ff&0x4000 != 0 || !bytes.Equal(ip[12:20], whole[12:20]) || !bytes.Equal(ip[4:6], whole[4:6]) {
					t.Fatalf("fragment with other addresses, identification or the DF flag: %x", ip[:20])
				}
				frags = append(frags, frag{int(ff&0x1fff) * 8, ip[20:], ff&0x2000 != 0})
			}
			sort.Slice(frags, func(i, j int) bool { return frags[i].off < frags[j].off })
			// Reassemble: the fragments tile the data, only the last has no More Fragments,
			// and the datagram that results is the one sent, with a good UDP checksum.
			var data []byte
			for i, f := range frags {
				if f.off != len(data) || f.more != (i < len(frags)-1) || (f.more && len(f.data)%8 != 0) {
					t.Fatalf("fragment %d at offset %d, more=%v (data so far %d)", i, f.off, f.more, len(data))
				}
				data = append(data, f.data...)
			}
			if len(frags) != 15 || !bytes.Equal(data, whole[20:]) {
				t.Fatalf("%d fragments; reassembled data differs from the datagram: %d bytes, want %d", len(frags), len(data), len(whole)-20)
			}
			if err := packettest.Verify(append(append([]byte(nil), whole[:20]...), data...)); err != nil {
				t.Errorf("reassembled: %v", err)
			}
			if d := delta(Offloads(), before); d.Fragmented != 1 || d.Fragments != 15 || d.OversizeDropped != 0 {
				t.Errorf("counters %+v", d)
			}
		})
	}

	t.Run("Don't Fragment", func(t *testing.T) {
		r := newUplinkRig(t, testUplinkMTU)
		_, df := udp(true, false)
		before := Offloads()
		if _, err := r.bubblePod.Write(r.bubbleRecord(df)); err != nil {
			t.Fatal(err)
		}
		if recs := readQuiet(r.cPd); len(recs) != 0 {
			t.Errorf("the pod got %d records", len(recs))
		}
		if d := delta(Offloads(), before); d.OversizeDropped != 1 || d.Fragmented != 0 || d.ReadDropped != 0 {
			t.Errorf("counters %+v", d)
		}
	})
	t.Run("IPv6", func(t *testing.T) {
		r := newUplinkRig(t, testUplinkMTU)
		f := &packet.EthernetFrame{DstMAC: net.HardwareAddr{2, 0, 0, 0, 0, 6}, SrcMAC: r.bubble.MAC(), EtherType: packet.EtherTypeIPv6, Payload: make([]byte, 3000)}
		f.Payload[0], f.Payload[6] = 0x60, 17
		binary.BigEndian.PutUint16(f.Payload[4:], 3000-40)
		before := Offloads()
		if _, err := r.bubblePod.Write(append(make([]byte, packet.VnetHeaderLen), mustMarshal(f)...)); err != nil {
			t.Fatal(err)
		}
		if recs := readQuiet(r.cPd); len(recs) != 0 {
			t.Errorf("the pod got %d records", len(recs))
		}
		if d := delta(Offloads(), before); d.OversizeDropped != 1 {
			t.Errorf("counters %+v", d)
		}
	})
}

// Frames that fit the pod MTU are read as they were: the same bytes, whatever the uplink
// MTU, and an uplink MTU that is not above the pod MTU changes nothing at all.
func TestBubbleFramesAtThePodMTUAreUnchanged(t *testing.T) {
	for _, uplink := range []int{testUplinkMTU, testPodMTU} {
		r := newUplinkRig(t, uplink)
		ip := packettest.TCP(testOutside, 443, "10.244.1.6", 80, packet.TCPAck, string(payloadOf(testPodMTU-40)))
		if len(ip) != testPodMTU {
			t.Fatalf("test packet of %d bytes", len(ip))
		}
		rec := r.bubbleRecord(ip)
		before := Offloads()
		if _, err := r.bubblePod.Write(rec); err != nil {
			t.Fatal(err)
		}
		got := readQuiet(r.cPd)
		want := append([]byte(nil), rec...)
		copy(want[packet.VnetHeaderLen:], []byte{2, 0, 0, 0, 0, 6}) // the MAC of the pod, set by the bridge
		if len(got) != 1 || !bytes.Equal(got[0], want) {
			t.Fatalf("uplink %d: the pod got %d records, or not the one sent", uplink, len(got))
		}
		if d := delta(Offloads(), before); d != (OffloadStats{}) {
			t.Errorf("uplink %d: counters %+v", uplink, d)
		}
	}
}

// A TSO segment that a pod sends to the outside and that fits the uplink MTU is written
// to the bubble as one ordinary packet: no virtio header, right lengths and checksums,
// the payload and the flags of the segment. One that does not fit is written with its GSO
// header, like every segment if the uplink MTU is the pod MTU.
func TestPodSegmentIsJoinedForTheBubble(t *testing.T) {
	gw := net.HardwareAddr{2, 0, 0, 0, 0, 1}
	aMAC := net.HardwareAddr{2, 0, 0, 0, 0, 4}
	for _, flags := range []byte{packet.TCPAck, packet.TCPAck | 0x08, packet.TCPAck | 0x08 | packet.TCPFin, packet.TCPAck | 0x80} {
		r := newUplinkRig(t, testUplinkMTU)
		const size, data = testSegment, 40 * testSegment
		before := Offloads()
		if _, err := r.aPod.Write(tsoRecord(aMAC, gw, "10.244.1.4", "8.8.8.8", size, data, flags)); err != nil {
			t.Fatal(err)
		}
		recs := readQuiet(r.bubblePod)
		if len(recs) != 1 {
			t.Fatalf("flags %#x: the bubble got %d records", flags, len(recs))
		}
		if h := packet.ParseVnetHeader(recs[0]); h != (packet.VnetHeader{}) {
			t.Errorf("flags %#x: the packet has the virtio header %+v", flags, h)
		}
		fr, err := packet.ParseEthernet(recs[0][packet.VnetHeaderLen:])
		if err != nil {
			t.Fatal(err)
		}
		ip := fr.Payload
		if len(ip) != 40+data || int(binary.BigEndian.Uint16(ip[2:])) != len(ip) {
			t.Fatalf("flags %#x: packet of %d bytes, header says %d", flags, len(ip), binary.BigEndian.Uint16(ip[2:]))
		}
		if err := packettest.Verify(ip); err != nil { // the header sum and the TCP sum, recomputed
			t.Errorf("flags %#x: %v", flags, err)
		}
		if ip[20+13] != flags || !bytes.Equal(ip[40:], payloadOf(data)) {
			t.Errorf("flags %#x: the packet has flags %#x, or other bytes", flags, ip[20+13])
		}
		if d := delta(Offloads(), before); d.JoinedForUplink != 1 || d.PassedSuperFrames != 0 || d.ReadSuperFrames != 0 {
			t.Errorf("flags %#x: counters %+v", flags, d)
		}
	}

	t.Run("one that does not fit", func(t *testing.T) {
		r := newUplinkRig(t, testUplinkMTU)
		// 65521 bytes of IP packet: one above the uplink MTU, and with its Ethernet header the
		// 65535 bytes that a read of the TAP holds.
		rec := tsoRecord(aMAC, gw, "10.244.1.4", "8.8.8.8", testSegment, testUplinkMTU+1-40, packet.TCPAck)
		want := append([]byte(nil), rec...)
		copy(want[packet.VnetHeaderLen:], r.bubble.MAC())
		before := Offloads()
		if _, err := r.aPod.Write(rec); err != nil {
			t.Fatal(err)
		}
		got := readQuiet(r.bubblePod)
		if len(got) != 1 || !bytes.Equal(got[0], want) {
			t.Fatalf("the bubble got %d records, or not the segment with its GSO header", len(got))
		}
		if d := delta(Offloads(), before); d.JoinedForUplink != 0 || d.PassedSuperFrames != 1 {
			t.Errorf("counters %+v", d)
		}
	})
	t.Run("an uplink MTU that is the pod MTU", func(t *testing.T) {
		r := newUplinkRig(t, testPodMTU)
		rec := tsoRecord(aMAC, gw, "10.244.1.4", "8.8.8.8", testSegment, 40*testSegment, packet.TCPAck)
		want := append([]byte(nil), rec...)
		copy(want[packet.VnetHeaderLen:], r.bubble.MAC())
		if _, err := r.aPod.Write(rec); err != nil {
			t.Fatal(err)
		}
		if got := readQuiet(r.bubblePod); len(got) != 1 || !bytes.Equal(got[0], want) {
			t.Fatalf("the bubble got %d records, or not the segment as the pod sent it", len(got))
		}
	})
}
