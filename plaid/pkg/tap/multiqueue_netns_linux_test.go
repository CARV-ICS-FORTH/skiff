//go:build linux

package tap

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
)

// The tests here run pods whose TAPs have several queues, in network namespaces of their own
// in a user namespace, so that real kernels' TCP and tun driver are at both ends: a pod's
// stack and the driver choose the queue of each packet the pod sends, as they do in
// production, and the TapEndpoint chooses the queue of each frame it writes.

// mqSide is one pod's TAP: the queues it is made with, and how many of their files the
// endpoint is given (an older plaidd keeps the first and closes the others).
type mqSide struct{ queues, used int }

type mqCase struct {
	name  string
	a, b  mqSide
	plain bool // no offloads, no pipeline
}

var mqCases = []mqCase{
	{name: "4 queues each", a: mqSide{4, 4}, b: mqSide{4, 4}},
	{name: "4 queues each, plain", a: mqSide{4, 4}, b: mqSide{4, 4}, plain: true},
	{name: "old plugin on one side", a: mqSide{1, 1}, b: mqSide{4, 4}},
	{name: "old plaidd: only the first file", a: mqSide{4, 1}, b: mqSide{4, 1}},
	{name: "2 queues against 4", a: mqSide{2, 2}, b: mqSide{4, 4}},
}

// mqPod is a pod: its namespace, the files of its TAP, and sockets in it.
type mqPod struct {
	name   string
	ip     string
	mac    net.HardwareAddr
	side   mqSide
	ns     *netns
	files  []*os.File
	lfds   [4]int // listeners, on lports
	cfds   [4]int // client sockets, bound to the ports chosen for them
	lports [4]int
	sports [4]int
	ep     *TapEndpoint
}

const mqStreams = 4

// tcpFlow is the start of an IPv4 packet of the TCP flow from src to dst: the header and the ports.
func tcpFlow(src, dst string, sport, dport int) []byte {
	ip := make([]byte, 24)
	ip[0], ip[8], ip[9] = 0x45, 64, packet.IPProtocolTCP
	copy(ip[12:], net.ParseIP(src).To4())
	copy(ip[16:], net.ParseIP(dst).To4())
	binary.BigEndian.PutUint16(ip[20:], uint16(sport))
	binary.BigEndian.PutUint16(ip[22:], uint16(dport))
	return ip
}

// pickPorts chooses the source ports of the streams from src to dst, which go to the
// ports dports of dst, so that the frames of the streams that are written to dst's TAP go
// to different queues (the hash of the flow decides, as it does for every frame), and
// the answers written to src's, as far as it has queues for it.
func pickPorts(src, dst string, dports [mqStreams]int, srcQueues, dstQueues int) (ports [mqStreams]int) {
	answers := map[int]bool{}
	for i := range ports {
		for p := 30000 + 1000*i; ; p++ {
			to := packet.FlowIndex(packet.EtherTypeIPv4, tcpFlow(src, dst, p, dports[i]), dstQueues)
			back := packet.FlowIndex(packet.EtherTypeIPv4, tcpFlow(dst, src, dports[i], p), srcQueues)
			if to != i%dstQueues || answers[back] && len(answers) < srcQueues {
				continue
			}
			answers[back], ports[i] = true, p
			break
		}
	}
	return ports
}

// pollable makes a file of f's TAP queue that the endpoint reads as plaidd does.
func pollable(f *os.File) *os.File {
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		panic(err)
	}
	_ = f.Close()
	return FileFromFD(fd, "tap")
}

// newMQPod makes the pod's namespace with its TAP, listeners on lports and client sockets
// bound to sports.
func newMQPod(t *testing.T, name, ip, mac string, side mqSide, lports, sports [mqStreams]int) *mqPod {
	t.Helper()
	p := &mqPod{name: name, ip: ip, side: side, lports: lports, sports: sports}
	p.mac, _ = net.ParseMAC(mac)
	var err error
	if p.ns, err = newNetns(); err != nil {
		t.Skipf("cannot create a network namespace: %v", err)
	}
	t.Cleanup(p.ns.close)
	if err := p.ns.do(func() (err error) {
		// No IPv6: its router solicitations and multicast reports would send a frame out of
		// the TAP on any queue now and then, and the queues of a pod that has one stream
		// would not be idle.
		for _, k := range []string{"all", "default"} {
			if err = os.WriteFile("/proc/sys/net/ipv6/conf/"+k+"/disable_ipv6", []byte("1"), 0); err != nil {
				return err
			}
		}
		p.files, err = CreateAndConfigureTapQueues(TapConfig{Name: "pod0", IP: net.ParseIP(ip), Mask: net.CIDRMask(24, 32), MAC: p.mac, MTU: 1450}, side.queues)
		if err != nil {
			return err
		}
		for i := range p.lfds {
			if p.lfds[i], err = listener(lports[i]); err != nil {
				return err
			}
			if p.cfds[i], err = tcpSocket(); err != nil {
				return err
			}
			if err = syscall.Bind(p.cfds[i], &syscall.SockaddrInet4{Port: sports[i], Addr: [4]byte(net.ParseIP(ip).To4())}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("pod %s: %v", name, err)
	}
	if len(p.files) != side.queues {
		t.Skipf("this kernel gave %d queues of %d: no multi-queue TAPs", len(p.files), side.queues)
	}
	return p
}

// endpoint makes the pod's endpoint with the files it is to have, as plaidd's handler does: all that it
// is given, and those that an older one keeps (the first) with the others closed.
func (p *mqPod) endpoint(t *testing.T, offload bool) *TapEndpoint {
	t.Helper()
	var files []*os.File
	for i, f := range p.files {
		if i < p.side.used {
			files = append(files, pollable(f))
		} else {
			_ = f.Close()
		}
	}
	p.ep = NewTapEndpointQueues(p.name, p.name, net.ParseIP(p.ip), p.mac, files, offload)
	if offload && !p.ep.Offload() {
		t.Skip("this kernel does not give TAPs a virtio header and offloads")
	}
	return p.ep
}

type mqFlow struct {
	src, dst     [4]byte
	sport, dport uint16
}

// mqReads records the queue that each flow's frames are read from. The first SYN of a
// connection is left out: the pod's stack sends it before the driver has seen a frame of
// the flow, so it chooses by itself. The driver learns the queue of a flow from the frames
// written to it, after it has passed them to the pod's stack; so the first answer, which
// the stack makes meanwhile (the SYN-ACK of a server, the ACK of a client), and the first
// segment that another core sends before the driver has learned, also go out on the queue
// that the driver chooses by itself, and everything after them where it was told.
type mqReads struct {
	mu    sync.Mutex
	reads map[int]int            // the reads that left frames, by queue
	flows map[mqFlow]map[int]int // the frames read of each flow, by queue
}

// strays returns, for the flow with the frames read by queue, how many were not read on the
// queue that most were.
func strays(queues map[int]int) int {
	total, most := 0, 0
	for _, n := range queues {
		total += n
		most = max(most, n)
	}
	return total - most
}

// mqStrayLimit is the most frames of a flow that may be read on another queue than the rest.
const mqStrayLimit = 2

func (r *mqReads) hook(queue int, frames [][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reads == nil {
		r.reads = map[int]int{}
	}
	r.reads[queue]++
	for _, f := range frames {
		if len(f) < packet.EthernetHeaderLen+packet.IPv4HeaderMinLen+20 || binary.BigEndian.Uint16(f[12:]) != packet.EtherTypeIPv4 {
			continue
		}
		ip := f[packet.EthernetHeaderLen:]
		hl := int(ip[0]&0xf) * 4
		if ip[9] != packet.IPProtocolTCP || len(ip) < hl+20 || ip[hl+13]&0x12 == 0x02 {
			continue
		}
		k := mqFlow{src: [4]byte(ip[12:16]), dst: [4]byte(ip[16:20]), sport: binary.BigEndian.Uint16(ip[hl:]), dport: binary.BigEndian.Uint16(ip[hl+2:])}
		if r.flows == nil {
			r.flows = map[mqFlow]map[int]int{}
		}
		if r.flows[k] == nil {
			r.flows[k] = map[int]int{}
		}
		r.flows[k][queue]++
	}
}

// queuesUsed counts the queues of ep that have read, by what its read hook (reads.hook) has
// seen, and written.
func queuesUsed(ep *TapEndpoint, reads *mqReads) (read, written int, counts string) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	for _, q := range ep.queues {
		if reads.reads[q.idx] > 0 {
			read++
		}
		if q.writes.Load() > 0 {
			written++
		}
		counts += fmt.Sprintf(" q%d:%dr/%dw", q.idx, reads.reads[q.idx], q.writes.Load())
	}
	return
}

func newMQBridge() *bridge.Bridge {
	_, nodeCIDR, _ := net.ParseCIDR("10.99.0.0/24")
	_, clusterCIDR, _ := net.ParseCIDR("10.99.0.0/16")
	gwMAC, _ := net.ParseMAC("02:00:00:00:00:01")
	return bridge.NewBridge(bridge.BridgeConfig{GatewayIP: net.ParseIP("10.99.0.1"), GatewayMAC: gwMAC, NodeCIDR: nodeCIDR, ClusterCIDR: clusterCIDR})
}

// newMQPods makes the two pods of a case, a at 10.99.0.2 and b at 10.99.0.3, each with a
// stream for each of its client sockets to the other.
func newMQPods(t *testing.T, c mqCase) (a, b *mqPod) {
	aListen, bListen := [mqStreams]int{5101, 5102, 5103, 5104}, [mqStreams]int{5001, 5002, 5003, 5004}
	aPorts := pickPorts("10.99.0.2", "10.99.0.3", bListen, c.a.used, c.b.used)
	bPorts := pickPorts("10.99.0.3", "10.99.0.2", aListen, c.b.used, c.a.used)
	a = newMQPod(t, "a", "10.99.0.2", "02:00:00:00:00:02", c.a, aListen, aPorts)
	b = newMQPod(t, "b", "10.99.0.3", "02:00:00:00:00:03", c.b, bListen, bPorts)
	return a, b
}

// TestMultiQueueTAPsWithRealKernels runs four TCP streams in each direction between two
// pods, at the same time, through the bridge, whose TAPs have several queues (and, in the
// other cases, one, or several of which plaidd is given only the first, as an older plaidd
// does). Every stream arrives whole and in order. The frames of a pod are written to its
// TAP on all its queues (the ports of the streams are chosen so that the hash of their flows
// spreads them over the queues), read on more than one, and every flow's frames are read on
// one queue (but for the first frames of the pod's stack after the first frame written for
// it, see mqReads): the driver sends the pod's packets out on the queue that the flow's frames
// were last written to.
func TestMultiQueueTAPsWithRealKernels(t *testing.T) {
	if mode := os.Getenv("PLAID_MQ_CHILD"); mode != "" {
		n, _ := strconv.Atoi(mode)
		multiQueueScenario(t, mqCases[n])
		return
	}
	needUserns(t)
	for i, c := range mqCases {
		t.Run(c.name, func(t *testing.T) {
			runInUserns(t, "TestMultiQueueTAPsWithRealKernels", "PLAID_MQ_CHILD="+strconv.Itoa(i), 3*time.Minute)
		})
	}
}

func multiQueueScenario(t *testing.T, c mqCase) {
	a, b := newMQPods(t, c)
	br := newMQBridge()
	var reads [2]mqReads
	for i, p := range []*mqPod{a, b} {
		ep := p.endpoint(t, !c.plain)
		if !c.plain {
			ep.SetPipeline()
			ep.SetPassthrough()
		}
		ep.readHook = reads[i].hook
		if err := br.AddEndpoint(ep); err != nil {
			t.Fatal(err)
		}
		ep.StartReadLoop(context.Background(), br)
		defer ep.Close()
	}
	if a.ep.Queues() != c.a.used || b.ep.Queues() != c.b.used {
		t.Fatalf("%d and %d queues, want %d and %d", a.ep.Queues(), b.ep.Queues(), c.a.used, c.b.used)
	}

	type result struct{ sent, got streamResult }
	var res [2][mqStreams]result
	var wg sync.WaitGroup
	for from, pods := range [][2]*mqPod{{a, b}, {b, a}} {
		src, dst := pods[0], pods[1]
		for i := 0; i < mqStreams; i++ {
			wg.Add(2)
			go func() { defer wg.Done(); res[from][i].got = serve(dst.lfds[i]) }()
			go func() {
				defer wg.Done()
				res[from][i].sent = send(src.cfds[i], dst.ip, dst.lports[i])
			}()
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatalf("timed out; counters %+v", Offloads())
	}

	want := fmt.Sprintf("%d bytes, 0 wrong", podBytes)
	for from := range res {
		for i, r := range res[from] {
			if r.sent.err != nil || r.got.err != nil || !strings.HasPrefix(r.got.String(), want) || r.sent.total != podBytes {
				t.Errorf("stream %d from pod %d: sender %+v, receiver %+v", i, from, r.sent, r.got)
			}
		}
	}
	st := Offloads()
	if st.ReadDropped != 0 {
		t.Errorf("%d frames were dropped", st.ReadDropped)
	}
	if !c.plain && (st.ReadSuperFrames+st.PassedSuperFrames == 0) {
		t.Errorf("the offloads were not used: %+v", st)
	}
	for i, p := range []*mqPod{a, b} {
		read, written, counts := queuesUsed(p.ep, &reads[i])
		t.Logf("pod %s, %d queues:%s", p.name, p.ep.Queues(), counts)
		// What a pod is written is spread by the hash of the flows, whose ports were chosen
		// for it: on every queue. What it sends goes out as the driver has been told by the
		// writes: on more than one queue if it has them.
		if written != p.ep.Queues() {
			t.Errorf("pod %s: frames were written to %d of %d queues", p.name, written, p.ep.Queues())
		}
		if p.ep.Queues() > 1 && read < 2 {
			t.Errorf("pod %s: frames were read on %d of %d queues", p.name, read, p.ep.Queues())
		}
		reads[i].mu.Lock()
		most := 0
		for k, queues := range reads[i].flows {
			most = max(most, strays(queues))
			if n := strays(queues); n > mqStrayLimit {
				t.Errorf("pod %s: the frames of flow %v were read on queues %v", p.name, k, queues)
			}
		}
		t.Logf("pod %s: %d flows read, at most %d frames of a flow off its queue", p.name, len(reads[i].flows), most)
		if len(reads[i].flows) < mqStreams {
			t.Errorf("pod %s: %d flows seen, want at least %d", p.name, len(reads[i].flows), mqStreams)
		}
		reads[i].mu.Unlock()
	}
}

// What the kernel's tun driver keeps for the device and what for each queue, which decides
// where CreateAndConfigureTapQueues and NewTapEndpointQueues set it: the flags, the virtio
// header size, the send buffer and the offloads are the device's: set through one queue they
// are those of all, and a queue that is closed leaves the others attached.
func TestMultiQueueTAPSettings(t *testing.T) {
	if os.Getenv("PLAID_MQ_CHILD") == "settings" {
		multiQueueSettings(t)
		return
	}
	needUserns(t)
	runInUserns(t, "TestMultiQueueTAPSettings", "PLAID_MQ_CHILD=settings", 3*time.Minute)
}

func multiQueueSettings(t *testing.T) {
	mac, _ := net.ParseMAC("02:00:00:00:00:03")
	files, err := CreateAndConfigureTapQueues(TapConfig{Name: "pod0", IP: net.ParseIP("10.99.0.3"), Mask: net.CIDRMask(24, 32), MAC: mac, MTU: 1450}, 4)
	if err != nil {
		t.Skipf("cannot create a TAP: %v", err)
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	if len(files) != 4 {
		t.Skipf("%d queues: no multi-queue TAPs here", len(files))
	}
	const tunGetSndBuf, tunSetSndBuf, tunGetVnetHdrSz = 0x800454d3, 0x400454d4, 0x800454d7
	get := func(f *os.File, req uintptr) int32 {
		var v int32
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(&v))); errno != 0 {
			t.Fatalf("ioctl %#x: %v", req, errno)
		}
		return v
	}
	set := func(f *os.File, req uintptr, v int32) {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(&v))); errno != 0 {
			t.Fatalf("ioctl %#x: %v", req, errno)
		}
	}
	for i, f := range files {
		var req ifreq
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), TUNGETIFF, uintptr(unsafe.Pointer(&req))); errno != 0 {
			t.Fatal(errno)
		}
		if req.flags&(IFF_MULTI_QUEUE|IFF_VNET_HDR|IFF_NO_PI|IFF_TAP) != IFF_MULTI_QUEUE|IFF_VNET_HDR|IFF_NO_PI|IFF_TAP {
			t.Errorf("queue %d: flags %#x", i, req.flags)
		}
		if got := get(f, tunGetVnetHdrSz); got != packet.VnetHeaderLen {
			t.Errorf("queue %d: virtio header size %d, want %d", i, got, packet.VnetHeaderLen)
		}
	}
	// Set through the second queue, seen through all.
	set(files[1], tunSetSndBuf, 123456)
	set(files[2], TUNSETVNETHDRSZ, 12)
	for i, f := range files {
		if got := get(f, tunGetSndBuf); got != 123456 {
			t.Errorf("queue %d: send buffer %d after it was set through queue 1", i, got)
		}
		if got := get(f, tunGetVnetHdrSz); got != 12 {
			t.Errorf("queue %d: virtio header size %d after it was set through queue 2", i, got)
		}
	}
	set(files[0], TUNSETVNETHDRSZ, packet.VnetHeaderLen)

	if _, err := exec.LookPath("ethtool"); err == nil {
		features := func() string {
			out, _ := exec.Command("ethtool", "-k", "pod0").CombinedOutput()
			for _, l := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(l, "tcp-segmentation-offload:") {
					return strings.Fields(l)[1]
				}
			}
			return "?"
		}
		if got := features(); got != "off" {
			t.Errorf("TSO is %s before it was asked for", got)
		}
		if err := enableOffloads(files[3]); err != nil {
			t.Fatal(err)
		}
		if got := features(); got != "on" {
			t.Errorf("TSO is %s after queue 3 asked for it: the offloads are not the device's", got)
		}
	}

	// The first queue is not special: the device stays when it is closed, with the others.
	_ = files[0].Close()
	if out, _ := exec.Command("ip", "-d", "link", "show", "pod0").CombinedOutput(); !strings.Contains(string(out), "numqueues 3") {
		t.Errorf("the device after queue 0 was closed: %s", out)
	}
}

// hasTAP reports whether the pod's TAP exists. A TAP that nobody made persistent goes away
// when the last of its queues is closed in the kernel, not merely when its descriptors are.
func (p *mqPod) hasTAP() bool {
	return p.ns.do(func() error { return ipCmd("link", "show", "pod0") }) == nil
}

// Removing a pod while its streams run closes every queue of its TAP and ends every
// goroutine of the endpoint, whichever of the queues have frames coming in on them: with a
// single stream most of the queues have none, and a read of one in the kernel would keep
// the queue, and the TAP and the pod's network namespace with it, if the files were not
// pollable (see FileFromFD).
func TestMultiQueueRemoveWhileTrafficFlows(t *testing.T) {
	if mode := os.Getenv("PLAID_MQ_CHILD"); strings.HasPrefix(mode, "remove") {
		streams, _ := strconv.Atoi(strings.TrimPrefix(mode, "remove"))
		multiQueueRemove(t, streams)
		return
	}
	needUserns(t)
	for _, streams := range []int{mqStreams, 1} {
		t.Run(fmt.Sprintf("%d streams each way", streams), func(t *testing.T) {
			runInUserns(t, "TestMultiQueueRemoveWhileTrafficFlows", "PLAID_MQ_CHILD=remove"+strconv.Itoa(streams), 3*time.Minute)
		})
	}
}

func multiQueueRemove(t *testing.T, streams int) {
	c := mqCase{a: mqSide{4, 4}, b: mqSide{4, 4}}
	a, b := newMQPods(t, c)
	before := goroutines()
	br := newMQBridge()
	var counted [2]mqReads
	for i, p := range []*mqPod{a, b} {
		ep := p.endpoint(t, true)
		ep.SetPipeline()
		ep.SetPassthrough()
		ep.readHook = counted[i].hook
		if err := br.AddEndpoint(ep); err != nil {
			t.Fatal(err)
		}
		ep.StartReadLoop(context.Background(), br)
	}
	if !a.hasTAP() || !b.hasTAP() {
		t.Fatal("the pods have no TAPs")
	}

	// Streams that go on until stopped, with timeouts so that they notice.
	var stop sync.WaitGroup
	var connected atomic.Int32
	var halt = make(chan struct{})
	tv := syscall.NsecToTimeval(int64(100 * time.Millisecond))
	for _, pods := range [][2]*mqPod{{a, b}, {b, a}} {
		src, dst := pods[0], pods[1]
		for i := 0; i < streams; i++ {
			stop.Add(2)
			go func() {
				defer stop.Done()
				conn, _, err := syscall.Accept(dst.lfds[i])
				if err != nil {
					return
				}
				defer syscall.Close(conn)
				connected.Add(1)
				_ = syscall.SetsockoptTimeval(conn, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
				buf := make([]byte, 1<<16)
				for {
					select {
					case <-halt:
						return
					default:
					}
					if n, err := syscall.Read(conn, buf); n == 0 && err == nil {
						return
					}
				}
			}()
			go func() {
				defer stop.Done()
				defer syscall.Close(src.cfds[i])
				if syscall.Connect(src.cfds[i], &syscall.SockaddrInet4{Port: dst.lports[i], Addr: [4]byte(net.ParseIP(dst.ip).To4())}) != nil {
					return
				}
				_ = syscall.SetsockoptTimeval(src.cfds[i], syscall.SOL_SOCKET, syscall.SO_SNDTIMEO, &tv)
				buf := make([]byte, 1<<16)
				for {
					select {
					case <-halt:
						return
					default:
					}
					_, _ = syscall.Write(src.cfds[i], buf)
				}
			}()
		}
	}
	waitUntil(t, "the streams to connect", func() bool { return int(connected.Load()) == 2*streams })
	waitUntil(t, "traffic on both pods", func() bool {
		ra, _, _ := queuesUsed(a.ep, &counted[0])
		rb, _, _ := queuesUsed(b.ep, &counted[1])
		return ra > 0 && rb > 0
	})
	_, _, counts := queuesUsed(a.ep, &counted[0])
	t.Logf("pod a before it is removed:%s", counts)

	if err := br.RemoveEndpoint("a"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the removed pod's TAP to go away", func() bool { return !a.hasTAP() })
	if !b.hasTAP() {
		t.Fatal("the other pod lost its TAP")
	}
	if err := br.RemoveEndpoint("b"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the other TAP to go away", func() bool { return !b.hasTAP() })
	close(halt)
	stop.Wait()
	settle(t, before)
	if n := inFlight.Load(); n != 0 {
		t.Errorf("%d buffers were not released", n)
	}
}
