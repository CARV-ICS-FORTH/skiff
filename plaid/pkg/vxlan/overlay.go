package vxlan

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	// RxQueueLen is how many received datagrams may wait for the bridge on each receive
	// socket (0: defaultRxQueueLen). A queued read holds a pool buffer of 64 KiB, so the
	// worst case is RxQueueLen x 64 KiB for each socket (512 x 64 KiB = 32 MiB by
	// default); it is reached only while the queue is full. With the offload a read holds
	// several datagrams.
	RxQueueLen int
	// RcvBuf is each receive socket's receive buffer in bytes (0: defaultRcvBuf). The
	// kernel caps the request at net.core.rmem_max; Stats reports what it granted.
	RcvBuf int
	// Sockets is how many receive sockets Start opens (0: DefaultSockets, at most
	// MaxSockets), and SendPerReceive times as many send sockets. Adopt takes the sockets
	// it is given and ignores it.
	Sockets int
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

// defaultRcvBuf is a receive socket's buffer: room for a burst from a fast sender while
// the reader is busy.
const defaultRcvBuf = 8 << 20

// Transport says where the overlay's UDP sockets came from.
const (
	TransportBound   = "bound"   // opened by this process (standalone Plaid)
	TransportAdopted = "adopted" // opened elsewhere (the physical host) and handed over
)

// Stats are the overlay's packet and error counters.
type Stats struct {
	Transport string
	Address   string // the local address of the (first) receive socket, empty before attachment
	TxPackets uint64 // datagrams sent
	TxSends   uint64 // system calls that sent them: fewer than TxPackets with UDP_SEGMENT
	TxErrors  uint64 // datagrams that were not sent (no route, marshal or socket errors)
	RxReads   uint64 // system calls that received datagrams: fewer than the datagrams with UDP_GRO
	RxPackets uint64 // valid VXLAN packets handed to the bridge, as many as RxPerSocket adds up to
	RxInvalid uint64 // datagrams that were not valid VXLAN
	RxDropped uint64 // valid packets the bridge did not accept
	// RxQueueDrops are datagrams read from a socket but dropped because the queue to the
	// bridge was full; SocketDrops are datagrams the kernel dropped because a socket's
	// buffer was full, summed over the receive sockets, each as of the last datagram read
	// from it (it moves only when one gets through).
	RxQueueDrops uint64
	SocketDrops  uint64
	RcvBuf       int // a receive socket's effective receive buffer in bytes, 0 if unknown
	// How many receive and send sockets there are, and how many packets each receive socket
	// handed to the bridge: how evenly the kernel spread the datagrams.
	RxSockets, TxSockets int
	RxPerSocket          []uint64
	// Whether sends use UDP_SEGMENT and the receive sockets have UDP_GRO. Both are false
	// without Offload, or where the kernel lacks them (UDP_SEGMENT is switched off for good
	// if the first send with it fails).
	SegmentOffload bool
	ReceiveOffload bool
}

// OverlayEngine manages sending and receiving VXLAN packets over UDP.
//
// It receives on several sockets in one SO_REUSEPORT group, each served by its own reader
// and worker (an rxLane), and sends from sockets of its own bound to ports of their own,
// chosen by the flow of each frame (see flowSocket). The kernel puts a datagram on the
// receive socket by a hash of its addresses and ports, so a flow, which leaves one source
// port, stays on one lane and keeps its order, while different flows share the cores.
type OverlayEngine struct {
	cfg       OverlayConfig
	routes    *RouteTable
	injector  FrameInjector
	socks     atomic.Pointer[sockets]
	transport atomic.Value // string
	// attachMu serializes the choice of the sockets: an engine has them for its lifetime.
	attachMu   sync.Mutex
	closeOnce  sync.Once
	closedChan chan struct{}

	txPackets, txSends, txErrors, rxReads, rxInvalid, rxDropped, rxQueueDrops atomic.Uint64
	rcvBuf                                                                    atomic.Int64
	// gso: sends use UDP_SEGMENT; gsoWorked: one did; gro: the receive sockets have UDP_GRO.
	gso, gsoWorked, gro atomic.Bool
}

// sockets are an engine's sockets, set once when it is attached.
type sockets struct {
	rx []*rxLane
	// tx are the send sockets, never none. They only send: nothing reads them, and
	// datagrams that reach them (to a port of theirs) are none of the overlay's.
	tx []*net.UDPConn
}

// rxLane is a receive socket with the reader and the worker that serve it.
type rxLane struct {
	oe        *OverlayEngine
	conn      *net.UDPConn
	delivered atomic.Uint64 // valid VXLAN packets handed to the bridge
	drops     atomic.Uint64 // the socket's drop count at the last datagram read from it
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
	if cfg.Sockets <= 0 {
		cfg.Sockets = DefaultSockets
	}

	return &OverlayEngine{
		cfg:        cfg,
		routes:     routes,
		injector:   injector,
		closedChan: make(chan struct{}),
	}
}

// Start opens the sockets itself (see ListenSockets) and begins listening for VXLAN
// frames. Standalone Plaid does this; in a Skiff bubble the sockets are opened on the
// physical host and handed over instead (see Adopt).
func (oe *OverlayEngine) Start(ctx context.Context) error {
	oe.attachMu.Lock()
	defer oe.attachMu.Unlock()
	if oe.socks.Load() != nil {
		return errors.New("overlay transport is already attached")
	}
	addrStr := fmt.Sprintf("%s:%d", oe.cfg.BindAddress, oe.cfg.Port)
	laddr, err := net.ResolveUDPAddr("udp", addrStr)
	if err != nil {
		return fmt.Errorf("failed to resolve overlay UDP address %s: %w", addrStr, err)
	}

	rx, tx, err := ListenSockets("udp", laddr.IP, laddr.Port, oe.cfg.Sockets)
	if err != nil {
		return fmt.Errorf("failed to listen on VXLAN port %d: %w", oe.cfg.Port, err)
	}

	oe.attach(ctx, rx, tx, TransportBound)
	return nil
}

// Adopt makes rx the overlay's receive sockets and tx its send sockets, and begins
// reading from the receive sockets. The engine never rebinds them; closing the engine
// closes them. An engine that already has sockets refuses (and leaves them to the
// caller).
func (oe *OverlayEngine) Adopt(ctx context.Context, rx, tx []*net.UDPConn) error {
	oe.attachMu.Lock()
	defer oe.attachMu.Unlock()
	if oe.socks.Load() != nil {
		return errors.New("overlay transport is already attached")
	}
	select {
	case <-oe.closedChan:
		return errors.New("overlay engine is closed")
	default:
	}
	if len(rx) == 0 {
		return errors.New("no receive socket")
	}
	if len(tx) == 0 {
		return errors.New("no send socket")
	}
	oe.attach(ctx, rx, tx, TransportAdopted)
	return nil
}

// attach tunes the sockets and starts the goroutines that serve them. The caller holds attachMu.
func (oe *OverlayEngine) attach(ctx context.Context, rx, tx []*net.UDPConn, transport string) {
	s := &sockets{tx: tx}
	gro := oe.cfg.Offload && segmentOffload
	for i, conn := range rx {
		// Neither setting is essential: without them the overlay works with the kernel's
		// buffer and without its drop count, and Stats shows what the socket really has.
		_ = conn.SetReadBuffer(oe.cfg.RcvBuf)
		if i == 0 {
			oe.rcvBuf.Store(int64(rcvBuf(conn)))
		}
		_ = enableDropCount(conn)
		// UDP_SEGMENT without UDP_GRO makes a receiver drop more: the sender runs ahead of
		// it. So the offload is one feature, on when the receive side can have it.
		if gro {
			gro = enableGRO(conn) == nil
		}
		s.rx = append(s.rx, &rxLane{oe: oe, conn: conn})
	}
	oe.gro.Store(gro)
	oe.gso.Store(gro)

	oe.transport.Store(transport)
	oe.socks.Store(s)
	for _, l := range s.rx {
		queue := make(chan rxRead, oe.cfg.RxQueueLen)
		go l.readLoop(ctx, queue)
		go l.processLoop(ctx, queue)
	}
}

// LocalPort returns the actual bound port of the receive sockets (useful for tests using
// port 0).
func (oe *OverlayEngine) LocalPort() int {
	if s := oe.socks.Load(); s != nil {
		return s.rx[0].conn.LocalAddr().(*net.UDPAddr).Port
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
		RxInvalid:    oe.rxInvalid.Load(),
		RxDropped:    oe.rxDropped.Load(),
		RxQueueDrops: oe.rxQueueDrops.Load(),
		RcvBuf:       int(oe.rcvBuf.Load()),

		SegmentOffload: oe.gso.Load(),
		ReceiveOffload: oe.gro.Load(),
	}
	if t, ok := oe.transport.Load().(string); ok {
		st.Transport = t
	}
	if s := oe.socks.Load(); s != nil {
		st.Address = s.rx[0].conn.LocalAddr().String()
		st.RxSockets, st.TxSockets = len(s.rx), len(s.tx)
		for _, l := range s.rx {
			n := l.delivered.Load()
			st.RxPerSocket = append(st.RxPerSocket, n)
			st.RxPackets += n
			st.SocketDrops += l.drops.Load()
		}
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
func (l *rxLane) readLoop(ctx context.Context, queue chan<- rxRead) {
	defer close(queue)
	oe := l.oe
	buf := packet.GetBuffer()
	defer func() { buf.Release() }()
	oob := make([]byte, rxOOBLen)

	for !oe.stopped(ctx) {
		n, oobn, _, _, err := l.conn.ReadMsgUDPAddrPort(buf.B[:cap(buf.B)], oob)
		if err != nil {
			continue // the loop ends if the engine closed the socket
		}
		oe.rxReads.Add(1)
		if drops, ok := parseDropCount(oob[:oobn]); ok {
			l.drops.Store(uint64(drops))
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

// processLoop is the single consumer of its lane's queue: datagrams reach the injector
// one at a time and in the order they were read. When the injector can hold deliveries,
// they are held while the queue has more, up to rxDrainMax reads, so that frames for one
// destination are delivered together; nothing waits for more to arrive. Once the engine
// stops it releases what is left. The engine's other lanes run their own processLoop at
// the same time, each with a batch of its own, into the same injector.
func (l *rxLane) processLoop(ctx context.Context, queue <-chan rxRead) {
	sink := l.oe.injector
	var batch InjectBatch
	if bi, ok := sink.(BatchInjector); ok {
		batch = bi.NewBatch()
		defer batch.Release()
		sink = batch
	}
	for r := range queue {
		l.processRead(ctx, sink, r)
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
				l.processRead(ctx, sink, r)
			default:
				break drain
			}
		}
		_ = batch.Flush()
	}
}

// processRead handles the datagrams of one read and releases its buffer.
func (l *rxLane) processRead(ctx context.Context, sink FrameInjector, r rxRead) {
	if !l.oe.stopped(ctx) {
		for rest := r.buf.B; len(rest) > 0; {
			n := len(rest)
			if r.seg > 0 && r.seg < n {
				n = r.seg
			}
			l.process(sink, rest[:n])
			rest = rest[n:]
		}
	}
	r.buf.Release()
}

// process handles one datagram; the frame it injects is valid only during the call.
func (l *rxLane) process(sink FrameInjector, datagram []byte) {
	oe := l.oe
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
			l.delivered.Add(1)
		}
	}
}

// Close closes the UDP sockets and terminates background readers and workers.
func (oe *OverlayEngine) Close() error {
	var err error
	oe.closeOnce.Do(func() {
		close(oe.closedChan)
		oe.attachMu.Lock()
		defer oe.attachMu.Unlock()
		if s := oe.socks.Load(); s != nil {
			for _, l := range s.rx {
				err = errors.Join(err, l.conn.Close())
			}
			for _, c := range s.tx {
				err = errors.Join(err, c.Close())
			}
		}
	})
	return err
}
