package vxlan

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"plaid/pkg/rdma"
)

// fakeFabric is an InfiniBand fabric in memory for the RDMA transport's tests: devices with
// a LID each, whose RC QPs deliver a send into a receive buffer posted to the peer QP's SRQ,
// as the HCAs would. A send that finds no receive posted waits for one ("receiver not
// ready", retried for ever as rnr_retry 7 does); a QP that is not connected, or in the error
// state, flushes what it is given; a message larger than the receive buffer fails both ends.
// Failures and events are injected on demand.
type fakeFabric struct {
	mu   sync.Mutex
	devs map[string]*fakeDev
	qps  map[uint32]*fakeQP
	next uint32
	// What a real device would not survive: an object used after it was destroyed, or
	// destroyed while something made from it is alive (the order the verbs require).
	violations []string
}

func (f *fakeFabric) violate(format string, a ...any) {
	f.violations = append(f.violations, fmt.Sprintf(format, a...))
}

// checkOrder reports what the transport did that a real device would not have survived.
func (f *fakeFabric) checkOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.violations...)
}

func newFakeFabric() *fakeFabric {
	return &fakeFabric{devs: map[string]*fakeDev{}, qps: map[uint32]*fakeQP{}, next: 100}
}

// add makes a device with an active port of the given LID.
func (f *fakeFabric) add(name string, lid uint16) *fakeDev {
	d := &fakeDev{f: f, name: name, live: map[any]string{}, events: make(chan rdma.Event, 8), closed: make(chan struct{}),
		attr: rdma.PortAttr{State: rdma.PortActive, LinkLayer: rdma.LinkInfiniBand, LID: lid, MTU: 4096,
			GIDPrefix: 0xfe80000000000000, RateGbps: 56}}
	f.mu.Lock()
	f.devs[name] = d
	f.mu.Unlock()
	return d
}

func (f *fakeFabric) open(p rdma.Port) (rdmaDevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.devs[p.Device]
	if d == nil {
		return nil, fmt.Errorf("no device %s", p.Device)
	}
	return d, nil
}

type fakeDev struct {
	f        *fakeFabric
	live     map[any]string // the MRs, CQs, SRQs and QPs made and not yet destroyed
	gone     bool
	name     string
	attr     rdma.PortAttr
	events   chan rdma.Event
	closed   chan struct{}
	once     sync.Once
	failNext rdma.WCStatus // the next send of a QP of this device completes with it (0: none)
}

func (d *fakeDev) Attr() (rdma.PortAttr, error) {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	return d.attr, nil
}

// made notes an object made from the device; destroyed, that it is gone, which is a
// violation if something made from it is still alive (users: the QPs of a CQ or SRQ, and for
// the device everything).
func (d *fakeDev) made(o any, what string) {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	if d.gone {
		d.f.violate("%s made on a closed device", what)
	}
	d.live[o] = what
}

func (d *fakeDev) destroyed(o any, users func(*fakeQP) bool) {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	what, ok := d.live[o]
	if !ok {
		d.f.violate("an object destroyed twice, or not made")
		return
	}
	if d.gone {
		d.f.violate("%s destroyed after its device was closed", what)
	}
	for _, q := range d.f.qps {
		if users != nil && users(q) {
			d.f.violate("%s destroyed while QP %d uses it", what, q.num)
		}
	}
	delete(d.live, o)
}

func (d *fakeDev) Register(mem []byte) (rdmaMR, error) {
	m := &fakeMR{dev: d, mem: mem, base: uintptr(unsafe.Pointer(unsafe.SliceData(mem)))}
	d.made(m, "a memory region")
	return m, nil
}

func (d *fakeDev) NewCQ(int) (rdmaCQ, error) {
	c := &fakeCQ{dev: d, notify: make(chan struct{}, 1), closed: make(chan struct{})}
	d.made(c, "a CQ")
	return c, nil
}

func (d *fakeDev) NewSRQ(int) (rdmaSRQ, error) {
	s := &fakeSRQ{f: d.f, dev: d}
	d.made(s, "an SRQ")
	return s, nil
}

func (d *fakeDev) NewQP(cq rdmaCQ, srq rdmaSRQ, _ int) (rdmaQP, error) {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	q := &fakeQP{dev: d, num: d.f.next, cq: cq.(*fakeCQ), srq: srq.(*fakeSRQ)}
	d.f.next++
	d.f.qps[q.num] = q
	d.live[q] = "a QP"
	return q, nil
}

func (d *fakeDev) NextEvent() (rdma.Event, error) {
	select {
	case ev := <-d.events:
		return ev, nil
	case <-d.closed:
		return rdma.Event{}, rdma.ErrClosed
	}
}

func (d *fakeDev) Close() error {
	d.once.Do(func() {
		d.f.mu.Lock()
		for _, what := range d.live {
			d.f.violate("the device closed while %s made from it is alive", what)
		}
		d.gone = true
		d.f.mu.Unlock()
		close(d.closed)
	})
	return nil
}

// breakQPs makes the device's QPs that exist now unreachable: their sends are not
// acknowledged, so they complete with "retry counter exceeded", as when the peer's HCA or
// the path is gone. QPs made later work.
func (d *fakeDev) breakQPs() {
	d.f.mu.Lock()
	defer d.f.mu.Unlock()
	for _, q := range d.f.qps {
		if q.dev == d {
			q.broken = true
		}
	}
}

func (d *fakeDev) failNextSend(st rdma.WCStatus) {
	d.f.mu.Lock()
	d.failNext = st
	d.f.mu.Unlock()
}

type fakeMR struct {
	dev  *fakeDev
	mem  []byte
	base uintptr
}

func (m *fakeMR) Close() error {
	m.dev.destroyed(m, func(*fakeQP) bool { return false })
	return nil
}

func (m *fakeMR) bytes(addr uintptr, n int) []byte {
	off := int(addr - m.base)
	return m.mem[off : off+n]
}

type fakeCQ struct {
	dev    *fakeDev
	mu     sync.Mutex
	wcs    []rdma.WC
	armed  bool
	notify chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (c *fakeCQ) push(wc rdma.WC) {
	c.mu.Lock()
	c.wcs = append(c.wcs, wc)
	if c.armed {
		c.armed = false
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
	c.mu.Unlock()
}

func (c *fakeCQ) Poll(out []rdma.WC) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := copy(out, c.wcs)
	c.wcs = c.wcs[n:]
	return n, nil
}

func (c *fakeCQ) Arm() error {
	c.mu.Lock()
	c.armed = true
	c.mu.Unlock()
	return nil
}

func (c *fakeCQ) Wait() error {
	select {
	case <-c.notify:
		return nil
	case <-c.closed:
		return rdma.ErrClosed
	}
}

func (c *fakeCQ) Close() error {
	c.once.Do(func() {
		c.dev.destroyed(c, func(q *fakeQP) bool { return q.cq == c })
		close(c.closed)
	})
	return nil
}

// fakeSRQ holds posted receives, and the sends that wait for one.
type fakeSRQ struct {
	f       *fakeFabric
	dev     *fakeDev
	gone    bool
	mr      *fakeMR
	posted  []rdma.RecvWR
	waiting []fakeSend
}

type fakeSend struct {
	from *fakeQP
	to   *fakeQP
	wr   rdma.SendWR
	data []byte
}

func (s *fakeSRQ) Post(mr rdmaMR, wrs []rdma.RecvWR) (int, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.gone {
		s.f.violate("receives posted to a destroyed SRQ")
		return 0, errors.New("fake: SRQ destroyed")
	}
	s.mr = mr.(*fakeMR)
	s.posted = append(s.posted, wrs...)
	for len(s.waiting) > 0 && len(s.posted) > 0 {
		w := s.waiting[0]
		s.waiting = s.waiting[1:]
		s.f.deliver(w)
	}
	return len(wrs), nil
}

func (s *fakeSRQ) Close() error {
	s.dev.destroyed(s, func(q *fakeQP) bool { return q.srq == s })
	s.f.mu.Lock()
	s.gone = true
	s.f.mu.Unlock()
	return nil
}

// postedCount is how many receives the SRQ holds.
func (s *fakeSRQ) postedCount() int {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return len(s.posted)
}

type fakeQP struct {
	dev    *fakeDev
	num    uint32
	cq     *fakeCQ
	srq    *fakeSRQ
	peer   *fakeQP
	rts    bool
	err    bool
	broken bool
}

func (q *fakeQP) Num() uint32 { return q.num }

func (q *fakeQP) Connect(a rdma.Address, _ uint32, _ int) error {
	q.dev.f.mu.Lock()
	defer q.dev.f.mu.Unlock()
	p := q.dev.f.qps[a.QPN]
	if p == nil || p.dev.attr.LID != a.LID {
		return errors.New("fake: no such QP")
	}
	q.peer, q.rts = p, true
	return nil
}

func (q *fakeQP) Post(mr rdmaMR, wrs []rdma.SendWR) (int, error) {
	f := q.dev.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.qps[q.num]; !ok {
		f.violate("sends posted to destroyed QP %d", q.num)
		return 0, errors.New("fake: QP destroyed")
	}
	for _, wr := range wrs {
		switch {
		case !q.rts || q.err || q.peer == nil:
			q.cq.push(rdma.WC{ID: wr.ID, Op: rdma.OpSend, Status: rdma.WCWRFlushErr, QPN: q.num})
		case q.broken || q.peer.err:
			q.err = true
			q.cq.push(rdma.WC{ID: wr.ID, Op: rdma.OpSend, Status: rdma.WCRetryExcErr, QPN: q.num})
		case q.dev.failNext != 0:
			q.err = true
			q.cq.push(rdma.WC{ID: wr.ID, Op: rdma.OpSend, Status: q.dev.failNext, QPN: q.num})
			q.dev.failNext = 0
		default:
			data := append([]byte(nil), mr.(*fakeMR).bytes(wr.Addr, int(wr.Len))...)
			s := fakeSend{from: q, to: q.peer, wr: wr, data: data}
			if len(q.peer.srq.posted) == 0 {
				q.peer.srq.waiting = append(q.peer.srq.waiting, s) // receiver not ready: retried until it is
				continue
			}
			f.deliver(s)
		}
	}
	return len(wrs), nil
}

// deliver puts a send into the next receive of its peer's SRQ. The caller holds f.mu.
func (f *fakeFabric) deliver(s fakeSend) {
	srq := s.to.srq
	r := srq.posted[0]
	srq.posted = srq.posted[1:]
	if len(s.data) > int(r.Len) {
		s.to.err, s.from.err = true, true
		s.to.cq.push(rdma.WC{ID: r.ID, Op: rdma.OpRecv, Status: rdma.WCLocLenErr, QPN: s.to.num})
		s.from.cq.push(rdma.WC{ID: s.wr.ID, Op: rdma.OpSend, Status: rdma.WCRemInvReqErr, QPN: s.from.num})
		return
	}
	copy(srq.mr.bytes(r.Addr, len(s.data)), s.data)
	s.to.cq.push(rdma.WC{ID: r.ID, Op: rdma.OpRecv, Len: uint32(len(s.data)), Imm: s.wr.Imm, HasImm: true, QPN: s.to.num})
	if s.wr.Signaled {
		s.from.cq.push(rdma.WC{ID: s.wr.ID, Op: rdma.OpSend, QPN: s.from.num})
	}
}

func (q *fakeQP) SetError() error {
	f := q.dev.f
	f.mu.Lock()
	defer f.mu.Unlock()
	q.err = true
	// The sends of this QP that wait for a receive are flushed.
	for _, p := range f.qps {
		keep := p.srq.waiting[:0]
		for _, w := range p.srq.waiting {
			if w.from == q {
				q.cq.push(rdma.WC{ID: w.wr.ID, Op: rdma.OpSend, Status: rdma.WCWRFlushErr, QPN: q.num})
				continue
			}
			keep = append(keep, w)
		}
		p.srq.waiting = keep
	}
	return nil
}

func (q *fakeQP) Close() error {
	f := q.dev.f
	f.mu.Lock()
	if _, ok := f.qps[q.num]; !ok {
		f.violate("QP %d destroyed twice", q.num)
	}
	delete(f.qps, q.num)
	f.mu.Unlock()
	q.dev.destroyed(q, nil)
	return nil
}
