package bridge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"plaid/pkg/packet"
	"plaid/pkg/svc"
)

var (
	ErrEndpointNotFound = errors.New("endpoint not found")
)

func ipAdd(ip net.IP, n uint32) net.IP {
	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}
	val := binary.BigEndian.Uint32(ip4) + n
	res := make(net.IP, 4)
	binary.BigEndian.PutUint32(res, val)
	return res
}

// MakeSlirpMAC returns the libslirp emulated host MAC for an IP address: 52:55:IP0:IP1:IP2:IP3
func MakeSlirpMAC(ip net.IP) net.HardwareAddr {
	ip4 := ip.To4()
	if ip4 == nil {
		return net.HardwareAddr{0x52, 0x55, 0x00, 0x00, 0x00, 0x00}
	}
	return net.HardwareAddr{0x52, 0x55, ip4[0], ip4[1], ip4[2], ip4[3]}
}

// Endpoint represents any entity connected to the user-space bridge.
type Endpoint interface {
	ID() string
	Name() string
	IP() net.IP
	MAC() net.HardwareAddr
	// Write delivers one Ethernet frame. The caller reuses frame's memory as soon as
	// Write returns, so an implementation must finish with it, or copy it, before then.
	Write(frame []byte) error
	Close() error
}

// writeFrame encodes frame into a pooled buffer and writes it to ep.
func writeFrame(ep Endpoint, frame *packet.EthernetFrame) error {
	buf := packet.GetBuffer()
	defer buf.Release()
	var err error
	if buf.B, err = frame.MarshalTo(buf.B); err != nil {
		return err
	}
	return ep.Write(buf.B)
}

// floodFrame is writeFrame for every endpoint.
func (b *Bridge) floodFrame(frame *packet.EthernetFrame) error {
	buf := packet.GetBuffer()
	defer buf.Release()
	var err error
	if buf.B, err = frame.MarshalTo(buf.B); err != nil {
		return err
	}
	return b.flood(nil, buf.B)
}

// OverlayBatcher returns an OverlayBatch for the cross-host overlay (e.g. VXLAN), which
// every packet that needs to go to another node is sent through.
type OverlayBatcher func() OverlayBatch

// OutboundHandler is invoked when a packet needs outbound NAT (e.g. slirp4netns).
type OutboundHandler func(frame *packet.EthernetFrame) error

// FilterHandler is a hook evaluated on incoming frames (user-space br_netfilter).
// Returning false causes the packet to be dropped.
type FilterHandler func(frame *packet.EthernetFrame, srcEP Endpoint) bool

// BridgeConfig configures the user-space bridge parameters.
type BridgeConfig struct {
	GatewayIP   net.IP
	GatewayMAC  net.HardwareAddr
	GatewayMode string
	NodeCIDR    *net.IPNet
	ClusterCIDR *net.IPNet
	FDBTTL      time.Duration
}

// Bridge is the user-space L2/L3 switch.
type Bridge struct {
	mu          sync.RWMutex
	endpoints   map[string]Endpoint
	ipToEp      map[string]Endpoint
	fdb         *FDB
	gatewayIP   net.IP
	gatewayMAC  net.HardwareAddr
	gatewayMode string
	bubble      Endpoint
	routeKnown  func(net.IP) bool
	nodeCIDR    *net.IPNet
	clusterCIDR *net.IPNet

	overlayBatcher  OverlayBatcher
	outboundHandler OutboundHandler
	filterHandler   FilterHandler

	// services translates traffic to Service addresses (tap mode). It owns two kinds of
	// destination: the Service range and its own source address, to which the backends
	// answer. Packets to either are handed to it before anything else decides where they
	// go, including the guard against the bubble sending non-cluster traffic back.
	services *svc.Engine
}

// NewBridge creates and initializes a Bridge.
func NewBridge(cfg BridgeConfig) *Bridge {
	fdbTTL := cfg.FDBTTL
	if fdbTTL <= 0 {
		fdbTTL = 5 * time.Minute
	}

	b := &Bridge{
		endpoints:   make(map[string]Endpoint),
		ipToEp:      make(map[string]Endpoint),
		fdb:         NewFDB(fdbTTL),
		gatewayIP:   cfg.GatewayIP,
		gatewayMAC:  cfg.GatewayMAC,
		gatewayMode: cfg.GatewayMode,
		nodeCIDR:    cfg.NodeCIDR,
		clusterCIDR: cfg.ClusterCIDR,
	}

	return b
}

// SetBubbleEndpoint attaches the bubble kernel without registering it as a pod.
// GatewayMAC is Plaid's synthetic next-hop MAC; the endpoint MAC is the TAP's MAC.
func (b *Bridge) SetBubbleEndpoint(ep Endpoint) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bubble = ep
}

func (b *Bridge) SetRouteChecker(check func(net.IP) bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.routeKnown = check
}

func (b *Bridge) BubbleEndpoint() Endpoint {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.bubble
}

func (b *Bridge) knownRemote(ip net.IP) bool {
	if b.clusterCIDR == nil || !b.clusterCIDR.Contains(ip) || (b.nodeCIDR != nil && b.nodeCIDR.Contains(ip)) {
		return false
	}
	b.mu.RLock()
	check := b.routeKnown
	b.mu.RUnlock()
	return check == nil || check(ip)
}

func (b *Bridge) writeBubble(x *Batch, frame *packet.EthernetFrame) error {
	b.mu.RLock()
	ep := b.bubble
	b.mu.RUnlock()
	if ep == nil {
		return fmt.Errorf("bubble gateway is unavailable")
	}
	frame.DstMAC = ep.MAC()
	if len(frame.SrcMAC) != 6 || bytes.Equal(frame.SrcMAC, ep.MAC()) {
		frame.SrcMAC = b.gatewayMAC
	}
	return x.deliver(ep, frame, nil)
}

// SetServiceEngine attaches the Service engine (tap mode).
func (b *Bridge) SetServiceEngine(e *svc.Engine) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.services = e
}

func (b *Bridge) serviceEngine() *svc.Engine {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.services
}

// SetOverlayBatcher sets the overlay for cross-host packets: the bridge holds the frames
// for it until the end of a Batch and sends them through the OverlayBatch that n makes.
func (b *Bridge) SetOverlayBatcher(n OverlayBatcher) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.overlayBatcher = n
}

// SetOutboundHandler sets the handler for external/internet egress packets.
func (b *Bridge) SetOutboundHandler(h OutboundHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.outboundHandler = h
}

// SetFilterHandler sets the packet filtering hook.
func (b *Bridge) SetFilterHandler(h FilterHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.filterHandler = h
}

// AddEndpoint registers a new pod or virtual endpoint with the bridge.
func (b *Bridge) AddEndpoint(ep Endpoint) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := ep.ID()
	if old, exists := b.endpoints[id]; exists {
		_ = old.Close()
		delete(b.endpoints, id)
		if old.IP() != nil {
			delete(b.ipToEp, old.IP().String())
		}
		if old.MAC() != nil {
			b.fdb.Delete(old.MAC())
		}
		b.fdb.DeleteByEndpoint(id)
	}

	b.endpoints[id] = ep
	if ep.IP() != nil {
		b.ipToEp[ep.IP().String()] = ep
	}
	if ep.MAC() != nil {
		b.fdb.SetStatic(ep.MAC(), ep)
	}

	return nil
}

// RemoveEndpoint unregisters an endpoint and cleans up its FDB entries.
func (b *Bridge) RemoveEndpoint(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	ep, exists := b.endpoints[id]
	if !exists {
		return ErrEndpointNotFound
	}

	delete(b.endpoints, id)
	if ep.IP() != nil {
		delete(b.ipToEp, ep.IP().String())
	}
	if ep.MAC() != nil {
		b.fdb.Delete(ep.MAC())
	}
	b.fdb.DeleteByEndpoint(id)

	return ep.Close()
}

// GetEndpoint returns an endpoint by its ID.
func (b *Bridge) GetEndpoint(id string) (Endpoint, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	ep, ok := b.endpoints[id]
	return ep, ok
}

// GetEndpointByIP returns an endpoint matching an IP address.
func (b *Bridge) GetEndpointByIP(ip net.IP) (Endpoint, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	ep, ok := b.ipToEp[ip.String()]
	return ep, ok
}

// Endpoints returns a slice of all currently registered endpoints.
func (b *Bridge) Endpoints() []Endpoint {
	b.mu.RLock()
	defer b.mu.RUnlock()
	eps := make([]Endpoint, 0, len(b.endpoints))
	for _, ep := range b.endpoints {
		eps = append(eps, ep)
	}
	return eps
}

// ProcessFrame receives a raw Ethernet frame from an endpoint and forwards it.
func (b *Bridge) ProcessFrame(srcEP Endpoint, raw []byte) error {
	x := b.NewBatch()
	defer x.Release()
	err := x.ProcessFrame(srcEP, raw)
	if ferr := x.Flush(); err == nil {
		err = ferr
	}
	return err
}

// ProcessFrame is Bridge.ProcessFrame within the batch: what the frame sends out is held
// until the batch is flushed.
func (x *Batch) ProcessFrame(srcEP Endpoint, raw []byte) error {
	return x.processFrame(srcEP, raw, false)
}

// processFrame is ProcessFrame for a frame whose transport checksum is known to be right
// if trusted (see packet.EthernetFrame).
func (x *Batch) processFrame(srcEP Endpoint, raw []byte, trusted bool) error {
	b := x.b
	frame, err := packet.ParseEthernet(raw)
	if err != nil {
		return fmt.Errorf("failed to parse ethernet frame: %w", err)
	}
	frame.Trusted = trusted
	x.src = srcEP

	// A super-frame that would only be looked at is not filtered or learnt from.
	if !x.sup.dry {
		// Filter hook (user-space br_netfilter)
		b.mu.RLock()
		filter := b.filterHandler
		b.mu.RUnlock()
		if filter != nil && !filter(frame, srcEP) {
			// Dropped by policy
			x.sup.whole = x.sup.on
			return nil
		}

		// Dynamic MAC learning
		if srcEP != nil && srcEP != b.BubbleEndpoint() && frame.SrcMAC != nil && !bytes.Equal(frame.SrcMAC, b.gatewayMAC) {
			b.fdb.Learn(frame.SrcMAC, srcEP)
		}
	}

	// Handle Broadcast / Multicast
	if frame.IsBroadcast() || frame.IsMulticast() {
		if x.sup.on {
			return nil // goes to everyone, or answers the sender
		}
		return b.handleBroadcast(srcEP, frame, raw)
	}

	// Handle Unicast
	return b.handleUnicast(x, srcEP, frame, raw)
}

func (b *Bridge) handleBroadcast(srcEP Endpoint, frame *packet.EthernetFrame, raw []byte) error {
	// If it's an ARP request, check if it's asking for known local endpoints or bridge/gateway/slirp
	if frame.EtherType == packet.EtherTypeARP {
		arpPkt, err := packet.ParseARP(frame.Payload)
		if err == nil && arpPkt.Operation == packet.ARPOperationRequest {
			// 1. Check if target IP belongs to another known local pod
			b.mu.RLock()
			targetEP, exists := b.ipToEp[arpPkt.TargetIP.String()]
			b.mu.RUnlock()
			if exists && targetEP != nil && targetEP.MAC() != nil {
				// Proxy ARP response for local pod
				replyFrame, err := packet.NewARPReply(arpPkt, targetEP.MAC(), arpPkt.TargetIP)
				if err == nil {
					replyBytes, err := replyFrame.Marshal()
					if err == nil && srcEP != nil {
						return srcEP.Write(replyBytes)
					}
				}
			}

			// 2. Check if asking for Gateway IP, slirp host/DNS alias, or remote overlay pod
			if b.gatewayMAC != nil {
				// The Service engine's address answers for the engine, whoever asks: the
				// bubble's kernel routes Service addresses through it, and backends on
				// this node reply to it on-link.
				if eng := b.serviceEngine(); eng != nil && b.gatewayMode == "tap" && arpPkt.TargetIP.Equal(eng.SNATAddr()) {
					replyFrame, err := packet.NewARPReply(arpPkt, b.gatewayMAC, arpPkt.TargetIP)
					if err == nil {
						replyBytes, err := replyFrame.Marshal()
						if err == nil && srcEP != nil {
							return srcEP.Write(replyBytes)
						}
					}
				}

				// Gateway ARP
				if b.gatewayIP != nil && arpPkt.TargetIP.Equal(b.gatewayIP) && srcEP != b.BubbleEndpoint() {
					arpMAC := b.gatewayMAC
					if b.gatewayMode == "tap" && b.BubbleEndpoint() != nil {
						arpMAC = b.BubbleEndpoint().MAC()
					}
					replyFrame, err := packet.NewARPReply(arpPkt, arpMAC, b.gatewayIP)
					if err == nil {
						replyBytes, err := replyFrame.Marshal()
						if err == nil && srcEP != nil {
							return srcEP.Write(replyBytes)
						}
					}
				}

				// Slirp Host / DNS ARP (52:55:IP:IP:IP:IP)
				if b.gatewayMode != "tap" && b.nodeCIDR != nil {
					baseIP := b.nodeCIDR.IP.Mask(b.nodeCIDR.Mask)
					hostIP := ipAdd(baseIP, 2)
					dnsIP := ipAdd(baseIP, 3)
					if (hostIP != nil && arpPkt.TargetIP.Equal(hostIP)) ||
						(dnsIP != nil && arpPkt.TargetIP.Equal(dnsIP)) {
						slirpMAC := MakeSlirpMAC(arpPkt.TargetIP)
						replyFrame, err := packet.NewARPReply(arpPkt, slirpMAC, arpPkt.TargetIP)
						if err == nil {
							replyBytes, err := replyFrame.Marshal()
							if err == nil && srcEP != nil {
								return srcEP.Write(replyBytes)
							}
						}
					}
				}

				// Overlay remote pod ARP
				if b.knownRemote(arpPkt.TargetIP) {
					replyFrame, err := packet.NewARPReply(arpPkt, b.gatewayMAC, arpPkt.TargetIP)
					if err == nil {
						replyBytes, err := replyFrame.Marshal()
						if err == nil && srcEP != nil {
							return srcEP.Write(replyBytes)
						}
					}
				}
				if b.gatewayMode == "tap" && b.nodeCIDR != nil && b.nodeCIDR.Contains(arpPkt.TargetIP) {
					// An unknown local pod must not be resolved or flooded to the TAP.
					return nil
				}
			}
		}
	}
	if b.gatewayMode == "tap" {
		return nil
	}

	// Flood to all other local endpoints
	return b.flood(srcEP, raw)
}

func (b *Bridge) handleUnicast(x *Batch, srcEP Endpoint, frame *packet.EthernetFrame, raw []byte) error {
	if b.gatewayMode == "tap" && frame.EtherType == packet.EtherTypeIPv4 {
		ipPkt, err := packet.ParseIPv4(frame.Payload)
		if err != nil {
			return err
		}
		return b.routeTapTraffic(x, srcEP, frame, ipPkt.Header.DstIP)
	}
	// 1. Direct local delivery: Lookup destination MAC in FDB
	targetEP := b.fdb.Lookup(frame.DstMAC)
	if targetEP != nil {
		if srcEP != nil && targetEP.ID() == srcEP.ID() {
			// Hairpin or self-destination
			return nil
		}
		return x.deliver(targetEP, frame, raw)
	}

	// 2. If destination is a slirp emulated MAC (52:55:xx:xx:xx:xx), forward to outbound handler
	if len(frame.DstMAC) == 6 && frame.DstMAC[0] == 0x52 && frame.DstMAC[1] == 0x55 {
		b.mu.RLock()
		outbound := b.outboundHandler
		b.mu.RUnlock()
		if outbound != nil {
			if x.sup.on {
				return nil
			}
			return outbound(frame)
		}
	}

	// 3. If destination is the Gateway MAC, inspect IP header
	if b.gatewayMAC != nil && bytes.Equal(frame.DstMAC, b.gatewayMAC) {
		return b.routeGatewayTraffic(x, srcEP, frame)
	}

	// 3. If destination MAC is unknown and it's IPv4:
	if frame.EtherType == packet.EtherTypeIPv4 {
		ipPkt, err := packet.ParseIPv4(frame.Payload)
		if err == nil {
			dstIP := ipPkt.Header.DstIP
			// Is it a local pod by IP?
			b.mu.RLock()
			localTarget, ok := b.ipToEp[dstIP.String()]
			b.mu.RUnlock()
			if ok && localTarget != nil {
				return x.deliver(localTarget, frame, raw)
			}

			// Is it an overlay remote node destination?
			if b.clusterCIDR != nil && b.clusterCIDR.Contains(dstIP) &&
				(b.nodeCIDR == nil || !b.nodeCIDR.Contains(dstIP)) {
				b.mu.RLock()
				overlay := b.overlayBatcher != nil
				b.mu.RUnlock()
				if overlay {
					return x.sendOverlay(frame, dstIP)
				}
			}

			// Outbound internet / non-cluster destination
			b.mu.RLock()
			outbound := b.outboundHandler
			b.mu.RUnlock()
			if outbound != nil {
				if x.sup.on {
					return nil
				}
				return outbound(frame)
			}
		}
	}

	// Unknown unicast: flood to all other endpoints
	if x.sup.on {
		return nil
	}
	return b.flood(srcEP, raw)
}

func (b *Bridge) routeTapTraffic(x *Batch, srcEP Endpoint, frame *packet.EthernetFrame, dstIP net.IP) error {
	if eng := b.serviceEngine(); eng != nil {
		var dst [4]byte
		copy(dst[:], dstIP.To4())
		if eng.Owns(dst) {
			if x.sup.on {
				return b.serviceSuper(x, eng, frame)
			}
			return b.serviceTraffic(x, eng, frame.Payload)
		}
	}
	b.mu.RLock()
	local := b.ipToEp[dstIP.String()]
	overlay := b.overlayBatcher != nil
	b.mu.RUnlock()
	if local != nil {
		if srcEP != nil && srcEP.ID() == local.ID() {
			return nil
		}
		frame.DstMAC = local.MAC()
		if srcEP == nil {
			frame.SrcMAC = b.gatewayMAC
		}
		return x.deliver(local, frame, nil)
	}
	if b.gatewayIP != nil && dstIP.Equal(b.gatewayIP) {
		return b.writeBubble(x, frame)
	}
	if b.knownRemote(dstIP) {
		if !overlay {
			return fmt.Errorf("overlay unavailable for %s", dstIP)
		}
		return x.sendOverlay(frame, dstIP)
	}
	if b.clusterCIDR != nil && b.clusterCIDR.Contains(dstIP) {
		return nil
	}
	if srcEP == b.BubbleEndpoint() {
		return fmt.Errorf("bubble emitted non-overlay destination %s", dstIP)
	}
	return b.writeBubble(x, frame)
}

// serviceTraffic hands a packet to the Service engine and sends what comes out: the
// translated packet towards its backend (or back to the client), or an ICMP refusal to
// the client. Everything the engine emits is routed here by where its destination is,
// never through the checks for packets that come from the bubble or from pods, which it
// is not.
func (b *Bridge) serviceTraffic(x *Batch, eng *svc.Engine, ipPacket []byte) error {
	res := eng.Process(ipPacket)
	if res.Verdict == svc.Drop {
		return nil
	}
	return b.routeEngineOutput(x, res.Packet, net.IP(res.Dst[:]))
}

// serviceSuper is serviceTraffic for the TCP super-frame being routed (x.sup): the engine
// translates it as one packet and the result is routed like any packet the engine emits,
// except that it goes whole to one endpoint that takes super-frames, or to a node that does
// over the overlay. Where it cannot (the overlay otherwise, a pod without offloads, the pod
// that sent it) the bridge cuts it now, after the translation, and routes the segments one
// by one, not through the engine again. An ICMP
// refusal is an ordinary packet. Once the engine has the frame it is not for the caller to
// cut. Before that, a frame whose IP header does not state the length of all of it is left
// to the segments, as the engine's packet is what the header says.
func (b *Bridge) serviceSuper(x *Batch, eng *svc.Engine, frame *packet.EthernetFrame) error {
	s := &x.sup
	if ip := frame.Payload; len(ip) < packet.IPv4HeaderMinLen || int(binary.BigEndian.Uint16(ip[2:])) != len(ip) {
		return nil
	}
	if s.dry {
		s.whole = true
		return nil
	}
	res := eng.ProcessSuper(frame.Payload)
	switch res.Verdict {
	case svc.Drop:
		s.whole = true
		return nil
	case svc.Reply:
		s.on, s.whole = false, true
		return b.routeEngineOutput(x, res.Packet, net.IP(res.Dst[:]))
	}
	dstIP := net.IP(res.Dst[:])
	err := b.routeEngineOutput(x, res.Packet, dstIP) // while s.on, only a delivery that takes it whole does anything
	if s.whole {
		return err
	}
	s.on, s.whole = false, true
	segments, serr := x.seg.Segment(s.h, s.raw)
	if serr != nil {
		return serr
	}
	for _, seg := range segments {
		if serr := b.routeEngineOutput(x, seg[packet.EthernetHeaderLen:], dstIP); serr != nil && err == nil {
			err = serr
		}
	}
	return err
}

func (b *Bridge) routeEngineOutput(x *Batch, ipPacket []byte, dstIP net.IP) error {
	frame := &packet.EthernetFrame{SrcMAC: b.gatewayMAC, EtherType: packet.EtherTypeIPv4, Payload: ipPacket}
	b.mu.RLock()
	local := b.ipToEp[dstIP.String()]
	overlay := b.overlayBatcher != nil
	b.mu.RUnlock()
	switch {
	case local != nil:
		frame.DstMAC = local.MAC()
		return x.deliver(local, frame, nil)
	case b.gatewayIP != nil && dstIP.Equal(b.gatewayIP):
		// The client is the bubble itself.
		return b.writeBubble(x, frame)
	case b.knownRemote(dstIP):
		if !overlay {
			return fmt.Errorf("overlay unavailable for %s", dstIP)
		}
		return x.sendOverlay(frame, dstIP)
	case b.clusterCIDR != nil && b.clusterCIDR.Contains(dstIP):
		// A pod address that is not here and not routed: nowhere to go.
		return nil
	}
	// Not a pod: a Service backend outside the pod network (the API server, say), for
	// the bubble's kernel to route.
	return b.writeBubble(x, frame)
}

func (b *Bridge) routeGatewayTraffic(x *Batch, srcEP Endpoint, frame *packet.EthernetFrame) error {
	if frame.EtherType != packet.EtherTypeIPv4 {
		return nil
	}

	ipPkt, err := packet.ParseIPv4(frame.Payload)
	if err != nil {
		return err
	}

	dstIP := ipPkt.Header.DstIP

	// 0. If destination is the Gateway IP itself and it's an ICMP Echo Request, reply!
	if b.gatewayIP != nil && dstIP.Equal(b.gatewayIP) && ipPkt.Header.Protocol == packet.IPProtocolICMP {
		replyData := packet.CreateICMPEchoReply(ipPkt)
		if replyData != nil && srcEP != nil {
			replyEther := &packet.EthernetFrame{
				DstMAC:    frame.SrcMAC,
				SrcMAC:    b.gatewayMAC,
				EtherType: packet.EtherTypeIPv4,
				Payload:   replyData,
			}
			return x.deliver(srcEP, replyEther, nil)
		}
		return nil
	}

	// 1. If destination is another local pod
	b.mu.RLock()
	localTarget, ok := b.ipToEp[dstIP.String()]
	b.mu.RUnlock()
	if ok && localTarget != nil {
		// Rewrite destination MAC to the pod's MAC
		frame.DstMAC = localTarget.MAC()
		return x.deliver(localTarget, frame, nil)
	}

	// 2. If destination is in the cluster overlay (remote node)
	if b.clusterCIDR != nil && b.clusterCIDR.Contains(dstIP) &&
		(b.nodeCIDR == nil || !b.nodeCIDR.Contains(dstIP)) {
		b.mu.RLock()
		overlay := b.overlayBatcher != nil
		b.mu.RUnlock()
		if overlay {
			return x.sendOverlay(frame, dstIP)
		}
	}

	// 3. External / default route: send to slirp4netns
	b.mu.RLock()
	outbound := b.outboundHandler
	b.mu.RUnlock()
	if outbound != nil {
		if x.sup.on {
			return nil
		}
		return outbound(frame)
	}

	return nil
}

// InjectFrame injects a frame into the bridge (e.g. from VXLAN or slirp4netns return traffic).
func (b *Bridge) InjectFrame(frame *packet.EthernetFrame) error {
	x := b.NewBatch()
	defer x.Release()
	err := x.InjectFrame(frame)
	if ferr := x.Flush(); err == nil {
		err = ferr
	}
	return err
}

// InjectFrame is Bridge.InjectFrame within the batch: what the frame sends out is held
// until the batch is flushed. It is also the routing of InjectSuper, which sets x.sup:
// then only a delivery to the one endpoint that takes the frame whole does anything, and
// where the routing would do anything else with it, it leaves the frame alone.
func (x *Batch) InjectFrame(frame *packet.EthernetFrame) error {
	b := x.b
	x.src = nil
	if b.gatewayMode == "tap" {
		if frame.EtherType != packet.EtherTypeIPv4 {
			return nil
		}
		ipPkt, err := packet.ParseIPv4(frame.Payload)
		if err != nil {
			return err
		}
		if b.gatewayIP != nil && ipPkt.Header.DstIP.Equal(b.gatewayIP) {
			frame.SrcMAC = b.gatewayMAC
			return b.writeBubble(x, frame)
		}
		// A backend on another node answering the Service engine's address.
		if eng := b.serviceEngine(); eng != nil && eng.SNATAddr().Equal(ipPkt.Header.DstIP) {
			if x.sup.on {
				return b.serviceSuper(x, eng, frame)
			}
			return b.serviceTraffic(x, eng, frame.Payload)
		}
		if b.nodeCIDR == nil || !b.nodeCIDR.Contains(ipPkt.Header.DstIP) {
			return nil
		}
		b.mu.RLock()
		target := b.ipToEp[ipPkt.Header.DstIP.String()]
		b.mu.RUnlock()
		if target == nil {
			return nil
		}
		frame.DstMAC = target.MAC()
		frame.SrcMAC = b.gatewayMAC
		return x.deliver(target, frame, nil)
	}
	if frame.IsBroadcast() || frame.IsMulticast() {
		if x.sup.on {
			return nil
		}
		return b.floodFrame(frame)
	}

	// Try lookup by destination IP first
	var targetEP Endpoint
	if frame.EtherType == packet.EtherTypeIPv4 {
		ipPkt, err := packet.ParseIPv4(frame.Payload)
		if err == nil {
			b.mu.RLock()
			targetEP = b.ipToEp[ipPkt.Header.DstIP.String()]
			b.mu.RUnlock()
		}
	}

	// If not found by IP, try lookup by destination MAC in FDB
	if targetEP == nil {
		targetEP = b.fdb.Lookup(frame.DstMAC)
	}

	if targetEP != nil {
		frame.DstMAC = targetEP.MAC()

		// If packet is from an on-link slirp host (52:55... and inside nodeCIDR), preserve slirp's SrcMAC.
		// Otherwise (cross-host VXLAN or external Internet via gateway), rewrite SrcMAC to gatewayMAC.
		isSlirpLocal := false
		if frame.EtherType == packet.EtherTypeIPv4 && len(frame.SrcMAC) == 6 &&
			frame.SrcMAC[0] == 0x52 && frame.SrcMAC[1] == 0x55 && b.nodeCIDR != nil {
			ipPkt, err := packet.ParseIPv4(frame.Payload)
			if err == nil && b.nodeCIDR.Contains(ipPkt.Header.SrcIP) {
				isSlirpLocal = true
			}
		}

		if !isSlirpLocal && b.gatewayMAC != nil {
			frame.SrcMAC = b.gatewayMAC
		}

		return x.deliver(targetEP, frame, nil)
	}

	// Flood if still unknown
	if x.sup.on {
		return nil
	}
	return b.floodFrame(frame)
}

// InjectSuper is InjectFrame for a TCP super-frame that came through the overlay and that
// the virtio header h describes (packet.CheckTSO accepts them). The frame lies in buf,
// after the room for a virtio header, and the batch owns the buffer from the call on. The
// frame is delivered whole to the one endpoint that takes super-frames if that is where
// InjectFrame would deliver it, which is how a pod with offloads gets it, or translated
// whole if it is for the Service engine, as the super-frames of the pods and the bubble
// are. An endpoint whose writer can take the buffer is given it, and writes the frame from
// it when its turn comes, without a copy. Otherwise the frame is cut into the segments it
// stands for, with their checksums done, and they are injected one by one. Whole or cut,
// it is never sent over the overlay again, and the buffer is released before the call
// returns unless a writer took it.
func (x *Batch) InjectSuper(h packet.VnetHeader, buf *packet.Buffer) error {
	raw := buf.B[packet.VnetHeaderLen:]
	frame, err := packet.ParseEthernet(raw)
	if err != nil {
		buf.Release()
		return err
	}
	x.sup = superFrame{on: true, h: h, raw: raw, buf: buf}
	err = x.InjectFrame(frame)
	whole, rest := x.sup.whole, x.sup.buf
	if rest != nil {
		defer rest.Release() // no writer took it: released once the segments, if any, are routed
	}
	x.sup = superFrame{}
	if whole {
		return err
	}
	segments, serr := x.seg.Segment(h, raw)
	if serr != nil {
		return serr
	}
	for _, seg := range segments {
		f, perr := packet.ParseEthernet(seg)
		if perr == nil {
			f.Trusted = true // the checksums are the ones the Segmenter has just computed
			perr = x.InjectFrame(f)
		}
		if perr != nil && err == nil {
			err = perr
		}
	}
	return err
}

func (b *Bridge) flood(srcEP Endpoint, raw []byte) error {
	b.mu.RLock()
	eps := make([]Endpoint, 0, len(b.endpoints))
	for _, ep := range b.endpoints {
		if srcEP != nil && ep.ID() == srcEP.ID() {
			continue
		}
		eps = append(eps, ep)
	}
	b.mu.RUnlock()

	var firstErr error
	for _, ep := range eps {
		if err := ep.Write(raw); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
