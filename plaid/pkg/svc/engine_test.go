package svc

import (
	"encoding/binary"
	"math/rand"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"plaid/pkg/packet"
	"plaid/pkg/packet/packettest"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func ip(s string) [4]byte {
	var a [4]byte
	copy(a[:], net.ParseIP(s).To4())
	return a
}

const (
	snat    = "10.244.1.2"
	vip     = "10.43.0.50"
	vip2    = "10.43.0.51"
	client  = "10.244.1.5"
	client2 = "10.244.1.6"
	local   = "10.244.1.7" // a backend on this node
	remote  = "10.244.2.9" // a backend on another node
)

func newEngine(t *testing.T, mutate func(*Config)) (*Engine, *clock) {
	t.Helper()
	_, cidr, _ := net.ParseCIDR("10.43.0.0/16")
	ck := &clock{t: time.Unix(1_000_000, 0)}
	cfg := Config{ServiceCIDR: cidr, SNATAddr: net.ParseIP(snat), Now: ck.Now, Rand: rand.New(rand.NewSource(1))}
	if mutate != nil {
		mutate(&cfg)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e, ck
}

func service(id, vip string, port uint16, proto Proto, backends ...Backend) Service {
	return Service{ID: id, VIP: ip(vip), Port: port, Proto: proto, Backends: backends}
}

func be(addr string, port uint16) Backend { return Backend{IP: ip(addr), Port: port, Eligible: true} }

func must(t *testing.T, r Result) packettest.Tuple {
	t.Helper()
	if r.Verdict != Forward {
		t.Fatalf("verdict %v, want Forward", r.Verdict)
	}
	if err := packettest.Verify(r.Packet); err != nil {
		t.Fatalf("translated packet: %v", err)
	}
	tu, err := packettest.Parse(r.Packet)
	if err != nil {
		t.Fatal(err)
	}
	if got := net.IP(r.Dst[:]).String(); got != tu.Dst {
		t.Fatalf("result destination %s, packet destination %s", got, tu.Dst)
	}
	return tu
}

func TestTCPConversationIsTranslatedBothWays(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("a", vip, 80, TCP, be(remote, 8080))})

	out := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")))
	if out.Src != snat || out.Dst != remote || out.Dport != 8080 {
		t.Fatalf("forward %+v", out)
	}
	sport := out.Sport
	reply := must(t, e.Process(packettest.TCP(remote, 8080, snat, sport, packet.TCPSyn|packet.TCPAck, "")))
	if reply != (packettest.Tuple{Src: vip, Sport: 80, Dst: client, Dport: 40000}) {
		t.Fatalf("reply %+v", reply)
	}
	// Data both ways on the established flow uses the same translation.
	out = must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPAck, "request")))
	if out.Sport != sport || out.Dst != remote {
		t.Fatalf("data forward %+v", out)
	}
	must(t, e.Process(packettest.TCP(remote, 8080, snat, sport, packet.TCPAck, "response")))
	// The payload is carried unchanged.
	r := e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPAck, "request"))
	if got := string(r.Packet[len(r.Packet)-7:]); got != "request" {
		t.Errorf("payload %q", got)
	}
	st := e.Stats()
	if st.Flows != 1 || st.FlowsCreated != 1 || st.Forwarded != 3 || st.Returned != 2 {
		t.Errorf("stats %+v", st)
	}
}

func TestUDPIsTranslatedIncludingWithoutChecksum(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("dns", vip, 53, UDP, be(local, 5353))})
	for _, noSum := range []bool{false, true} {
		sport := uint16(50000)
		if noSum {
			sport = 50001
		}
		out := must(t, e.Process(packettest.UDP(client, sport, vip, 53, "query", noSum)))
		if out.Dst != local || out.Dport != 5353 || out.Src != snat {
			t.Fatalf("forward %+v", out)
		}
		back := must(t, e.Process(packettest.UDP(local, 5353, snat, out.Sport, "answer", noSum)))
		if back != (packettest.Tuple{Src: vip, Sport: 53, Dst: client, Dport: sport}) {
			t.Fatalf("reply %+v", back)
		}
	}
}

func TestClientsAndServicesAreKeptApart(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{
		service("web", vip, 80, TCP, be(local, 8080)),
		service("web-tls", vip, 443, TCP, be(remote, 8443)),
		service("other", vip2, 80, TCP, be(remote, 9090)),
	})
	cases := []struct {
		vip  string
		port uint16
		want packettest.Tuple
	}{
		{vip, 80, packettest.Tuple{Src: snat, Dst: local, Dport: 8080}},
		{vip, 443, packettest.Tuple{Src: snat, Dst: remote, Dport: 8443}},
		{vip2, 80, packettest.Tuple{Src: snat, Dst: remote, Dport: 9090}},
	}
	for _, c := range cases {
		got := must(t, e.Process(packettest.TCP(client, 41000, c.vip, c.port, packet.TCPSyn, "")))
		got.Sport = 0
		if got != c.want {
			t.Errorf("%s:%d -> %+v, want %+v", c.vip, c.port, got, c.want)
		}
	}
}

func TestSourcePortsAreKeptAndCollisionsResolved(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("a", vip, 80, TCP, be(remote, 8080)), service("b", vip2, 80, TCP, be(local, 8080))})

	a := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")))
	if a.Sport != 40000 {
		t.Errorf("the client's port was not kept: %d", a.Sport)
	}
	// Another client with the same port towards the same backend needs another one.
	b := must(t, e.Process(packettest.TCP(client2, 40000, vip, 80, packet.TCPSyn, "")))
	if b.Sport == a.Sport {
		t.Fatal("two clients share a source port towards one backend")
	}
	// The same port towards a different backend is fine.
	c := must(t, e.Process(packettest.TCP(client2, 40000, vip2, 80, packet.TCPSyn, "")))
	if c.Sport != 40000 {
		t.Errorf("the port was not reused for another backend: %d", c.Sport)
	}
	// Each reply goes to its own client.
	ra := must(t, e.Process(packettest.TCP(remote, 8080, snat, a.Sport, packet.TCPSyn|packet.TCPAck, "")))
	rb := must(t, e.Process(packettest.TCP(remote, 8080, snat, b.Sport, packet.TCPSyn|packet.TCPAck, "")))
	if ra.Dst != client || rb.Dst != client2 || ra.Dport != 40000 || rb.Dport != 40000 {
		t.Errorf("replies %+v %+v", ra, rb)
	}
}

func TestHairpinReachesTheClientItself(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("self", vip, 80, TCP, be(client, 8080))})
	out := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")))
	if out.Dst != client || out.Src != snat {
		t.Fatalf("hairpin forward %+v", out)
	}
	back := must(t, e.Process(packettest.TCP(client, 8080, snat, out.Sport, packet.TCPSyn|packet.TCPAck, "")))
	if back.Src != vip || back.Dst != client || back.Dport != 40000 {
		t.Fatalf("hairpin reply %+v", back)
	}
}

func TestOnlyEligibleBackendsTakeNewFlows(t *testing.T) {
	e, _ := newEngine(t, nil)
	ineligible := Backend{IP: ip(local), Port: 8080}
	e.SetServices([]Service{service("a", vip, 80, TCP, ineligible, be(remote, 8080))})
	for i := 0; i < 200; i++ {
		got := must(t, e.Process(packettest.TCP(client, uint16(10000+i), vip, 80, packet.TCPSyn, "")))
		if got.Dst != remote {
			t.Fatalf("flow %d went to %s", i, got.Dst)
		}
	}
	// Both take traffic when both are eligible.
	e.SetServices([]Service{service("a", vip, 80, TCP, be(local, 8080), be(remote, 8080))})
	seen := map[string]int{}
	for i := 0; i < 400; i++ {
		got := must(t, e.Process(packettest.TCP(client2, uint16(10000+i), vip, 80, packet.TCPSyn, "")))
		seen[got.Dst]++
	}
	if seen[local] < 100 || seen[remote] < 100 {
		t.Errorf("distribution %v", seen)
	}
}

func TestFlowsStayOnTheirBackendThroughChanges(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("a", vip, 80, TCP, be(local, 8080), be(remote, 8080))})
	first := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")))

	// The backend stops being eligible (not ready) or other backends come and go: the flow stays.
	for _, table := range [][]Backend{
		{{IP: ip(first.Dst), Port: 8080}, be("10.244.3.3", 8080)},
		{be("10.244.3.3", 8080), {IP: ip(first.Dst), Port: 8080, Eligible: true}, be("10.244.4.4", 8080)},
	} {
		e.SetServices([]Service{service("a", vip, 80, TCP, table...)})
		got := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPAck, "more")))
		if got.Dst != first.Dst || got.Sport != first.Sport {
			t.Fatalf("the flow moved: %+v -> %+v", first, got)
		}
	}

	// The backend disappears from the table: its flows go, and a TCP packet is not given a new backend.
	e.SetServices([]Service{service("a", vip, 80, TCP, be("10.244.3.3", 8080))})
	if r := e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPAck, "more")); r.Verdict != Drop {
		t.Errorf("a TCP packet of a flow whose backend is gone got %v", r.Verdict)
	}
	if e.Stats().FlowsPurged != 1 || e.Stats().Flows != 0 {
		t.Errorf("stats %+v", e.Stats())
	}
	// A UDP client, by contrast, simply gets a new flow.
	e.SetServices([]Service{service("u", vip, 53, UDP, be(local, 53), be(remote, 53))})
	u := must(t, e.Process(packettest.UDP(client, 50000, vip, 53, "q", false)))
	e.SetServices([]Service{service("u", vip, 53, UDP, be("10.244.3.3", 53))})
	again := must(t, e.Process(packettest.UDP(client, 50000, vip, 53, "q", false)))
	if again.Dst != "10.244.3.3" || u.Dst == again.Dst {
		t.Errorf("udp %+v then %+v", u, again)
	}
}

func TestADeletedOrReplacedServiceOwnsNoFlows(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("old-uid", vip, 80, TCP, be(local, 8080))})
	old := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")))
	e.SetServices([]Service{service("old-uid", vip, 53, UDP, be(local, 53))})
	udp := must(t, e.Process(packettest.UDP(client, 40001, vip, 53, "q", false)))

	// The Service is deleted and another takes its address: the TCP flow drains on the old
	// backend, and UDP flows are dropped.
	e.SetServices([]Service{service("new-uid", vip, 80, TCP, be(remote, 9090)), service("new-uid", vip, 53, UDP, be(remote, 9053))})
	got := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPAck, "data")))
	if got.Dst != old.Dst {
		t.Errorf("the draining flow moved to %s", got.Dst)
	}
	if u := must(t, e.Process(packettest.UDP(client, 40001, vip, 53, "q", false))); u.Dst != remote || u.Dst == udp.Dst {
		t.Errorf("a UDP flow of the old Service survived: %+v", u)
	}
	// A new connection on the same tuple belongs to the new Service.
	fresh := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")))
	if fresh.Dst != remote || fresh.Dport != 9090 {
		t.Errorf("new connection %+v", fresh)
	}
}

func TestNoEligibleBackendIsRefusedAndUnknownAddressesAreDropped(t *testing.T) {
	e, _ := newEngine(t, nil)
	if r := e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")); r.Verdict != Drop {
		t.Errorf("before any table: %v", r.Verdict)
	}
	e.SetServices([]Service{
		service("empty", vip, 80, TCP),
		service("unready", vip, 53, UDP, Backend{IP: ip(local), Port: 53}),
	})
	for _, pkt := range [][]byte{
		packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, ""),
		packettest.UDP(client, 50000, vip, 53, "query", false),
	} {
		r := e.Process(pkt)
		if r.Verdict != Reply || r.Dst != ip(client) {
			t.Fatalf("verdict %v to %v", r.Verdict, r.Dst)
		}
		l, err := packet.ParseL3(r.Packet)
		if err != nil || l.Proto != packet.IPProtocolICMP || packet.Checksum16(r.Packet[:20]) != 0 || packet.Checksum16(r.Packet[20:]) != 0 {
			t.Fatalf("ICMP reply %v %+v", err, l)
		}
		if icmp := r.Packet[20:]; icmp[0] != 3 || icmp[1] != 3 {
			t.Errorf("ICMP type/code %d/%d", icmp[0], icmp[1])
		}
		if string(r.Packet[28:48]) != string(pkt[:20]) {
			t.Error("the ICMP error does not quote the packet")
		}
	}
	// A TCP packet that is not a SYN is not refused with an error: there is no connection.
	if r := e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPAck, "")); r.Verdict != Drop {
		t.Errorf("non-SYN to a service without backends: %v", r.Verdict)
	}
	for _, pkt := range [][]byte{
		packettest.TCP(client, 40000, vip, 81, packet.TCPSyn, ""), // no such port
		packettest.UDP(client, 40000, vip2, 53, "q", false),       // no such address
		packettest.TCP(client, 40000, vip, 53, packet.TCPSyn, ""), // UDP port, TCP packet
	} {
		if r := e.Process(pkt); r.Verdict != Drop {
			t.Errorf("unknown Service: %v", r.Verdict)
		}
	}
	st := e.Stats()
	// (the packet sent before the first table counts as an unknown Service too)
	if st.Rejected != 2 || st.DropsNoService != 4 || st.DropsNoFlow != 1 || st.Flows != 0 {
		t.Errorf("stats %+v", st)
	}
}

func TestTCPWithoutAFlowIsDropped(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("a", vip, 80, TCP, be(local, 8080))})
	for _, flags := range []byte{packet.TCPAck, packet.TCPSyn | packet.TCPAck, packet.TCPFin | packet.TCPAck, packet.TCPRst, 0} {
		if r := e.Process(packettest.TCP(client, 40000, vip, 80, flags, "")); r.Verdict != Drop {
			t.Errorf("flags %#x created a flow", flags)
		}
	}
	// A reply nobody asked for, or from the wrong backend, is dropped.
	out := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")))
	for _, bad := range [][]byte{
		packettest.TCP(local, 8080, snat, out.Sport+1, packet.TCPAck, ""),
		packettest.TCP(remote, 8080, snat, out.Sport, packet.TCPAck, ""),
		packettest.TCP(local, 9999, snat, out.Sport, packet.TCPAck, ""),
		packettest.UDP(local, 8080, snat, out.Sport, "x", false),
	} {
		if r := e.Process(bad); r.Verdict != Drop {
			t.Errorf("a stray reply was delivered: %+v", r)
		}
	}
}

func TestUnsupportedPacketsAreCounted(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("a", vip, 80, TCP, be(local, 8080))})
	frag := packettest.UDP(client, 1, vip, 80, "data", false)
	binary.BigEndian.PutUint16(frag[6:8], 0x2000)
	icmp := packet.CreateICMPEchoReply(&packet.IPv4Packet{Header: packet.IPv4Header{Protocol: 1, SrcIP: net.ParseIP(client), DstIP: net.ParseIP(vip)}, Payload: []byte{8, 0, 0, 0, 0, 0, 0, 0}})
	for name, pkt := range map[string][]byte{"fragment": frag, "icmp": icmp, "garbage": {1, 2, 3}, "sctp": func() []byte {
		p := packettest.UDP(client, 1, vip, 80, "data", false)
		p[9] = 132
		return p
	}()} {
		if r := e.Process(pkt); r.Verdict != Drop {
			t.Errorf("%s: %v", name, r.Verdict)
		}
	}
	st := e.Stats()
	if st.DropsUnsupported != 4 || len(st.RecentDrops) != 4 {
		t.Errorf("stats %+v", st)
	}
	joined := strings.Join(st.RecentDrops, "\n")
	for _, want := range []string{"fragment of protocol 17", "ICMP type 0", "not an IPv4 packet", "protocol 132"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the drops do not say %q:\n%s", want, joined)
		}
	}
	for i := 0; i < 40; i++ { // the memory is bounded
		e.Process([]byte{1, 2, 3})
	}
	if n := len(e.Stats().RecentDrops); n != recentDropsKept {
		t.Errorf("%d drops remembered, want %d", n, recentDropsKept)
	}
}

func TestFlowsExpireByState(t *testing.T) {
	e, ck := newEngine(t, nil)
	e.SetServices([]Service{service("t", vip, 80, TCP, be(local, 8080)), service("u", vip, 53, UDP, be(local, 53))})
	to := DefaultTimeouts()

	// UDP: 30 s without a reply, 2 minutes once there is one.
	u1 := must(t, e.Process(packettest.UDP(client, 50000, vip, 53, "q", false)))
	u2 := must(t, e.Process(packettest.UDP(client, 50001, vip, 53, "q", false)))
	must(t, e.Process(packettest.UDP(local, 53, snat, u2.Sport, "a", false)))
	ck.Advance(to.UDPUnreplied + time.Second)
	e.Sweep()
	if r := e.Process(packettest.UDP(local, 53, snat, u1.Sport, "a", false)); r.Verdict != Drop {
		t.Error("an unreplied UDP flow outlived its timeout")
	}
	must(t, e.Process(packettest.UDP(local, 53, snat, u2.Sport, "a", false)))
	ck.Advance(to.UDPReplied + time.Second)
	e.Sweep()
	if e.Stats().Flows != 0 {
		t.Errorf("flows left: %+v", e.Stats())
	}

	// TCP: a SYN nobody answers ends at the SYN timeout; an established flow lives on; a
	// reset ends quickly; both FINs close it.
	syn := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")))
	est := must(t, e.Process(packettest.TCP(client, 40001, vip, 80, packet.TCPSyn, "")))
	must(t, e.Process(packettest.TCP(local, 8080, snat, est.Sport, packet.TCPSyn|packet.TCPAck, "")))
	rst := must(t, e.Process(packettest.TCP(client, 40002, vip, 80, packet.TCPSyn, "")))
	must(t, e.Process(packettest.TCP(local, 8080, snat, rst.Sport, packet.TCPRst, "")))
	ck.Advance(to.TCPReset + time.Second)
	e.Sweep()
	if e.Stats().Flows != 2 {
		t.Fatalf("after the reset timeout: %+v", e.Stats())
	}
	ck.Advance(to.TCPSynSent)
	e.Sweep()
	if r := e.Process(packettest.TCP(local, 8080, snat, syn.Sport, packet.TCPSyn|packet.TCPAck, "")); r.Verdict != Drop {
		t.Error("a SYN that nobody answered outlived its timeout")
	}
	ck.Advance(to.TCPEstablished - to.TCPSynSent - time.Minute)
	must(t, e.Process(packettest.TCP(client, 40001, vip, 80, packet.TCPAck, "")))
	// Closing: one FIN leaves the half-closed timeout, the second the short one.
	must(t, e.Process(packettest.TCP(client, 40001, vip, 80, packet.TCPFin|packet.TCPAck, "")))
	must(t, e.Process(packettest.TCP(local, 8080, snat, est.Sport, packet.TCPFin|packet.TCPAck, "")))
	ck.Advance(to.TCPClosing + time.Second)
	e.Sweep()
	if e.Stats().Flows != 0 {
		t.Errorf("a closed flow stays: %+v", e.Stats())
	}
	// A SYN on the tuple of a closed flow is a new connection.
	again := must(t, e.Process(packettest.TCP(client, 40001, vip, 80, packet.TCPSyn, "")))
	if again.Dst != local {
		t.Errorf("%+v", again)
	}
}

func TestANewSynReplacesAFlowThatIsClosing(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("a", vip, 80, TCP, be(local, 8080), be(remote, 8080))})
	first := must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")))
	must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPFin|packet.TCPAck, "")))
	for i := 0; i < 20; i++ { // connection reuse after close picks a backend afresh
		must(t, e.Process(packettest.TCP(client, 40000, vip, 80, packet.TCPSyn, "")))
	}
	if e.Stats().Flows != 1 {
		t.Errorf("flows %d, first went to %s", e.Stats().Flows, first.Dst)
	}
}

func TestTheFlowTableIsBounded(t *testing.T) {
	e, _ := newEngine(t, func(c *Config) { c.MaxFlows = 50 })
	e.SetServices([]Service{service("a", vip, 80, TCP, be(local, 8080)), service("n", vip2, 80, TCP)})
	for i := 0; i < 100; i++ {
		r := e.Process(packettest.TCP(client, uint16(10000+i), vip, 80, packet.TCPSyn, ""))
		if (i < 50) != (r.Verdict == Forward) {
			t.Fatalf("flow %d: %v", i, r.Verdict)
		}
	}
	st := e.Stats()
	if st.Flows != 50 || st.DropsFlowLimit != 50 {
		t.Errorf("stats %+v", st)
	}
	// Existing flows continue, refusals still work, and expiry makes room.
	must(t, e.Process(packettest.TCP(client, 10000, vip, 80, packet.TCPAck, "data")))
	if r := e.Process(packettest.TCP(client, 20000, vip2, 80, packet.TCPSyn, "")); r.Verdict != Reply {
		t.Errorf("a refusal needs no flow: %v", r.Verdict)
	}
}

func TestManyClientsWithTheSameSourcePortGetDistinctOnesPerBackend(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetServices([]Service{service("a", vip, 80, TCP, be(local, 8080))})
	seen := map[uint16]bool{}
	for i := 0; i < 3000; i++ {
		c := net.IPv4(10, 244, byte(1+i/250), byte(1+i%250)).String() // distinct clients, one source port
		r := e.Process(packettest.TCP(c, 40000, vip, 80, packet.TCPSyn, ""))
		if r.Verdict != Forward {
			t.Fatalf("flow %d: %v", i, r.Verdict)
		}
		tu, _ := packettest.Parse(r.Packet)
		if seen[tu.Sport] {
			t.Fatalf("source port %d used twice for one backend", tu.Sport)
		}
		seen[tu.Sport] = true
	}
	if e.Stats().Flows != 3000 {
		t.Errorf("flows %d", e.Stats().Flows)
	}
}

func TestConcurrentTrafficUpdatesAndExpiry(t *testing.T) {
	e, ck := newEngine(t, nil)
	tables := [][]Service{
		{service("a", vip, 80, TCP, be(local, 8080), be(remote, 8080)), service("d", vip, 53, UDP, be(local, 53))},
		{service("a", vip, 80, TCP, be(remote, 8080)), service("d", vip, 53, UDP, be(remote, 53), be(local, 53))},
		{service("b", vip, 80, TCP, be(local, 9090))},
	}
	e.SetServices(tables[0])
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				c := net.IPv4(10, 244, 1, byte(10+w)).String()
				sport := uint16(20000 + i%500)
				if r := e.Process(packettest.TCP(c, sport, vip, 80, packet.TCPSyn, "")); r.Verdict == Forward {
					tu, _ := packettest.Parse(r.Packet)
					e.Process(packettest.TCP(net.IP(r.Dst[:]).String(), tu.Dport, snat, tu.Sport, packet.TCPSyn|packet.TCPAck, ""))
				}
				if r := e.Process(packettest.UDP(c, sport, vip, 53, "q", false)); r.Verdict == Forward {
					tu, _ := packettest.Parse(r.Packet)
					e.Process(packettest.UDP(net.IP(r.Dst[:]).String(), tu.Dport, snat, tu.Sport, "a", false))
				}
			}
		}(w)
	}
	for i := 0; i < 300; i++ {
		e.SetServices(tables[i%len(tables)])
		ck.Advance(time.Second)
		if i%10 == 0 {
			e.Sweep()
		}
		_ = e.Stats()
		_ = e.Services()
	}
	close(stop)
	wg.Wait()
	// Whatever happened, the two maps describe the same flows.
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.flows) != len(e.rev) {
		t.Fatalf("%d flows but %d reverse entries", len(e.flows), len(e.rev))
	}
	for k, f := range e.flows {
		if e.rev[f.rev] != f || f.fwd != k {
			t.Fatal("flow maps disagree")
		}
	}
}

func TestOwnsAndConfiguration(t *testing.T) {
	e, _ := newEngine(t, nil)
	for addr, want := range map[string]bool{vip: true, "10.43.255.255": true, snat: true, client: false, "10.44.0.1": false} {
		if got := e.Owns(ip(addr)); got != want {
			t.Errorf("Owns(%s) = %v", addr, got)
		}
	}
	_, cidr, _ := net.ParseCIDR("10.43.0.0/16")
	for name, cfg := range map[string]Config{
		"no CIDR":          {SNATAddr: net.ParseIP(snat)},
		"IPv6 SNAT":        {ServiceCIDR: cidr, SNATAddr: net.ParseIP("fd00::1")},
		"SNAT in the CIDR": {ServiceCIDR: cidr, SNATAddr: net.ParseIP("10.43.0.2")},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	e.SetServices([]Service{service("a", vip, 80, TCP, be(local, 8080), Backend{IP: ip(remote), Port: 8080}), service("b", vip, 53, UDP)})
	got := e.Services()
	if len(got) != 2 || got[0].Port != 53 || got[1].Backends != 2 || got[1].Eligible != 1 {
		t.Errorf("Services() = %+v", got)
	}
}
