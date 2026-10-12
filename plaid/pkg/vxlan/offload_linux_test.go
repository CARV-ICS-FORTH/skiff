//go:build linux

package vxlan

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"plaid/pkg/packet"
)

// frameOf is a frame of size bytes in all on the wire (the VXLAN header included),
// distinguishable by tag.
func frameOf(size int, tag byte) *packet.EthernetFrame {
	f := testFrame(size - packet.VXLANHeaderLen - packet.EthernetHeaderLen)
	f.Payload[0] = tag
	return f
}

func skipWithoutSegmentation(t *testing.T, oe *OverlayEngine) {
	t.Helper()
	if !oe.Stats().SegmentOffload {
		t.Skip("this kernel has no UDP_GRO, which UDP_SEGMENT goes with")
	}
}

// The datagrams of a batch are on the wire as they are when each is sent alone, and a
// run of equal sizes, a destination change, a shorter or longer datagram, the count and
// the size limits each end a send.
func TestTxBatchRuns(t *testing.T) {
	type send struct {
		size int
		to   int
	}
	rep := func(n, size, to int) []send {
		out := make([]send, n)
		for i := range out {
			out[i] = send{size, to}
		}
		return out
	}
	cat := func(parts ...[]send) (out []send) {
		for _, p := range parts {
			out = append(out, p...)
		}
		return
	}
	for _, tc := range []struct {
		name  string
		sends []send
		calls uint64 // system calls with UDP_SEGMENT: runs, a datagram alone being a plain send
	}{
		{"one datagram", rep(1, 1500, 0), 1},
		{"equal sizes", rep(10, 1500, 0), 1},
		{"jumbo", rep(7, 9000, 0), 1},
		{"a shorter one ends the run", cat(rep(3, 1500, 0), rep(1, 700, 0), rep(1, 1500, 0)), 2},
		{"a shorter one is joined only as the last", cat(rep(3, 1500, 0), rep(2, 700, 0)), 2},
		{"a longer one starts a run", cat(rep(2, 1000, 0), rep(2, 1500, 0)), 2},
		{"destinations alternate", cat(rep(1, 1500, 0), rep(1, 1500, 1), rep(1, 1500, 0), rep(1, 1500, 1)), 4},
		{"destinations in turn", cat(rep(4, 1500, 0), rep(4, 1500, 1)), 2},
		{"64 datagrams", rep(64, 200, 0), 1},
		{"65 datagrams", rep(65, 200, 0), 2},
		{"the byte limit", rep(8, 9000, 0), 2}, // 7 x 9000 = 63000; 8 would be 72000 > 65507
		{"just under the byte limit", rep(32, 2000, 0), 1},
		{"just over the byte limit", rep(33, 2000, 0), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, OverlayConfig{Offload: true}, 2)
			skipWithoutSegmentation(t, f.oe)
			ref := newFixture(t, OverlayConfig{}, 2)

			tb, rb := f.oe.NewBatch(), ref.oe.NewBatch()
			defer tb.Release()
			defer rb.Release()
			want := [2][][]byte{}
			for i, s := range tc.sends {
				fr := frameOf(s.size, byte(i))
				vtep, _ := net.ParseMAC("02:00:00:00:00:02")
				want[s.to] = append(want[s.to], referenceDatagram(7, vtep, fr.SrcMAC, fr.Payload))
				if err := tb.Send(fr, f.dst(s.to)); err != nil {
					t.Fatal(err)
				}
				if err := rb.Send(frameOf(s.size, byte(i)), ref.dst(s.to)); err != nil {
					t.Fatal(err)
				}
			}
			if err := tb.Flush(); err != nil {
				t.Fatal(err)
			}
			if err := rb.Flush(); err != nil {
				t.Fatal(err)
			}

			for to := 0; to < 2; to++ {
				got, plain := f.receive(t, to, len(want[to])), ref.receive(t, to, len(want[to]))
				for i := range want[to] {
					if !bytes.Equal(got[i], want[to][i]) {
						t.Errorf("sink %d datagram %d differs from the reference encoding", to, i)
					}
					if !bytes.Equal(got[i], plain[i]) {
						t.Errorf("sink %d datagram %d differs from the datagram of an engine without the offload", to, i)
					}
				}
			}
			st, rst := f.oe.Stats(), ref.oe.Stats()
			if st.TxPackets != uint64(len(tc.sends)) || st.TxErrors != 0 {
				t.Errorf("TxPackets %d, TxErrors %d for %d datagrams", st.TxPackets, st.TxErrors, len(tc.sends))
			}
			if st.TxSends != tc.calls {
				t.Errorf("%d system calls, want %d", st.TxSends, tc.calls)
			}
			if rst.TxSends != uint64(len(tc.sends)) {
				t.Errorf("without the offload: %d system calls for %d datagrams", rst.TxSends, len(tc.sends))
			}
		})
	}
}

// The overlay's socket listens on the wildcard address in a deployment, which Go opens as a
// dual-stack IPv6 socket: UDP_SEGMENT works on it for IPv4 destinations too.
func TestSegmentationOnTheWildcardSocket(t *testing.T) {
	f := newFixture(t, OverlayConfig{BindAddress: "0.0.0.0", Offload: true}, 1)
	skipWithoutSegmentation(t, f.oe)
	tb := f.oe.NewBatch()
	defer tb.Release()
	for i := 0; i < 5; i++ {
		if err := tb.Send(frameOf(1500, byte(i)), f.dst(0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tb.Flush(); err != nil {
		t.Fatal(err)
	}
	for i, d := range f.receive(t, 0, 5) {
		if d[packet.VXLANHeaderLen+packet.EthernetHeaderLen] != byte(i) {
			t.Errorf("datagram %d is out of order", i)
		}
	}
	if st := f.oe.Stats(); st.TxSends != 1 || st.TxPackets != 5 {
		t.Errorf("TxSends %d, TxPackets %d, want one send of 5", st.TxSends, st.TxPackets)
	}
}

func TestSendIsABatchOfOne(t *testing.T) {
	f := newFixture(t, OverlayConfig{Offload: true}, 1)
	for i := 0; i < 3; i++ {
		if err := f.oe.Send(frameOf(1500, byte(i)), f.dst(0)); err != nil {
			t.Fatal(err)
		}
	}
	f.receive(t, 0, 3)
	if st := f.oe.Stats(); st.TxPackets != 3 || st.TxSends != 3 {
		t.Errorf("TxPackets %d, TxSends %d", st.TxPackets, st.TxSends)
	}
}

func TestBatchErrorsAreCountedAtSendAndFlush(t *testing.T) {
	f := newFixture(t, OverlayConfig{Offload: true}, 1)
	tb := f.oe.NewBatch()
	defer tb.Release()
	if err := tb.Send(frameOf(500, 1), net.IPv4(10, 9, 9, 9)); err == nil {
		t.Error("a frame without a route was accepted")
	}
	if err := tb.Send(frameOf(500, 2), f.dst(0)); err != nil {
		t.Fatal(err)
	}
	if err := tb.Flush(); err != nil {
		t.Fatal(err)
	}
	if st := f.oe.Stats(); st.TxErrors != 1 || st.TxPackets != 1 {
		t.Errorf("TxErrors %d, TxPackets %d", st.TxErrors, st.TxPackets)
	}
	// Nothing held is sent twice.
	if err := tb.Flush(); err != nil {
		t.Fatal(err)
	}
	if st := f.oe.Stats(); st.TxPackets != 1 {
		t.Errorf("TxPackets %d after a second Flush", st.TxPackets)
	}
}

// If the first send with UDP_SEGMENT fails because the kernel or the device cannot do it,
// the datagrams go out one by one and so do all after, with a message.
func TestSegmentationUnavailableFallsBack(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EINVAL, syscall.ENOPROTOOPT, syscall.EIO} {
		t.Run(errno.Error(), func(t *testing.T) {
			f := newFixture(t, OverlayConfig{Offload: true}, 1)
			skipWithoutSegmentation(t, f.oe)
			calls := 0
			old := sendRunSegmented
			sendRunSegmented = func(*net.UDPConn, []byte, []byte, netip.AddrPort) error { calls++; return errno }
			defer func() { sendRunSegmented = old }()

			for round := 0; round < 2; round++ {
				tb := f.oe.NewBatch()
				for i := 0; i < 4; i++ {
					if err := tb.Send(frameOf(1500, byte(i)), f.dst(0)); err != nil {
						t.Fatal(err)
					}
				}
				if err := tb.Flush(); err != nil {
					t.Fatal(err)
				}
				tb.Release()
				got := f.receive(t, 0, 4)
				for i, d := range got {
					if d[packet.VXLANHeaderLen+packet.EthernetHeaderLen] != byte(i) {
						t.Errorf("round %d: datagram %d out of order", round, i)
					}
				}
			}
			st := f.oe.Stats()
			if calls != 1 || st.SegmentOffload || st.TxSends != 1+4+4 || st.TxPackets != 8 || st.TxErrors != 0 {
				t.Errorf("segmented sends tried %d times; stats %+v", calls, st)
			}
		})
	}
}

// A path that cannot carry the datagram size fails the send with EMSGSIZE; that is not the
// kernel lacking the feature: the datagrams go out one by one, and the next batch
// tries UDP_SEGMENT again.
func TestSegmentedSendTooBigFallsBackWithoutGivingUp(t *testing.T) {
	f := newFixture(t, OverlayConfig{Offload: true}, 1)
	skipWithoutSegmentation(t, f.oe)
	old := sendRunSegmented
	sendRunSegmented = func(*net.UDPConn, []byte, []byte, netip.AddrPort) error { return syscall.EMSGSIZE }
	defer func() { sendRunSegmented = old }()

	tb := f.oe.NewBatch()
	defer tb.Release()
	for i := 0; i < 3; i++ {
		_ = tb.Send(frameOf(1500, byte(i)), f.dst(0))
	}
	if err := tb.Flush(); err != nil {
		t.Fatal(err)
	}
	f.receive(t, 0, 3)
	if st := f.oe.Stats(); !st.SegmentOffload || st.TxPackets != 3 {
		t.Errorf("stats %+v", st)
	}
}

// -- receiving

// cmsg encodes one control message.
func cmsg(level, typ int, data []byte) []byte {
	b := make([]byte, syscall.CmsgSpace(len(data)))
	h := (*syscall.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level, h.Type = int32(level), int32(typ)
	h.SetLen(syscall.CmsgLen(len(data)))
	copy(b[syscall.CmsgLen(0):], data)
	return b
}

func ne32(v uint32) []byte { return binary.NativeEndian.AppendUint32(nil, v) }

func TestParseControlMessages(t *testing.T) {
	gro := cmsg(unix.SOL_UDP, unix.UDP_GRO, ne32(1450))
	ovfl := cmsg(syscall.SOL_SOCKET, syscall.SO_RXQ_OVFL, ne32(77))
	other := cmsg(syscall.SOL_SOCKET, syscall.SCM_TIMESTAMP, make([]byte, 16))
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	for _, tc := range []struct {
		name  string
		oob   []byte
		seg   int
		drops uint32
		hasD  bool
	}{
		{"nothing", nil, 0, 0, false},
		{"GRO alone", gro, 1450, 0, false},
		{"drops alone", ovfl, 0, 77, true},
		{"drops, then GRO", cat(ovfl, gro), 1450, 77, true},
		{"GRO, then drops", cat(gro, ovfl), 1450, 77, true},
		{"others around", cat(other, gro, other, ovfl), 1450, 77, true},
		{"a 16-bit size", cmsg(unix.SOL_UDP, unix.UDP_GRO, []byte{0xaa, 0x05}), int(binary.NativeEndian.Uint16([]byte{0xaa, 0x05})), 0, false},
		{"truncated", gro[:len(gro)-9], 0, 0, false},
		{"a length of zero", make([]byte, 32), 0, 0, false},
		{"the wrong level", cmsg(syscall.SOL_SOCKET, unix.UDP_GRO, ne32(9)), 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseGROSize(tc.oob); got != tc.seg {
				t.Errorf("segment size %d, want %d", got, tc.seg)
			}
			drops, ok := parseDropCount(tc.oob)
			if drops != tc.drops || ok != tc.hasD {
				t.Errorf("drop count %d, %v; want %d, %v", drops, ok, tc.drops, tc.hasD)
			}
		})
	}
	if rxOOBLen < len(cat(ovfl, gro)) {
		t.Errorf("the control buffer of %d bytes cannot hold both messages (%d)", rxOOBLen, len(cat(ovfl, gro)))
	}
}

// A read that holds several datagrams back to back is processed as the datagrams it holds.
func TestProcessReadSplitsAtTheSegmentSize(t *testing.T) {
	l := &rxLane{oe: NewOverlayEngine(OverlayConfig{}, NewRouteTable(), nil)}
	for _, tc := range []struct {
		name  string
		sizes []int
		seg   int
		want  string
	}{
		{"one datagram, no size", []int{300}, 0, "[0]"},
		{"one datagram, size larger", []int{300}, 1500, "[0]"},
		{"equal", []int{1500, 1500, 1500}, 1500, "[0 1 2]"},
		{"a short last one", []int{1500, 1500, 700}, 1500, "[0 1 2]"},
		{"sizes without a size are one datagram", []int{1500, 1500}, 0, "[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := packet.GetBuffer()
			defer buf.Release()
			for i, size := range tc.sizes {
				buf.B = append(buf.B, datagram(uint32(i), size)...)
			}
			// datagram() puts the sequence number in the first four payload bytes.
			inj := &seqInjector{}
			l.processRead(context.Background(), inj, rxRead{buf: buf, seg: tc.seg})
			if got := fmt.Sprint(inj.delivered()); got != tc.want {
				t.Errorf("frames %s, want %s", got, tc.want)
			}
		})
	}
}

// UDP_SEGMENT from a bare socket to an engine with UDP_GRO: the datagrams arrive whole, in
// order, whether the kernel hands them over together or not, and with fewer reads than
// datagrams when it does.
func TestReceiveSegmentedDatagrams(t *testing.T) {
	for _, offload := range []bool{true, false} {
		t.Run(fmt.Sprintf("receiver offload=%v", offload), func(t *testing.T) {
			inj := &seqInjector{}
			oe := startEngine(t, OverlayConfig{Offload: offload, RcvBuf: 4 << 20}, inj)
			if offload && !oe.Stats().ReceiveOffload {
				t.Skip("this kernel has no UDP_GRO")
			}
			conn := dialEngine(t, oe)
			rc, _ := conn.SyscallConn()
			var cerr error
			if err := rc.Control(func(fd uintptr) { cerr = unix.SetsockoptInt(int(fd), unix.SOL_UDP, unix.UDP_SEGMENT, 1450) }); err != nil || cerr != nil {
				t.Skipf("no UDP_SEGMENT here: %v %v", err, cerr)
			}
			const bursts, per = 20, 10
			seq := uint32(0)
			for b := 0; b < bursts; b++ {
				var burst []byte
				for i := 0; i < per; i++ {
					size := 1450
					if i == per-1 {
						size = 900 // the last may be shorter
					}
					burst = append(burst, datagram(seq, size)...)
					seq++
				}
				if _, err := conn.Write(burst); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Millisecond)
			}
			waitFor(t, "all datagrams", func() bool { return inj.n.Load() == bursts*per })
			for i, s := range inj.delivered() {
				if s != uint32(i) {
					t.Fatalf("datagram %d carries sequence %d: order or content lost", i, s)
				}
			}
			st := oe.Stats()
			t.Logf("offload=%v: %d datagrams in %d reads", offload, st.RxPackets, st.RxReads)
			if st.RxPackets != bursts*per || st.RxInvalid != 0 {
				t.Errorf("stats %+v", st)
			}
			if !offload && st.RxReads != bursts*per {
				t.Errorf("without UDP_GRO the %d datagrams took %d reads", bursts*per, st.RxReads)
			}
		})
	}
}

// Two engines, with the offload on and off in each combination, carry frames both ways.
func TestMixedOffloadPeers(t *testing.T) {
	for _, tc := range []struct{ sender, receiver bool }{{true, true}, {true, false}, {false, true}, {false, false}} {
		t.Run(fmt.Sprintf("sender=%v receiver=%v", tc.sender, tc.receiver), func(t *testing.T) {
			inj := &seqInjector{}
			rx := startEngine(t, OverlayConfig{Offload: tc.receiver, RcvBuf: 4 << 20}, inj)

			tx := NewOverlayEngine(OverlayConfig{BindAddress: "127.0.0.1", Offload: tc.sender}, routeTo(rx.LocalPort()), nil)
			if err := tx.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer tx.Close()

			const n = 200
			b := tx.NewBatch()
			defer b.Release()
			sent := 0
			for sent < n {
				for i := 0; i < 20; i++ { // batches of 20 frames of one size, now and then a different one
					size := 1500
					if i == 19 {
						size = 600
					}
					f := frameOf(size, 0)
					binary.BigEndian.PutUint32(f.Payload, uint32(sent))
					if err := b.Send(f, net.IPv4(10, 244, 2, 10)); err != nil {
						t.Fatal(err)
					}
					sent++
				}
				if err := b.Flush(); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Millisecond)
			}
			waitFor(t, "all frames", func() bool { return inj.n.Load() == n })
			for i, s := range inj.delivered() {
				if s != uint32(i) {
					t.Fatalf("frame %d carries sequence %d", i, s)
				}
			}
			t.Logf("tx: %d datagrams in %d sends; rx: %d in %d reads", tx.Stats().TxPackets, tx.Stats().TxSends, rx.Stats().RxPackets, rx.Stats().RxReads)
		})
	}
}

// batchRecorder is an injector that holds frames until Flush, and notes when it is flushed.
type batchRecorder struct {
	mu      sync.Mutex
	held    []uint32
	flushes [][]uint32
	freed   bool
}

func (r *batchRecorder) NewBatch() InjectBatch { return r }
func (r *batchRecorder) InjectSuper(_ packet.VnetHeader, buf *packet.Buffer) error {
	buf.Release()
	return nil
}
func (r *batchRecorder) InjectFrame(f *packet.EthernetFrame) error {
	r.mu.Lock()
	r.held = append(r.held, binary.BigEndian.Uint32(f.Payload))
	r.mu.Unlock()
	return nil
}
func (r *batchRecorder) Flush() error {
	r.mu.Lock()
	r.flushes = append(r.flushes, r.held)
	r.held = nil
	r.mu.Unlock()
	return nil
}
func (r *batchRecorder) Release() {
	r.mu.Lock()
	r.freed = true
	r.mu.Unlock()
}

func (r *batchRecorder) total() (n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.flushes {
		n += len(f)
	}
	return n
}

// The worker holds what an injector holds while the queue has more and flushes when it
// has not; frames stay in order across flushes, and the batch is released at the end.
func TestWorkerFlushesWhenTheQueueRunsDry(t *testing.T) {
	inj := &batchRecorder{}
	l := &rxLane{oe: NewOverlayEngine(OverlayConfig{}, NewRouteTable(), inj)}
	queue := make(chan rxRead, 100)
	for i := 0; i < 70; i++ {
		buf := packet.GetBuffer()
		buf.B = append(buf.B, datagram(uint32(i), 200)...)
		queue <- rxRead{buf: buf}
	}
	close(queue)
	l.processLoop(context.Background(), queue)

	if !inj.freed {
		t.Error("the batch was not released")
	}
	var all []uint32
	for _, f := range inj.flushes {
		if len(f) > rxDrainMax {
			t.Errorf("a flush of %d reads, at most %d are held", len(f), rxDrainMax)
		}
		all = append(all, f...)
	}
	if len(inj.flushes) != 3 || len(all) != 70 { // 32 + 32 + 6
		t.Errorf("%d flushes, %d frames", len(inj.flushes), len(all))
	}
	for i, s := range all {
		if s != uint32(i) {
			t.Fatalf("frame %d carries sequence %d", i, s)
		}
	}
}

func TestWorkerFlushesAnIdleQueueAtOnce(t *testing.T) {
	inj := &batchRecorder{}
	oe := startEngine(t, OverlayConfig{}, inj)
	c := dialEngine(t, oe)
	if _, err := c.Write(datagram(1, 200)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the datagram to be flushed, though nothing follows", func() bool { return inj.total() == 1 })
}

// -- allocations

func TestOffloadSendAllocations(t *testing.T) {
	f := newFixture(t, OverlayConfig{Offload: true}, 1)
	frame := frameOf(1500, 1)
	dst := f.dst(0)
	one := testing.AllocsPerRun(50, func() { _ = f.oe.Send(frame, dst) })
	perBatch := testing.AllocsPerRun(50, func() {
		tb := f.oe.NewBatch()
		for i := 0; i < 32; i++ {
			_ = tb.Send(frame, dst)
		}
		_ = tb.Flush()
		tb.Release()
	})
	t.Logf("allocations: Send %.0f; a batch of 32 datagrams %.0f (segmentation: %v)", one, perBatch, f.oe.Stats().SegmentOffload)
	if perBatch > one {
		t.Errorf("a batch of 32 costs %.0f allocations, one Send %.0f", perBatch, one)
	}
}

// Receiving adds no allocation per datagram to what parsing costs, whether the datagrams arrive
// one by one or, with UDP_SEGMENT at the sender and UDP_GRO here, together, in fewer reads.
func TestOverlayReceiveAllocations(t *testing.T) {
	const size, total = 1450, 500
	d := datagram(1, size)
	parse := testing.AllocsPerRun(100, func() { _, _ = packet.ParseVXLAN(d) })

	for _, per := range []int{1, 10} { // datagrams per write
		t.Run(fmt.Sprintf("%d per write", per), func(t *testing.T) {
			writes := total / per
			inj := &seqInjector{seqs: make([]uint32, 0, total+2*per)} // so that recording allocates nothing
			oe := startEngine(t, OverlayConfig{Offload: per > 1, RcvBuf: 4 << 20}, inj)
			if per > 1 && !oe.Stats().ReceiveOffload {
				t.Skip("this kernel has no UDP_GRO")
			}
			c := dialEngine(t, oe)
			if per > 1 {
				rc, _ := c.SyscallConn()
				var cerr error
				if err := rc.Control(func(fd uintptr) { cerr = unix.SetsockoptInt(int(fd), unix.SOL_UDP, unix.UDP_SEGMENT, size) }); err != nil || cerr != nil {
					t.Skipf("no UDP_SEGMENT here: %v %v", err, cerr)
				}
			}
			burst := bytes.Repeat(d, per)
			send := func(n int) {
				want := inj.n.Load() + int64(n*per)
				for i := 0; i < n; i++ {
					if _, err := c.Write(burst); err != nil {
						t.Fatal(err)
					}
					time.Sleep(200 * time.Microsecond)
				}
				waitFor(t, "the writes", func() bool { return inj.n.Load() == want })
			}
			send(2) // warm the pool and the poller
			reads := oe.Stats().RxReads

			// The sender, reader and worker run concurrently: the heap's counter sees them all.
			// The sender and the injector allocate nothing per datagram.
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			send(writes)
			runtime.ReadMemStats(&after)
			perDatagram := float64(after.Mallocs-before.Mallocs) / float64(total)
			t.Logf("allocations per datagram: %.2f (ParseVXLAN alone: %.0f); %d datagrams in %d reads", perDatagram, parse, total, oe.Stats().RxReads-reads)
			// -race makes the buffer pool drop buffers, which adds a fraction.
			if perDatagram > parse+0.75 {
				t.Errorf("%.2f allocations per datagram, parsing accounts for %.0f", perDatagram, parse)
			}
		})
	}
}
