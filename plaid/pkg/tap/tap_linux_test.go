//go:build linux

package tap

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
)

// tapOpts say how socketTap makes a TAP.
type tapOpts struct {
	name          string           // the endpoint's name and the interface's; "tap" if empty
	id            byte             // the pod is 10.244.1.id with the MAC 02:00:00:00:00:id; 0x10 if 0
	ip            net.IP           // instead of 10.244.1.id
	mac           net.HardwareAddr // instead of 02:00:00:00:00:id
	queues        int              // the TAP's queues, each a socket pair of its own; 1 if 0
	sndbuf        int              // the TAP end's send buffer, 4 MiB if 0: a small one makes writes to the pod wait for it to read
	vnet, offload bool             // the TAP claims a virtio header, and offloads
	pipeline      bool             // the endpoint has a pipeline
}

// socketTap returns an endpoint over one end of a packet socket pair, which behaves like a
// TAP in the one way that matters here: a read or a write is one frame. The other end is the
// "pod". Both ends are non-blocking, so that closing the endpoint wakes its reader as it does
// for a TAP in the poller.
func socketTap(t testing.TB, o tapOpts) (*TapEndpoint, *os.File) {
	t.Helper()
	ep, pods := socketTapQueues(t, o)
	return ep, pods[0]
}

// socketTapQueues is socketTap for a TAP with o.queues queues; the result has the pod's end of
// each.
func socketTapQueues(t testing.TB, o tapOpts) (*TapEndpoint, []*os.File) {
	t.Helper()
	if o.name == "" {
		o.name = "tap"
	}
	if o.id == 0 {
		o.id = 0x10
	}
	if o.ip == nil {
		o.ip = net.IPv4(10, 244, 1, o.id)
	}
	if o.mac == nil {
		o.mac = net.HardwareAddr{2, 0, 0, 0, 0, o.id}
	}
	if o.queues == 0 {
		o.queues = 1
	}
	var taps, pods []*os.File
	for q := 0; q < o.queues; q++ {
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Skipf("no socket pair: %v", err)
		}
		for i, fd := range fds {
			_ = syscall.SetNonblock(fd, true)
			size := 4 << 20
			if i == 0 && o.sndbuf > 0 {
				size = o.sndbuf
			}
			_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, size)
		}
		a, b := os.NewFile(uintptr(fds[0]), o.name), os.NewFile(uintptr(fds[1]), o.name+"-pod")
		t.Cleanup(func() { a.Close(); b.Close() })
		taps, pods = append(taps, a), append(pods, b)
	}
	ep := NewTapEndpointQueues(o.name, o.name, o.ip, o.mac, taps, false)
	if o.vnet {
		for i, q := range ep.queues {
			q.vw = newVnetWriter(taps[i])
		}
		ep.vnet = true
		ep.offload = o.offload
	}
	if o.pipeline {
		ep.SetPipeline()
	}
	return ep, pods
}

// sumFrame makes the checksums of the IPv4 packet ip (no options) right after a test has
// changed it: the header's and, for a TCP segment, the segment's.
func sumFrame(ip []byte) {
	binary.BigEndian.PutUint16(ip[10:], 0)
	binary.BigEndian.PutUint16(ip[10:], packet.Checksum16(ip[:20]))
	if ip[9] != packet.IPProtocolTCP {
		return
	}
	tcp := ip[20:]
	binary.BigEndian.PutUint16(tcp[16:], 0)
	pseudo := append(append([]byte(nil), ip[12:20]...), 0, packet.IPProtocolTCP, byte(len(tcp)>>8), byte(len(tcp)))
	binary.BigEndian.PutUint16(tcp[16:], packet.Checksum16(append(pseudo, tcp...)))
}

// tcpFrame is an Ethernet frame with a TCP segment of n payload bytes at seq, with the
// checksums done; segments of 1398 bytes from 1000 on are consecutive.
func tcpFrame(seq uint32, n int, flags byte) *packet.EthernetFrame {
	ip := make([]byte, 40+n)
	ip[0], ip[8], ip[9] = 0x45, 64, 6
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	binary.BigEndian.PutUint16(ip[4:], uint16(seq/1398)) // consecutive segments have consecutive ids, as a sender's do
	copy(ip[12:], []byte{10, 244, 1, 4, 10, 244, 1, 5})
	tcp := ip[20:]
	binary.BigEndian.PutUint16(tcp[0:], 4000)
	binary.BigEndian.PutUint16(tcp[2:], 80)
	binary.BigEndian.PutUint32(tcp[4:], seq)
	tcp[12], tcp[13] = 5<<4, flags
	binary.BigEndian.PutUint16(tcp[14:], 500)
	for i := range tcp[20:] {
		tcp[20+i] = byte(seq) + byte(i)
	}
	sumFrame(ip)
	return &packet.EthernetFrame{
		DstMAC: net.HardwareAddr{2, 0, 0, 0, 0, 0x10}, SrcMAC: net.HardwareAddr{2, 0, 0, 0, 0, 0x11},
		EtherType: packet.EtherTypeIPv4, Payload: ip,
	}
}

func read(t testing.TB, f *os.File) []byte {
	t.Helper()
	_ = f.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 70000)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}

func TestWriteAddsTheVirtioHeaderOnlyWhereTheTAPHasOne(t *testing.T) {
	frame := bytes.Repeat([]byte{0xab}, 100)
	for _, vnet := range []bool{true, false} {
		ep, pod := socketTap(t, tapOpts{vnet: vnet, offload: false})
		if err := ep.Write(frame); err != nil {
			t.Fatal(err)
		}
		got := read(t, pod)
		want := frame
		if vnet {
			want = append(make([]byte, packet.VnetHeaderLen), frame...)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("vnet=%v: the pod received %d bytes, want %d: a header of zeros and the frame", vnet, len(got), len(want))
		}
	}
}

func TestClosedEndpointRefusesWrites(t *testing.T) {
	ep, _ := socketTap(t, tapOpts{vnet: true, offload: true})
	w := ep.NewWriter()
	if err := w.Add(tcpFrame(1, 100, 0x10)); err != nil {
		t.Fatal(err)
	}
	ep.Close()
	if err := ep.Write([]byte("x")); err == nil {
		t.Error("Write after Close succeeded")
	}
	if err := w.Flush(); err == nil {
		t.Error("a writer flushed to a closed endpoint")
	}
	w.Release()
}

func TestNoWriterWithoutOffloads(t *testing.T) {
	for _, tc := range []struct{ vnet, offload bool }{{false, false}, {true, false}} {
		ep, _ := socketTap(t, tapOpts{vnet: tc.vnet, offload: tc.offload})
		if w := ep.NewWriter(); w != nil {
			t.Errorf("vnet=%v offload=%v: got a writer", tc.vnet, tc.offload)
		}
	}
}

func TestWriterJoinsSegmentsAndWritesOnce(t *testing.T) {
	ep, pod := socketTap(t, tapOpts{vnet: true, offload: true})
	before := Offloads()

	w := ep.NewWriter()
	arp := &packet.EthernetFrame{DstMAC: net.HardwareAddr{2, 0, 0, 0, 0, 0x10}, SrcMAC: net.HardwareAddr{2, 0, 0, 0, 0, 0x11}, EtherType: packet.EtherTypeARP, Payload: make([]byte, 28)}
	var sent []*packet.EthernetFrame
	for i := 0; i < 5; i++ {
		sent = append(sent, tcpFrame(1000+uint32(i)*1398, 1398, 0x10))
	}
	for _, f := range sent {
		if err := w.Add(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Add(arp); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	w.Release()

	super, plain := read(t, pod), read(t, pod)
	h := packet.ParseVnetHeader(super)
	if h.GSOType != packet.VnetGSOTCPv4 || h.GSOSize != 1398 || h.Flags != packet.VnetNeedsCsum || h.CsumStart != 34 || h.CsumOffset != 16 || h.HdrLen != 54 {
		t.Errorf("super-frame header %+v", h)
	}
	if len(super) != packet.VnetHeaderLen+14+40+5*1398 {
		t.Errorf("super-frame of %d bytes", len(super))
	}
	if got := packet.ParseVnetHeader(plain); got != (packet.VnetHeader{}) {
		t.Errorf("the ARP frame has a header %+v", got)
	}
	if !bytes.Equal(plain[packet.VnetHeaderLen:], mustMarshal(arp)) {
		t.Error("the ARP frame was changed")
	}
	if got := Offloads(); got.WriteSuperFrames != before.WriteSuperFrames+1 || got.WriteSegments != before.WriteSegments+5 {
		t.Errorf("counters %+v, before %+v", got, before)
	}

	// The pod's side cuts it again: what comes back is what was sent.
	if _, err := pod.Write(super); err != nil {
		t.Fatal(err)
	}
	frames, err := ep.ReadFrames()
	if err != nil || len(frames) != 5 {
		t.Fatalf("read %d frames, %v", len(frames), err)
	}
	for i, f := range frames {
		if !bytes.Equal(f, mustMarshal(sent[i])) {
			t.Errorf("segment %d differs from the frame that was written", i)
		}
	}
	if got := Offloads(); got.ReadSuperFrames != before.ReadSuperFrames+1 || got.ReadSegments != before.ReadSegments+5 {
		t.Errorf("counters %+v, before %+v", got, before)
	}
}

func mustMarshal(f *packet.EthernetFrame) []byte {
	b, err := f.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

func TestReadFrames(t *testing.T) {
	plain := mustMarshal(tcpFrame(1, 100, 0x10))
	vnetHeader := func(h packet.VnetHeader) []byte {
		b := make([]byte, packet.VnetHeaderLen)
		h.Put(b)
		return b
	}
	for _, tc := range []struct {
		name    string
		vnet    bool
		record  []byte
		frames  int
		dropped uint64
	}{
		{"a TAP without the header", false, plain, 1, 0},
		{"a plain frame", true, append(vnetHeader(packet.VnetHeader{}), plain...), 1, 0},
		{"a frame less than a header", true, plain[:5], 0, 1},
		{"an unsupported GSO type", true, append(vnetHeader(packet.VnetHeader{GSOType: packet.VnetNeedsCsum + 4, GSOSize: 100}), plain...), 0, 1},
		{"a checksum that is out of the frame", true, append(vnetHeader(packet.VnetHeader{Flags: packet.VnetNeedsCsum, CsumStart: 34, CsumOffset: 200}), plain...), 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ep, pod := socketTap(t, tapOpts{vnet: tc.vnet, offload: true})
			before := Offloads()
			if _, err := pod.Write(tc.record); err != nil {
				t.Fatal(err)
			}
			frames, err := ep.ReadFrames()
			if err != nil || len(frames) != tc.frames {
				t.Fatalf("%d frames, %v; want %d", len(frames), err, tc.frames)
			}
			if tc.frames == 1 && !bytes.Equal(frames[0], plain) {
				t.Error("the frame was changed")
			}
			if got := Offloads().ReadDropped - before.ReadDropped; got != tc.dropped {
				t.Errorf("%d dropped, want %d", got, tc.dropped)
			}
		})
	}
}

// Reading with the header, writing a frame with it, writing through a writer and writing
// a TSO segment whole allocate nothing per frame.
func TestEndpointAllocations(t *testing.T) {
	ep, pod := socketTap(t, tapOpts{vnet: true, offload: true})
	frame := mustMarshal(tcpFrame(1, 1398, 0x10))
	buf := make([]byte, 70000)
	drain := func(n int) {
		for i := 0; i < n; i++ {
			if _, err := pod.Read(buf); err != nil {
				t.Fatal(err)
			}
		}
	}

	if got := testing.AllocsPerRun(50, func() { _ = ep.Write(frame); drain(1) }); got != 0 {
		t.Errorf("Write allocates %.0f", got)
	}

	var frames []*packet.EthernetFrame
	for i := 0; i < 40; i++ {
		frames = append(frames, tcpFrame(1000+uint32(i)*1398, 1398, 0x10))
	}
	if got := testing.AllocsPerRun(50, func() {
		w := ep.NewWriter()
		for _, f := range frames {
			_ = w.Add(f)
		}
		_ = w.Flush()
		w.Release()
		drain(1)
	}); got != 0 {
		t.Errorf("a writer allocates %.0f for 40 segments", got)
	}

	rec := podRecord(1000, 40)
	tso, _ := packet.ParseEthernet(rec[packet.VnetHeaderLen:])
	h := packet.ParseVnetHeader(rec)
	if got := testing.AllocsPerRun(50, func() {
		w := ep.NewWriter()
		_ = w.(bridge.SuperWriter).AddSuper(h, tso)
		_ = w.Flush()
		w.Release()
		drain(1)
	}); got != 0 {
		t.Errorf("a writer allocates %.0f for a TSO segment of 40 segments", got)
	}

	// A super-frame to read: write 40 segments, take the record.
	w := ep.NewWriter()
	for _, f := range frames {
		_ = w.Add(f)
	}
	_ = w.Flush()
	w.Release()
	super := read(t, pod)
	if got := testing.AllocsPerRun(50, func() {
		_, _ = pod.Write(super)
		if f, err := ep.ReadFrames(); err != nil || len(f) != 40 {
			t.Fatalf("%d frames, %v", len(f), err)
		}
	}); got != 0 {
		t.Errorf("ReadFrames allocates %.0f for a super-frame of 40 segments", got)
	}
}

// -- two namespaces (see newPod and runStream)

const (
	podPort  = 5001
	podBytes = 16 << 20
)

func pattern(off int) byte { return byte(off % 251) }

// TestOffloadsBetweenTwoPods is the whole path with a real kernel's TCP on both ends. Two
// "pods" live in network namespaces of their own, each with a TAP that Plaid serves, and
// a TCP stream runs from one to the other through the bridge: with offloads the sender's
// stack hands over TSO segments of up to 64 KiB, Plaid cuts them into MTU-sized frames
// for the bridge, and joins the frames that reach the receiver again into segments for its
// stack, or, with passthrough, hands them to the receiver's TAP as they are; without
// offloads, every frame is an MTU-sized one with the virtio header of a TAP that has
// offloads off. The stacks verify every checksum they are given, so a stream that
// arrives whole and in order shows that frames were cut and joined correctly. The test
// runs in a user and network namespace of its own, which needs no privileges. The "ecn"
// modes negotiate ECN in both pods; as nothing marks packets on this path the sender never
// sets CWR, so the stream carries no segment of the ECN type: that case is covered by the
// unit tests, and these modes show that a stream with ECN negotiated is unharmed.
func TestOffloadsBetweenTwoPods(t *testing.T) {
	if os.Getenv("PLAID_TAP_CHILD") != "" {
		mode := os.Getenv("PLAID_TAP_CHILD")
		twoPods(t, strings.HasPrefix(mode, "offload"), strings.Contains(mode, "+pipeline"), strings.Contains(mode, "+passthrough"), strings.Contains(mode, "+ecn"))
		return
	}
	needUserns(t)
	for _, mode := range []string{"offload", "plain", "offload+pipeline", "plain+pipeline", "offload+passthrough", "offload+pipeline+passthrough", "plain+passthrough", "offload+ecn", "offload+pipeline+passthrough+ecn"} {
		t.Run(mode, func(t *testing.T) { runInUserns(t, "TestOffloadsBetweenTwoPods", "PLAID_TAP_CHILD="+mode, time.Minute) })
	}
}

func twoPods(t *testing.T, offload, pipeline, passthrough, ecn bool) {
	podCfg := func(n byte) TapConfig {
		return TapConfig{Name: "pod0", IP: net.IPv4(10, 99, 0, n), Mask: net.CIDRMask(24, 32), MAC: net.HardwareAddr{2, 0, 0, 0, 0, n}, MTU: 1450}
	}
	server, client := newPod(t, podCfg(3), ecn), newPod(t, podCfg(2), ecn)

	_, nodeCIDR, _ := net.ParseCIDR("10.99.0.0/24")
	_, clusterCIDR, _ := net.ParseCIDR("10.99.0.0/16")
	gwMAC, _ := net.ParseMAC("02:00:00:00:00:01")
	b := bridge.NewBridge(bridge.BridgeConfig{GatewayIP: net.ParseIP("10.99.0.1"), GatewayMAC: gwMAC, NodeCIDR: nodeCIDR, ClusterCIDR: clusterCIDR})

	for _, spec := range []struct {
		id  string
		n   byte
		pod *nsPod
	}{{"server", 3, server}, {"client", 2, client}} {
		cfg := podCfg(spec.n)
		ep := NewTapEndpoint(spec.id, spec.id, cfg.IP, cfg.MAC, spec.pod.tap, offload)
		if !ep.vnet || ep.Offload() != offload {
			t.Skipf("this kernel does not give the TAP a virtio header or offloads (vnet %v, offload %v)", ep.vnet, ep.Offload())
		}
		if pipeline {
			ep.SetPipeline()
		}
		if passthrough {
			ep.SetPassthrough()
		}
		if err := b.AddEndpoint(ep); err != nil {
			t.Fatal(err)
		}
		ep.StartReadLoop(context.Background(), b)
	}

	runStream(t, "stream between the pods",
		func() streamResult { return serve(server.lfd) },
		func() streamResult { return send(client.cfd, "10.99.0.3", podPort) },
		func() string { return fmt.Sprintf("counters %+v", Offloads()) })
	st := Offloads()
	t.Logf("TAP offloads: %+v", st)
	switch {
	case !offload && (st.ReadSuperFrames != 0 || st.WriteSuperFrames != 0 || st.PassedSuperFrames != 0):
		t.Errorf("offloads were used though they are off: %+v", st)
	case offload && passthrough:
		// The sender's TSO segments go to the receiver as they are; the few that are cut
		// are those the stacks make before the stream has found its size.
		if st.PassedSuperFrames == 0 || st.PassedBytes < podBytes/2 || st.ReadSuperFrames > st.PassedSuperFrames/10 {
			t.Errorf("the sender's TSO segments were not passed through: %+v", st)
		}
	case offload && (st.ReadSuperFrames == 0 || st.ReadSegments <= 2*st.ReadSuperFrames):
		t.Errorf("the sender's stack handed over no TSO segments: %+v", st)
	case offload && (st.WriteSuperFrames == 0 || st.WriteSegments <= 2*st.WriteSuperFrames):
		t.Errorf("no frames were joined for the receiver: %+v", st)
	case offload && st.PassedSuperFrames != 0:
		t.Errorf("TSO segments were passed through though passthrough is off: %+v", st)
	}
	if st.ReadDropped != 0 { // with ecn too: a TSO segment with CWR has the ECN type, which is cut like the others
		t.Errorf("%d frames were dropped", st.ReadDropped)
	}
}

// What it costs to deliver frames to a pod does not grow with the offloads: a batch of
// frames injected into a bridge allocates no more when the pod's TAP joins them than when
// it takes them one by one.
func TestBridgeDeliveryAllocations(t *testing.T) {
	mac := net.HardwareAddr{2, 0, 0, 0, 0, 0x10}
	ext := net.HardwareAddr{2, 0xaa, 0, 0, 0, 1}
	var frames []*packet.EthernetFrame
	for i := 0; i < 40; i++ {
		f := tcpFrame(1000+uint32(i)*1398, 1398, 0x10)
		f.DstMAC, f.SrcMAC = mac, ext
		frames = append(frames, f)
	}
	measure := func(offload bool, writes int) float64 {
		ep, pod := socketTap(t, tapOpts{vnet: true, offload: offload})
		b := bridge.NewBridge(bridge.BridgeConfig{})
		_ = b.AddEndpoint(ep)
		buf := make([]byte, 70000)
		x := b.NewBatch()
		defer x.Release()
		return testing.AllocsPerRun(50, func() {
			for _, f := range frames {
				_ = x.InjectFrame(f)
			}
			_ = x.Flush()
			for i := 0; i < writes; i++ { // what the pod's side reads, so that its buffer does not fill
				if _, err := pod.Read(buf); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	plain, joined := measure(false, 40), measure(true, 1)
	t.Logf("allocations for 40 frames: one by one %.0f, joined %.0f", plain, joined)
	if joined > plain {
		t.Errorf("joining costs %.0f allocations for 40 frames, one by one %.0f", joined, plain)
	}
}
