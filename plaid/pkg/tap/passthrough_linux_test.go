//go:build linux

package tap

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
)

var (
	gatewayMAC = net.HardwareAddr{2, 0, 0, 0, 0, 1}
	tapModeCfg = bridge.BridgeConfig{
		GatewayMode: "tap", GatewayIP: net.IPv4(10, 244, 1, 1), GatewayMAC: gatewayMAC,
		NodeCIDR:    &net.IPNet{IP: net.IPv4(10, 244, 1, 0), Mask: net.CIDRMask(24, 32)},
		ClusterCIDR: &net.IPNet{IP: net.IPv4(10, 244, 0, 0), Mask: net.CIDRMask(16, 32)},
	}
)

// overlayFunc is an OverlayBatch that gives every frame to fn at once.
type overlayFunc func(*packet.EthernetFrame, net.IP) error

func (f overlayFunc) Send(fr *packet.EthernetFrame, dst net.IP) error { return f(fr, dst) }
func (overlayFunc) Flush() error                                      { return nil }
func (overlayFunc) Release()                                          {}

// setOverlayFunc makes fn the overlay of br.
func setOverlayFunc(br *bridge.Bridge, fn func(*packet.EthernetFrame, net.IP) error) {
	br.SetOverlayBatcher(func() bridge.OverlayBatch { return overlayFunc(fn) })
}

// passOpts say how passTaps makes its pods.
type passOpts struct {
	pipeline, passthrough bool
	cfg                   bridge.BridgeConfig
	sndbuf                int  // the send buffer of the TAP end of "b"
	plainB                bool // "b" has no offloads
}

// passTaps is a bridge with the pods 10.244.1.4 (the sender, "a") and 10.244.1.5 ("b"), whose
// read loops are not started (start does it): the pods can send before they are, so that
// frames are waiting when the first read comes.
func passTaps(t testing.TB, o passOpts) (a, b *TapEndpoint, aPod, bPod *os.File, br *bridge.Bridge, start func()) {
	t.Helper()
	br = bridge.NewBridge(o.cfg)
	a, aPod = pipeTap(t, "a", 4, o.pipeline, 0)
	b, bPod = socketTap(t, tapOpts{name: "b", id: 5, sndbuf: o.sndbuf, vnet: true, offload: !o.plainB, pipeline: o.pipeline})
	for _, ep := range []*TapEndpoint{a, b} {
		if o.passthrough {
			ep.SetPassthrough()
		}
		if err := br.AddEndpoint(ep); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	start = func() {
		for _, ep := range []*TapEndpoint{a, b} {
			ep.StartReadLoop(context.Background(), br)
		}
	}
	return
}

// podRecordTo is podRecord for a stream to dstIP, whose frames are addressed to dstMAC.
func podRecordTo(seq uint32, n int, dstMAC net.HardwareAddr, dstIP net.IP) []byte {
	return record(n, func(i int) *packet.EthernetFrame {
		f := flowFrame(seq+uint32(i*segSize), false)
		f.DstMAC = dstMAC
		copy(f.Payload[16:20], dstIP.To4())
		sumFrame(f.Payload)
		return f
	})
}

var podB = net.IPv4(10, 244, 1, 5)

// readQuiet reads the records that arrive until none does for a while.
func readQuiet(pod *os.File) [][]byte {
	var recs [][]byte
	buf := make([]byte, 70000)
	for {
		_ = pod.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := pod.Read(buf)
		if err != nil {
			return recs
		}
		recs = append(recs, append([]byte(nil), buf[:n]...))
	}
}

func delta(after, before OffloadStats) OffloadStats {
	return OffloadStats{
		ReadSuperFrames: after.ReadSuperFrames - before.ReadSuperFrames, ReadSegments: after.ReadSegments - before.ReadSegments,
		ReadDropped: after.ReadDropped - before.ReadDropped, WriteSuperFrames: after.WriteSuperFrames - before.WriteSuperFrames,
		WriteSegments: after.WriteSegments - before.WriteSegments, PassedSuperFrames: after.PassedSuperFrames - before.PassedSuperFrames,
		PassedBytes: after.PassedBytes - before.PassedBytes, JoinedForUplink: after.JoinedForUplink - before.JoinedForUplink,
		UplinkSuperFrames: after.UplinkSuperFrames - before.UplinkSuperFrames, MSSRaised: after.MSSRaised - before.MSSRaised,
		MSSLowered: after.MSSLowered - before.MSSLowered, Fragmented: after.Fragmented - before.Fragmented,
		Fragments: after.Fragments - before.Fragments, OversizeDropped: after.OversizeDropped - before.OversizeDropped,
	}
}

// A TSO segment goes to the other pod as it is, with the pod's header and the MAC
// addresses the bridge sets, written in place by the delivering goroutine; with the ECN bit
// too.
func TestPassthroughWritesTheSuperFrameWhole(t *testing.T) {
	for _, pipeline := range []bool{false, true} {
		for _, ecn := range []bool{false, true} {
			t.Run(fmt.Sprintf("pipeline=%v/ecn=%v", pipeline, ecn), func(t *testing.T) {
				_, _, aPod, bPod, _, start := passTaps(t, passOpts{pipeline: pipeline, passthrough: true, cfg: tapModeCfg})
				start()
				rec := podRecordTo(1000, 11, gatewayMAC, podB)
				want := append([]byte(nil), rec...)
				copy(want[packet.VnetHeaderLen:], []byte{2, 0, 0, 0, 0, 5}) // the destination's MAC, set by the bridge
				if ecn {
					rec[1] |= packet.VnetGSOECN
					want[1] |= packet.VnetGSOECN
				}
				before, pipes := Offloads(), Pipeline()
				if _, err := aPod.Write(rec); err != nil {
					t.Fatal(err)
				}
				got := readQuiet(bPod)
				if len(got) != 1 || !bytes.Equal(got[0], want) {
					t.Fatalf("the pod got %d records; want one, the one sent with its MACs rewritten", len(got))
				}
				d := delta(Offloads(), before)
				if d.PassedSuperFrames != 1 || d.PassedBytes != uint64(len(rec)-packet.VnetHeaderLen) || d.ReadSuperFrames != 0 || d.ReadSegments != 0 || d.WriteSuperFrames != 0 {
					t.Errorf("counters %+v", d)
				}
				if p := Pipeline(); pipeline && p.WriteInline-pipes.WriteInline != 1 || p.WriteQueued != pipes.WriteQueued {
					t.Errorf("the write was not made in place: %+v -> %+v", pipes, p)
				}
			})
		}
	}
}

// The bubble's TAP, when it has offloads, takes TSO segments whole.
func TestPassthroughWritesTheSuperFrameToTheBubble(t *testing.T) {
	_, _, aPod, _, br, start := passTaps(t, passOpts{pipeline: true, passthrough: true, cfg: tapModeCfg})
	bubble, bubblePod := pipeTap(t, "bubble", 9, false, 0)
	br.SetBubbleEndpoint(bubble)
	start()
	rec := podRecordTo(1000, 11, gatewayMAC, net.IPv4(8, 8, 8, 8))
	want := append([]byte(nil), rec...)
	copy(want[packet.VnetHeaderLen:], bubble.MAC())
	if _, err := aPod.Write(rec); err != nil {
		t.Fatal(err)
	}
	if got := readQuiet(bubblePod); len(got) != 1 || !bytes.Equal(got[0], want) {
		t.Fatalf("the bubble got %d records, or not the one sent", len(got))
	}
}

func TestPassthroughNeedsOffloads(t *testing.T) {
	ep, _ := socketTap(t, tapOpts{vnet: true, offload: false})
	ep.SetPassthrough()
	if ep.passthrough {
		t.Error("an endpoint without offloads passes TSO segments through")
	}
	ep, _ = socketTap(t, tapOpts{vnet: true, offload: true})
	ep.SetPassthrough()
	if !ep.passthrough {
		t.Error("an endpoint with offloads does not")
	}
}

// Wherever a TSO segment cannot go whole, what happens with it is what happens without
// passthrough: the same segments, the same counters.
func TestPassthroughLeavesTheRestAsItWas(t *testing.T) {
	type outcome struct {
		records [][]byte // what the pod "b" was written
		overlay [][]byte // what went over the overlay
		st      OffloadStats
	}
	run := func(t *testing.T, o passOpts, rec []byte) outcome {
		_, _, aPod, bPod, br, start := passTaps(t, o)
		var mu sync.Mutex
		var out outcome
		setOverlayFunc(br, func(f *packet.EthernetFrame, _ net.IP) error {
			mu.Lock()
			out.overlay = append(out.overlay, mustMarshal(f))
			mu.Unlock()
			return nil
		})
		br.SetRouteChecker(func(ip net.IP) bool { return ip.Equal(net.IPv4(10, 244, 2, 4)) })
		start()
		before := Offloads()
		if _, err := aPod.Write(rec); err != nil {
			t.Fatal(err)
		}
		out.records = readQuiet(bPod)
		mu.Lock()
		defer mu.Unlock()
		out.st = delta(Offloads(), before)
		return out
	}
	udpGSO := podRecordTo(1000, 11, gatewayMAC, podB)
	udpGSO[1] = 3 // VIRTIO_NET_HDR_GSO_UDP: not TCP
	badCsum := podRecordTo(1000, 11, gatewayMAC, podB)
	packet.VnetHeader{Flags: packet.VnetNeedsCsum, GSOType: packet.VnetGSOTCPv4, HdrLen: 54, GSOSize: segSize, CsumStart: 20, CsumOffset: 16}.Put(badCsum)
	noCsum := podRecordTo(1000, 11, gatewayMAC, podB)
	noCsum[0] = 0 // no NEEDS_CSUM
	ecnToRemote := podRecordTo(1000, 11, gatewayMAC, net.IPv4(10, 244, 2, 4))
	ecnToRemote[1] |= packet.VnetGSOECN // cut like any TSO segment

	for _, tc := range []struct {
		name   string
		cfg    bridge.BridgeConfig
		plainB bool // the pod "b" has no offloads
		rec    []byte
		whole  bool // goes whole with passthrough, so that the outcomes differ
	}{
		{"a pod", tapModeCfg, false, podRecordTo(1000, 11, gatewayMAC, podB), true},
		{"a pod without offloads", tapModeCfg, true, podRecordTo(1000, 11, gatewayMAC, podB), false},
		{"a remote pod", tapModeCfg, false, podRecordTo(1000, 11, gatewayMAC, net.IPv4(10, 244, 2, 4)), false},
		{"a remote pod, ECN type", tapModeCfg, false, ecnToRemote, false},
		{"the sender", tapModeCfg, false, podRecordTo(1000, 11, gatewayMAC, net.IPv4(10, 244, 1, 4)), false},
		{"an unused address", tapModeCfg, false, podRecordTo(1000, 11, gatewayMAC, net.IPv4(10, 244, 1, 77)), false},
		{"a flood", bridge.BridgeConfig{}, false, podRecordTo(1000, 11, net.HardwareAddr{2, 0, 0, 0, 0xee, 1}, net.IPv4(10, 244, 1, 77)), false},
		{"multicast", bridge.BridgeConfig{}, false, podRecordTo(1000, 11, net.HardwareAddr{1, 0, 0x5e, 0, 0, 1}, podB), false},
		{"a GSO type that is not TCP", tapModeCfg, false, udpGSO, false},
		{"a header that does not describe the frame", tapModeCfg, false, badCsum, false},
		{"a TSO segment with no checksum to do", tapModeCfg, false, noCsum, false},
	} {
		for _, pipeline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pipeline=%v", tc.name, pipeline), func(t *testing.T) {
				on := run(t, passOpts{pipeline: pipeline, passthrough: true, cfg: tc.cfg, plainB: tc.plainB}, tc.rec)
				off := run(t, passOpts{pipeline: pipeline, cfg: tc.cfg, plainB: tc.plainB}, tc.rec)
				if tc.whole {
					if on.st.PassedSuperFrames != 1 || len(on.records) != 1 {
						t.Errorf("passthrough on: %+v, %d records", on.st, len(on.records))
					}
					// The segments, joined again, are the segment that was sent.
					if !reflect.DeepEqual(on.records, off.records) {
						t.Error("the pod got another record than when the segment is cut and joined")
					}
					return
				}
				if on.st.PassedSuperFrames != 0 || !reflect.DeepEqual(on, off) {
					t.Errorf("passthrough on: %d records, %d overlay frames, %+v; off: %d, %d, %+v",
						len(on.records), len(on.overlay), on.st, len(off.records), len(off.overlay), off.st)
				}
				if strings.HasPrefix(tc.name, "a remote pod") && (len(on.overlay) != 11 || on.st.ReadDropped != 0) {
					t.Errorf("%d frames over the overlay, %d dropped; want 11 and none", len(on.overlay), on.st.ReadDropped)
				}
			})
		}
	}
}

// modalEndpoint is a TAP whose writer takes TSO segments whole only when refuse is false,
// so that a test can choose, frame by frame, whether the bridge delivers one whole or
// cuts it.
type modalEndpoint struct {
	*TapEndpoint
	refuse atomic.Bool
}

type withoutSuper struct{ bridge.FrameWriter }

func (m *modalEndpoint) NewWriter() bridge.FrameWriter {
	w := m.TapEndpoint.NewWriter()
	if w != nil && m.refuse.Load() {
		return withoutSuper{w}
	}
	return w
}

// A stream to one pod of single frames, TSO segments that go whole, and TSO segments that
// are cut and joined (the bridge asked for them to go whole and then could not deliver
// them so) arrives in order.
func TestPassthroughKeepsOrderWithCutFrames(t *testing.T) {
	for _, pipeline := range []bool{false, true} {
		t.Run(fmt.Sprintf("pipeline=%v", pipeline), func(t *testing.T) {
			br := bridge.NewBridge(bridge.BridgeConfig{})
			a, aPod := pipeTap(t, "a", 4, pipeline, 0)
			b, bPod := pipeTap(t, "b", 5, pipeline, 0)
			a.SetPassthrough()
			m := &modalEndpoint{TapEndpoint: b}
			_ = br.AddEndpoint(a)
			_ = br.AddEndpoint(m)
			t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
			// The filter runs when the bridge handles a frame, before it routes it: it sets
			// whether the destination takes this one whole. The big ones it refuses.
			br.SetFilterHandler(func(f *packet.EthernetFrame, src bridge.Endpoint) bool {
				if src == bridge.Endpoint(a) {
					m.refuse.Store(len(f.Payload) > 20000)
				}
				return true
			})
			a.StartReadLoop(context.Background(), br)
			b.StartReadLoop(context.Background(), br)

			sizes := []int{1, 11, 21, 5, 1, 1, 21, 11}
			const rounds = 60
			total, groups := 0, 0
			for r := 0; r < rounds; r++ {
				for _, n := range sizes {
					total += n
					groups++
				}
			}
			go func() {
				seq := uint32(1000)
				for g := 0; g < groups; g++ {
					n := sizes[g%len(sizes)]
					if _, err := aPod.Write(podRecord(seq, n)); err != nil {
						return
					}
					seq += uint32(n * segSize)
					if g%7 == 0 {
						time.Sleep(200 * time.Microsecond)
					}
				}
			}()
			before := Offloads()
			readSegments(t, bPod, 1000, total, false)
			d := delta(Offloads(), before)
			t.Logf("%d segments: %d TSO segments went whole, %d were cut", total, d.PassedSuperFrames, d.ReadSuperFrames)
			if d.PassedSuperFrames == 0 || d.ReadSuperFrames == 0 {
				t.Errorf("the test did not mix whole and cut TSO segments: %+v", d)
			}
		})
	}
}

// fillTAP writes records of the size of a frame to the TAP until it takes no more, and
// returns how many it took. Nothing reads the pod's end meanwhile, so every later write
// to the TAP, by whoever, waits until the test reads.
func fillTAP(t *testing.T, ep *TapEndpoint) int {
	t.Helper()
	rc, err := ep.file.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	rec := append(make([]byte, packet.VnetHeaderLen), mustMarshal(flowFrame(0, true))...)
	for n := 0; ; n++ {
		var errno error
		if err := rc.Write(func(fd uintptr) bool { _, errno = syscall.Write(int(fd), rec); return true }); err != nil {
			t.Fatal(err)
		}
		switch errno {
		case nil:
		case syscall.EAGAIN:
			return n
		default:
			t.Fatal(errno)
		}
	}
}

// While the writer goroutine is busy with earlier writes, a TSO segment is copied and
// queued behind them, not written ahead of them. The TAP is full, so that the writer is
// stuck with the first of them until the test reads, however the goroutines are scheduled.
func TestWholeSuperFrameQueuesBehindWrites(t *testing.T) {
	_, b, _, bPod, br, start := passTaps(t, passOpts{pipeline: true, passthrough: true, sndbuf: 16 << 10})
	start()
	filler := fillTAP(t, b)
	const frames = 8 // fewer than the queue holds
	x := br.NewBatch()
	for i := 0; i < frames; i++ { // as the overlay's worker delivers
		_ = x.InjectFrame(flowFrame(1000+uint32(i*segSize), true))
	}
	_ = x.Flush() // every write is queued: the writer has not finished the first
	x.Release()
	if got := b.wpending.Load(); got != frames {
		t.Fatalf("%d writes with the writer, want %d", got, frames)
	}
	a, _ := br.GetEndpoint("a")
	before, pipes := Offloads(), Pipeline()
	rec := podRecord(1000+frames*segSize, 11)
	whole, err := br.ProcessSuperFrame(a, packet.ParseVnetHeader(rec), rec[packet.VnetHeaderLen:])
	if !whole || err != nil {
		t.Fatalf("whole=%v, %v", whole, err)
	}
	if p := Pipeline(); p.WriteQueued != pipes.WriteQueued+1 || p.WriteInline != pipes.WriteInline {
		t.Errorf("the write was not queued: %+v -> %+v", pipes, p)
	}
	if d := delta(Offloads(), before); d.PassedSuperFrames != 1 {
		t.Errorf("counters %+v", d)
	}
	buf := make([]byte, 70000)
	for i := 0; i < filler; i++ {
		_ = bPod.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := bPod.Read(buf); err != nil {
			t.Fatalf("filler record %d of %d: %v", i, filler, err)
		}
	}
	readSegments(t, bPod, 1000, frames+11, true)
}

// Passing a TSO segment through allocates the same however many segments it stands for,
// and less than cutting it.
func TestPassthroughAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("pools drop buffers at random under the race detector")
	}
	measure := func(passthrough bool, n int) float64 {
		a, _, aPod, bPod, br, _ := passTaps(t, passOpts{passthrough: passthrough, cfg: tapModeCfg})
		rec, buf := podRecordTo(1000, n, gatewayMAC, podB), make([]byte, 70000)
		run := func() {
			if _, err := aPod.Write(rec); err != nil {
				t.Fatal(err)
			}
			frames, err := a.readFrames(&a.rb, br)
			if err != nil {
				t.Fatal(err)
			}
			a.rb.frames = frames
			a.forward(br, &a.rb)
			if _, err := bPod.Read(buf); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 20; i++ {
			run()
		}
		return testing.AllocsPerRun(50, run)
	}
	few, many, cut := measure(true, 3), measure(true, 40), measure(false, 40)
	t.Logf("allocations for a TSO segment through the bridge: whole, of 3 segments %.0f, of 40 %.0f; cut and joined, of 40 %.0f", few, many, cut)
	if few != many || many >= cut {
		t.Errorf("whole: %.0f for 3 segments, %.0f for 40; cut: %.0f for 40", few, many, cut)
	}
}

// BenchmarkPassthrough is BenchmarkBridgeThroughput with and without passthrough: what a
// same-node stream costs per TSO segment.
func BenchmarkPassthrough(b *testing.B) {
	for _, pipeline := range []bool{false, true} {
		for _, passthrough := range []bool{false, true} {
			b.Run(fmt.Sprintf("pipeline=%v/passthrough=%v", pipeline, passthrough), func(b *testing.B) {
				_, _, aPod, bPod, _, start := passTaps(b, passOpts{pipeline: pipeline, passthrough: passthrough})
				start()
				rec := podRecord(1000, 40)
				go func() {
					for {
						if _, err := aPod.Write(rec); err != nil {
							return
						}
					}
				}()
				buf := make([]byte, 70000)
				b.SetBytes(int64(len(rec)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := bPod.Read(buf); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				_ = aPod.Close()
			})
		}
	}
}
