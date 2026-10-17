package vxlan

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"plaid/pkg/packet"
	"plaid/pkg/rdma"
)

// The RDMA transport carries what the overlay sends to a node over reliable-connected QPs
// instead of the UDP sockets, when both nodes have an active InfiniBand port in one subnet
// and both offer it. Everything else is as over UDP: the same VXLAN datagrams, the same
// receive lanes and their workers, the same bridge.
//
// Links. A node offers a link to every node it has a route to, with HELLO datagrams over
// the UDP overlay (see rdma_control.go): its QPs for that node, one per lane, and how far it
// is. A node connects its QPs to the numbers a HELLO gives and answers with its own; a link
// is up at a node once its QPs are connected to the peer's and the peer's HELLO says that
// the peer's are connected to its own, so that neither sends to a QP that cannot receive
// yet. A link is a generation of a node's QPs: a link that fails is replaced by a new one,
// never repaired, and the numbers of the generation and of the session (a random number of
// the transport) in every HELLO tell the old from the new.
//
// Messages. One RDMA message holds what one UDP_SEGMENT send would: a run of VXLAN
// datagrams of one size, the last possibly shorter (immediate data immRun and the size), or
// a whole TCP super-frame, a VXLAN header with VXLANFlagWhole, the virtio header and the
// frame (immWhole). The receiving lane hands a run to its worker as a read of its socket,
// and a super-frame as one that was put together from chunks. A frame's flow picks the
// send socket a run is for (packet.FlowIndex), and the send socket the QP, so that a flow
// stays on one QP and in order, and the QP the receiving lane.
//
// Memory. One registered region: each lane's receive slots, posted to its SRQ, and a pool of
// send slots. A message is copied into a send slot and posted; the slot comes back with the
// send's completion. A received message is handed on in its slot, as an owned
// packet.Buffer, whose release posts the slot to the SRQ again. A QP has at most
// maxInFlight sends outstanding; a message that finds it full, or no send slot free, is
// dropped and counted, as a full socket buffer drops: the bridge never waits for a peer.
//
// Failures. A send that its peer's HCA did not acknowledge for about 7 s (see
// rdma.QPTimeout) fails its link, which is recoverable: the peer is on UDP at once, and a
// new link is offered while the port is active and the peer answers. Any other error of a
// completion is taken for a bug and ends RDMA to that peer for the transport's life, as do
// rdmaMaxFailures failures. The device's events: a port that goes down takes the links down
// until it is active again, a new LID makes new links, a fatal device error ends RDMA.

const (
	rdmaSlotSize        = 66 << 10 // a VXLAN header, a virtio header and a frame of 64 KiB, or a run of 65,507 bytes
	rdmaDefaultRecv     = 128      // receive slots per lane
	rdmaDefaultSend     = 512      // send slots
	rdmaDefaultInFlight = 64       // sends outstanding per QP
	rdmaMaxFailures     = 3        // recoverable failures of links to one peer before it stays on UDP
	rdmaHelloEvery      = time.Second
	rdmaNoneHold        = 30 * time.Second // how long a NONE stops the offers to its sender
	rdmaSetupWarn       = time.Minute
	rdmaDyingMax        = 10 * time.Second // a failed link's QPs are destroyed once their sends are back, or after this
	rdmaCQPeers         = 256

	immKindShift = 24
	immRun       = 1 << immKindShift
	immWhole     = 2 << immKindShift
	immSizeMask  = 1<<immKindShift - 1
)

// RDMAConfig asks the engine to offer the RDMA transport on a port.
type RDMAConfig struct {
	Port        rdma.Port
	RecvSlots   int // per lane (0: 128)
	SendSlots   int // (0: 512)
	MaxInFlight int // per QP (0: 64)
}

// peerState is where a peer's link stands.
type peerState int

const (
	peerOffering   peerState = iota // HELLOs are sent; not up yet
	peerUp                          // traffic goes over RDMA
	peerWaiting                     // a link failed or was taken down, or the port is down: offered again (when the port is active)
	peerNotOffered                  // the peer said NONE: offered again later
	peerGivenUp                     // a fatal error, or too many failures: UDP for the transport's life
)

func (s peerState) String() string {
	return [...]string{"setting up", "up", "down, offered again", "not offered by the peer", "given up"}[s]
}

type rdmaTransport struct {
	oe          *OverlayEngine
	dev         rdmaDevice
	port        rdma.Port
	session     uint32
	mem         []byte
	mr          rdmaMR
	lanes       []*rdmaLane
	recvSlots   int
	sendBase    int        // the first send slot's index
	sendFree    chan int32 // free send slots
	maxInFlight int

	mu         sync.RWMutex
	attr       rdma.PortAttr // the port's, current
	portActive bool
	dead       bool // a fatal device error: no RDMA any more
	peers      map[netip.Addr]*rdmaPeer
	byQPN      map[uint32]*linkQP
	dying      []*rdmaLink

	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	txMsgs, txBytes, rxMsgs, rxBytes, txFull, txNoSlot, completionErrors, linksUp, linkFailures atomic.Uint64
}

// rdmaLane is a receive lane: its CQ (sends and receives of the QPs of every peer on this
// lane), its SRQ and receive slots, and the UDP lane whose worker takes what it receives.
type rdmaLane struct {
	t      *rdmaTransport
	i      int
	cq     rdmaCQ
	srq    rdmaSRQ
	bufs   []*packet.Buffer // one per receive slot, owned: its release posts the slot again
	first  int              // the index of the lane's first receive slot
	posted atomic.Int64
	rx     *rxLane
}

// rdmaPeer is a node the transport offers links to. Its fields are the transport's mu's,
// but for cur, which the send path reads.
type rdmaPeer struct {
	addr      netip.Addr
	port      int  // its VXLAN port, from its route or its HELLO
	routed    bool // the node has a route to it (else it is known by its HELLO only)
	state     peerState
	why       string // the last failure, NONE or setup error, for the status
	link      *rdmaLink
	gen       uint32 // the last generation made
	peerSess  uint32 // the peer's session, as its last HELLO said
	failures  int
	lastHello time.Time
	offered   time.Time // when the current offer began
	warned    bool
	noneUntil time.Time
	cur       atomic.Pointer[rdmaLink] // the link that is up, nil otherwise
}

// rdmaLink is a generation of a node's QPs for a peer.
type rdmaLink struct {
	peer      *rdmaPeer
	gen       uint32
	qps       []*linkQP
	k         int    // how many of qps are connected: the smaller lane count of the two nodes
	connected uint64 // the peer's (session, generation) its QPs are connected to; 0 not yet
	diedAt    time.Time
}

// linkQP is one QP of a link, with the send slots it has posted, in order.
type linkQP struct {
	link *rdmaLink
	lane int
	qp   rdmaQP
	psn  uint32
	mu   sync.Mutex
	fifo []int32
}

func unsafeSliceData(b []byte) unsafe.Pointer { return unsafe.Pointer(unsafe.SliceData(b)) }

func sessGen(sess, gen uint32) uint64 { return uint64(sess)<<32 | uint64(gen) }

func random24() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:]) & 0xffffff
}

func newSession() uint32 {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		if s := binary.BigEndian.Uint32(b[:]); s != 0 {
			return s
		}
	}
}

// startRDMA opens the port and sets the transport up for the engine's lanes. The caller
// starts nothing on error: the overlay goes on over UDP.
func startRDMA(oe *OverlayEngine, cfg RDMAConfig, rx []*rxLane) (*rdmaTransport, error) {
	if cfg.RecvSlots <= 0 {
		cfg.RecvSlots = rdmaDefaultRecv
	}
	if cfg.SendSlots <= 0 {
		cfg.SendSlots = rdmaDefaultSend
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = rdmaDefaultInFlight
	}
	dev, err := openRDMA(cfg.Port)
	if err != nil {
		return nil, err
	}
	attr, err := dev.Attr()
	if err != nil {
		_ = dev.Close()
		return nil, err
	}
	if attr.LinkLayer != rdma.LinkInfiniBand {
		_ = dev.Close()
		return nil, fmt.Errorf("%s is %s, not InfiniBand", cfg.Port, attr.LinkLayer)
	}
	t := &rdmaTransport{
		oe: oe, dev: dev, port: cfg.Port, session: newSession(), attr: attr,
		portActive: attr.State == rdma.PortActive, recvSlots: cfg.RecvSlots, maxInFlight: cfg.MaxInFlight,
		peers: make(map[netip.Addr]*rdmaPeer), byQPN: make(map[uint32]*linkQP), closed: make(chan struct{}),
	}
	nl := len(rx)
	t.sendBase = nl * cfg.RecvSlots
	total := t.sendBase + cfg.SendSlots
	// The region stays mapped for the process's life, even after Close: a TAP writer may still
	// hold a received slot then, and it is the process's end anyway.
	if t.mem, err = rdma.Map(total * rdmaSlotSize); err != nil {
		_ = dev.Close()
		return nil, fmt.Errorf("map %d MiB for RDMA: %w", total*rdmaSlotSize>>20, err)
	}
	if t.mr, err = dev.Register(t.mem); err != nil {
		_ = dev.Close()
		return nil, err
	}
	t.sendFree = make(chan int32, cfg.SendSlots)
	for i := 0; i < cfg.SendSlots; i++ {
		t.sendFree <- int32(t.sendBase + i)
	}
	fail := func(err error) (*rdmaTransport, error) {
		t.destroy()
		return nil, err
	}
	for i, l := range rx {
		ln := &rdmaLane{t: t, i: i, first: i * cfg.RecvSlots, rx: l}
		// A lane's CQ takes the completions of the receives and of the sends of every QP on it:
		// up to RecvSlots receives, and maxInFlight sends of each peer's QP, for up to
		// rdmaCQPeers peers before the lane's goroutine would have to fall behind to overrun it
		// (which would show as a CQ error event).
		if ln.cq, err = dev.NewCQ(cfg.RecvSlots + rdmaCQPeers*cfg.MaxInFlight); err != nil {
			return fail(err)
		}
		if ln.srq, err = dev.NewSRQ(cfg.RecvSlots); err != nil {
			_ = ln.cq.Close()
			return fail(err)
		}
		t.lanes = append(t.lanes, ln)
		ln.bufs = make([]*packet.Buffer, cfg.RecvSlots)
		wrs := make([]rdma.RecvWR, cfg.RecvSlots)
		for j := range ln.bufs {
			slot := int32(ln.first + j)
			ln.bufs[j] = packet.NewOwnedBuffer(func(*packet.Buffer) { ln.repost(slot) })
			wrs[j] = t.recvWR(slot)
		}
		if n, err := ln.srq.Post(t.mr, wrs); err != nil {
			return fail(fmt.Errorf("post %d receives: %w", cfg.RecvSlots, err))
		} else {
			ln.posted.Add(int64(n))
		}
	}
	for _, ln := range t.lanes {
		t.wg.Add(1)
		go ln.run()
	}
	t.wg.Add(2)
	go t.events()
	go t.tick()
	return t, nil
}

func (t *rdmaTransport) slot(i int32) []byte {
	return t.mem[int(i)*rdmaSlotSize : (int(i)+1)*rdmaSlotSize]
}

func (t *rdmaTransport) slotAddr(i int32) uintptr {
	return uintptr(unsafeSliceData(t.mem)) + uintptr(int(i)*rdmaSlotSize)
}

func (t *rdmaTransport) recvWR(slot int32) rdma.RecvWR {
	return rdma.RecvWR{ID: uint64(slot), Addr: t.slotAddr(slot), Len: rdmaSlotSize}
}

// repost gives a receive slot back to its lane's SRQ.
func (ln *rdmaLane) repost(slot int32) {
	select {
	case <-ln.t.closed:
		return
	default:
	}
	if n, err := ln.srq.Post(ln.t.mr, []rdma.RecvWR{ln.t.recvWR(slot)}); err == nil && n == 1 {
		ln.posted.Add(1)
	}
}

// --- sending -------------------------------------------------------------------------------

// upLink is the link to the node at a that is up, or nil.
func (t *rdmaTransport) upLink(a netip.Addr) *rdmaLink {
	t.mu.RLock()
	p := t.peers[a]
	t.mu.RUnlock()
	if p == nil {
		return nil
	}
	return p.cur.Load()
}

// sendRun sends a run of VXLAN datagrams of seg bytes (the last may be shorter) on the
// link's QP for the send socket sock.
func (t *rdmaTransport) sendRun(l *rdmaLink, sock int, data []byte, seg int) {
	t.post(l, sock, immRun|uint32(seg), data)
}

// sendWhole sends a TCP super-frame whole: the VXLAN header, the virtio header h and the
// frame, its MAC addresses set.
func (t *rdmaTransport) sendWhole(l *rdmaLink, sock int, vni uint32, h packet.VnetHeader, frame *packet.EthernetFrame) {
	var hdr [packet.VXLANHeaderLen + packet.VnetHeaderLen + packet.EthernetHeaderLen]byte
	b := packet.AppendVXLANHeader(hdr[:0], packet.VXLANFlags|packet.VXLANFlagWhole, vni)
	b = b[:packet.VXLANHeaderLen+packet.VnetHeaderLen]
	h.Put(b[packet.VXLANHeaderLen:])
	b = append(b, frame.DstMAC...)
	b = append(b, frame.SrcMAC...)
	b = binary.BigEndian.AppendUint16(b, frame.EtherType)
	t.post(l, sock, immWhole, b, frame.Payload)
}

// post copies parts into a send slot and posts it on the link's QP for sock, unless the QP
// has maxInFlight sends outstanding or no slot is free: then the message is dropped and
// counted. A QP that cannot post fails its link.
func (t *rdmaTransport) post(l *rdmaLink, sock int, imm uint32, parts ...[]byte) {
	q := l.qps[sock%l.k]
	q.mu.Lock()
	if len(q.fifo) >= t.maxInFlight {
		q.mu.Unlock()
		t.txFull.Add(1)
		return
	}
	var slot int32
	select {
	case slot = <-t.sendFree:
	default:
		q.mu.Unlock()
		t.txNoSlot.Add(1)
		return
	}
	buf, n := t.slot(slot), 0
	for _, p := range parts {
		n += copy(buf[n:], p)
	}
	_, err := q.qp.Post(t.mr, []rdma.SendWR{{ID: uint64(slot), Addr: t.slotAddr(slot), Len: uint32(n), Imm: imm, Signaled: true}})
	if err == nil {
		q.fifo = append(q.fifo, slot)
	}
	q.mu.Unlock()
	if err != nil {
		t.sendFree <- slot
		t.linkFailed(l, fmt.Sprintf("a send could not be posted: %v", err), true)
		return
	}
	t.txMsgs.Add(1)
	t.txBytes.Add(uint64(n))
}

// sendDone frees the send slots of q up to slot, which RC completed in order.
func (q *linkQP) sendDone(t *rdmaTransport, slot int32) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, s := range q.fifo {
		if s == slot {
			for _, f := range q.fifo[:i+1] {
				t.sendFree <- f
			}
			q.fifo = append(q.fifo[:0], q.fifo[i+1:]...)
			return
		}
	}
}

// --- receiving -----------------------------------------------------------------------------

// run takes the lane's completions until the CQ is closed: it polls, and waits on the CQ's
// channel (through the netpoller) when there is nothing.
func (ln *rdmaLane) run() {
	defer ln.t.wg.Done()
	wcs := make([]rdma.WC, rdma.MaxBatch)
	for {
		n, err := ln.cq.Poll(wcs)
		if err != nil {
			return
		}
		if n == 0 {
			if ln.cq.Arm() != nil {
				return
			}
			if n, err = ln.cq.Poll(wcs); err != nil {
				return
			}
			if n == 0 {
				if ln.cq.Wait() != nil {
					return // closed
				}
				continue
			}
		}
		for i := range wcs[:n] {
			ln.complete(&wcs[i])
		}
	}
}

func (ln *rdmaLane) complete(wc *rdma.WC) {
	t := ln.t
	t.mu.RLock()
	q := t.byQPN[wc.QPN]
	t.mu.RUnlock()
	slot := int32(wc.ID)
	if wc.Op == rdma.OpSend {
		if q != nil {
			q.sendDone(t, slot)
		}
		if wc.Status != rdma.WCSuccess && q != nil {
			t.completionFailed(q, wc.Status)
		}
		return
	}
	j := int(slot) - ln.first
	if j < 0 || j >= len(ln.bufs) {
		return // not this lane's: cannot be
	}
	ln.posted.Add(-1)
	if wc.Status != rdma.WCSuccess {
		ln.repost(slot)
		if q != nil {
			t.completionFailed(q, wc.Status)
		}
		return
	}
	if q == nil {
		ln.repost(slot) // a QP already gone: nothing to tell where it came from
		return
	}
	t.rxMsgs.Add(1)
	t.rxBytes.Add(uint64(wc.Len))
	data := t.slot(slot)[:wc.Len]
	src := netip.AddrPortFrom(q.link.peer.addr, 0) // a node's address, which is what the workers use
	buf := ln.bufs[j]
	var r rxRead
	switch wc.Imm &^ immSizeMask {
	case immRun:
		seg := int(wc.Imm & immSizeMask)
		if seg < packet.VXLANHeaderLen || len(data) == 0 {
			ln.invalid(slot)
			return
		}
		buf.Hold(data)
		r = rxRead{buf: buf, seg: seg, src: src}
	case immWhole:
		if len(data) < packet.VXLANHeaderLen+packet.VnetHeaderLen+packet.EthernetHeaderLen ||
			data[0]&(packet.VXLANFlags|packet.VXLANFlagWhole) != packet.VXLANFlags|packet.VXLANFlagWhole {
			ln.invalid(slot)
			return
		}
		buf.Hold(data[packet.VXLANHeaderLen:])
		r = rxRead{buf: buf, src: src, whole: true}
	default:
		ln.invalid(slot)
		return
	}
	t.oe.rxReads.Add(1)
	select {
	case ln.rx.queue <- r:
	default:
		t.oe.rxQueueDrops.Add(uint64(r.datagrams()))
		buf.Release() // posts the slot again
	}
}

func (ln *rdmaLane) invalid(slot int32) {
	ln.t.oe.rxInvalid.Add(1)
	ln.repost(slot)
}

// --- links ---------------------------------------------------------------------------------

// completionFailed is a completion in error on q: the flush of a QP that is going away is
// expected; "retry counter exceeded" (the peer's HCA did not answer) is recoverable; anything
// else is taken for a bug.
func (t *rdmaTransport) completionFailed(q *linkQP, st rdma.WCStatus) {
	if st == rdma.WCWRFlushErr {
		return
	}
	t.completionErrors.Add(1)
	t.linkFailed(q.link, st.String(), st == rdma.WCRetryExcErr)
}

// linkFailed takes a link down: the peer is on UDP at once, the link's QPs are flushed and
// destroyed once their sends are back, and the peer is told. A recoverable failure lets the
// peer be offered a new link, up to rdmaMaxFailures; any other ends RDMA to it.
func (t *rdmaTransport) linkFailed(l *rdmaLink, why string, recoverable bool) {
	t.mu.Lock()
	p := l.peer
	if p.link != l {
		t.mu.Unlock()
		return // an old link, already down
	}
	wasUp := p.state == peerUp
	t.takeDown(p)
	p.failures++
	t.linkFailures.Add(1)
	p.why = why
	given := !recoverable || p.failures >= rdmaMaxFailures
	if given {
		p.state = peerGivenUp
	} else {
		p.state = peerWaiting
	}
	down := t.controlFor(p, ctlDown, l)
	port, failures := p.port, p.failures
	t.mu.Unlock()
	t.sendControl(p.addr, port, down)
	switch {
	case given && !recoverable:
		fmt.Printf("[plaidd/rdma] The RDMA link to %s failed (%s), which should not happen: %s stays on UDP\n", p.addr, why, p.addr)
	case given:
		fmt.Printf("[plaidd/rdma] The RDMA link to %s failed (%s), failure %d of %d: %s stays on UDP\n", p.addr, why, failures, rdmaMaxFailures, p.addr)
	case wasUp:
		fmt.Printf("[plaidd/rdma] The RDMA link to %s failed (%s): on UDP until a new link is up\n", p.addr, why)
	}
}

// takeDown ends the peer's link, if any: the send path stops using it, its QPs go to the
// error state (what they have posted completes, flushed, which frees the send slots), and
// they are destroyed later. The caller holds t.mu.
func (t *rdmaTransport) takeDown(p *rdmaPeer) {
	l := p.link
	p.cur.Store(nil)
	p.link = nil
	if p.state == peerUp {
		p.state = peerOffering // the callers say more where they know more
	}
	if l == nil {
		return
	}
	for _, q := range l.qps {
		_ = q.qp.SetError()
	}
	l.diedAt = time.Now()
	t.dying = append(t.dying, l)
}

// newLink makes a generation of QPs for the peer. The caller holds t.mu.
func (t *rdmaTransport) newLink(p *rdmaPeer) (*rdmaLink, error) {
	p.gen++
	l := &rdmaLink{peer: p, gen: p.gen}
	for i, ln := range t.lanes {
		qp, err := t.dev.NewQP(ln.cq, ln.srq, t.maxInFlight)
		if err != nil {
			for _, q := range l.qps {
				delete(t.byQPN, q.qp.Num())
				_ = q.qp.Close()
			}
			return nil, err
		}
		q := &linkQP{link: l, lane: i, qp: qp, psn: random24()}
		l.qps = append(l.qps, q)
		t.byQPN[qp.Num()] = q
	}
	return l, nil
}

// destroyDying destroys the QPs of links that are down once their sends have come back,
// or after rdmaDyingMax. The caller holds t.mu.
func (t *rdmaTransport) destroyDying(now time.Time) {
	keep := t.dying[:0]
	for _, l := range t.dying {
		busy := false
		for _, q := range l.qps {
			q.mu.Lock()
			busy = busy || len(q.fifo) > 0
			q.mu.Unlock()
		}
		if busy && now.Sub(l.diedAt) < rdmaDyingMax {
			keep = append(keep, l)
			continue
		}
		for _, q := range l.qps {
			delete(t.byQPN, q.qp.Num())
			_ = q.qp.Close()
			q.mu.Lock()
			for _, s := range q.fifo { // sends that never completed: their slots are free now
				t.sendFree <- s
			}
			q.fifo = nil
			q.mu.Unlock()
		}
	}
	t.dying = keep
}

// --- the control datagrams --------------------------------------------------------------

// controlFor is a control datagram about l (nil: no link) for p. The caller holds t.mu.
func (t *rdmaTransport) controlFor(p *rdmaPeer, typ byte, l *rdmaLink) []byte {
	c := control{typ: typ, port: uint16(t.oe.LocalPort()), session: t.session, lid: t.attr.LID,
		mtu: uint16(t.attr.MTU), gidPrefix: t.attr.GIDPrefix, up: p.state == peerUp}
	if l != nil {
		c.gen = l.gen
		c.peerSess, c.peerGen = uint32(l.connected>>32), uint32(l.connected)
		if typ == ctlHello {
			for _, q := range l.qps {
				c.qps = append(c.qps, qpAddr{qpn: q.qp.Num(), psn: q.psn})
			}
		}
	}
	return c.appendTo(nil, t.oe.cfg.DefaultVNI)
}

// sendControl sends a control datagram to a node's VXLAN port from the first send socket.
func (oe *OverlayEngine) sendControlTo(a netip.Addr, port int, b []byte) {
	s := oe.socks.Load()
	if s == nil || port <= 0 || port > 65535 || b == nil {
		return
	}
	_, _ = s.tx[0].conn.WriteToUDPAddrPort(b, netip.AddrPortFrom(a, uint16(port)))
}

func (t *rdmaTransport) sendControl(a netip.Addr, port int, b []byte) {
	t.oe.sendControlTo(a, port, b)
}

// peer is the peer at a, made if new. The caller holds t.mu.
func (t *rdmaTransport) peer(a netip.Addr, port int) *rdmaPeer {
	p := t.peers[a]
	if p == nil {
		p = &rdmaPeer{addr: a, port: port, offered: time.Now()}
		t.peers[a] = p
	}
	return p
}

// control handles a control datagram's payload from the node at a.
func (t *rdmaTransport) control(a netip.Addr, b []byte) {
	c, err := parseControl(b)
	if err != nil {
		t.oe.rxInvalid.Add(1)
		return
	}
	t.mu.Lock()
	p := t.peer(a, int(c.port))
	p.port = int(c.port)
	var reply []byte
	switch c.typ {
	case ctlNone:
		t.takeDown(p)
		if p.state != peerGivenUp {
			p.state, p.why, p.noneUntil = peerNotOffered, "the peer does not offer RDMA to this node", time.Now().Add(rdmaNoneHold)
		}
	case ctlDown:
		if p.link != nil && p.link.connected == sessGen(c.session, c.gen) {
			wasUp := p.state == peerUp
			t.takeDown(p)
			if p.state != peerGivenUp {
				p.state, p.why = peerWaiting, "the peer took the link down"
			}
			if wasUp {
				fmt.Printf("[plaidd/rdma] %s took its RDMA link down: on UDP until a new link is up\n", a)
			}
		}
	case ctlHello:
		reply = t.hello(p, c)
	}
	port := p.port
	t.mu.Unlock()
	t.sendControl(a, port, reply)
}

// hello handles a HELLO and returns the answer, if any. The caller holds t.mu.
func (t *rdmaTransport) hello(p *rdmaPeer, c control) []byte {
	if t.dead || p.state == peerGivenUp || c.gidPrefix != t.attr.GIDPrefix || !t.portActive {
		return t.controlFor(p, ctlNone, nil)
	}
	if p.peerSess != 0 && p.peerSess != c.session { // the peer started again: a fresh start
		p.failures = 0
	}
	p.peerSess = c.session
	key := sessGen(c.session, c.gen)
	if p.link != nil && p.link.connected != 0 && p.link.connected != key {
		t.takeDown(p) // connected to a link the peer no longer has
	}
	if p.state == peerNotOffered || p.state == peerWaiting {
		p.state, p.offered, p.warned = peerOffering, time.Now(), false
	}
	if p.link == nil {
		l, err := t.newLink(p)
		if err != nil {
			p.why = "QPs could not be made: " + err.Error()
			return nil
		}
		p.link = l
	}
	l := p.link
	changed := false
	if l.connected == 0 {
		l.k = min(len(l.qps), len(c.qps))
		mtu := min(t.attr.MTU, int(c.mtu))
		for i := 0; i < l.k; i++ {
			if err := l.qps[i].qp.Connect(rdma.Address{LID: c.lid, QPN: c.qps[i].qpn, PSN: c.qps[i].psn}, l.qps[i].psn, mtu); err != nil {
				p.why = "QPs could not be connected: " + err.Error()
				t.takeDown(p)
				return nil
			}
		}
		l.connected = key
		changed = true
	}
	if c.peerSess == t.session && c.peerGen == l.gen && p.state != peerUp {
		p.state, p.why = peerUp, ""
		p.cur.Store(l)
		t.linksUp.Add(1)
		fmt.Printf("[plaidd/rdma] RDMA link to %s up (LID %d, %d QPs)\n", p.addr, c.lid, l.k)
	}
	// The peer needs an answer unless it is up on this very link: its QPs connected to these
	// and it knowing that these are connected to its own.
	if changed || !c.up || c.peerSess != t.session || c.peerGen != l.gen {
		return t.controlFor(p, ctlHello, l)
	}
	return nil
}

// noRDMA answers a HELLO when the engine has no transport: NONE, so that the peer stops.
func (oe *OverlayEngine) noRDMA(a netip.Addr, b []byte) {
	c, err := parseControl(b)
	if err != nil {
		oe.rxInvalid.Add(1)
		return
	}
	if c.typ != ctlHello {
		return
	}
	none := control{typ: ctlNone, port: uint16(oe.LocalPort()), session: 1}
	oe.sendControlTo(a, int(c.port), none.appendTo(nil, oe.cfg.DefaultVNI))
}

// --- the transport's own goroutines ------------------------------------------------------

// tick, every rdmaHelloEvery: the peers follow the routes, links not up are offered, and the
// QPs of links that are down are destroyed.
func (t *rdmaTransport) tick() {
	defer t.wg.Done()
	tk := time.NewTicker(rdmaHelloEvery)
	defer tk.Stop()
	for {
		t.offer(time.Now())
		select {
		case <-t.closed:
			return
		case <-tk.C:
		}
	}
}

type outgoing struct {
	a    netip.Addr
	port int
	b    []byte
}

func (t *rdmaTransport) offer(now time.Time) {
	routed := make(map[netip.Addr]int)
	for _, r := range t.oe.routes.Routes() {
		a, ok := netip.AddrFromSlice(r.RemoteHostIP)
		if !ok {
			continue
		}
		port := r.Port
		if port <= 0 {
			port = t.oe.cfg.Port
		}
		routed[a.Unmap()] = port
	}
	var out []outgoing
	t.mu.Lock()
	for a, port := range routed {
		p := t.peer(a, port)
		p.routed = true
	}
	for a, p := range t.peers {
		if _, ok := routed[a]; !ok && p.routed { // the route went: so does the peer
			t.takeDown(p)
			delete(t.peers, a)
			continue
		}
		if t.dead || !t.portActive {
			continue
		}
		switch p.state {
		case peerGivenUp, peerUp:
			continue
		case peerNotOffered:
			if now.Before(p.noneUntil) {
				continue
			}
			p.state, p.offered, p.warned = peerOffering, now, false
		case peerWaiting:
			p.state, p.offered, p.warned = peerOffering, now, false
		}
		if p.link == nil {
			l, err := t.newLink(p)
			if err != nil {
				p.why = "QPs could not be made: " + err.Error()
				continue
			}
			p.link = l
		}
		if now.Sub(p.offered) > rdmaSetupWarn && !p.warned {
			p.warned = true
			fmt.Printf("[plaidd/rdma] No RDMA link to %s after %v of offering it; %s stays on UDP meanwhile\n", a, rdmaSetupWarn, a)
		}
		if now.Sub(p.lastHello) >= rdmaHelloEvery/2 {
			p.lastHello = now
			out = append(out, outgoing{a, p.port, t.controlFor(p, ctlHello, p.link)})
		}
	}
	t.destroyDying(now)
	t.mu.Unlock()
	for _, o := range out {
		t.sendControl(o.a, o.port, o.b)
	}
}

// events follows the device's asynchronous events until the transport is closed.
func (t *rdmaTransport) events() {
	defer t.wg.Done()
	for {
		ev, err := t.dev.NextEvent()
		if err != nil {
			return
		}
		switch ev.Type {
		case rdma.EventPortError:
			t.allDown(func() { t.portActive = false }, "the port went down")
			fmt.Printf("[plaidd/rdma] %s is down: on UDP until it is active again\n", t.port)
		case rdma.EventPortActive:
			t.mu.Lock()
			t.portActive = true
			t.mu.Unlock()
			fmt.Printf("[plaidd/rdma] %s is active\n", t.port)
		case rdma.EventLIDChange, rdma.EventClientReregister:
			if a, err := t.dev.Attr(); err == nil {
				t.allDown(func() { t.attr = a; t.portActive = a.State == rdma.PortActive }, "the port's LID changed")
				fmt.Printf("[plaidd/rdma] %s: %s (LID %d): new links\n", t.port, ev.Type, a.LID)
			}
		case rdma.EventDeviceFatal:
			t.allDown(func() { t.dead = true }, "the device failed")
			fmt.Printf("[plaidd/rdma] %s failed (a fatal device error): RDMA ends, everything on UDP\n", t.port)
		case rdma.EventQPFatal, rdma.EventQPRequestError, rdma.EventQPAccessError:
			t.mu.RLock()
			q := t.byQPN[ev.QPN]
			t.mu.RUnlock()
			if q != nil {
				t.linkFailed(q.link, ev.Type.String(), false)
			}
		}
	}
}

// allDown takes every link down, not as a failure of a peer, after change (under t.mu),
// and tells the peers, so that they do not count it as one either.
func (t *rdmaTransport) allDown(change func(), why string) {
	var out []outgoing
	t.mu.Lock()
	change()
	for _, p := range t.peers {
		if l := p.link; l != nil && l.connected != 0 {
			out = append(out, outgoing{p.addr, p.port, t.controlFor(p, ctlDown, l)})
		}
		t.takeDown(p)
		if p.state != peerGivenUp {
			p.state, p.why = peerWaiting, why
		}
		if t.dead {
			p.state = peerGivenUp
		}
	}
	t.mu.Unlock()
	for _, o := range out {
		t.sendControl(o.a, o.port, o.b)
	}
}

// close stops the transport: the links, the goroutines, the device. The memory stays
// mapped (see startRDMA).
func (t *rdmaTransport) close() {
	t.closeOnce.Do(func() {
		close(t.closed)
		t.mu.Lock()
		for _, p := range t.peers {
			t.takeDown(p)
		}
		t.mu.Unlock()
		t.destroy()
	})
}

// destroy releases what startRDMA made, in the verbs' order.
func (t *rdmaTransport) destroy() {
	t.mu.Lock()
	for _, l := range t.dying {
		for _, q := range l.qps {
			_ = q.qp.Close()
		}
	}
	t.dying = nil
	t.byQPN = map[uint32]*linkQP{}
	t.mu.Unlock()
	for _, ln := range t.lanes {
		if ln.cq != nil {
			_ = ln.cq.Close() // ends the lane's goroutine
		}
	}
	// The events goroutine ends when the device is closed, the ticker on t.closed.
	select {
	case <-t.closed:
	default:
		close(t.closed)
	}
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	_ = t.dev.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	for _, ln := range t.lanes {
		if ln.srq != nil {
			_ = ln.srq.Close()
		}
	}
	if t.mr != nil {
		_ = t.mr.Close()
	}
}

// --- status ------------------------------------------------------------------------------

// RDMAStats is what Stats says about the transport.
type RDMAStats struct {
	Port       string // DEVICE:PORT
	LID        uint16
	MTU        int
	RateGbps   int
	PortActive bool
	Failed     bool // a fatal device error ended RDMA
	Peers      []RDMAPeer
	// Messages and bytes sent and received; sends dropped because the QP had its maximum
	// in flight (TxFull) or no send slot was free (TxNoSlot); completions in error (but the
	// flushes of links taken down); links that came up and that failed.
	TxMessages, TxBytes, RxMessages, RxBytes, TxFull, TxNoSlot, CompletionErrors, LinksUp, LinkFailures uint64
	RecvPosted                                                                                          []int64 // per lane
	PinnedBytes                                                                                         int
}

// RDMAPeer is a peer's link as Stats says it.
type RDMAPeer struct {
	Address  string
	State    string
	Why      string
	Failures int
	QPs      int
}

func (t *rdmaTransport) stats() *RDMAStats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	st := &RDMAStats{
		Port: t.port.String(), LID: t.attr.LID, MTU: t.attr.MTU, RateGbps: t.attr.RateGbps,
		PortActive: t.portActive, Failed: t.dead,
		TxMessages: t.txMsgs.Load(), TxBytes: t.txBytes.Load(), RxMessages: t.rxMsgs.Load(), RxBytes: t.rxBytes.Load(),
		TxFull: t.txFull.Load(), TxNoSlot: t.txNoSlot.Load(), CompletionErrors: t.completionErrors.Load(),
		LinksUp: t.linksUp.Load(), LinkFailures: t.linkFailures.Load(), PinnedBytes: len(t.mem),
	}
	for _, ln := range t.lanes {
		st.RecvPosted = append(st.RecvPosted, ln.posted.Load())
	}
	for a, p := range t.peers {
		rp := RDMAPeer{Address: a.String(), State: p.state.String(), Why: p.why, Failures: p.failures}
		if l := p.cur.Load(); l != nil {
			rp.QPs = l.k
		}
		st.Peers = append(st.Peers, rp)
	}
	sort.Slice(st.Peers, func(i, j int) bool { return st.Peers[i].Address < st.Peers[j].Address })
	return st
}
