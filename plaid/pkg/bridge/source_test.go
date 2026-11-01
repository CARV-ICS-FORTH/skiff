package bridge

import (
	"encoding/binary"
	"net"
	"testing"

	"plaid/pkg/packet"
)

func sourceFrame(mac net.HardwareAddr, etherType uint16, payload []byte) []byte {
	raw := append(append(make([]byte, 6), mac...), 0, 0)
	binary.BigEndian.PutUint16(raw[12:], etherType)
	return append(raw, payload...)
}

func TestIsOwnSource(t *testing.T) {
	ip := net.IPv4(10, 244, 0, 5).To4()
	other := net.IPv4(10, 244, 0, 6).To4()
	mac := net.HardwareAddr{2, 0, 0, 0, 0, 5}
	otherMAC := net.HardwareAddr{2, 0, 0, 0, 0, 6}
	ipv4 := func(src net.IP) []byte { p := make([]byte, 40); p[0] = 0x45; copy(p[12:], src); return p }
	arp := func(senderMAC net.HardwareAddr, sender net.IP) []byte {
		p := make([]byte, 28)
		copy(p[8:], senderMAC)
		copy(p[14:], sender)
		return p
	}
	for name, c := range map[string]struct {
		raw  []byte
		want bool
	}{
		"its own IPv4":        {sourceFrame(mac, packet.EtherTypeIPv4, ipv4(ip)), true},
		"another IPv4":        {sourceFrame(mac, packet.EtherTypeIPv4, ipv4(other)), false},
		"another MAC":         {sourceFrame(otherMAC, packet.EtherTypeIPv4, ipv4(ip)), false},
		"its own ARP":         {sourceFrame(mac, packet.EtherTypeARP, arp(mac, ip)), true},
		"an address probe":    {sourceFrame(mac, packet.EtherTypeARP, arp(mac, net.IPv4zero.To4())), true},
		"ARP for another":     {sourceFrame(mac, packet.EtherTypeARP, arp(mac, other)), false},
		"ARP, another MAC":    {sourceFrame(mac, packet.EtherTypeARP, arp(otherMAC, ip)), false},
		"IPv6, its own MAC":   {sourceFrame(mac, 0x86dd, make([]byte, 60)), true},
		"IPv6, another MAC":   {sourceFrame(otherMAC, 0x86dd, make([]byte, 60)), false},
		"not an IPv4 header":  {sourceFrame(mac, packet.EtherTypeIPv4, []byte("not an IPv4 header, 20 long")), true},
		"a short IPv4 packet": {sourceFrame(mac, packet.EtherTypeIPv4, make([]byte, 10)), true},
		"another protocol":    {sourceFrame(mac, 0x88cc, make([]byte, 10)), true},
	} {
		if got := isOwnSource(ip, mac, c.raw); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// A pod's frame with another source does not reach another pod, and is counted; the bubble's do.
func TestBridgeDropsFramesWithAnotherSource(t *testing.T) {
	b := NewBridge(BridgeConfig{GatewayIP: net.IPv4(10, 244, 0, 1), GatewayMAC: net.HardwareAddr{2, 0, 0, 0, 0, 1}})
	from := newMockEndpoint("a", "a", "10.244.0.5", "02:00:00:00:00:05")
	to := newMockEndpoint("b", "b", "10.244.0.6", "02:00:00:00:00:06")
	if err := b.AddEndpoint(from); err != nil {
		t.Fatal(err)
	}
	if err := b.AddEndpoint(to); err != nil {
		t.Fatal(err)
	}
	frame := func(src net.IP) []byte {
		p := make([]byte, 40)
		p[0], p[9] = 0x45, 17
		copy(p[12:], src.To4())
		copy(p[16:], to.IP().To4())
		raw := sourceFrame(from.MAC(), packet.EtherTypeIPv4, p)
		copy(raw, to.MAC())
		return raw
	}
	if err := b.ProcessFrame(from, frame(net.IPv4(10, 244, 0, 99))); err != nil {
		t.Fatal(err)
	}
	if got := to.ReceivedCount(); got != 0 || b.SourceDropped() != 1 {
		t.Fatalf("delivered %d, dropped %d", got, b.SourceDropped())
	}
	if err := b.ProcessFrame(from, frame(from.IP())); err != nil {
		t.Fatal(err)
	}
	if got := to.ReceivedCount(); got != 1 || b.SourceDropped() != 1 {
		t.Errorf("its own: delivered %d, dropped %d", got, b.SourceDropped())
	}
}
