package vxlan

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"

	"plaid/pkg/packet"
)

// FrameInjector receives decapsulated Ethernet frames to deliver locally.
type FrameInjector interface {
	InjectFrame(frame *packet.EthernetFrame) error
}

// OverlayConfig holds configuration for the VXLAN overlay engine.
type OverlayConfig struct {
	BindAddress string // e.g. "0.0.0.0"
	Port        int    // e.g. 8472
	DefaultVNI  uint32 // e.g. 1
	// RxQueueLen is how many received datagrams may wait for the bridge (0: defaultRxQueueLen).
	// A queued datagram holds a pool buffer of 64 KiB, so the worst case is RxQueueLen x 64
	// KiB (512 x 64 KiB = 32 MiB by default); it is reached only while the queue is full.
	RxQueueLen int
	// RcvBuf is the socket's receive buffer in bytes (0: defaultRcvBuf). The kernel caps the
	// request at net.core.rmem_max; Stats reports what it granted.
	RcvBuf int
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
	TxPackets uint64
	TxErrors  uint64 // sends that failed (no route, marshal or socket errors)
	RxPackets uint64 // valid VXLAN packets handed to the bridge
	RxInvalid uint64 // datagrams that were not valid VXLAN
	RxDropped uint64 // valid packets the bridge did not accept
	// RxQueueDrops are datagrams read from the socket but dropped because the queue to the
	// bridge was full; SocketDrops are datagrams the kernel dropped because the socket's
	// buffer was full, as of the last datagram read (it moves only when one gets through).
	RxQueueDrops uint64
	SocketDrops  uint64
	RcvBuf       int // the socket's effective receive buffer in bytes, 0 if unknown
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

	txPackets, txErrors, rxPackets, rxInvalid, rxDropped, rxQueueDrops, socketDrops atomic.Uint64
	rcvBuf                                                                          atomic.Int64
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

	oe.transport.Store(transport)
	oe.conn.Store(conn)
	queue := make(chan *packet.Buffer, oe.cfg.RxQueueLen)
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
		TxErrors:     oe.txErrors.Load(),
		RxPackets:    oe.rxPackets.Load(),
		RxInvalid:    oe.rxInvalid.Load(),
		RxDropped:    oe.rxDropped.Load(),
		RxQueueDrops: oe.rxQueueDrops.Load(),
		SocketDrops:  oe.socketDrops.Load(),
		RcvBuf:       int(oe.rcvBuf.Load()),
	}
	if t, ok := oe.transport.Load().(string); ok {
		st.Transport = t
	}
	if c := oe.conn.Load(); c != nil {
		st.Address = c.LocalAddr().String()
	}
	return st
}

// Send encapsulates an Ethernet frame into a VXLAN packet and sends it to the remote node.
func (oe *OverlayEngine) Send(frame *packet.EthernetFrame, dstIP net.IP) (err error) {
	conn := oe.conn.Load()
	if conn == nil {
		return fmt.Errorf("overlay engine not started")
	}
	defer func() {
		if err != nil {
			oe.txErrors.Add(1)
		} else {
			oe.txPackets.Add(1)
		}
	}()

	route, err := oe.routes.Lookup(dstIP)
	if err != nil {
		return fmt.Errorf("no overlay route for destination IP %s: %w", dstIP, err)
	}

	// Rewrite destination MAC to remote VTEP MAC if route specifies it
	if len(route.VtepMAC) == 6 {
		frame.DstMAC = route.VtepMAC
	}

	vni := route.VNI
	if vni == 0 {
		vni = oe.cfg.DefaultVNI
	}

	remotePort := route.Port
	if remotePort <= 0 {
		remotePort = oe.cfg.Port
	}

	remoteIP, ok := netip.AddrFromSlice(route.RemoteHostIP)
	if !ok || remotePort > math.MaxUint16 {
		return fmt.Errorf("invalid overlay remote address %s port %d", route.RemoteHostIP, remotePort)
	}
	remoteAddr := netip.AddrPortFrom(remoteIP.Unmap(), uint16(remotePort))

	// The header and the frame go into one pooled buffer; the write is synchronous, so
	// the buffer is free again once it returns.
	buf := packet.GetBuffer()
	defer buf.Release()
	buf.B = packet.AppendVXLANHeader(buf.B, vni)
	buf.B, err = frame.MarshalTo(buf.B)
	if err != nil {
		return fmt.Errorf("failed to marshal inner frame: %w", err)
	}

	_, err = conn.WriteToUDPAddrPort(buf.B, remoteAddr)
	return err
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

// readLoop does nothing but empty the socket into queue, so that a burst waits in user
// space, where it is cheap to hold, rather than overflowing the socket's small buffer
// while a datagram is being processed. It never blocks: when the queue is full the
// datagram is dropped and counted. It closes queue when it returns.
func (oe *OverlayEngine) readLoop(ctx context.Context, conn *net.UDPConn, queue chan<- *packet.Buffer) {
	defer close(queue)
	buf := packet.GetBuffer()
	defer func() { buf.Release() }()
	oob := make([]byte, dropCountOOBLen)

	for !oe.stopped(ctx) {
		n, oobn, _, _, err := conn.ReadMsgUDPAddrPort(buf.B[:cap(buf.B)], oob)
		if err != nil {
			continue // the loop ends if the engine closed the socket
		}
		if drops, ok := parseDropCount(oob[:oobn]); ok {
			oe.socketDrops.Store(uint64(drops))
		}

		buf.B = buf.B[:n]
		select {
		case queue <- buf:
			buf = packet.GetBuffer()
		default:
			oe.rxQueueDrops.Add(1) // buf is read into again
		}
	}
}

// processLoop is the single consumer of queue: datagrams reach the injector one at a
// time and in the order they were read. Once the engine stops it releases what is left.
func (oe *OverlayEngine) processLoop(ctx context.Context, queue <-chan *packet.Buffer) {
	for buf := range queue {
		if !oe.stopped(ctx) {
			oe.process(buf.B)
		}
		buf.Release()
	}
}

// process handles one datagram; the frame it injects is valid only during the call.
func (oe *OverlayEngine) process(datagram []byte) {
	if len(datagram) < packet.VXLANHeaderLen {
		oe.rxInvalid.Add(1)
		return
	}

	vxlanPkt, err := packet.ParseVXLAN(datagram)
	if err != nil {
		oe.rxInvalid.Add(1)
		return
	}

	if oe.injector != nil && vxlanPkt.InnerFrame != nil {
		if err := oe.injector.InjectFrame(vxlanPkt.InnerFrame); err != nil {
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
