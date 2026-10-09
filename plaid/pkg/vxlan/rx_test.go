package vxlan

import (
	"context"
	"encoding/binary"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"plaid/pkg/packet"
)

// seqInjector records the sequence number each frame carries in its payload. While gate
// is open (nil) it does not hold the caller; otherwise InjectFrame waits for it to be
// closed, which models a bridge that is slower than the socket.
type seqInjector struct {
	gate chan struct{}
	mu   sync.Mutex
	seqs []uint32
	n    atomic.Int64
	// untrusted counts the frames that were not marked as having a verified checksum.
	untrusted atomic.Int64
}

func (s *seqInjector) InjectFrame(frame *packet.EthernetFrame) error {
	if s.gate != nil {
		<-s.gate
	}
	if !frame.Trusted {
		s.untrusted.Add(1)
	}
	seq := binary.BigEndian.Uint32(frame.Payload) // the payload is valid only during the call
	s.mu.Lock()
	s.seqs = append(s.seqs, seq)
	s.mu.Unlock()
	s.n.Add(1)
	return nil
}

func (s *seqInjector) delivered() []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint32(nil), s.seqs...)
}

// datagram is a VXLAN packet of size bytes in all whose inner frame carries seq.
func datagram(seq uint32, size int) []byte {
	payload := make([]byte, size-packet.VXLANHeaderLen-packet.EthernetHeaderLen)
	binary.BigEndian.PutUint32(payload, seq)
	f := testFrame(0)
	f.Payload = payload
	b, err := f.Marshal()
	if err != nil {
		panic(err)
	}
	return packet.MarshalVXLAN(packet.DefaultVNI, b)
}

func startEngine(t *testing.T, cfg OverlayConfig, inj FrameInjector) *OverlayEngine {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cfg.BindAddress, cfg.Port = "127.0.0.1", 0
	oe := NewOverlayEngine(cfg, NewRouteTable(), inj)
	if err := oe.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); oe.Close() })
	return oe
}

func dialEngine(t *testing.T, oe *OverlayEngine) *net.UDPConn {
	t.Helper()
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: loopback, Port: oe.LocalPort()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The frames of datagrams that came through the socket are handed on as Trusted: the
// kernel verified the UDP checksum that covers them.
func TestReceivedFramesAreTrusted(t *testing.T) {
	inj := &seqInjector{}
	oe := startEngine(t, OverlayConfig{}, inj)
	c := dialEngine(t, oe)
	for i := uint32(0); i < 10; i++ {
		if _, err := c.Write(datagram(i, 200)); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "all frames", func() bool { return inj.n.Load() == 10 })
	if n := inj.untrusted.Load(); n != 0 {
		t.Errorf("%d frames were not marked Trusted", n)
	}
}

// A burst that arrives while the bridge is busy waits in the queue and is delivered in
// full and in order.
func TestOverlayQueueAbsorbsABurst(t *testing.T) {
	const sent = 300
	inj := &seqInjector{gate: make(chan struct{})}
	oe := startEngine(t, OverlayConfig{RxQueueLen: 512, RcvBuf: 1 << 20}, inj)
	c := dialEngine(t, oe)

	for i := uint32(0); i < sent; i++ {
		if _, err := c.Write(datagram(i, 200)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Microsecond) // the burst is faster than the bridge, not than the reader
	}
	close(inj.gate)
	waitFor(t, "all frames", func() bool { return inj.n.Load() == sent })

	for i, seq := range inj.delivered() {
		if seq != uint32(i) {
			t.Fatalf("frame %d carries sequence %d: order was not kept", i, seq)
		}
	}
	if st := oe.Stats(); st.RxQueueDrops != 0 || st.SocketDrops != 0 || st.RxPackets != sent {
		t.Errorf("stats after the burst = %+v", st)
	}
}

// A queue that fills up drops what does not fit, without blocking the reader, and counts it.
func TestOverlayQueueOverflowIsCounted(t *testing.T) {
	const sent, queue = 100, 8
	inj := &seqInjector{gate: make(chan struct{})}
	oe := startEngine(t, OverlayConfig{RxQueueLen: queue, RcvBuf: 1 << 20}, inj)
	c := dialEngine(t, oe)

	for i := uint32(0); i < sent; i++ {
		if _, err := c.Write(datagram(i, 200)); err != nil {
			t.Fatal(err)
		}
	}
	// The worker holds one frame and the queue holds eight: the rest is dropped.
	waitFor(t, "the queue to overflow", func() bool { return oe.Stats().RxQueueDrops >= sent-queue-1 })
	close(inj.gate)
	waitFor(t, "every datagram to be accounted for", func() bool {
		st := oe.Stats()
		return uint64(inj.n.Load())+st.RxQueueDrops+st.SocketDrops == sent
	})

	st := oe.Stats()
	if st.RxQueueDrops == 0 {
		t.Error("nothing was counted as dropped by the queue")
	}
	t.Logf("delivered %d, queue drops %d, socket drops %d of %d", inj.n.Load(), st.RxQueueDrops, st.SocketDrops, sent)
	seqs := inj.delivered()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("frames out of order: %v", seqs)
		}
	}
}

// The kernel's own drop count reaches Stats. Drops that happen before the engine takes
// the socket are included, because the kernel's counter starts with the socket; it is
// reported on the next datagram that gets through.
func TestOverlayReportsSocketDrops(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_RXQ_OVFL is Linux only")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Skipf("cannot open a UDP socket: %v", err)
	}
	if err := conn.SetReadBuffer(4096); err != nil {
		t.Fatal(err)
	}
	c, err := net.DialUDP("udp4", nil, conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Nobody reads yet: the buffer holds a few of these and the kernel drops the rest.
	sent := 0
	for ; sent < 200; sent++ {
		if _, err := c.Write(datagram(uint32(sent), 1400)); err != nil {
			t.Fatal(err)
		}
	}

	inj := &seqInjector{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oe := NewOverlayEngine(OverlayConfig{BindAddress: "127.0.0.1"}, NewRouteTable(), inj)
	if err := oe.Adopt(ctx, conn); err != nil {
		t.Fatal(err)
	}
	defer oe.Close()

	// The count rides on a delivered datagram: send marks until one arrives.
	for marked := false; !marked; {
		if _, err := c.Write(datagram(uint32(sent), 1400)); err != nil {
			t.Fatal(err)
		}
		sent++
		marked = func() bool {
			deadline := time.Now().Add(20 * time.Millisecond)
			for time.Now().Before(deadline) {
				if s := inj.delivered(); len(s) > 0 && s[len(s)-1] == uint32(sent-1) {
					return true
				}
				time.Sleep(time.Millisecond)
			}
			return false
		}()
		if sent > 2000 {
			t.Fatal("no marker datagram was delivered")
		}
	}

	// The last datagram sent was delivered, so every earlier one is either delivered or
	// among the kernel's drops: nothing else can lose a datagram here.
	st := oe.Stats()
	t.Logf("sent %d, delivered %d, socket drops %d, queue drops %d", sent, inj.n.Load(), st.SocketDrops, st.RxQueueDrops)
	if st.SocketDrops == 0 {
		t.Error("the kernel dropped datagrams but Stats reports none")
	}
	if got := uint64(inj.n.Load()) + st.SocketDrops + st.RxQueueDrops; got != uint64(sent) {
		t.Errorf("delivered + socket drops + queue drops = %d, sent %d", got, sent)
	}
}

func TestOverlayReceiveBuffer(t *testing.T) {
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Skipf("cannot open a UDP socket: %v", err)
	}
	def := rcvBuf(probe)
	probe.Close()
	if def == 0 {
		t.Skip("the receive buffer cannot be read on this platform")
	}

	for _, req := range []int{1 << 20, 8 << 20} {
		oe := startEngine(t, OverlayConfig{RcvBuf: req}, newMockInjector())
		got := oe.Stats().RcvBuf
		t.Logf("kernel default %d, requested %d, effective %d", def, req, got)
		if got < def {
			t.Errorf("effective receive buffer %d is below the kernel default %d", got, def)
		}
	}
	want := startEngine(t, OverlayConfig{RcvBuf: defaultRcvBuf}, newMockInjector()).Stats().RcvBuf
	if got := startEngine(t, OverlayConfig{}, newMockInjector()).Stats().RcvBuf; got != want {
		t.Errorf("without RcvBuf the buffer is %d, want the default's %d", got, want)
	}
}
