package packet_test

import (
	"encoding/binary"
	"math/rand"
	"net"
	"testing"

	"plaid/pkg/packet"
	"plaid/pkg/packet/packettest"
)

func TestParseL3(t *testing.T) {
	pkt := packettest.TCP("10.0.0.1", 1000, "10.0.0.2", 80, packet.TCPSyn, "")
	l, err := packet.ParseL3(append(pkt, make([]byte, 18)...)) // padding after the packet
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Pkt) != len(pkt) || l.HdrLen != 20 || l.Proto != packet.IPProtocolTCP || l.Fragment {
		t.Errorf("parsed %+v", l)
	}
	if err := l.HasPorts(); err != nil {
		t.Error(err)
	}
	if l.SrcPort() != 1000 || l.DstPort() != 80 || l.TCPFlags() != packet.TCPSyn {
		t.Errorf("ports %d -> %d flags %x", l.SrcPort(), l.DstPort(), l.TCPFlags())
	}

	bad := map[string][]byte{
		"too short":    pkt[:10],
		"not IPv4":     append([]byte{0x60}, pkt[1:]...),
		"short header": append([]byte{0x44}, pkt[1:]...),
		"total too long": func() []byte {
			p := append([]byte(nil), pkt...)
			binary.BigEndian.PutUint16(p[2:4], uint16(len(p)+1))
			return p
		}(),
		"total < header": func() []byte { p := append([]byte(nil), pkt...); binary.BigEndian.PutUint16(p[2:4], 10); return p }(),
	}
	for name, p := range bad {
		if _, err := packet.ParseL3(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFragmentsAndTruncatedTransportHaveNoPorts(t *testing.T) {
	pkt := packettest.UDP("10.0.0.1", 1, "10.0.0.2", 2, "data", false)
	for _, ff := range []uint16{0x2000, 0x0001, 0x2001} { // more fragments, offset, both
		p := append([]byte(nil), pkt...)
		binary.BigEndian.PutUint16(p[6:8], ff)
		l, err := packet.ParseL3(p)
		if err != nil || !l.Fragment || l.HasPorts() == nil {
			t.Errorf("flags/offset %#x: fragment=%v err=%v", ff, l.Fragment, err)
		}
	}
	short := packettest.TCP("10.0.0.1", 1, "10.0.0.2", 2, 0, "")
	binary.BigEndian.PutUint16(short[2:4], 30) // TCP header cut to 10 bytes
	if l, err := packet.ParseL3(short); err != nil || l.HasPorts() == nil {
		t.Errorf("truncated TCP header accepted: %v", err)
	}
}

func addr(r *rand.Rand) *[4]byte {
	var a [4]byte
	r.Read(a[:])
	return &a
}

func TestRewriteKeepsChecksumsValid(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 5000; i++ {
		payload := string(make([]byte, r.Intn(120))) // odd and even lengths
		var pkt []byte
		switch i % 3 {
		case 0:
			pkt = packettest.TCP("10.244.1.5", uint16(r.Intn(65535)+1), "10.43.0.10", 53, packet.TCPAck, payload)
		case 1:
			pkt = packettest.UDP("10.244.1.5", uint16(r.Intn(65535)+1), "10.43.0.10", 53, payload, false)
		default:
			pkt = packettest.UDP("10.244.1.5", uint16(r.Intn(65535)+1), "10.43.0.10", 53, payload, true)
		}
		l, err := packet.ParseL3(pkt)
		if err != nil {
			t.Fatal(err)
		}
		src, dst := addr(r), addr(r)
		sport, dport := uint16(r.Intn(65535)+1), uint16(r.Intn(65535)+1)
		l.Rewrite(src, dst, &sport, &dport)
		if err := packettest.Verify(pkt); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		got, _ := packettest.Parse(pkt)
		want := packettest.Tuple{Src: net.IP(src[:]).String(), Sport: sport, Dst: net.IP(dst[:]).String(), Dport: dport}
		if got != want {
			t.Fatalf("tuple %+v, want %+v", got, want)
		}
		if i%3 == 2 && binary.BigEndian.Uint16(pkt[20+6:20+8]) != 0 {
			t.Fatal("a UDP datagram without a checksum was given one")
		}
	}
}

func TestRewriteChangesOnlyWhatIsAsked(t *testing.T) {
	pkt := packettest.TCP("10.244.1.5", 4000, "10.43.0.10", 53, packet.TCPAck, "hello")
	orig := append([]byte(nil), pkt...)
	l, _ := packet.ParseL3(pkt)
	dst := [4]byte{10, 244, 2, 7}
	port := uint16(8053)
	l.Rewrite(nil, &dst, nil, &port)
	if err := packettest.Verify(pkt); err != nil {
		t.Fatal(err)
	}
	got, _ := packettest.Parse(pkt)
	if got.Src != "10.244.1.5" || got.Sport != 4000 || got.Dst != "10.244.2.7" || got.Dport != 8053 {
		t.Errorf("tuple %+v", got)
	}
	// Everything outside the addresses, the destination port and the two checksums is untouched.
	for i := range pkt {
		switch {
		case i >= 10 && i < 12, i >= 16 && i < 20, i == 22, i == 23, i == 36, i == 37:
		default:
			if pkt[i] != orig[i] {
				t.Errorf("byte %d changed: %#x -> %#x", i, orig[i], pkt[i])
			}
		}
	}
}

// A rewrite that would leave a UDP checksum of zero sends 0xffff instead: zero means
// "no checksum" on the wire.
func TestUDPChecksumIsNeverRewrittenToZero(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	hits := 0
	for i := 0; i < 400000 && hits < 3; i++ {
		pkt := packettest.UDP("10.0.0.1", 1000, "10.43.0.1", 53, "x", false)
		l, _ := packet.ParseL3(pkt)
		sport := uint16(r.Intn(65535) + 1)
		l.Rewrite(nil, nil, &sport, nil)
		sum := binary.BigEndian.Uint16(pkt[26:28])
		if sum == 0 {
			t.Fatal("a checksummed UDP datagram ended up with checksum zero")
		}
		if sum == 0xffff {
			hits++
		}
		if err := packettest.Verify(pkt); err != nil {
			t.Fatal(err)
		}
	}
	if hits == 0 {
		t.Skip("no rewrite produced the all-ones checksum in this run")
	}
}

func TestRewriteWithIPOptions(t *testing.T) {
	base := packettest.UDP("10.0.0.1", 1000, "10.43.0.1", 53, "data", false)
	// Insert four bytes of options: header length 24.
	pkt := append(append(append([]byte(nil), base[:20]...), 1, 1, 1, 0), base[20:]...)
	pkt[0] = 0x46
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	binary.BigEndian.PutUint16(pkt[10:12], 0)
	binary.BigEndian.PutUint16(pkt[10:12], packet.Checksum16(pkt[:24]))
	l, err := packet.ParseL3(pkt)
	if err != nil || l.HdrLen != 24 {
		t.Fatalf("parse: %v %+v", err, l)
	}
	dst := [4]byte{10, 244, 2, 3}
	l.Rewrite(nil, &dst, nil, nil)
	if err := packettest.Verify(pkt); err != nil {
		t.Fatal(err)
	}
}

func TestICMPDestUnreachable(t *testing.T) {
	orig := packettest.TCP("10.244.1.5", 4000, "10.43.0.10", 53, packet.TCPSyn, "payload")
	l, _ := packet.ParseL3(orig)
	out := packet.ICMPDestUnreachable(l, net.ParseIP("10.43.0.10"), packet.ICMPPortUnreachable)
	r, err := packet.ParseL3(out)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Checksum16(out[:20]) != 0 || packet.Checksum16(out[20:]) != 0 {
		t.Error("bad checksum in the ICMP error")
	}
	if net.IP(r.Src[:]).String() != "10.43.0.10" || net.IP(r.Dst[:]).String() != "10.244.1.5" || r.Proto != packet.IPProtocolICMP {
		t.Errorf("addresses %v -> %v proto %d", net.IP(r.Src[:]), net.IP(r.Dst[:]), r.Proto)
	}
	icmp := out[20:]
	if icmp[0] != 3 || icmp[1] != 3 {
		t.Errorf("type/code %d/%d", icmp[0], icmp[1])
	}
	if string(icmp[8:]) != string(orig[:28]) { // the IP header and eight bytes of TCP
		t.Error("the original packet is not quoted")
	}
}

func TestChecksum16(t *testing.T) {
	// The worked example of RFC 1071 section 3.
	data := []byte{0x00, 0x01, 0xf2, 0x03, 0xf4, 0xf5, 0xf6, 0xf7}
	if got := packet.Checksum16(data); got != ^uint16(0xddf2) {
		t.Errorf("checksum = %#x", got)
	}
}
