// Package svc is Plaid's Service engine: it translates traffic to Service addresses
// (virtual IPs) into traffic to their backends, in user space, without terminating
// anything. It knows nothing of Kubernetes: its input is a table of "address:port/protocol
// has these backends", which a connector (pkg/k8s) keeps current, and the packets that
// plaidd's bridge hands to it.
//
// The translation is a destination NAT of the Service address to a backend plus a
// source NAT to the engine's own address (SNATAddr, an address of the node's pod network
// that is never given to a pod). The source NAT is what brings the replies back: a
// backend answers the engine's address, which the bridge delivers here wherever the
// backend is (same node, another node, or the bubble's kernel for a backend outside
// the pod network), and the engine restores the addresses the client expects. A backend
// therefore sees the engine's address and not the client's, as with kube-proxy's
// masquerade of Service traffic.
//
// State rules:
//   - A flow is pinned to its backend for its lifetime. Eligibility decides only where
//     new flows go; a backend that is no longer eligible keeps the flows it has, and a
//     backend that disappears from the table loses them.
//   - A TCP flow is created only by a SYN. A TCP packet with no flow is dropped (the
//     state was lost, or the packet is stray); it is never given a new backend.
//   - A flow belongs to the Service (by ID) that created it. When that Service is gone
//     (even if another now has its address), UDP flows are dropped and TCP flows drain
//     until they end; a new SYN for the same tuple makes a new flow for the new Service.
//   - A Service address and port with no eligible backend is refused with an ICMP port
//     unreachable (as kube-proxy does); one that is not in the table is dropped.
//   - State is bounded: the flow table has a limit, and exhaustion drops new flows and
//     is counted. Nothing else about the engine's state grows with traffic.
//
// Not handled, and counted as drops: IP fragments, ICMP, and other transport protocols.
package svc

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"sync"
	"time"

	"plaid/pkg/packet"
)

// Proto is the transport protocol of a Service port.
type Proto uint8

const (
	TCP Proto = packet.IPProtocolTCP
	UDP Proto = packet.IPProtocolUDP
)

func (p Proto) String() string {
	switch p {
	case TCP:
		return "TCP"
	case UDP:
		return "UDP"
	}
	return fmt.Sprintf("proto-%d", uint8(p))
}

// Backend is one destination behind a Service port.
type Backend struct {
	IP   [4]byte
	Port uint16
	// Eligible backends receive new flows. An ineligible backend (no longer ready, but
	// still present) only keeps the flows it already has.
	Eligible bool
}

// Service is one address, port and protocol with its backends.
type Service struct {
	// ID identifies the Service object (for Kubernetes: namespace, name, UID and port). A
	// different ID at the same address is a different Service, which owns none of the
	// existing flows.
	ID       string
	VIP      [4]byte
	Port     uint16
	Proto    Proto
	Backends []Backend
}

// Timeouts are how long a flow lives without traffic, by state.
type Timeouts struct {
	TCPSynSent     time.Duration // a SYN that has had no answer
	TCPEstablished time.Duration
	TCPHalfClosed  time.Duration // one side sent FIN
	TCPClosing     time.Duration // both sides sent FIN
	TCPReset       time.Duration // after RST
	UDPUnreplied   time.Duration
	UDPReplied     time.Duration
}

// DefaultTimeouts follow kube-proxy's conntrack defaults where it has one.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		TCPSynSent:     60 * time.Second,
		TCPEstablished: 24 * time.Hour,
		TCPHalfClosed:  2 * time.Minute,
		TCPClosing:     time.Minute,
		TCPReset:       10 * time.Second,
		UDPUnreplied:   30 * time.Second,
		UDPReplied:     2 * time.Minute,
	}
}

// Config configures an engine.
type Config struct {
	// ServiceCIDR is the range of Service addresses.
	ServiceCIDR *net.IPNet
	// SNATAddr is the engine's own address: the source of everything it sends to a backend.
	SNATAddr net.IP
	// MaxFlows bounds the flow table (default 262144).
	MaxFlows int
	Timeouts Timeouts
	// Now and Rand are replaceable for tests.
	Now  func() time.Time
	Rand *rand.Rand
}

// Verdict says what to do with a processed packet.
type Verdict int

const (
	Drop    Verdict = iota
	Forward         // send Result.Packet towards Result.Dst
	Reply           // send Result.Packet (an ICMP error) back to the sender, Result.Dst
)

// Result is the outcome of Process.
type Result struct {
	Verdict Verdict
	Packet  []byte
	Dst     [4]byte
}

// Stats are the engine's counters and gauges.
type Stats struct {
	Synced           bool   // a table has been installed
	Services         int    // Service ports in the table
	Backends         int    // backends over all of them
	Flows            int    // live flows
	FlowsCreated     uint64 // flows created, ever
	FlowsExpired     uint64 // flows removed by timeout
	FlowsPurged      uint64 // flows removed because their backend or Service went away
	Forwarded        uint64 // packets translated towards a backend
	Returned         uint64 // packets translated back to a client
	Rejected         uint64 // ICMP unreachables sent: no eligible backend
	DropsNoService   uint64 // to an address in the Service range that is not in the table
	DropsNoFlow      uint64 // TCP without a flow, or a reply without one
	DropsFlowLimit   uint64 // a new flow would exceed MaxFlows
	DropsPorts       uint64 // no source port left for the backend
	DropsUnsupported uint64 // fragments, ICMP, other protocols, malformed packets
	// RecentDrops describes the last few packets dropped, newest last.
	RecentDrops []string
}

// recentDropsKept is how many dropped packets the engine remembers for diagnostics.
const recentDropsKept = 16

type fwdKey struct {
	proto Proto
	cIP   [4]byte
	cPort uint16
	vIP   [4]byte
	vPort uint16
}

type revKey struct {
	proto Proto
	bIP   [4]byte
	bPort uint16
	sPort uint16
}

type svcKey struct {
	proto Proto
	vIP   [4]byte
	vPort uint16
}

type flow struct {
	fwd     fwdKey
	rev     revKey
	svcID   string
	state   flowState
	finFwd  bool
	finRev  bool
	expires time.Time
	orphan  bool // its Service is gone; it drains
}

type flowState uint8

const (
	stNew         flowState = iota // TCP: SYN seen; UDP: no reply yet
	stEstablished                  // TCP: answered; UDP: replied
	stClosing                      // TCP: FIN seen
	stReset                        // TCP: RST seen
)

// Engine is the Service engine. It is safe for concurrent use.
type Engine struct {
	cfg    Config
	snat   [4]byte
	snatIP net.IP

	mu       sync.Mutex
	drops    []string
	synced   bool
	services map[svcKey]*Service
	flows    map[fwdKey]*flow
	rev      map[revKey]*flow
	stats    Stats
}

// New creates an engine.
func New(cfg Config) (*Engine, error) {
	if cfg.ServiceCIDR == nil || cfg.ServiceCIDR.IP.To4() == nil {
		return nil, errors.New("the Service CIDR must be an IPv4 network")
	}
	snat := cfg.SNATAddr.To4()
	if snat == nil {
		return nil, errors.New("the SNAT address must be an IPv4 address")
	}
	if cfg.ServiceCIDR.Contains(snat) {
		return nil, errors.New("the SNAT address must be outside the Service CIDR")
	}
	if cfg.MaxFlows <= 0 {
		cfg.MaxFlows = 262144
	}
	if cfg.Timeouts == (Timeouts{}) {
		cfg.Timeouts = DefaultTimeouts()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	e := &Engine{
		cfg:      cfg,
		snatIP:   snat,
		services: map[svcKey]*Service{},
		flows:    map[fwdKey]*flow{},
		rev:      map[revKey]*flow{},
	}
	copy(e.snat[:], snat)
	return e, nil
}

// SNATAddr is the engine's own address.
func (e *Engine) SNATAddr() net.IP { return e.snatIP }

// ServiceCIDR is the range of Service addresses.
func (e *Engine) ServiceCIDR() *net.IPNet { return e.cfg.ServiceCIDR }

// Owns reports whether packets to dst are the engine's: a Service address, or its own.
func (e *Engine) Owns(dst [4]byte) bool {
	return dst == e.snat || e.cfg.ServiceCIDR.Contains(net.IP(dst[:]))
}

// SetServices installs a complete table, replacing the previous one. Flows follow the
// rules of the package comment: one whose backend is gone is dropped, and so is a UDP
// flow whose Service is gone; a TCP flow of a vanished Service drains.
func (e *Engine) SetServices(table []Service) {
	e.mu.Lock()
	defer e.mu.Unlock()
	services := make(map[svcKey]*Service, len(table))
	backends := 0
	for i := range table {
		s := table[i]
		s.Backends = append([]Backend(nil), s.Backends...)
		services[svcKey{s.Proto, s.VIP, s.Port}] = &s
		backends += len(s.Backends)
	}
	e.services = services
	e.synced = true
	e.stats.Services = len(services)
	e.stats.Backends = backends
	for _, f := range e.flows {
		s := services[svcKey{f.fwd.proto, f.fwd.vIP, f.fwd.vPort}]
		switch {
		case s == nil || s.ID != f.svcID:
			if f.fwd.proto == UDP {
				e.removeLocked(f)
				e.stats.FlowsPurged++
			} else {
				f.orphan = true
			}
		case !hasBackend(s, f.rev.bIP, f.rev.bPort):
			e.removeLocked(f)
			e.stats.FlowsPurged++
		default:
			f.orphan = false
		}
	}
}

func hasBackend(s *Service, ip [4]byte, port uint16) bool {
	for _, b := range s.Backends {
		if b.IP == ip && b.Port == port {
			return true
		}
	}
	return false
}

func (e *Engine) removeLocked(f *flow) {
	delete(e.flows, f.fwd)
	delete(e.rev, f.rev)
}

// Sweep removes expired flows. The engine also expires a flow when a packet finds it.
func (e *Engine) Sweep() {
	now := e.cfg.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range e.flows {
		if !now.Before(f.expires) {
			e.removeLocked(f)
			e.stats.FlowsExpired++
		}
	}
}

// Stats returns the counters.
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.stats
	st.Synced = e.synced
	st.Flows = len(e.flows)
	st.RecentDrops = append([]string(nil), e.drops...)
	return st
}

// ServiceInfo describes one Service port for diagnostics.
type ServiceInfo struct {
	ID       string
	VIP      net.IP
	Port     uint16
	Proto    Proto
	Backends int
	Eligible int
	Flows    int
	// Addresses are the backends themselves.
	Addresses []Backend
}

// Services lists the table with the number of live flows of each entry.
func (e *Engine) Services() []ServiceInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	flows := map[svcKey]int{}
	for _, f := range e.flows {
		flows[svcKey{f.fwd.proto, f.fwd.vIP, f.fwd.vPort}]++
	}
	out := make([]ServiceInfo, 0, len(e.services))
	for k, s := range e.services {
		info := ServiceInfo{ID: s.ID, VIP: net.IP(k.vIP[:]).To4(), Port: k.vPort, Proto: k.proto, Backends: len(s.Backends), Flows: flows[k], Addresses: append([]Backend(nil), s.Backends...)}
		for _, b := range s.Backends {
			if b.Eligible {
				info.Eligible++
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if c := compareIP(out[i].VIP, out[j].VIP); c != 0 {
			return c < 0
		}
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Proto < out[j].Proto
	})
	return out
}

func compareIP(a, b net.IP) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Process translates one IPv4 packet whose destination the engine owns. The packet is not
// modified; a Forward or Reply result carries a new one.
func (e *Engine) Process(pkt []byte) Result {
	l, err := packet.ParseL3(pkt)
	if err != nil || l.HasPorts() != nil || (l.Proto != packet.IPProtocolTCP && l.Proto != packet.IPProtocolUDP) {
		e.mu.Lock()
		e.stats.DropsUnsupported++
		e.dropLocked(pkt, l, err)
		e.mu.Unlock()
		return Result{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if l.Dst == e.snat {
		return e.reverseLocked(l)
	}
	return e.forwardLocked(l)
}

// dropLocked remembers what a dropped packet looked like, for diagnostics.
func (e *Engine) dropLocked(pkt []byte, l packet.L3, parseErr error) {
	var s string
	switch {
	case parseErr != nil:
		s = fmt.Sprintf("%v: %d bytes", parseErr, len(pkt))
	case l.Fragment:
		s = fmt.Sprintf("fragment of protocol %d %s -> %s, %d bytes", l.Proto, net.IP(l.Src[:]), net.IP(l.Dst[:]), len(l.Pkt))
	case l.Proto == packet.IPProtocolICMP && len(l.Transport()) >= 2:
		s = fmt.Sprintf("ICMP type %d code %d %s -> %s, %d bytes", l.Transport()[0], l.Transport()[1], net.IP(l.Src[:]), net.IP(l.Dst[:]), len(l.Pkt))
	default:
		s = fmt.Sprintf("protocol %d %s -> %s, %d bytes", l.Proto, net.IP(l.Src[:]), net.IP(l.Dst[:]), len(l.Pkt))
	}
	if len(e.drops) == recentDropsKept {
		e.drops = e.drops[1:]
	}
	e.drops = append(e.drops, s)
}

func (e *Engine) timeoutLocked(f *flow) time.Duration {
	t := e.cfg.Timeouts
	if f.fwd.proto == UDP {
		if f.state == stEstablished {
			return t.UDPReplied
		}
		return t.UDPUnreplied
	}
	switch f.state {
	case stNew:
		return t.TCPSynSent
	case stReset:
		return t.TCPReset
	case stClosing:
		if f.finFwd && f.finRev {
			return t.TCPClosing
		}
		return t.TCPHalfClosed
	}
	return t.TCPEstablished
}

func (e *Engine) touchLocked(f *flow, now time.Time) { f.expires = now.Add(e.timeoutLocked(f)) }

// tcpFlagsLocked advances a TCP flow's state by the flags of a packet in one direction.
func (e *Engine) tcpFlagsLocked(f *flow, flags uint8, forward bool) {
	switch {
	case flags&packet.TCPRst != 0:
		f.state = stReset
	case flags&packet.TCPFin != 0:
		if f.state != stReset {
			f.state = stClosing
		}
		if forward {
			f.finFwd = true
		} else {
			f.finRev = true
		}
	case !forward && f.state == stNew:
		f.state = stEstablished
	}
}

func (e *Engine) forwardLocked(l packet.L3) Result {
	now := e.cfg.Now()
	key := fwdKey{proto: Proto(l.Proto), cIP: l.Src, cPort: l.SrcPort(), vIP: l.Dst, vPort: l.DstPort()}
	flags := uint8(0)
	if key.proto == TCP {
		flags = l.TCPFlags()
	}
	f := e.flows[key]
	if f != nil && !now.Before(f.expires) {
		e.removeLocked(f)
		e.stats.FlowsExpired++
		f = nil
	}
	// A new SYN on a tuple whose flow has ended, or belongs to a vanished Service, starts over.
	if f != nil && key.proto == TCP && flags&packet.TCPSyn != 0 && flags&packet.TCPAck == 0 &&
		(f.orphan || f.state == stClosing || f.state == stReset) {
		e.removeLocked(f)
		f = nil
	}
	if f == nil {
		var r Result
		f, r = e.newFlowLocked(l, key, flags, now)
		if f == nil {
			return r
		}
	}
	if key.proto == TCP {
		e.tcpFlagsLocked(f, flags, true)
	}
	e.touchLocked(f, now)

	out := append([]byte(nil), l.Pkt...)
	nl, _ := packet.ParseL3(out)
	bIP := f.rev.bIP
	bPort := f.rev.bPort
	sPort := f.rev.sPort
	nl.Rewrite(&e.snat, &bIP, &sPort, &bPort)
	e.stats.Forwarded++
	return Result{Verdict: Forward, Packet: out, Dst: bIP}
}

// newFlowLocked creates the flow for a first packet, or returns the result that disposes of it.
func (e *Engine) newFlowLocked(l packet.L3, key fwdKey, flags uint8, now time.Time) (*flow, Result) {
	svc := e.services[svcKey{key.proto, key.vIP, key.vPort}]
	if svc == nil {
		e.stats.DropsNoService++
		return nil, Result{}
	}
	if key.proto == TCP && (flags&packet.TCPSyn == 0 || flags&packet.TCPAck != 0) {
		e.stats.DropsNoFlow++
		return nil, Result{}
	}
	var eligible []Backend
	for _, b := range svc.Backends {
		if b.Eligible {
			eligible = append(eligible, b)
		}
	}
	if len(eligible) == 0 {
		e.stats.Rejected++
		return nil, Result{
			Verdict: Reply,
			Packet:  packet.ICMPDestUnreachable(l, net.IP(key.vIP[:]), packet.ICMPPortUnreachable),
			Dst:     key.cIP,
		}
	}
	if len(e.flows) >= e.cfg.MaxFlows {
		e.stats.DropsFlowLimit++
		return nil, Result{}
	}
	b := eligible[e.cfg.Rand.Intn(len(eligible))]
	sPort, ok := e.allocPortLocked(key.proto, b, key.cPort)
	if !ok {
		e.stats.DropsPorts++
		return nil, Result{}
	}
	f := &flow{
		fwd:   key,
		rev:   revKey{proto: key.proto, bIP: b.IP, bPort: b.Port, sPort: sPort},
		svcID: svc.ID,
	}
	e.flows[key] = f
	e.rev[f.rev] = f
	e.stats.FlowsCreated++
	return f, Result{}
}

// Source ports for the engine's address: the client's own port when the backend has not
// seen it from the engine, else one from the ephemeral range.
const (
	portRangeLow  = 32768
	portRangeHigh = 60999
)

func (e *Engine) allocPortLocked(proto Proto, b Backend, preferred uint16) (uint16, bool) {
	try := func(p uint16) bool {
		_, taken := e.rev[revKey{proto, b.IP, b.Port, p}]
		return !taken
	}
	if preferred != 0 && try(preferred) {
		return preferred, true
	}
	span := portRangeHigh - portRangeLow + 1
	for i := 0; i < 64; i++ {
		if p := uint16(portRangeLow + e.cfg.Rand.Intn(span)); try(p) {
			return p, true
		}
	}
	start := e.cfg.Rand.Intn(span)
	for i := 0; i < span; i++ {
		if p := uint16(portRangeLow + (start+i)%span); try(p) {
			return p, true
		}
	}
	return 0, false
}

func (e *Engine) reverseLocked(l packet.L3) Result {
	now := e.cfg.Now()
	key := revKey{proto: Proto(l.Proto), bIP: l.Src, bPort: l.SrcPort(), sPort: l.DstPort()}
	f := e.rev[key]
	if f != nil && !now.Before(f.expires) {
		e.removeLocked(f)
		e.stats.FlowsExpired++
		f = nil
	}
	if f == nil {
		e.stats.DropsNoFlow++
		return Result{}
	}
	if key.proto == TCP {
		e.tcpFlagsLocked(f, l.TCPFlags(), false)
	} else if f.state == stNew {
		f.state = stEstablished
	}
	e.touchLocked(f, now)

	out := append([]byte(nil), l.Pkt...)
	nl, _ := packet.ParseL3(out)
	vIP, cIP := f.fwd.vIP, f.fwd.cIP
	nl.Rewrite(&vIP, &cIP, &f.fwd.vPort, &f.fwd.cPort)
	e.stats.Returned++
	return Result{Verdict: Forward, Packet: out, Dst: cIP}
}
