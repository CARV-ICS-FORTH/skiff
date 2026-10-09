package bridge

import (
	"bytes"
	"fmt"
	"net"
	"testing"

	"plaid/pkg/packet"
)

// batchEndpoint is a mock endpoint that takes frames in batches: its writer records what it
// is given and writes it, as a list of events, when flushed.
type batchEndpoint struct {
	*mockEndpoint
	noWriter bool
	events   []string // "add:<first payload byte>", "flush"
	flushed  [][]byte // what flushes wrote
	adds     [][]byte
	released int
}

func (e *batchEndpoint) NewWriter() FrameWriter {
	if e.noWriter {
		return nil
	}
	return &recordingWriter{e: e}
}

type recordingWriter struct {
	e    *batchEndpoint
	held [][]byte
}

func (w *recordingWriter) Add(f *packet.EthernetFrame) error {
	b, _ := f.Marshal()
	w.held = append(w.held, b)
	w.e.adds = append(w.e.adds, b)
	w.e.events = append(w.e.events, fmt.Sprintf("add:%d", f.Payload[0]))
	return nil
}

func (w *recordingWriter) Flush() error {
	w.e.events = append(w.e.events, "flush")
	w.e.flushed = append(w.e.flushed, w.held...)
	w.held = nil
	return nil
}

func (w *recordingWriter) Release() { w.e.released++ }

// recordingOverlay is an OverlayBatch that records the sends it is given.
type recordingOverlay struct {
	sends   []string
	flushes int
	freed   bool
}

func (o *recordingOverlay) Send(f *packet.EthernetFrame, dst net.IP) error {
	o.sends = append(o.sends, fmt.Sprintf("%s:%d", dst, f.Payload[20])) // the tag after the IP header
	return nil
}
func (o *recordingOverlay) Flush() error { o.flushes++; return nil }
func (o *recordingOverlay) Release()     { o.freed = true }

// funcOverlay is an OverlayBatch that gives every frame to fn at once, for the tests that
// only want to see what goes to the overlay.
type funcOverlay func(*packet.EthernetFrame, net.IP) error

func (f funcOverlay) Send(fr *packet.EthernetFrame, dst net.IP) error { return f(fr, dst) }
func (funcOverlay) Flush() error                                      { return nil }
func (funcOverlay) Release()                                          {}

// setOverlayFunc makes fn the overlay of b.
func setOverlayFunc(b *Bridge, fn func(*packet.EthernetFrame, net.IP) error) {
	b.SetOverlayBatcher(func() OverlayBatch { return funcOverlay(fn) })
}

func batchFixture() (*Bridge, *mockEndpoint, *batchEndpoint) {
	b, pod1, _ := setupTestBridge()
	pod3 := &batchEndpoint{mockEndpoint: newMockEndpoint("pod3", "tap3", "10.244.1.4", "02:00:00:00:00:04")}
	_ = b.AddEndpoint(pod3)
	return b, pod1, pod3
}

func unicast(src, dst net.HardwareAddr, tag byte) []byte {
	f := &packet.EthernetFrame{DstMAC: dst, SrcMAC: src, EtherType: packet.EtherTypeIPv4, Payload: []byte{tag, 2, 3, 4}}
	raw, _ := f.Marshal()
	return raw
}

func TestProcessFramesHoldsDeliveriesUntilTheEnd(t *testing.T) {
	b, pod1, pod3 := batchFixture()
	frames := [][]byte{unicast(pod1.MAC(), pod3.MAC(), 1), unicast(pod1.MAC(), pod3.MAC(), 2), unicast(pod1.MAC(), pod3.MAC(), 3)}
	if err := b.ProcessFrames(pod1, frames); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(pod3.events), "[add:1 add:2 add:3 flush]"; got != want {
		t.Errorf("the endpoint saw %s, want %s: one flush, after the frames, in order", got, want)
	}
	if pod3.ReceivedCount() != 0 || pod3.released != 1 {
		t.Errorf("%d frames went through Write, writer released %d times", pod3.ReceivedCount(), pod3.released)
	}
	for i, f := range pod3.flushed {
		if !bytes.Equal(f, frames[i]) {
			t.Errorf("frame %d differs from what was sent", i)
		}
	}
}

func TestProcessFrameFlushesBeforeItReturns(t *testing.T) {
	b, pod1, pod3 := batchFixture()
	if err := b.ProcessFrame(pod1, unicast(pod1.MAC(), pod3.MAC(), 9)); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(pod3.events); got != "[add:9 flush]" {
		t.Errorf("events %s", got)
	}
}

func TestEndpointWithoutWriterGetsPlainWrites(t *testing.T) {
	b, pod1, pod3 := batchFixture()
	pod3.noWriter = true
	raw := unicast(pod1.MAC(), pod3.MAC(), 5)
	if err := b.ProcessFrames(pod1, [][]byte{raw, raw}); err != nil {
		t.Fatal(err)
	}
	if pod3.ReceivedCount() != 2 || len(pod3.events) != 0 {
		t.Errorf("%d writes, events %v", pod3.ReceivedCount(), pod3.events)
	}
	if !bytes.Equal(pod3.LastPacket(), raw) {
		t.Error("the frame was not passed as it arrived")
	}
}

func TestBatchOrderPerDestination(t *testing.T) {
	b, pod1, pod3 := batchFixture()
	pod2, _ := b.GetEndpoint("pod2")
	var frames [][]byte
	for i := byte(0); i < 6; i++ {
		dst := pod3.MAC()
		if i%2 == 1 {
			dst = pod2.MAC()
		}
		frames = append(frames, unicast(pod1.MAC(), dst, i))
	}
	if err := b.ProcessFrames(pod1, frames); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(pod3.events); got != "[add:0 add:2 add:4 flush]" {
		t.Errorf("batch endpoint saw %s", got)
	}
	if n := pod2.(*mockEndpoint).ReceivedCount(); n != 3 {
		t.Errorf("the plain endpoint got %d frames", n)
	}
	for i, tag := range []byte{1, 3, 5} {
		if p := pod2.(*mockEndpoint).packets[i][14]; p != tag {
			t.Errorf("plain endpoint frame %d has tag %d, want %d", i, p, tag)
		}
	}
}

func TestOverlayFramesAreBatched(t *testing.T) {
	b, pod1, _ := batchFixture()
	ov := &recordingOverlay{}
	made := 0
	b.SetOverlayBatcher(func() OverlayBatch { made++; return ov })

	toRemote := func(tag byte, dst byte) []byte {
		f := &packet.EthernetFrame{DstMAC: b.gatewayMAC, SrcMAC: pod1.MAC(), EtherType: packet.EtherTypeIPv4,
			Payload: []byte{0x45, 0, 0, 24, 0, 0, 0x40, 0, 64, 17, 0, 0, 10, 244, 1, 2, 10, 244, 2, dst, tag, 2, 3, 4}}
		raw, _ := f.Marshal()
		return raw
	}
	if err := b.ProcessFrames(pod1, [][]byte{toRemote(1, 5), toRemote(2, 5), toRemote(3, 6)}); err != nil {
		t.Fatal(err)
	}
	if made != 1 || ov.flushes != 1 || !ov.freed {
		t.Errorf("%d batches made, %d flushes, released %v", made, ov.flushes, ov.freed)
	}
	if got := fmt.Sprint(ov.sends); got != "[10.244.2.5:1 10.244.2.5:2 10.244.2.6:3]" {
		t.Errorf("sends %s", got)
	}
}

func TestInjectBatchHoldsUntilFlush(t *testing.T) {
	b, _, pod3 := batchFixture()
	ext, _ := net.ParseMAC("02:aa:00:00:00:01")
	x := b.NewBatch()
	defer x.Release()
	for tag := byte(1); tag <= 3; tag++ {
		f := &packet.EthernetFrame{DstMAC: pod3.MAC(), SrcMAC: ext, EtherType: packet.EtherTypeIPv4, Payload: []byte{tag, 2, 3, 4}}
		if err := x.InjectFrame(f); err != nil {
			t.Fatal(err)
		}
	}
	if got := fmt.Sprint(pod3.events); got != "[add:1 add:2 add:3]" || len(pod3.flushed) != 0 {
		t.Fatalf("before Flush: events %s, %d frames written", got, len(pod3.flushed))
	}
	if err := x.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(pod3.flushed) != 3 || pod3.released != 1 {
		t.Errorf("after Flush: %d frames written, writer released %d times", len(pod3.flushed), pod3.released)
	}
	// The batch can be used again.
	f := &packet.EthernetFrame{DstMAC: pod3.MAC(), SrcMAC: ext, EtherType: packet.EtherTypeIPv4, Payload: []byte{4, 2, 3, 4}}
	if err := x.InjectFrame(f); err != nil {
		t.Fatal(err)
	}
	_ = x.Flush()
	if len(pod3.flushed) != 4 {
		t.Errorf("%d frames written after reuse", len(pod3.flushed))
	}
}

// Batching adds no allocation per frame to what routing a frame costs without it.
func TestBatchAllocations(t *testing.T) {
	b, pod1, pod3 := batchFixture()
	pod3.noWriter = true // nothing to record: the plain path, then the batched one
	raw := unicast(pod1.MAC(), pod3.MAC(), 1)
	frames := [][]byte{raw, raw, raw, raw}
	plain := testing.AllocsPerRun(50, func() {
		for _, f := range frames {
			_ = b.ProcessFrame(pod1, f)
		}
		pod3.packets = pod3.packets[:0]
	})
	batched := testing.AllocsPerRun(50, func() {
		_ = b.ProcessFrames(pod1, frames)
		pod3.packets = pod3.packets[:0]
	})
	t.Logf("allocations for 4 frames: one by one %.0f, batched %.0f", plain, batched)
	if batched > plain {
		t.Errorf("batching costs %.0f allocations for 4 frames, one by one %.0f", batched, plain)
	}
}

// The frames of a batch reach an endpoint that takes them one at a time as they are, in order.
func TestProcessFramesDeliversInOrder(t *testing.T) {
	b, pod1, pod2 := setupTestBridge()
	var frames [][]byte
	for tag := byte(0); tag < 5; tag++ {
		frames = append(frames, unicast(pod1.MAC(), pod2.MAC(), tag))
	}
	if err := b.ProcessFrames(pod1, frames); err != nil {
		t.Fatal(err)
	}
	if pod2.ReceivedCount() != len(frames) {
		t.Fatalf("pod2 got %d frames, want %d", pod2.ReceivedCount(), len(frames))
	}
	for i, f := range frames {
		if !bytes.Equal(pod2.packets[i], f) {
			t.Errorf("frame %d was changed or is out of order", i)
		}
	}
}
