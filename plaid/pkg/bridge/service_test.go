package bridge

import (
	"bytes"
	"net"
	"testing"

	"plaid/pkg/packet"
	"plaid/pkg/packet/packettest"
	"plaid/pkg/svc"
)

const (
	vipA    = "10.43.0.50"
	snatIP  = "10.244.1.2"
	hostAPI = "192.168.64.3" // a Service backend that is not a pod
)

func ip4(s string) [4]byte {
	var a [4]byte
	copy(a[:], net.ParseIP(s).To4())
	return a
}

type serviceFixture struct {
	b       *Bridge
	eng     *svc.Engine
	pod     *mockEndpoint // a client
	peer    *mockEndpoint // a backend on this node
	bubble  *mockEndpoint
	overlay []*packet.EthernetFrame
}

func newServiceFixture(t *testing.T, table ...svc.Service) *serviceFixture {
	t.Helper()
	b, pod, peer, bubble := tapTestBridge(t)
	_, cidr, _ := net.ParseCIDR("10.43.0.0/16")
	eng, err := svc.New(svc.Config{ServiceCIDR: cidr, SNATAddr: net.ParseIP(snatIP)})
	if err != nil {
		t.Fatal(err)
	}
	eng.SetServices(table)
	b.SetServiceEngine(eng)
	f := &serviceFixture{b: b, eng: eng, pod: pod, peer: peer, bubble: bubble}
	setOverlayFunc(b, func(frame *packet.EthernetFrame, _ net.IP) error {
		f.overlay = append(f.overlay, frame)
		return nil
	})
	return f
}

func backend(addr string, port uint16) svc.Backend {
	return svc.Backend{IP: ip4(addr), Port: port, Eligible: true}
}

func web(backends ...svc.Backend) svc.Service {
	return svc.Service{ID: "web", VIP: ip4(vipA), Port: 80, Proto: svc.TCP, Backends: backends}
}

// send delivers an IP packet to the bridge as a frame from srcEP.
func (f *serviceFixture) send(t *testing.T, src *mockEndpoint, pkt []byte) {
	t.Helper()
	dstMAC := f.b.gatewayMAC
	if src != f.bubble {
		dstMAC = f.bubble.MAC()
	}
	frame := &packet.EthernetFrame{SrcMAC: src.MAC(), DstMAC: dstMAC, EtherType: packet.EtherTypeIPv4, Payload: pkt}
	raw, _ := frame.Marshal()
	if err := f.b.ProcessFrame(src, raw); err != nil {
		t.Fatalf("ProcessFrame: %v", err)
	}
}

func lastIP(t *testing.T, ep *mockEndpoint) (*packet.EthernetFrame, packettest.Tuple) {
	t.Helper()
	if ep.ReceivedCount() == 0 {
		t.Fatalf("%s received nothing", ep.Name())
	}
	fr, err := packet.ParseEthernet(ep.LastPacket())
	if err != nil {
		t.Fatal(err)
	}
	if err := packettest.Verify(fr.Payload); err != nil {
		t.Fatalf("%s: %v", ep.Name(), err)
	}
	tu, err := packettest.Parse(fr.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return fr, tu
}

func TestServiceToALocalBackendAndBack(t *testing.T) {
	f := newServiceFixture(t, web(backend("10.244.1.5", 8080)))
	f.send(t, f.pod, packettest.TCP("10.244.1.4", 40000, vipA, 80, packet.TCPSyn, ""))

	fr, got := lastIP(t, f.peer)
	if got.Src != snatIP || got.Dst != "10.244.1.5" || got.Dport != 8080 {
		t.Fatalf("the backend received %+v", got)
	}
	if !bytes.Equal(fr.DstMAC, f.peer.MAC()) || !bytes.Equal(fr.SrcMAC, f.b.gatewayMAC) {
		t.Errorf("MACs %s -> %s", fr.SrcMAC, fr.DstMAC)
	}
	if f.bubble.ReceivedCount() != 0 {
		t.Error("Service traffic went through the bubble's kernel")
	}

	// The backend replies to the engine's address (on-link: the pod resolves it first).
	f.send(t, f.peer, packettest.TCP("10.244.1.5", 8080, snatIP, got.Sport, packet.TCPSyn|packet.TCPAck, ""))
	fr, back := lastIP(t, f.pod)
	if back != (packettest.Tuple{Src: vipA, Sport: 80, Dst: "10.244.1.4", Dport: 40000}) {
		t.Fatalf("the client received %+v", back)
	}
	if !bytes.Equal(fr.DstMAC, f.pod.MAC()) {
		t.Errorf("reply MAC %s", fr.DstMAC)
	}
}

func TestServiceToARemoteBackendAndBack(t *testing.T) {
	f := newServiceFixture(t, web(backend("10.244.2.4", 8080)))
	f.send(t, f.pod, packettest.TCP("10.244.1.4", 40000, vipA, 80, packet.TCPSyn, ""))
	if len(f.overlay) != 1 {
		t.Fatalf("overlay frames: %d", len(f.overlay))
	}
	got, _ := packettest.Parse(f.overlay[0].Payload)
	if got.Src != snatIP || got.Dst != "10.244.2.4" || got.Dport != 8080 {
		t.Fatalf("sent over the overlay: %+v", got)
	}
	if err := packettest.Verify(f.overlay[0].Payload); err != nil {
		t.Fatal(err)
	}

	// The reply arrives over the overlay, addressed to the engine's address.
	reply := &packet.EthernetFrame{SrcMAC: f.b.gatewayMAC, DstMAC: f.b.gatewayMAC, EtherType: packet.EtherTypeIPv4,
		Payload: packettest.TCP("10.244.2.4", 8080, snatIP, got.Sport, packet.TCPSyn|packet.TCPAck, "")}
	if err := f.b.InjectFrame(reply); err != nil {
		t.Fatal(err)
	}
	_, back := lastIP(t, f.pod)
	if back != (packettest.Tuple{Src: vipA, Sport: 80, Dst: "10.244.1.4", Dport: 40000}) {
		t.Fatalf("the client received %+v", back)
	}
}

func TestServiceToABackendOutsideThePodNetworkGoesThroughTheKernel(t *testing.T) {
	f := newServiceFixture(t, web(backend(hostAPI, 6443)))
	f.send(t, f.pod, packettest.TCP("10.244.1.4", 40000, vipA, 80, packet.TCPSyn, ""))
	fr, got := lastIP(t, f.bubble)
	if got.Src != snatIP || got.Dst != hostAPI || got.Dport != 6443 {
		t.Fatalf("the kernel received %+v", got)
	}
	if !bytes.Equal(fr.DstMAC, f.bubble.MAC()) {
		t.Errorf("MAC %s", fr.DstMAC)
	}

	// The kernel's answer comes back from the bubble to the engine's address: it is not
	// the bubble sending non-cluster traffic, and the client gets it.
	f.send(t, f.bubble, packettest.TCP(hostAPI, 6443, snatIP, got.Sport, packet.TCPSyn|packet.TCPAck, ""))
	_, back := lastIP(t, f.pod)
	if back != (packettest.Tuple{Src: vipA, Sport: 80, Dst: "10.244.1.4", Dport: 40000}) {
		t.Fatalf("the client received %+v", back)
	}
}

func TestTheBubbleAsAClientOfAService(t *testing.T) {
	f := newServiceFixture(t, web(backend("10.244.1.5", 8080), backend("10.244.2.4", 8080)))
	// Whichever backend is chosen, the answer goes back to the bubble's gateway address.
	for i := 0; i < 20; i++ {
		before := f.bubble.ReceivedCount()
		f.send(t, f.bubble, packettest.TCP("10.244.1.1", uint16(41000+i), vipA, 80, packet.TCPSyn, ""))
		var sport uint16
		var backendIP string
		if n := len(f.overlay); n > 0 && f.peer.ReceivedCount() == 0 {
			tu, _ := packettest.Parse(f.overlay[n-1].Payload)
			sport, backendIP = tu.Sport, tu.Dst
			f.overlay = nil
		} else {
			_, tu := lastIP(t, f.peer)
			sport, backendIP = tu.Sport, tu.Dst
			f.peer.mu.Lock()
			f.peer.packets = nil
			f.peer.mu.Unlock()
		}
		reply := packettest.TCP(backendIP, 8080, snatIP, sport, packet.TCPSyn|packet.TCPAck, "")
		if backendIP == "10.244.1.5" {
			f.send(t, f.peer, reply)
		} else {
			if err := f.b.InjectFrame(&packet.EthernetFrame{SrcMAC: f.b.gatewayMAC, DstMAC: f.b.gatewayMAC, EtherType: packet.EtherTypeIPv4, Payload: reply}); err != nil {
				t.Fatal(err)
			}
		}
		if f.bubble.ReceivedCount() != before+1 {
			t.Fatalf("the bubble did not get the answer (backend %s)", backendIP)
		}
		_, back := lastIP(t, f.bubble)
		if back.Src != vipA || back.Dst != "10.244.1.1" || back.Dport != uint16(41000+i) {
			t.Fatalf("the bubble received %+v", back)
		}
	}
}

func TestTheGuardStillRefusesOtherBubbleTraffic(t *testing.T) {
	f := newServiceFixture(t, web(backend("10.244.1.5", 8080)))
	frame := &packet.EthernetFrame{SrcMAC: f.bubble.MAC(), DstMAC: f.b.gatewayMAC, EtherType: packet.EtherTypeIPv4,
		Payload: packettest.TCP("10.244.1.1", 1000, "8.8.8.8", 53, packet.TCPSyn, "")}
	raw, _ := frame.Marshal()
	if err := f.b.ProcessFrame(f.bubble, raw); err == nil {
		t.Error("the bubble sent external traffic back to itself without complaint")
	}
	if f.bubble.ReceivedCount() != 0 {
		t.Error("external traffic from the bubble was reflected to it")
	}
	// A pod's external traffic still goes to the kernel.
	f.send(t, f.pod, packettest.TCP("10.244.1.4", 1000, "8.8.8.8", 53, packet.TCPSyn, ""))
	if _, got := lastIP(t, f.bubble); got.Dst != "8.8.8.8" {
		t.Errorf("%+v", got)
	}
}

func TestTheEnginesAddressAnswersARP(t *testing.T) {
	f := newServiceFixture(t)
	ask := func(src *mockEndpoint, target string) *packet.ARPPacket {
		req := &packet.ARPPacket{Operation: packet.ARPOperationRequest, SenderMAC: src.MAC(), SenderIP: src.IP(), TargetIP: net.ParseIP(target), TargetMAC: make(net.HardwareAddr, 6)}
		payload, _ := req.Marshal()
		frame := &packet.EthernetFrame{SrcMAC: src.MAC(), DstMAC: net.HardwareAddr{255, 255, 255, 255, 255, 255}, EtherType: packet.EtherTypeARP, Payload: payload}
		raw, _ := frame.Marshal()
		before := src.ReceivedCount()
		if err := f.b.ProcessFrame(src, raw); err != nil {
			t.Fatal(err)
		}
		if src.ReceivedCount() == before {
			return nil
		}
		reply, _ := packet.ParseEthernet(src.LastPacket())
		parsed, _ := packet.ParseARP(reply.Payload)
		return parsed
	}
	for _, src := range []*mockEndpoint{f.pod, f.bubble, f.peer} {
		reply := ask(src, snatIP)
		if reply == nil || !bytes.Equal(reply.SenderMAC, f.b.gatewayMAC) {
			t.Errorf("%s got no answer for the engine's address", src.Name())
		}
	}
	// Other unassigned addresses of the node's network stay unresolved.
	if reply := ask(f.pod, "10.244.1.9"); reply != nil {
		t.Error("an unassigned address was resolved")
	}
}

func TestAServiceWithoutBackendsRefusesWithAnICMPError(t *testing.T) {
	f := newServiceFixture(t, web())
	f.send(t, f.pod, packettest.TCP("10.244.1.4", 40000, vipA, 80, packet.TCPSyn, ""))
	fr, err := packet.ParseEthernet(f.pod.LastPacket())
	if err != nil {
		t.Fatal(err)
	}
	l, err := packet.ParseL3(fr.Payload)
	if err != nil || l.Proto != packet.IPProtocolICMP || net.IP(l.Src[:]).String() != vipA || net.IP(l.Dst[:]).String() != "10.244.1.4" {
		t.Fatalf("reply %v %+v", err, l)
	}
	if icmp := fr.Payload[20:]; icmp[0] != 3 || icmp[1] != 3 {
		t.Errorf("ICMP %d/%d", icmp[0], icmp[1])
	}
	// The bubble gets the refusal too when it is the client.
	f.send(t, f.bubble, packettest.TCP("10.244.1.1", 41000, vipA, 80, packet.TCPSyn, ""))
	if f.bubble.ReceivedCount() != 1 {
		t.Error("the bubble did not get the refusal")
	}
}

func TestServiceAddressesAreNotHandedToTheKernelAnyMore(t *testing.T) {
	f := newServiceFixture(t, web(backend("10.244.1.5", 8080)))
	// An address of the Service range that is not in the table is dropped, not passed on.
	f.send(t, f.pod, packettest.TCP("10.244.1.4", 40000, "10.43.9.9", 80, packet.TCPSyn, ""))
	f.send(t, f.pod, packettest.UDP("10.244.1.4", 40000, vipA, 80, "x", false))
	if f.bubble.ReceivedCount() != 0 || f.peer.ReceivedCount() != 0 {
		t.Error("traffic to unknown Service addresses was delivered")
	}
	// A reply nobody asked for is dropped too.
	f.send(t, f.peer, packettest.TCP("10.244.1.5", 8080, snatIP, 50000, packet.TCPAck, ""))
	if f.pod.ReceivedCount() != 0 {
		t.Error("an unsolicited reply reached a pod")
	}
}

func TestDirectPodTrafficDoesNotTouchTheEngine(t *testing.T) {
	f := newServiceFixture(t, web(backend("10.244.1.5", 8080)))
	f.send(t, f.pod, packettest.TCP("10.244.1.4", 40000, "10.244.1.5", 8080, packet.TCPSyn, ""))
	_, got := lastIP(t, f.peer)
	if got.Src != "10.244.1.4" || got.Dst != "10.244.1.5" {
		t.Errorf("direct traffic was rewritten: %+v", got)
	}
	if st := f.eng.Stats(); st.Forwarded != 0 || st.Flows != 0 {
		t.Errorf("the engine saw direct pod traffic: %+v", st)
	}
}
