package vxlan

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"plaid/pkg/packet"
)

// FrameInjector receives decapsulated Ethernet frames to deliver locally.
type FrameInjector interface {
	InjectFrame(frame *packet.EthernetFrame) error
}

// BatchInjector is a FrameInjector that can hold what it delivers until the end of a
// run of frames, so as to deliver the frames for one destination together.
type BatchInjector interface {
	FrameInjector
	NewBatch() InjectBatch
}

// InjectBatch injects frames like a FrameInjector but may hold their delivery until
// Flush. The overlay's worker flushes it when it has nothing more to inject for the
// moment, and releases it when it stops.
type InjectBatch interface {
	FrameInjector
	Flush() error
	Release()
}

// OverlayConfig holds configuration for the VXLAN overlay engine.
type OverlayConfig struct {
	BindAddress string // e.g. "0.0.0.0"
	Port        int    // e.g. 8472
	DefaultVNI  uint32 // e.g. 1
	// RxQueueLen is how many received datagrams may wait for the bridge (0: defaultRxQueueLen).
	// A queued read holds a pool buffer of 64 KiB, so the worst case is RxQueueLen x 64
	// KiB (512 x 64 KiB = 32 MiB by default); it is reached only while the queue is full.
	// With the offload a read holds several datagrams.
	RxQueueLen int
	// RcvBuf is the socket's receive buffer in bytes (0: defaultRcvBuf). The kernel caps the
	// request at net.core.rmem_max; Stats reports what it granted.
	RcvBuf int
	// Offload asks the kernel to send runs of equal-sized datagrams to one node with a
	// single call (UDP_SEGMENT) and to hand over datagrams that arrive in a run from one
	// node with a single read (UDP_GRO). Datagrams on the wire are the same either way,
	// and a node with the offload talks to one without. Where the kernel has no such
	// feature the overlay sends and reads one datagram at a time.
	Offload bool
}

// defaultRxQueueLen is enough to absorb a TCP window's worth of jumbo frames arriving
// faster than the bridge takes them.
const defaultRxQueueLen = 512

// defaultRcvBuf is the socket's buffer: room for a burst from a fast sender while the
// reader is busy.
const defaultRcvBuf = 8 << 20

// Transport says where the overlay's UDP socket came from.
const (
	TransportBound   = "bound"   // opened by this process (standalone Plaid)
	TransportAdopted = "adopted" // opened elsewhere (the physical host) and handed over
)

// Stats are the overlay's packet and error counters.
type Stats struct {
	Transport string
	Address   string // the local address of the socket, empty before attachment
	TxPackets uint64 // datagrams sent
	TxSends   uint64 // system calls that sent them: fewer than TxPackets with UDP_SEGMENT
	TxErrors  uint64 // datagrams that were not sent (no route, marshal or socket errors)
	RxReads   uint64 // system calls that received datagrams: fewer than the datagrams with UDP_GRO
	RxPackets uint64 // valid VXLAN packets handed to the bridge
	RxInvalid uint64 // datagrams that were not valid VXLAN
	RxDropped uint64 // valid packets the bridge did not accept
	// RxQueueDrops are datagrams read from the socket but dropped because the queue to the
	// bridge was full; SocketDrops are datagrams the kernel dropped because the socket's
	// buffer was full, as of the last datagram read (it moves only when one gets through).
	RxQueueDrops uint64
	SocketDrops  uint64
	RcvBuf       int // the socket's effective receive buffer in bytes, 0 if unknown
	// Whether sends use UDP_SEGMENT and the socket has UDP_GRO. Both are false without
	// Offload, or where the kernel lacks them (UDP_SEGMENT is switched off for good if the
	// first send with it fails).
	SegmentOffload bool
	ReceiveOffload bool
}

// OverlayEngine manages sending and receiving VXLAN packets over UDP.
type OverlayEngine struct {
	cfg       OverlayConfig
	routes    *RouteTable
	injector  FrameInjector
	conn      atomic.Pointer[net.UDPConn]
	transport atomic.Value // string
	// attachMu serializes the choice of the socket: an engine has one for its lifetime.
	attachMu   sync.Mutex
	closeOnce  sync.Once
	closedChan chan struct{}

	txPackets, txSends, txErrors, rxReads, rxPackets, rxInvalid, rxDropped, rxQueueDrops, socketDrops atomic.Uint64
	rcvBuf                                                                                            atomic.Int64
	// gso: sends use UDP_SEGMENT; gsoWorked: one did; gro: the socket has UDP_GRO.
	gso, gsoWorked, gro atomic.Bool
}

// NewOverlayEngine creates an OverlayEngine instance.
func NewOverlayEngine(cfg OverlayConfig, routes *RouteTable, injector FrameInjector) *OverlayEngine {
	if cfg.DefaultVNI == 0 {
		cfg.DefaultVNI = packet.DefaultVNI
	}
	if cfg.RxQueueLen <= 0 {
		cfg.RxQueueLen = defaultRxQueueLen
	}
	if cfg.RcvBuf <= 0 {
		cfg.RcvBuf = defaultRcvBuf
	}

	return &OverlayEngine{
		cfg:        cfg,
		routes:     routes,
		injector:   injector,
		closedChan: make(chan struct{}),
	}
}

// Start opens the configured UDP port itself and begins listening for VXLAN frames.
// Standalone Plaid does this; in a Skiff bubble the socket is opened on the physical
// host and handed over instead (see Adopt).
func (oe *OverlayEngine) Start(ctx context.Context) error {
	oe.attachMu.Lock()
	defer oe.attachMu.Unlock()
	if oe.conn.Load() != nil {
		return errors.New("overlay transport is already attached")
	}
	addrStr := fmt.Sprintf("%s:%d", oe.cfg.BindAddress, oe.cfg.Port)
	laddr, err := net.ResolveUDPAddr("udp", addrStr)
	if err != nil {
		return fmt.Errorf("failed to resolve overlay UDP address %s: %w", addrStr, err)
	}

	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return fmt.Errorf("failed to listen on VXLAN port %d: %w", oe.cfg.Port, err)
	}

	oe.attach(ctx, conn, TransportBound)
	return nil
}

// SocketFromFile turns a descriptor received from another process into the overlay's
// UDP socket, after checking that it is what the overlay was configured to use: an
// unconnected IPv4 UDP socket bound to the given address and port. The file is not
// consumed; the returned connection has its own duplicate of the descriptor.
func SocketFromFile(f *os.File, bind net.IP, port int) (*net.UDPConn, error) {
	pc, err := net.FilePacketConn(f)
	if err != nil {
		return nil, fmt.Errorf("descriptor is not a datagram socket: %w", err)
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, errors.New("descriptor is not a UDP socket")
	}
	laddr, _ := conn.LocalAddr().(*net.UDPAddr)
	switch {
	case laddr == nil || laddr.IP.To4() == nil:
		_ = conn.Close()
		return nil, errors.New("UDP socket is not IPv4")
	case conn.RemoteAddr() != nil:
		_ = conn.Close()
		return nil, errors.New("UDP socket is connected")
	case laddr.Port != port:
		_ = conn.Close()
		return nil, fmt.Errorf("UDP socket is bound to port %d, want %d", laddr.Port, port)
	case !laddr.IP.To4().Equal(bind.To4()):
		_ = conn.Close()
		return nil, fmt.Errorf("UDP socket is bound to %s, want %s", laddr.IP, bind)
	}
	return conn, nil
}

// Adopt makes conn the overlay's socket and begins reading from it. The engine never
// rebinds it; closing the engine closes it. An engine that already has a socket
// refuses (and leaves conn to the caller).
func (oe *OverlayEngine) Adopt(ctx context.Context, conn *net.UDPConn) error {
	oe.attachMu.Lock()
	defer oe.attachMu.Unlock()
	if oe.conn.Load() != nil {
		return errors.New("overlay transport is already attached")
	}
	select {
	case <-oe.closedChan:
		return errors.New("overlay engine is closed")
	default:
	}
	oe.attach(ctx, conn, TransportAdopted)
	return nil
}

// attach tunes the socket and starts the goroutines that serve it. The caller holds attachMu.
func (oe *OverlayEngine) attach(ctx context.Context, conn *net.UDPConn, transport string) {
	// Neither setting is essential: without them the overlay works with the kernel's
	// buffer and without its drop count, and Stats shows what the socket really has.
	_ = conn.SetReadBuffer(oe.cfg.RcvBuf)
	oe.rcvBuf.Store(int64(rcvBuf(conn)))
	_ = enableDropCount(conn)
	// UDP_SEGMENT without UDP_GRO makes a receiver drop more: the sender runs ahead of
	// it. So the offload is one feature, on when the receive side can have it.
	if oe.cfg.Offload && segmentOffload {
		oe.gro.Store(enableGRO(conn) == nil)
		oe.gso.Store(oe.gro.Load())
	}

	oe.transport.Store(transport)
	oe.conn.Store(conn)
	queue := make(chan rxRead, oe.cfg.RxQueueLen)
	go oe.readLoop(ctx, conn, queue)
	go oe.processLoop(ctx, queue)
}

// LocalPort returns the actual bound port (useful for tests using port 0).
func (oe *OverlayEngine) LocalPort() int {
	if c := oe.conn.Load(); c != nil {
		return c.LocalAddr().(*net.UDPAddr).Port
	}
	return oe.cfg.Port
}

// Stats returns the counters and the transport in use.
func (oe *OverlayEngine) Stats() Stats {
	st := Stats{
		TxPackets:    oe.txPackets.Load(),
		TxSends:      oe.txSends.Load(),
		TxErrors:     oe.txErrors.Load(),
		RxReads:      oe.rxReads.Load(),
		RxPackets:    oe.rxPackets.Load(),
		RxInvalid:    oe.rxInvalid.Load(),
		RxDropped:    oe.rxDropped.Load(),
		RxQueueDrops: oe.rxQueueDrops.Load(),
		SocketDrops:  oe.socketDrops.Load(),
		RcvBuf:       int(oe.rcvBuf.Load()),

		SegmentOffload: oe.gso.Load(),
		ReceiveOffload: oe.gro.Load(),
	}
	if t, ok := oe.transport.Load().(string); ok {
		st.Transport = t
	}
	if c := oe.conn.Load(); c != nil {
		st.Address = c.LocalAddr().String()
	}
	return st
}

// stopped reports whether the engine is closed or ctx is done.
func (oe *OverlayEngine) stopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-oe.closedChan:
		return true
	default:
		return false
	}
}

// rxRead is one read from the socket: a datagram, or with UDP_GRO several of seg bytes
// each (the last may be shorter) one after the other.
type rxRead struct {
	buf *packet.Buffer
	seg int
}

// datagrams is how many datagrams the read holds.
func (r rxRead) datagrams() int {
	if r.seg <= 0 {
		return 1
	}
	return (len(r.buf.B) + r.seg - 1) / r.seg
}

// readLoop does nothing but empty the socket into queue, so that a burst waits in user
// space, where it is cheap to hold, rather than overflowing the socket's small buffer
// while a datagram is being processed. It never blocks: when the queue is full the
// datagrams are dropped and counted. It closes queue when it returns.
func (oe *OverlayEngine) readLoop(ctx context.Context, conn *net.UDPConn, queue chan<- rxRead) {
	defer close(queue)
	buf := packet.GetBuffer()
	defer func() { buf.Release() }()
	oob := make([]byte, rxOOBLen)

	for !oe.stopped(ctx) {
		n, oobn, _, _, err := conn.ReadMsgUDPAddrPort(buf.B[:cap(buf.B)], oob)
		if err != nil {
			continue // the loop ends if the engine closed the socket
		}
		oe.rxReads.Add(1)
		if drops, ok := parseDropCount(oob[:oobn]); ok {
			oe.socketDrops.Store(uint64(drops))
		}

		buf.B = buf.B[:n]
		r := rxRead{buf, parseGROSize(oob[:oobn])}
		select {
		case queue <- r:
			buf = packet.GetBuffer()
		default:
			oe.rxQueueDrops.Add(uint64(r.datagrams())) // buf is read into again
		}
	}
}

// rxDrainMax is how many reads the worker takes from the queue before it flushes what
// it injected.
const rxDrainMax = 32

// processLoop is the single consumer of queue: datagrams reach the injector one at a
// time and in the order they were read. When the injector can hold deliveries, they are
// held while the queue has more, up to rxDrainMax reads, so that frames for one
// destination are delivered together; nothing waits for more to arrive. Once the engine
// stops it releases what is left.
func (oe *OverlayEngine) processLoop(ctx context.Context, queue <-chan rxRead) {
	sink := oe.injector
	var batch InjectBatch
	if bi, ok := sink.(BatchInjector); ok {
		batch = bi.NewBatch()
		defer batch.Release()
		sink = batch
	}
	for r := range queue {
		oe.processRead(ctx, sink, r)
		if batch == nil {
			continue
		}
	drain:
		for i := 1; i < rxDrainMax; i++ {
			select {
			case r, ok := <-queue:
				if !ok {
					break drain
				}
				oe.processRead(ctx, sink, r)
			default:
				break drain
			}
		}
		_ = batch.Flush()
	}
}

// processRead handles the datagrams of one read and releases its buffer.
func (oe *OverlayEngine) processRead(ctx context.Context, sink FrameInjector, r rxRead) {
	if !oe.stopped(ctx) {
		for rest := r.buf.B; len(rest) > 0; {
			n := len(rest)
			if r.seg > 0 && r.seg < n {
				n = r.seg
			}
			oe.process(sink, rest[:n])
			rest = rest[n:]
		}
	}
	r.buf.Release()
}

// process handles one datagram; the frame it injects is valid only during the call.
func (oe *OverlayEngine) process(sink FrameInjector, datagram []byte) {
	if len(datagram) < packet.VXLANHeaderLen {
		oe.rxInvalid.Add(1)
		return
	}

	vxlanPkt, err := packet.ParseVXLAN(datagram)
	if err != nil {
		oe.rxInvalid.Add(1)
		return
	}

	if sink != nil && vxlanPkt.InnerFrame != nil {
		// The kernel verified the datagram's UDP checksum, which covers the whole frame,
		// and the engine never turns checksums off on its socket: the frame is as the
		// sending node sent it, and its TCP checksum need not be verified again.
		vxlanPkt.InnerFrame.Trusted = true
		if err := sink.InjectFrame(vxlanPkt.InnerFrame); err != nil {
			oe.rxDropped.Add(1)
		} else {
			oe.rxPackets.Add(1)
		}
	}
}

// Close closes the UDP socket and terminates background readers.
func (oe *OverlayEngine) Close() error {
	var err error
	oe.closeOnce.Do(func() {
		close(oe.closedChan)
		oe.attachMu.Lock()
		defer oe.attachMu.Unlock()
		if c := oe.conn.Load(); c != nil {
			err = c.Close()
		}
	})
	return err
}
