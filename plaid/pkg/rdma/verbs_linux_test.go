//go:build linux && cgo

package rdma

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"
	"unsafe"
)

// These tests need an active InfiniBand port (the tie nodes have one); without one they
// are skipped, saying so. Two QPs on the one port are connected to each other: the HCA
// carries their traffic as it does between nodes.

func openPort(t *testing.T) *Context {
	t.Helper()
	p, why, err := Choose(SysRoot, "")
	if errors.Is(err, ErrNoPort) {
		t.Skip("no active InfiniBand port on this host")
	} else if err != nil {
		t.Fatalf("choose a port: %v", err)
	}
	c, err := Open(p)
	if err != nil {
		t.Fatalf("open %v (%s): %v", p, why, err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return c
}

// pair is two connected QPs on one port, a's sends received by b, with their queues and
// one registered region: slots of slot bytes, the first half for sending, the second for
// receiving.
type pair struct {
	c               *Context
	mem             []byte
	mr              *MR
	scq, rcq        *CQ
	srq             *SRQ
	a, b            *QP
	slot, sendSlots int
	recvSlots       int
}

func newPair(t *testing.T, c *Context, slot, recvLen, slots int) *pair {
	t.Helper()
	p := &pair{c: c, slot: slot, sendSlots: slots, recvSlots: slots}
	var err error
	if p.mem, err = Map(2 * slots * slot); err != nil {
		t.Fatal(err)
	}
	if p.mr, err = c.Register(p.mem); err != nil {
		t.Fatal(err)
	}
	if p.scq, err = c.NewCQ(4 * slots); err != nil {
		t.Fatal(err)
	}
	if p.rcq, err = c.NewCQ(4 * slots); err != nil {
		t.Fatal(err)
	}
	if p.srq, err = c.NewSRQ(slots); err != nil {
		t.Fatal(err)
	}
	if p.a, err = c.NewQP(p.scq, p.rcq, p.srq, slots); err != nil {
		t.Fatal(err)
	}
	if p.b, err = c.NewQP(p.scq, p.rcq, p.srq, slots); err != nil {
		t.Fatal(err)
	}
	attr, err := c.Attr()
	if err != nil {
		t.Fatal(err)
	}
	pa, pb := rand.Uint32()&0xffffff, rand.Uint32()&0xffffff
	if err := p.a.Connect(Address{LID: attr.LID, QPN: p.b.Num(), PSN: pb}, pa, attr.MTU); err != nil {
		t.Fatal(err)
	}
	if err := p.b.Connect(Address{LID: attr.LID, QPN: p.a.Num(), PSN: pa}, pb, attr.MTU); err != nil {
		t.Fatal(err)
	}
	recvs := make([]RecvWR, slots)
	for i := range recvs {
		recvs[i] = RecvWR{ID: uint64(i), Addr: p.addr(slots + i), Len: uint32(recvLen)}
	}
	if n, err := p.srq.Post(p.mr, recvs); err != nil || n != slots {
		t.Fatalf("post %d receives: %d, %v", slots, n, err)
	}
	t.Cleanup(func() {
		for _, err := range []error{p.a.Close(), p.b.Close(), p.srq.Close(), p.scq.Close(), p.rcq.Close(), p.mr.Close(), Unmap(p.mem)} {
			if err != nil {
				t.Errorf("clean up: %v", err)
			}
		}
	})
	return p
}

func (p *pair) addr(slot int) uintptr {
	return uintptr(unsafe.Pointer(unsafe.SliceData(p.mem))) + uintptr(slot*p.slot)
}

func (p *pair) bytes(slot, n int) []byte { return p.mem[slot*p.slot : slot*p.slot+n] }

// collect takes n completions of cq, waiting on its channel (through the netpoller)
// whenever it is empty, as the overlay's lanes will.
func collect(t *testing.T, cq *CQ, n int) []WC {
	t.Helper()
	var out []WC
	wcs := make([]WC, MaxBatch)
	deadline := time.Now().Add(10 * time.Second)
	for len(out) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d completions after 10 s", len(out), n)
		}
		k, err := cq.Poll(wcs)
		if err != nil {
			t.Fatal(err)
		}
		if k > 0 {
			out = append(out, wcs[:k]...)
			continue
		}
		if err := cq.Arm(); err != nil {
			t.Fatal(err)
		}
		if k, _ = cq.Poll(wcs); k > 0 {
			out = append(out, wcs[:k]...)
			continue
		}
		if err := cq.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// next waits for the next completion of cq, without the testing.T, for other goroutines.
func next(cq *CQ) (WC, error) {
	wcs := make([]WC, 1)
	for {
		if k, err := cq.Poll(wcs); err != nil || k > 0 {
			return wcs[0], err
		}
		if err := cq.Arm(); err != nil {
			return WC{}, err
		}
		if k, err := cq.Poll(wcs); err != nil || k > 0 {
			return wcs[0], err
		}
		if err := cq.Wait(); err != nil {
			return WC{}, err
		}
	}
}

func TestPortAttr(t *testing.T) {
	c := openPort(t)
	a, err := c.Attr()
	if err != nil {
		t.Fatal(err)
	}
	if a.State != PortActive || a.LinkLayer != LinkInfiniBand || a.LID == 0 || a.MTU < 256 || a.GIDPrefix == 0 {
		t.Errorf("attributes of %v: %+v", c.Port(), a)
	}
	t.Logf("%v: LID %d, SM LID %d, MTU %d, %d Gb/s, GID prefix %#x, GUID %#x", c.Port(), a.LID, a.SMLID, a.MTU, a.RateGbps, a.GIDPrefix, a.GUID)
}

// Sends of every size the overlay makes, up to a whole super-frame of 64 KiB and more,
// arrive whole, in order, with their immediate data; a signaled send completes the
// unsignaled ones before it.
func TestSendsArriveWholeInOrder(t *testing.T) {
	c := openPort(t)
	const slot = 66 << 10
	p := newPair(t, c, slot, slot, 32)
	sizes := []int{1, 60, 1500, 4096, 4097, 9000, 65507, 65536 + 64, slot}
	var wrs []SendWR
	for i, n := range sizes {
		b := p.bytes(i, n)
		for j := range b {
			b[j] = byte(i*31 + j)
		}
		wrs = append(wrs, SendWR{ID: uint64(i), Addr: p.addr(i), Len: uint32(n), Imm: uint32(1000 + i), Signaled: i == len(sizes)-1})
	}
	if n, err := p.a.Post(p.mr, wrs); err != nil || n != len(wrs) {
		t.Fatalf("post: %d, %v", n, err)
	}
	sent := collect(t, p.scq, 1)
	if sent[0].Status != WCSuccess || sent[0].Op != OpSend || sent[0].ID != uint64(len(sizes)-1) || sent[0].QPN != p.a.Num() {
		t.Errorf("send completion %+v", sent[0])
	}
	got := collect(t, p.rcq, len(sizes))
	for i, wc := range got {
		if wc.Status != WCSuccess || wc.Op != OpRecv || !wc.HasImm || wc.Imm != uint32(1000+i) || int(wc.Len) != sizes[i] || wc.QPN != p.b.Num() {
			t.Errorf("receive %d: %+v", i, wc)
			continue
		}
		if !bytes.Equal(p.bytes(p.recvSlots+int(wc.ID), sizes[i]), p.bytes(i, sizes[i])) {
			t.Errorf("receive %d (%d bytes): the bytes differ", i, sizes[i])
		}
	}
}

// Many more sends than receives posted: the receiver re-posts each buffer as it is done
// with it, and the sender's HCA retries "receiver not ready" until there is one: nothing is
// lost (RNR retry 7), the bytes are the bytes.
func TestReceiverNotReadyIsWaitedFor(t *testing.T) {
	c := openPort(t)
	const slot, slots, total = 4096, 8, 2000
	p := newPair(t, c, slot, slot, slots)
	done := make(chan error, 1)
	go func() {
		var sent uint64
		for sent < total {
			var wrs []SendWR
			for k := 0; k < slots && sent < total; k++ {
				s := int(sent % slots)
				b := p.bytes(s, 8)
				for j := range b {
					b[j] = byte(sent >> (8 * j))
				}
				wrs = append(wrs, SendWR{ID: sent, Addr: p.addr(s), Len: 8, Imm: uint32(sent), Signaled: k == slots-1 || sent == total-1})
				sent++
			}
			if _, err := p.a.Post(p.mr, wrs); err != nil {
				done <- err
				return
			}
			if wc, err := next(p.scq); err != nil || wc.Status != WCSuccess { // the batch's slots are free again
				done <- fmt.Errorf("send completion %+v: %v", wc, err)
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < total; {
		for _, wc := range collect(t, p.rcq, 1) {
			v := uint64(0)
			b := p.bytes(p.recvSlots+int(wc.ID), 8)
			for j := range b {
				v |= uint64(b[j]) << (8 * j)
			}
			if wc.Status != WCSuccess || wc.Imm != uint32(i) || v != uint64(i) {
				t.Fatalf("receive %d: %+v, value %d", i, wc, v)
			}
			if _, err := p.srq.Post(p.mr, []RecvWR{{ID: wc.ID, Addr: p.addr(p.recvSlots + int(wc.ID)), Len: slot}}); err != nil {
				t.Fatal(err)
			}
			i++
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A message larger than the receive buffer fails both ends, as a bug of ours would: the
// receiver's completion says local length error, the sender's remote invalid request,
// and the QP that sent is in error afterwards (what is posted then is flushed).
func TestTooLargeAMessageFailsTheLink(t *testing.T) {
	c := openPort(t)
	p := newPair(t, c, 8192, 1024, 4)
	if _, err := p.a.Post(p.mr, []SendWR{{ID: 1, Addr: p.addr(0), Len: 2048, Signaled: true}}); err != nil {
		t.Fatal(err)
	}
	if wc := collect(t, p.rcq, 1)[0]; wc.Status != WCLocLenErr {
		t.Errorf("receiver: %v, want %v", wc.Status, WCLocLenErr)
	}
	if wc := collect(t, p.scq, 1)[0]; wc.Status != WCRemInvReqErr {
		t.Errorf("sender: %v, want %v", wc.Status, WCRemInvReqErr)
	}
	if _, err := p.a.Post(p.mr, []SendWR{{ID: 2, Addr: p.addr(1), Len: 16, Signaled: true}}); err == nil {
		if wc := collect(t, p.scq, 1)[0]; wc.Status != WCWRFlushErr {
			t.Errorf("after the error: %v, want %v", wc.Status, WCWRFlushErr)
		}
	}
}

// SetError flushes what is posted: the overlay's tear-down of a link.
func TestSetErrorFlushes(t *testing.T) {
	c := openPort(t)
	p := newPair(t, c, 4096, 4096, 4)
	if err := p.a.SetError(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.a.Post(p.mr, []SendWR{{ID: 7, Addr: p.addr(0), Len: 16, Signaled: true}}); err == nil {
		if wc := collect(t, p.scq, 1)[0]; wc.Status != WCWRFlushErr || wc.ID != 7 {
			t.Errorf("a send after SetError: %+v", wc)
		}
	}
}

// Close ends a Wait that has nothing to wait for, so that a lane's goroutine can stop.
func TestCloseEndsAWait(t *testing.T) {
	c := openPort(t)
	cq, err := c.NewCQ(16)
	if err != nil {
		t.Fatal(err)
	}
	if err := cq.Arm(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- cq.Wait() }()
	time.Sleep(100 * time.Millisecond)
	if err := cq.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("Wait after Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the Wait")
	}
	ev := make(chan error, 1)
	go func() { _, err := c.NextEvent(); ev <- err }()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-ev:
		t.Fatalf("NextEvent returned without an event: %v", err)
	default:
	}
}

// Registering memory pins it; a gigabyte is what the overlay will never need, but must work
// where the locked-memory limit allows it.
func TestRegisterLargeRegion(t *testing.T) {
	c := openPort(t)
	mem, err := Map(256 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer Unmap(mem)
	start := time.Now()
	mr, err := c.Register(mem)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("registered 256 MiB in %v", time.Since(start).Round(time.Millisecond))
	base := uintptr(unsafe.Pointer(unsafe.SliceData(mem)))
	if !mr.Contains(base, len(mem)) || mr.Contains(base, len(mem)+1) || mr.Contains(base-1, 1) {
		t.Error("Contains is wrong at the region's ends")
	}
	if err := mr.Close(); err != nil {
		t.Fatal(err)
	}
}

// An armed SRQ says once, by an asynchronous event with its ID, that fewer receives than its
// limit are posted.
func TestSRQLimitEvent(t *testing.T) {
	c := openPort(t)
	p := newPair(t, c, 4096, 4096, 8)
	if err := p.srq.Arm(4); err != nil {
		t.Fatal(err)
	}
	var wrs []SendWR
	for i := 0; i < 6; i++ {
		wrs = append(wrs, SendWR{ID: uint64(i), Addr: p.addr(i), Len: 64, Signaled: i == 5})
	}
	if _, err := p.a.Post(p.mr, wrs); err != nil {
		t.Fatal(err)
	}
	collect(t, p.rcq, 6) // 2 receives left posted, below the limit of 4
	got := make(chan Event, 1)
	go func() {
		if ev, err := c.NextEvent(); err == nil {
			got <- ev
		}
	}()
	select {
	case ev := <-got:
		if ev.Type != EventSRQLimitReached || ev.SRQ != p.srq.ID() {
			t.Errorf("event %+v (%s), want the SRQ limit of SRQ %#x", ev, ev.Type, p.srq.ID())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no SRQ limit event")
	}
}
