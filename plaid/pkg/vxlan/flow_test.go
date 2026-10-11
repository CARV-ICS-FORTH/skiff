package vxlan

import (
	"encoding/binary"
	"math/rand"
	"testing"

	"plaid/pkg/packet"
)

// ipv4Packet is an IPv4 packet of proto from src to dst with n bytes after its header;
// for TCP and UDP the first four are the ports. frag sets the fragment offset field.
func ipv4Packet(proto byte, src, dst [4]byte, sport, dport uint16, n int, frag uint16) []byte {
	ip := make([]byte, 20+n)
	ip[0], ip[8], ip[9] = 0x45, 64, proto
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	binary.BigEndian.PutUint16(ip[6:], frag)
	copy(ip[12:], src[:])
	copy(ip[16:], dst[:])
	binary.BigEndian.PutUint16(ip[20:], sport)
	binary.BigEndian.PutUint16(ip[22:], dport)
	return ip
}

func ipv6Packet(next byte, src, dst [16]byte, sport, dport uint16, n int) []byte {
	ip := make([]byte, 40+n)
	ip[0], ip[6], ip[7] = 0x60, next, 64
	binary.BigEndian.PutUint16(ip[4:], uint16(n))
	copy(ip[8:], src[:])
	copy(ip[24:], dst[:])
	binary.BigEndian.PutUint16(ip[40:], sport)
	binary.BigEndian.PutUint16(ip[42:], dport)
	return ip
}

var (
	podA = [4]byte{10, 244, 1, 4}
	podB = [4]byte{10, 244, 2, 9}
)

func TestFlowSocketKeepsAFlowOnOneSocket(t *testing.T) {
	const m = 16
	// Every segment of a TCP flow, whatever its size, flags or sequence number.
	want := flowSocket(packet.EtherTypeIPv4, ipv4Packet(packet.IPProtocolTCP, podA, podB, 4000, 80, 60, 0), m)
	for _, n := range []int{20, 21, 1000, 1398 + 20, 8900} {
		ip := ipv4Packet(packet.IPProtocolTCP, podA, podB, 4000, 80, n, 0)
		ip[4], ip[5], ip[24], ip[33] = byte(n), byte(n>>8), byte(n), 0x18 // IP ID, sequence number, flags
		if got := flowSocket(packet.EtherTypeIPv4, ip, m); got != want {
			t.Errorf("segment of %d bytes: socket %d, want %d", n, got, want)
		}
	}

	// The segments a super-frame is cut into are a flow's segments like any other.
	const size, segments = 1000, 5
	super := ipv4Packet(packet.IPProtocolTCP, podA, podB, 4000, 80, 20+size*segments, 0)
	super[32] = 5 << 4 // the TCP header's length
	frame := append(make([]byte, packet.EthernetHeaderLen), super...)
	frame[12], frame[13] = 0x08, 0x00
	h := packet.VnetHeader{Flags: packet.VnetNeedsCsum, GSOType: packet.VnetGSOTCPv4,
		HdrLen: packet.EthernetHeaderLen + 40, GSOSize: size, CsumStart: packet.EthernetHeaderLen + 20, CsumOffset: 16}
	var sg packet.Segmenter
	cut, err := sg.Segment(h, frame)
	if err != nil || len(cut) != segments {
		t.Fatalf("cutting the super-frame: %d frames, %v", len(cut), err)
	}
	for i, f := range cut {
		if got := flowSocket(packet.EtherTypeIPv4, f[packet.EthernetHeaderLen:], m); got != want {
			t.Errorf("segment %d of a super-frame: socket %d, want %d", i, got, want)
		}
	}

	// IPv6 likewise.
	var a6, b6 [16]byte
	a6[0], a6[15], b6[0], b6[15] = 0xfd, 4, 0xfd, 9
	want6 := flowSocket(packet.EtherTypeIPv6, ipv6Packet(packet.IPProtocolTCP, a6, b6, 4000, 80, 60), m)
	for _, n := range []int{20, 1000, 8900} {
		if got := flowSocket(packet.EtherTypeIPv6, ipv6Packet(packet.IPProtocolTCP, a6, b6, 4000, 80, n), m); got != want6 {
			t.Errorf("IPv6 segment of %d bytes: socket %d, want %d", n, got, want6)
		}
	}
}

func TestFlowSocketFragmentsAndOtherProtocols(t *testing.T) {
	const m = 16
	// The fragments of one datagram go together, though only the first has ports: the
	// flow of a fragment is its addresses and protocol.
	first := ipv4Packet(packet.IPProtocolUDP, podA, podB, 5000, 6000, 1400, 0x2000)
	rest := ipv4Packet(packet.IPProtocolUDP, podA, podB, 0xabcd, 0x1234, 600, 0x00b9)
	last := ipv4Packet(packet.IPProtocolUDP, podA, podB, 0x7777, 0x8888, 100, 0x0172)
	f1, f2, f3 := flowSocket(packet.EtherTypeIPv4, first, m), flowSocket(packet.EtherTypeIPv4, rest, m), flowSocket(packet.EtherTypeIPv4, last, m)
	if f1 != f2 || f2 != f3 {
		t.Errorf("fragments of one datagram on sockets %d, %d, %d", f1, f2, f3)
	}
	// And the same for IPv6, where a fragment header makes the protocol 44.
	var a6, b6 [16]byte
	a6[15], b6[15] = 1, 2
	g1 := flowSocket(packet.EtherTypeIPv6, ipv6Packet(44, a6, b6, 5000, 6000, 1400), m)
	g2 := flowSocket(packet.EtherTypeIPv6, ipv6Packet(44, a6, b6, 1, 2, 600), m)
	if g1 != g2 {
		t.Errorf("IPv6 fragments on sockets %d and %d", g1, g2)
	}

	// A protocol without ports is its addresses and protocol: its payload does not matter,
	// and other addresses make another flow.
	icmp := func(src, dst [4]byte, id uint16) []byte {
		return ipv4Packet(packet.IPProtocolICMP, src, dst, id, id+1, 56, 0)
	}
	if flowSocket(packet.EtherTypeIPv4, icmp(podA, podB, 1), m) != flowSocket(packet.EtherTypeIPv4, icmp(podA, podB, 999), m) {
		t.Error("ICMP between the same addresses went to different sockets")
	}
	used := map[int]bool{}
	for i := 0; i < 200; i++ {
		used[flowSocket(packet.EtherTypeIPv4, icmp(podA, [4]byte{10, 244, 2, byte(i)}, 1), m)] = true
	}
	if len(used) < m/2 {
		t.Errorf("ICMP to 200 addresses used %d of %d sockets", len(used), m)
	}

	// Anything else goes to the first socket, whatever the bytes, and so does a packet cut short.
	arp := make([]byte, 28)
	arp[7] = 1
	for _, c := range []struct {
		name string
		typ  uint16
		ip   []byte
	}{
		{"ARP", packet.EtherTypeARP, arp},
		{"an unknown type", 0x88cc, ipv4Packet(6, podA, podB, 1, 2, 20, 0)},
		{"a short IPv4 packet", packet.EtherTypeIPv4, ipv4Packet(6, podA, podB, 1, 2, 20, 0)[:19]},
		{"IPv4 that says it is IPv6", packet.EtherTypeIPv4, ipv6Packet(6, [16]byte{1}, [16]byte{2}, 1, 2, 20)},
		{"a short IPv6 packet", packet.EtherTypeIPv6, ipv6Packet(6, [16]byte{1}, [16]byte{2}, 1, 2, 20)[:39]},
		{"nothing", packet.EtherTypeIPv4, nil},
	} {
		if got := flowSocket(c.typ, c.ip, m); got != 0 {
			t.Errorf("%s: socket %d, want 0", c.name, got)
		}
	}
	// TCP cut short before its ports: addresses and protocol.
	if got, want := flowSocket(packet.EtherTypeIPv4, ipv4Packet(6, podA, podB, 1, 2, 4, 0)[:22], m),
		flowSocket(packet.EtherTypeIPv4, ipv4Packet(6, podA, podB, 3, 4, 4, 0)[:22], m); got != want {
		t.Errorf("TCP without its ports: sockets %d and %d for one pair of addresses", got, want)
	}
	// One socket is always the first.
	if got := flowSocket(packet.EtherTypeIPv4, ipv4Packet(6, podA, podB, 1, 2, 20, 0), 1); got != 0 {
		t.Errorf("with one socket: %d", got)
	}
}

func TestFlowSocketSpreadsFlows(t *testing.T) {
	for _, m := range []int{4, 16, 64} {
		for _, v6 := range []bool{false, true} {
			rng := rand.New(rand.NewSource(7))
			count := make([]int, m)
			const flows = 1000
			for i := 0; i < flows; i++ {
				sport, dport := uint16(1024+rng.Intn(60000)), uint16(1+rng.Intn(65000))
				if v6 {
					var a, b [16]byte
					a[0], a[15], b[0], b[14], b[15] = 0xfd, byte(rng.Intn(250)), 0xfd, byte(rng.Intn(250)), byte(rng.Intn(250))
					count[flowSocket(packet.EtherTypeIPv6, ipv6Packet(packet.IPProtocolTCP, a, b, sport, dport, 20), m)]++
					continue
				}
				src := [4]byte{10, 244, byte(rng.Intn(4)), byte(1 + rng.Intn(250))}
				dst := [4]byte{10, 244, byte(4 + rng.Intn(4)), byte(1 + rng.Intn(250))}
				count[flowSocket(packet.EtherTypeIPv4, ipv4Packet(packet.IPProtocolTCP, src, dst, sport, dport, 20, 0), m)]++
			}
			mean := float64(flows) / float64(m)
			for sock, n := range count {
				if n == 0 && m <= 16 {
					t.Errorf("m=%d v6=%v: socket %d unused: %v", m, v6, sock, count)
				}
				if float64(n) > 2*mean {
					t.Errorf("m=%d v6=%v: socket %d has %d flows, mean %.1f: %v", m, v6, sock, n, mean, count)
				}
			}
		}
	}
}

func TestFlowSocketAllocatesNothing(t *testing.T) {
	v4 := ipv4Packet(packet.IPProtocolTCP, podA, podB, 4000, 80, 1400, 0)
	v6 := ipv6Packet(packet.IPProtocolUDP, [16]byte{1}, [16]byte{2}, 4000, 80, 1400)
	sink := 0
	if n := testing.AllocsPerRun(100, func() {
		sink += flowSocket(packet.EtherTypeIPv4, v4, 16) + flowSocket(packet.EtherTypeIPv6, v6, 16) + flowSocket(packet.EtherTypeARP, v4, 16)
	}); n != 0 {
		t.Errorf("%v allocations per call", n)
	}
	_ = sink
}
