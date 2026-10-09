package bridge

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"plaid/pkg/packet"
)

func tapTestBridge(t *testing.T) (*Bridge, *mockEndpoint, *mockEndpoint, *mockEndpoint) {
	t.Helper()
	_, node, _ := net.ParseCIDR("10.244.1.0/24")
	_, cluster, _ := net.ParseCIDR("10.244.0.0/16")
	router, _ := net.ParseMAC("02:00:00:00:00:01")
	b := NewBridge(BridgeConfig{GatewayMode: "tap", GatewayIP: net.ParseIP("10.244.1.1"), GatewayMAC: router, NodeCIDR: node, ClusterCIDR: cluster})
	pod := newMockEndpoint("p1", "p1", "10.244.1.4", "02:00:00:00:01:04")
	peer := newMockEndpoint("p2", "p2", "10.244.1.5", "02:00:00:00:01:05")
	bubble := newMockEndpoint("bubble", "plaid0", "10.244.1.1", "02:00:00:00:00:02")
	if err := b.AddEndpoint(pod); err != nil {
		t.Fatal(err)
	}
	if err := b.AddEndpoint(peer); err != nil {
		t.Fatal(err)
	}
	b.SetBubbleEndpoint(bubble)
	b.SetRouteChecker(func(ip net.IP) bool {
		return net.ParseIP("10.244.2.1").Equal(ip) || net.ParseIP("10.244.2.4").Equal(ip)
	})
	return b, pod, peer, bubble
}

func testIPFrame(src, dst net.IP, srcMAC, dstMAC net.HardwareAddr, proto byte) *packet.EthernetFrame {
	ip := make([]byte, 28)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(len(ip)))
	ip[8] = 64
	ip[9] = proto
	copy(ip[12:16], src.To4())
	copy(ip[16:20], dst.To4())
	if proto == packet.IPProtocolICMP {
		ip[20] = 8
	}
	return &packet.EthernetFrame{SrcMAC: srcMAC, DstMAC: dstMAC, EtherType: packet.EtherTypeIPv4, Payload: ip}
}

func TestTapGatewayClassifiesBeforeFDB(t *testing.T) {
	b, pod, peer, bubble := tapTestBridge(t)
	overlayCalls := 0
	setOverlayFunc(b, func(_ *packet.EthernetFrame, ip net.IP) error {
		overlayCalls++
		if !ip.Equal(net.ParseIP("10.244.2.4")) {
			t.Errorf("unexpected overlay IP %s", ip)
		}
		return nil
	})
	for _, dst := range []string{"10.244.1.5", "10.244.2.4", "10.43.0.10", "10.244.1.1"} {
		frame := testIPFrame(pod.IP(), net.ParseIP(dst), pod.MAC(), bubble.MAC(), packet.IPProtocolTCP)
		raw, _ := frame.Marshal()
		if err := b.ProcessFrame(pod, raw); err != nil {
			t.Fatalf("%s: %v", dst, err)
		}
	}
	if peer.ReceivedCount() != 1 || overlayCalls != 1 || bubble.ReceivedCount() != 2 {
		t.Fatalf("classification: peer=%d overlay=%d bubble=%d", peer.ReceivedCount(), overlayCalls, bubble.ReceivedCount())
	}
	frame := testIPFrame(bubble.IP(), net.ParseIP("10.43.0.10"), bubble.MAC(), b.gatewayMAC, packet.IPProtocolTCP)
	raw, _ := frame.Marshal()
	if err := b.ProcessFrame(bubble, raw); err == nil {
		t.Fatal("bubble non-pod destination reflected")
	}
	if bubble.ReceivedCount() != 2 {
		t.Fatal("bubble frame was reflected")
	}
}

func TestTapGatewayARPAndRemoteReturn(t *testing.T) {
	b, pod, _, bubble := tapTestBridge(t)
	arp := func(src *mockEndpoint, dst string) *packet.ARPPacket {
		req := &packet.ARPPacket{Operation: packet.ARPOperationRequest, SenderMAC: src.MAC(), SenderIP: src.IP(), TargetIP: net.ParseIP(dst), TargetMAC: make(net.HardwareAddr, 6)}
		payload, _ := req.Marshal()
		frame := &packet.EthernetFrame{SrcMAC: src.MAC(), DstMAC: net.HardwareAddr{255, 255, 255, 255, 255, 255}, EtherType: packet.EtherTypeARP, Payload: payload}
		raw, _ := frame.Marshal()
		if err := b.ProcessFrame(src, raw); err != nil {
			t.Fatal(err)
		}
		if src.ReceivedCount() == 0 {
			return nil
		}
		reply, _ := packet.ParseEthernet(src.LastPacket())
		parsed, _ := packet.ParseARP(reply.Payload)
		return parsed
	}
	if reply := arp(pod, "10.244.1.1"); reply == nil || !bytes.Equal(reply.SenderMAC, bubble.MAC()) {
		t.Fatal("local gateway ARP did not advertise TAP MAC")
	}
	if reply := arp(bubble, "10.244.2.1"); reply == nil || !bytes.Equal(reply.SenderMAC, b.gatewayMAC) {
		t.Fatal("remote gateway ARP did not advertise Plaid next-hop MAC")
	}
	count := bubble.ReceivedCount()
	if reply := arp(bubble, "10.244.3.1"); reply != nil && bubble.ReceivedCount() != count {
		t.Fatal("unknown remote destination resolved")
	}
	frame := testIPFrame(net.ParseIP("10.244.2.4"), bubble.IP(), b.gatewayMAC, b.gatewayMAC, packet.IPProtocolTCP)
	if err := b.InjectFrame(frame); err != nil {
		t.Fatal(err)
	}
	if bubble.ReceivedCount() != count+1 {
		t.Fatal("remote gateway return not injected")
	}
	injected, _ := packet.ParseEthernet(bubble.LastPacket())
	if !bytes.Equal(injected.DstMAC, bubble.MAC()) {
		t.Fatalf("wrong TAP destination MAC %s", injected.DstMAC)
	}
}

func TestTapGatewayICMPOwnedByKernel(t *testing.T) {
	b, pod, _, bubble := tapTestBridge(t)
	frame := testIPFrame(pod.IP(), bubble.IP(), pod.MAC(), bubble.MAC(), packet.IPProtocolICMP)
	raw, _ := frame.Marshal()
	if err := b.ProcessFrame(pod, raw); err != nil {
		t.Fatal(err)
	}
	if bubble.ReceivedCount() != 1 || pod.ReceivedCount() != 0 {
		t.Fatal("ICMP was synthesized instead of delivered to kernel")
	}
}
