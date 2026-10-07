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
}

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

	txPackets, txErrors, rxPackets, rxInvalid, rxDropped atomic.Uint64
}

// NewOverlayEngine creates an OverlayEngine instance.
func NewOverlayEngine(cfg OverlayConfig, routes *RouteTable, injector FrameInjector) *OverlayEngine {
	if cfg.DefaultVNI == 0 {
		cfg.DefaultVNI = packet.DefaultVNI
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

	oe.transport.Store(TransportBound)
	oe.conn.Store(conn)
	go oe.readLoop(ctx, conn)
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
	oe.transport.Store(TransportAdopted)
	oe.conn.Store(conn)
	go oe.readLoop(ctx, conn)
	return nil
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
		TxPackets: oe.txPackets.Load(),
		TxErrors:  oe.txErrors.Load(),
		RxPackets: oe.rxPackets.Load(),
		RxInvalid: oe.rxInvalid.Load(),
		RxDropped: oe.rxDropped.Load(),
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

func (oe *OverlayEngine) readLoop(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 65535)

	for {
		select {
		case <-ctx.Done():
			return
		case <-oe.closedChan:
			return
		default:
		}

		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-oe.closedChan:
				return
			case <-ctx.Done():
				return
			default:
				continue
			}
		}

		if n < packet.VXLANHeaderLen {
			oe.rxInvalid.Add(1)
			continue
		}

		vxlanPkt, err := packet.ParseVXLAN(buf[:n])
		if err != nil {
			oe.rxInvalid.Add(1)
			continue
		}

		if oe.injector != nil && vxlanPkt.InnerFrame != nil {
			if err := oe.injector.InjectFrame(vxlanPkt.InnerFrame); err != nil {
				oe.rxDropped.Add(1)
			} else {
				oe.rxPackets.Add(1)
			}
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
