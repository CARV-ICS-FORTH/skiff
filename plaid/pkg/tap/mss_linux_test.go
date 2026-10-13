//go:build linux

package tap

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"testing"

	"plaid/pkg/packet"
	"plaid/pkg/packet/packettest"
)

// synPacket builds an IPv4 TCP packet with the flags and an MSS option, from sport to
// dport, with a complete checksum, unless partial: then the field holds the sum of the
// pseudo-header, as a pod's stack leaves it for the TAP to complete.
func synPacket(src string, sport uint16, dst string, dport uint16, flags byte, mss uint16, partial bool) []byte {
	tcp := make([]byte, 24)
	binary.BigEndian.PutUint16(tcp[0:], sport)
	binary.BigEndian.PutUint16(tcp[2:], dport)
	binary.BigEndian.PutUint32(tcp[4:], 1000)
	tcp[12], tcp[13] = 6<<4, flags
	binary.BigEndian.PutUint16(tcp[14:], 65535)
	copy(tcp[20:], []byte{2, 4, byte(mss >> 8), byte(mss)})
	ip := make([]byte, 20+len(tcp))
	ip[0], ip[8], ip[9] = 0x45, 64, packet.IPProtocolTCP
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	copy(ip[12:], net.ParseIP(src).To4())
	copy(ip[16:], net.ParseIP(dst).To4())
	copy(ip[20:], tcp)
	sumFrame(ip)
	if partial {
		packettest.Partial(ip)
	}
	return ip
}

// mssOf is the MSS option of the packet made by synPacket.
func mssOf(ip []byte) uint16 { return binary.BigEndian.Uint16(ip[20+22:]) }

// plainPodRecord is what a pod's stack writes for the IP packet ip, to the MAC dst: an empty
// virtio header, or, if partial, one that leaves the TCP checksum to the TAP.
func plainPodRecord(src, dst net.HardwareAddr, ip []byte, partial bool) []byte {
	rec := make([]byte, packet.VnetHeaderLen)
	if partial {
		packet.VnetHeader{Flags: packet.VnetNeedsCsum, CsumStart: 34, CsumOffset: 16}.Put(rec)
	}
	f := &packet.EthernetFrame{DstMAC: dst, SrcMAC: src, EtherType: packet.EtherTypeIPv4, Payload: ip}
	return append(rec, mustMarshal(f)...)
}

// ipOf is the IP packet of the only record in recs.
func ipOf(t *testing.T, recs [][]byte) []byte {
	t.Helper()
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	if h := packet.ParseVnetHeader(recs[0]); h != (packet.VnetHeader{}) {
		t.Fatalf("a handshake with the virtio header %+v", h)
	}
	return recs[0][packet.VnetHeaderLen+packet.EthernetHeaderLen:]
}

// The handshake of a pod to the bubble, SYN or SYN-ACK, gets the uplink's MSS: the
// bubble's stack then sends large packets, which plaidd makes TSO segments of the pod's MSS
// for the pod. It does for a handshake the pod left the checksum of to the TAP too (the
// reader completes it first), the checksum is right afterwards, and nothing else changes.
// With an uplink MTU that is the pod MTU nothing is changed.
func TestPodHandshakeToTheBubbleIsRaised(t *testing.T) {
	aMAC := net.HardwareAddr{2, 0, 0, 0, 0, 4}
	for _, flags := range []byte{packet.TCPSyn, packet.TCPSyn | packet.TCPAck} {
		for _, partial := range []bool{false, true} {
			r := newUplinkRig(t, testUplinkMTU)
			sent := synPacket("10.244.1.4", 40000, testOutside, 443, flags, 1410, partial)
			before := Offloads()
			if _, err := r.aPod.Write(plainPodRecord(aMAC, gatewayMAC, sent, partial)); err != nil {
				t.Fatal(err)
			}
			got := ipOf(t, readQuiet(r.bubblePod))
			if err := packettest.Verify(got); err != nil {
				t.Errorf("flags %#x partial %v: %v", flags, partial, err)
			}
			if mssOf(got) != testUplinkMTU-40 {
				t.Errorf("flags %#x partial %v: MSS %d, want %d", flags, partial, mssOf(got), testUplinkMTU-40)
			}
			want := synPacket("10.244.1.4", 40000, testOutside, 443, flags, testUplinkMTU-40, false)
			if !bytes.Equal(got, want) {
				t.Errorf("flags %#x partial %v: more than the MSS and the checksum changed", flags, partial)
			}
			if d := delta(Offloads(), before); d.MSSRaised != 1 || d.MSSLowered != 0 {
				t.Errorf("counters %+v", d)
			}
		}
	}

	t.Run("an MSS that is large enough", func(t *testing.T) {
		r := newUplinkRig(t, testUplinkMTU)
		sent := synPacket("10.244.1.4", 40000, testOutside, 443, packet.TCPSyn, 65480, false)
		before := Offloads()
		if _, err := r.aPod.Write(plainPodRecord(aMAC, gatewayMAC, sent, false)); err != nil {
			t.Fatal(err)
		}
		if got := ipOf(t, readQuiet(r.bubblePod)); !bytes.Equal(got, sent) {
			t.Error("the handshake changed")
		}
		if d := delta(Offloads(), before); d.MSSRaised != 0 {
			t.Errorf("counters %+v", d)
		}
	})
	t.Run("an uplink MTU that is the pod MTU", func(t *testing.T) {
		r := newUplinkRig(t, testPodMTU)
		sent := synPacket("10.244.1.4", 40000, testOutside, 443, packet.TCPSyn, 1410, false)
		before := Offloads()
		if _, err := r.aPod.Write(plainPodRecord(aMAC, gatewayMAC, sent, false)); err != nil {
			t.Fatal(err)
		}
		if got := ipOf(t, readQuiet(r.bubblePod)); !bytes.Equal(got, sent) {
			t.Error("the handshake changed")
		}
		if d := delta(Offloads(), before); d.MSSRaised != 0 {
			t.Errorf("counters %+v", d)
		}
	})
}

// A bubble that has no offloads is written frame by frame (TapEndpoint.Write), which
// raises the MSS as well.
func TestHandshakeIsRaisedByAWriteToTheBubble(t *testing.T) {
	bubble, bubblePod := pipeTap(t, "bubble", 9, false, 0)
	bubble.offload = false
	bubble.SetUplinkMTU(testPodMTU, testUplinkMTU)
	sent := synPacket("10.244.1.4", 40000, testOutside, 443, packet.TCPSyn|packet.TCPAck, 1410, false)
	before := Offloads()
	f := &packet.EthernetFrame{DstMAC: bubble.MAC(), SrcMAC: gatewayMAC, EtherType: packet.EtherTypeIPv4, Payload: sent}
	frame := mustMarshal(f)
	if err := bubble.Write(frame); err != nil {
		t.Fatal(err)
	}
	got := ipOf(t, readQuiet(bubblePod))
	if err := packettest.Verify(got); err != nil || mssOf(got) != testUplinkMTU-40 {
		t.Errorf("MSS %d, %v", mssOf(got), err)
	}
	if !bytes.Equal(frame, mustMarshal(f)) {
		t.Error("Write changed the frame it was given")
	}
	if d := delta(Offloads(), before); d.MSSRaised != 1 {
		t.Errorf("counters %+v", d)
	}
}

// The handshake that the bubble sends to a pod with offloads is lowered to the MSS of an
// Ethernet MTU of 1500; not to a pod without offloads, a pod on another node, or when it
// is below that; and not a handshake between pods.
func TestBubbleHandshakeToAPodIsLowered(t *testing.T) {
	r := newUplinkRig(t, testUplinkMTU)
	bubbleSYN := func(dst string, mss uint16) []byte {
		return r.bubbleRecord(synPacket(testOutside, 5555, dst, 80, packet.TCPSyn, mss, false))
	}

	before := Offloads()
	if _, err := r.bubblePod.Write(bubbleSYN("10.244.1.5", 65480)); err != nil {
		t.Fatal(err)
	}
	got := ipOf(t, readQuiet(r.bPod))
	if err := packettest.Verify(got); err != nil || mssOf(got) != 1460 {
		t.Errorf("a pod with offloads: MSS %d, %v; want 1460", mssOf(got), err)
	}
	if want := synPacket(testOutside, 5555, "10.244.1.5", 80, packet.TCPSyn, 1460, false); !bytes.Equal(got, want) {
		t.Error("more than the MSS and the checksum changed")
	}
	if d := delta(Offloads(), before); d.MSSLowered != 1 || d.MSSRaised != 0 {
		t.Errorf("counters %+v", d)
	}

	before = Offloads()
	if _, err := r.bubblePod.Write(bubbleSYN("10.244.1.5", 1410)); err != nil {
		t.Fatal(err)
	}
	if got := ipOf(t, readQuiet(r.bPod)); mssOf(got) != 1410 {
		t.Errorf("an MSS below 1460: %d", mssOf(got))
	}
	if d := delta(Offloads(), before); d.MSSLowered != 0 {
		t.Errorf("counters %+v", d)
	}

	if _, err := r.bubblePod.Write(bubbleSYN("10.244.1.6", 65480)); err != nil {
		t.Fatal(err)
	}
	if got := ipOf(t, readQuiet(r.cPd)); mssOf(got) != 65480 {
		t.Errorf("a pod without offloads: MSS %d, want it as it was", mssOf(got))
	}

	if _, err := r.bubblePod.Write(bubbleSYN(testRemote, 65480)); err != nil {
		t.Fatal(err)
	}
	readQuiet(r.bPod)
	fr := r.overlayFrames()
	if len(fr) != 1 {
		t.Fatalf("%d frames over the overlay", len(fr))
	}
	if mssOf(fr[0][packet.EthernetHeaderLen:]) != 65480 {
		t.Errorf("a pod on another node: MSS %d, want it as it was", mssOf(fr[0][packet.EthernetHeaderLen:]))
	}
	if d := delta(Offloads(), before); d.MSSLowered != 0 {
		t.Errorf("counters %+v", d)
	}

	// A handshake from a pod to a pod is the pods' business.
	if _, err := r.aPod.Write(plainPodRecord(net.HardwareAddr{2, 0, 0, 0, 0, 4}, gatewayMAC, synPacket("10.244.1.4", 40000, "10.244.1.5", 80, packet.TCPSyn, 8910, false), false)); err != nil {
		t.Fatal(err)
	}
	if got := ipOf(t, readQuiet(r.bPod)); mssOf(got) != 8910 {
		t.Errorf("a handshake between pods: MSS %d, want 8910", mssOf(got))
	}
	if d := delta(Offloads(), before); d.MSSLowered != 0 || d.MSSRaised != 0 {
		t.Errorf("counters %+v", d)
	}
}

// A NodePort connection through the Service engine: slirp4netns's SYN to the Service's IP
// reaches the backend with the MSS lowered, and the backend's SYN-ACK, which comes back
// through the engine (translated to the client's address), reaches the bubble with the
// MSS raised. Both counted.
func TestHandshakeThroughTheServiceEngine(t *testing.T) {
	r := newUplinkRig(t, testUplinkMTU)
	before := Offloads()
	if _, err := r.bubblePod.Write(r.bubbleRecord(synPacket(testOutside, 5555, testVIP, 80, packet.TCPSyn, testUplinkMTU-40, false))); err != nil {
		t.Fatal(err)
	}
	syn := ipOf(t, readQuiet(r.bPod))
	if err := packettest.Verify(syn); err != nil {
		t.Fatal(err)
	}
	tu, _ := packettest.Parse(syn)
	if tu.Src != testSNAT || tu.Dst != "10.244.1.5" || tu.Dport != 8080 || mssOf(syn) != 1460 {
		t.Fatalf("the backend got %+v with MSS %d", tu, mssOf(syn))
	}

	reply := synPacket("10.244.1.5", 8080, testSNAT, tu.Sport, packet.TCPSyn|packet.TCPAck, 1410, false)
	if _, err := r.bPod.Write(plainPodRecord(net.HardwareAddr{2, 0, 0, 0, 0, 5}, gatewayMAC, reply, false)); err != nil {
		t.Fatal(err)
	}
	back := ipOf(t, readQuiet(r.bubblePod))
	if err := packettest.Verify(back); err != nil {
		t.Fatal(err)
	}
	tu, _ = packettest.Parse(back)
	if tu.Src != testVIP || tu.Sport != 80 || tu.Dst != testOutside || tu.Dport != 5555 || mssOf(back) != testUplinkMTU-40 {
		t.Fatalf("the bubble got %+v with MSS %d", tu, mssOf(back))
	}
	if d := delta(Offloads(), before); d.MSSLowered != 1 || d.MSSRaised != 1 {
		t.Errorf("counters %+v", d)
	}
}

// syn6Packet is an IPv6 TCP SYN with an MSS option and a complete checksum.
func syn6Packet(mss uint16) []byte {
	tcp := make([]byte, 24)
	binary.BigEndian.PutUint16(tcp[0:], 40000)
	binary.BigEndian.PutUint16(tcp[2:], 443)
	tcp[12], tcp[13] = 6<<4, packet.TCPSyn
	binary.BigEndian.PutUint16(tcp[14:], 65535)
	copy(tcp[20:], []byte{2, 4, byte(mss >> 8), byte(mss)})
	ip := make([]byte, 40)
	ip[0], ip[6], ip[7] = 0x60, packet.IPProtocolTCP, 64
	binary.BigEndian.PutUint16(ip[4:], uint16(len(tcp)))
	ip[8+15], ip[24+15] = 4, 77
	ph := append(append([]byte(nil), ip[8:40]...), 0, 0, 0, byte(len(tcp)), 0, 0, 0, packet.IPProtocolTCP)
	binary.BigEndian.PutUint16(tcp[16:], packet.Checksum16(append(ph, tcp...)))
	return append(ip, tcp...)
}

// An IPv6 frame to a MAC that nobody has reaches the bridge's flood, which writes the one
// buffer to every other endpoint in turn: the bubble's raised MSS must stay out of what the
// endpoints written after it receive, whatever the order. Rounds repeat as the order of a
// flood is not fixed. (plaidd does not register the bubble with the bridge, so a flood does
// not reach it there; the test does, to check that Write leaves its argument alone.)
func TestFloodedHandshakeKeepsItsMSSForThePods(t *testing.T) {
	r := newUplinkRig(t, testUplinkMTU)
	if err := r.br.AddEndpoint(r.bubble); err != nil {
		t.Fatal(err)
	}
	aMAC := net.HardwareAddr{2, 0, 0, 0, 0, 4}
	const rounds = 8
	syn := syn6Packet(1410)
	f := &packet.EthernetFrame{DstMAC: net.HardwareAddr{2, 0, 0, 0, 0, 77}, SrcMAC: aMAC, EtherType: packet.EtherTypeIPv6, Payload: syn}
	rec := append(make([]byte, packet.VnetHeaderLen), mustMarshal(f)...)
	for i := 0; i < rounds; i++ {
		if _, err := r.aPod.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	mssOf6 := func(rec []byte) uint16 {
		return binary.BigEndian.Uint16(rec[packet.VnetHeaderLen+packet.EthernetHeaderLen+40+22:])
	}
	for name, tc := range map[string]struct {
		pod  *os.File
		want uint16
	}{"pod b": {r.bPod, 1410}, "pod c": {r.cPd, 1410}, "the bubble": {r.bubblePod, testUplinkMTU - 60}} {
		recs := readQuiet(tc.pod)
		if len(recs) != rounds {
			t.Errorf("%s got %d frames, want %d", name, len(recs), rounds)
		}
		for _, rec := range recs {
			if got := mssOf6(rec); got != tc.want {
				t.Errorf("%s got MSS %d, want %d", name, got, tc.want)
			}
		}
	}
}
