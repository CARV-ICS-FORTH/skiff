//go:build linux

package tap

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"plaid/pkg/packet"
)

const (
	tunDevice = "/dev/net/tun"

	IFF_TAP      = 0x0002
	IFF_NO_PI    = 0x1000
	IFF_VNET_HDR = 0x4000

	TUNSETIFF       = 0x400454ca
	TUNGETIFF       = 0x800454d2
	TUNSETOFFLOAD   = 0x400454d0
	TUNSETVNETHDRSZ = 0x400454d8

	// TUNSETOFFLOAD flags: the frames the pod sends may leave their checksums to us, and
	// be TCP segments of up to 64 KiB (the pod's TSO) which we cut.
	TUN_F_CSUM = 0x01
	TUN_F_TSO4 = 0x02
	TUN_F_TSO6 = 0x04

	SIOCSIFFLAGS   = 0x8914
	SIOCSIFADDR    = 0x8916
	SIOCSIFNETMASK = 0x891c
	SIOCSIFMTU     = 0x8922
	SIOCSIFHWADDR  = 0x8924
	SIOCADDRT      = 0x890b

	IFF_UP      = 0x1
	IFF_RUNNING = 0x40

	RTF_UP       = 0x0001
	RTF_GATEWAY  = 0x0002
	ARPHRD_ETHER = 1
)

// TapConfig holds network configuration parameters for the TAP interface.
type TapConfig struct {
	Name    string
	IP      net.IP
	Mask    net.IPMask
	Gateway net.IP
	MAC     net.HardwareAddr
	MTU     int
}

type ifreq struct {
	name  [16]byte
	flags uint16
	pad   [22]byte
}

type ifreqInt struct {
	name [16]byte
	val  int32
	pad  [20]byte
}

type ifreqSockaddr struct {
	name [16]byte
	addr syscall.RawSockaddrInet4
	pad  [8]byte
}

type ifreqHWAddr struct {
	name [16]byte
	addr struct {
		family uint16
		data   [14]byte
	}
	pad [8]byte
}

type rtentry struct {
	pad1    uint64
	dst     syscall.RawSockaddrInet4
	gateway syscall.RawSockaddrInet4
	genmask syscall.RawSockaddrInet4
	flags   uint16
	pad2    int16
	pad3    uint64
	pad4    uint64
	metric  int16
	pad5    [6]byte
	dev     *byte
	pad6    [24]byte
}

func nameTo16(name string) [16]byte {
	var b [16]byte
	copy(b[:], []byte(name))
	return b
}

// CreateAndConfigureTap opens /dev/net/tun, allocates a TAP interface, and configures IP/routes inside netns.
// The TAP has a virtio_net_hdr before every frame (IFF_VNET_HDR) so that offloads can be
// switched on for it (see NewTapEndpoint); it has none switched on here, so the header
// is zeros until then. A kernel that refuses the header gives a TAP without it.
func CreateAndConfigureTap(cfg TapConfig) (*os.File, error) {
	fd, err := syscall.Open(tunDevice, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", tunDevice, err)
	}

	var req ifreq
	req.flags = IFF_TAP | IFF_NO_PI | IFF_VNET_HDR
	if cfg.Name != "" {
		req.name = nameTo16(cfg.Name)
	}

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(TUNSETIFF), uintptr(unsafe.Pointer(&req)))
	if errno == syscall.EINVAL {
		req.flags = IFF_TAP | IFF_NO_PI
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(TUNSETIFF), uintptr(unsafe.Pointer(&req)))
	}
	if errno != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("ioctl TUNSETIFF failed: %w", errno)
	}

	if req.flags&IFF_VNET_HDR != 0 {
		hdrLen := int32(packet.VnetHeaderLen)
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(TUNSETVNETHDRSZ), uintptr(unsafe.Pointer(&hdrLen)))
		if errno != 0 {
			_ = syscall.Close(fd)
			return nil, fmt.Errorf("ioctl TUNSETVNETHDRSZ failed: %w", errno)
		}
	}

	actualName := string(req.name[:])
	for i, c := range req.name {
		if c == 0 {
			actualName = string(req.name[:i])
			break
		}
	}

	// Open socket for network configuration ioctls
	sock, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("failed to create config socket: %w", err)
	}
	defer syscall.Close(sock)

	// 1. Set MTU
	if cfg.MTU > 0 {
		var reqMTU ifreqInt
		reqMTU.name = nameTo16(actualName)
		reqMTU.val = int32(cfg.MTU)
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(sock), uintptr(SIOCSIFMTU), uintptr(unsafe.Pointer(&reqMTU)))
		if errno != 0 {
			_ = syscall.Close(fd)
			return nil, fmt.Errorf("ioctl SIOCSIFMTU (%d) failed on %s: %w", cfg.MTU, actualName, errno)
		}
	}

	// 2. Set MAC
	if len(cfg.MAC) == 6 {
		var reqMAC ifreqHWAddr
		reqMAC.name = nameTo16(actualName)
		reqMAC.addr.family = ARPHRD_ETHER
		copy(reqMAC.addr.data[0:6], cfg.MAC)
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(sock), uintptr(SIOCSIFHWADDR), uintptr(unsafe.Pointer(&reqMAC)))
		if errno != 0 {
			_ = syscall.Close(fd)
			return nil, fmt.Errorf("ioctl SIOCSIFHWADDR (%s) failed on %s: %w", cfg.MAC, actualName, errno)
		}
	}

	// 3. Set IP
	if cfg.IP != nil && cfg.IP.To4() != nil {
		var reqAddr ifreqSockaddr
		reqAddr.name = nameTo16(actualName)
		reqAddr.addr.Family = syscall.AF_INET
		copy(reqAddr.addr.Addr[:], cfg.IP.To4())
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(sock), uintptr(SIOCSIFADDR), uintptr(unsafe.Pointer(&reqAddr)))
		if errno != 0 {
			_ = syscall.Close(fd)
			return nil, fmt.Errorf("ioctl SIOCSIFADDR (%s) failed on %s: %w", cfg.IP, actualName, errno)
		}
	}

	// 4. Set Netmask
	if len(cfg.Mask) == 4 {
		var reqMask ifreqSockaddr
		reqMask.name = nameTo16(actualName)
		reqMask.addr.Family = syscall.AF_INET
		copy(reqMask.addr.Addr[:], cfg.Mask)
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(sock), uintptr(SIOCSIFNETMASK), uintptr(unsafe.Pointer(&reqMask)))
		if errno != 0 {
			_ = syscall.Close(fd)
			return nil, fmt.Errorf("ioctl SIOCSIFNETMASK failed on %s: %w", actualName, errno)
		}
	}

	// 5. Bring TAP interface UP
	var reqUp ifreq
	reqUp.name = nameTo16(actualName)
	reqUp.flags = IFF_UP | IFF_RUNNING
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(sock), uintptr(SIOCSIFFLAGS), uintptr(unsafe.Pointer(&reqUp)))
	if errno != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("ioctl SIOCSIFFLAGS UP failed on %s: %w", actualName, errno)
	}

	// 6. Bring loopback up
	var reqLo ifreq
	reqLo.name = nameTo16("lo")
	reqLo.flags = IFF_UP | IFF_RUNNING
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(sock), uintptr(SIOCSIFFLAGS), uintptr(unsafe.Pointer(&reqLo)))
	if errno != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("ioctl SIOCSIFFLAGS UP failed on lo: %w", errno)
	}

	// 7. Add default route
	if cfg.Gateway != nil && cfg.Gateway.To4() != nil {
		var rt rtentry
		rt.flags = RTF_UP | RTF_GATEWAY
		devBytes := append([]byte(actualName), 0)
		rt.dev = &devBytes[0]
		rt.dst.Family = syscall.AF_INET
		rt.genmask.Family = syscall.AF_INET
		rt.gateway.Family = syscall.AF_INET
		copy(rt.gateway.Addr[:], cfg.Gateway.To4())
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(sock), uintptr(SIOCADDRT), uintptr(unsafe.Pointer(&rt)))
		if errno != 0 && errno != syscall.EEXIST {
			_ = syscall.Close(fd)
			return nil, fmt.Errorf("ioctl SIOCADDRT default route via %s dev %s failed: %w", cfg.Gateway, actualName, errno)
		}
	}

	return os.NewFile(uintptr(fd), actualName), nil
}

// hasVnetHdr reports whether the TAP behind f has a virtio header before its frames.
func hasVnetHdr(f *os.File) bool {
	rc, err := f.SyscallConn()
	if err != nil {
		return false
	}
	has := false
	_ = rc.Control(func(fd uintptr) {
		var req ifreq
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(TUNGETIFF), uintptr(unsafe.Pointer(&req)))
		has = errno == 0 && req.flags&IFF_VNET_HDR != 0
	})
	return has
}

// enableOffloads tells the TAP behind f that it may be given frames with checksums to
// complete and TCP segments to cut, and may hand over its own. Unprivileged users may.
func enableOffloads(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(TUNSETOFFLOAD), TUN_F_CSUM|TUN_F_TSO4|TUN_F_TSO6)
	}); err != nil {
		return err
	}
	if errno != 0 {
		return fmt.Errorf("ioctl TUNSETOFFLOAD: %w", errno)
	}
	return nil
}

// zeroVnetHdr is the header of a frame that asks nothing of the TAP.
var zeroVnetHdr [packet.VnetHeaderLen]byte

// vnetWriter writes frames to a TAP that has a virtio header, with the headers in front
// in one writev, so that the frame is not copied to make room for them.
type vnetWriter struct {
	mu    sync.Mutex
	rc    syscall.RawConn
	iov   [3]syscall.Iovec
	cnt   uintptr // the iovecs in use
	hdr   [packet.VnetHeaderLen]byte
	eth   [packet.EthernetHeaderLen]byte
	n     int
	errno syscall.Errno
	do    func(fd uintptr) bool // writev, made once because a closure made per call is an allocation
}

func newVnetWriter(f *os.File) *vnetWriter {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil
	}
	w := &vnetWriter{rc: rc}
	w.do = w.writev
	return w
}

func (w *vnetWriter) writev(fd uintptr) bool {
	n, _, errno := syscall.Syscall(syscall.SYS_WRITEV, fd, uintptr(unsafe.Pointer(&w.iov[0])), w.cnt)
	if errno == syscall.EAGAIN {
		return false // wait until the TAP can take the frame
	}
	w.n, w.errno = int(n), errno
	return true
}

// set makes b the i'th part of the next write.
func (w *vnetWriter) set(i int, b []byte) {
	w.iov[i].Base = &b[0]
	w.iov[i].SetLen(len(b))
	w.cnt = uintptr(i + 1)
}

// send writes the parts that are set; the caller holds the lock.
func (w *vnetWriter) send(want int) error {
	err := w.rc.Write(w.do)
	for i := range w.iov {
		w.iov[i].Base = nil
	}
	switch {
	case err != nil:
		return err
	case w.errno != 0:
		return w.errno
	case w.n != want:
		return io.ErrShortWrite
	}
	return nil
}

func (w *vnetWriter) write(frame []byte) error {
	if len(frame) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.set(0, zeroVnetHdr[:])
	w.set(1, frame)
	return w.send(len(zeroVnetHdr) + len(frame))
}

// writeSuper writes frame with the virtio header h, which says how the TAP is to treat
// it, from three parts: the header, the Ethernet header, and the payload where it is.
func (w *vnetWriter) writeSuper(h packet.VnetHeader, frame *packet.EthernetFrame) error {
	if len(frame.DstMAC) != 6 || len(frame.SrcMAC) != 6 {
		return packet.ErrInvalidMAC
	}
	if len(frame.Payload) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	h.Put(w.hdr[:])
	copy(w.eth[0:], frame.DstMAC)
	copy(w.eth[6:], frame.SrcMAC)
	binary.BigEndian.PutUint16(w.eth[12:], frame.EtherType)
	w.set(0, w.hdr[:])
	w.set(1, w.eth[:])
	w.set(2, frame.Payload)
	return w.send(len(w.hdr) + len(w.eth) + len(frame.Payload))
}

// pollProbe tells whether the TAP has a frame waiting, without reading it. One goroutine
// uses it.
type pollProbe struct {
	rc    syscall.RawConn
	fds   [1]unix.PollFd
	ready bool
	do    func(fd uintptr) // poll, made once because a closure made per call is an allocation
}

func newPollProbe(f *os.File) *pollProbe {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil
	}
	p := &pollProbe{rc: rc}
	p.do = p.poll1
	return p
}

func (p *pollProbe) poll1(fd uintptr) {
	p.fds[0] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
	n, err := unix.Poll(p.fds[:], 0)
	p.ready = err == nil && n > 0 && p.fds[0].Revents&unix.POLLIN != 0
}

// poll reports whether a frame is waiting; where that cannot be told, that none is.
func (p *pollProbe) poll() bool {
	if p == nil || p.rc.Control(p.do) != nil {
		return false
	}
	return p.ready
}
