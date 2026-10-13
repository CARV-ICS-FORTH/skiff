//go:build linux

package tap

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
)

const segSize = 1398

// pipeTap is socketTap for the pipeline tests, with a TAP that has offloads and a name, a MAC
// and an IP of its own.
func pipeTap(t testing.TB, name string, id byte, pipeline bool, sndbuf int) (*TapEndpoint, *os.File) {
	t.Helper()
	ep, pods := pipeTapQueues(t, name, id, pipeline, sndbuf, 1)
	return ep, pods[0]
}

// pipeTapQueues is pipeTap for a TAP with several queues, each a socket pair of its own; the
// result has the pod's end of each.
func pipeTapQueues(t testing.TB, name string, id byte, pipeline bool, sndbuf, queues int) (*TapEndpoint, []*os.File) {
	t.Helper()
	return socketTapQueues(t, tapOpts{name: name, id: id, sndbuf: sndbuf, vnet: true, offload: true, pipeline: pipeline, queues: queues})
}

// twoTaps is passTaps with the read loops running, and no passthrough: a bridge with the pods
// 10.244.1.5 (the receiver, MAC ...:05, "b") and 10.244.1.4 (the sender, "a"); tcpFrame
// addresses a frame from the sender to the receiver.
func twoTaps(t testing.TB, pipeline bool, sndbuf int) (a, b *TapEndpoint, aPod, bPod *os.File, br *bridge.Bridge) {
	t.Helper()
	a, b, aPod, bPod, br, start := passTaps(t, passOpts{pipeline: pipeline, sndbuf: sndbuf})
	start()
	return
}

// flowFrame is a TCP segment of the stream from the sender to the receiver (or the same
// with another protocol, if udp, which nothing joins), with checksums done.
func flowFrame(seq uint32, udp bool) *packet.EthernetFrame {
	f := tcpFrame(seq, segSize, 0x10)
	f.DstMAC, f.SrcMAC = net.HardwareAddr{2, 0, 0, 0, 0, 5}, net.HardwareAddr{2, 0, 0, 0, 0, 4}
	copy(f.Payload[12:], []byte{10, 244, 1, 4, 10, 244, 1, 5})
	if udp {
		f.Payload[9] = 17
		sumFrame(f.Payload)
	}
	return f
}

// record is what a pod's stack writes to its TAP for n consecutive frames, mk(0), mk(1), ...:
// a single frame with an empty virtio header, or a super-frame, which the pod leaves to be
// cut and checksummed (a Coalescer makes what the kernel's TSO would).
func record(n int, mk func(i int) *packet.EthernetFrame) []byte {
	if n == 1 {
		return append(make([]byte, packet.VnetHeaderLen), mustMarshal(mk(0))...)
	}
	var c packet.Coalescer
	for i := 0; i < n; i++ {
		if f := mk(i); i == 0 {
			_ = c.Start(f)
		} else if !c.Merge(f) {
			panic("segments do not join")
		}
	}
	b, _ := c.Take()
	return append([]byte(nil), b...)
}

// podRecord is the record of n segments of the stream of flowFrame from seq on.
func podRecord(seq uint32, n int) []byte {
	return record(n, func(i int) *packet.EthernetFrame { return flowFrame(seq+uint32(i*segSize), false) })
}

// cutRecord returns the virtio header and the frames of a record read from a TAP: the
// record's own, or the segments of its super-frame, as copies.
func cutRecord(rec []byte) (packet.VnetHeader, [][]byte, error) {
	h := packet.ParseVnetHeader(rec)
	var sg packet.Segmenter
	segs, err := sg.Segment(h, append([]byte(nil), rec[packet.VnetHeaderLen:]...))
	if err != nil {
		return h, nil, err
	}
	out := make([][]byte, len(segs))
	for i, s := range segs {
		out[i] = append([]byte(nil), s...)
	}
	return h, out, nil
}

// recordFrames is cutRecord that fails the test.
func recordFrames(t testing.TB, rec []byte) (packet.VnetHeader, [][]byte) {
	t.Helper()
	h, out, err := cutRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	return h, out
}

// readSegments reads records from the receiving pod until n segments of the stream have
// arrived, and checks that they are the stream's, in order: the sequence numbers go on
// from first by segSize and the payload is the one tcpFrame makes. udp: the frames carry
// the sequence number only.
func readSegments(t testing.TB, pod *os.File, first uint32, n int, udp bool) {
	t.Helper()
	buf := make([]byte, 70000)
	next := first
	for got := 0; got < n; {
		_ = pod.SetReadDeadline(time.Now().Add(10 * time.Second))
		m, err := pod.Read(buf)
		if err != nil {
			t.Fatalf("after %d of %d segments: %v", got, n, err)
		}
		_, segs := recordFrames(t, buf[:m])
		for _, f := range segs {
			ip := f[14:]
			seq := binary.BigEndian.Uint32(ip[24:])
			if seq != next || len(f) != 14+40+segSize {
				t.Fatalf("segment %d has sequence %d and %d bytes, want %d and %d: order or content was lost", got, seq, len(f), next, 14+40+segSize)
			}
			if !udp {
				for i, c := range ip[40:] {
					if c != byte(seq)+byte(i) {
						t.Fatalf("segment %d: byte %d is %#x", got, i, c)
					}
				}
			}
			next += segSize
			got++
		}
	}
}

func goroutines() int { return runtime.NumGoroutine() }

// settle waits for the number of goroutines to be at most n.
func settle(t testing.TB, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for goroutines() > n {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("%d goroutines, want %d:\n%s", goroutines(), n, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitUntil(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A long stream through both pipelines arrives whole and in order, as it does without
// them; the records alternate between single frames and super-frames, so batches differ
// in size.
func TestPipelinesKeepOrder(t *testing.T) {
	for _, pipeline := range []bool{false, true} {
		t.Run(fmt.Sprintf("pipeline=%v", pipeline), func(t *testing.T) {
			_, _, aPod, bPod, _ := twoTaps(t, pipeline, 0)
			const groups = 400
			total := 0
			go func() {
				seq := uint32(1000)
				for g := 0; g < groups; g++ {
					n := 1 + g%3*10 // 1, 11 or 21 segments
					if _, err := aPod.Write(podRecord(seq, n)); err != nil {
						return
					}
					seq += uint32(n * segSize)
				}
			}()
			for g := 0; g < groups; g++ {
				total += 1 + g%3*10
			}
			before := Pipeline()
			readSegments(t, bPod, 1000, total, false)
			t.Logf("%d segments; queue full: read %d, write %d", total, Pipeline().ReadQueueFull-before.ReadQueueFull, Pipeline().WriteQueueFull-before.WriteQueueFull)
		})
	}
}

// The same stream with stage 2 slow now and then and the pod sending in bursts with gaps,
// so that batches alternate between being run by the reader (frames not waiting, stage 2
// idle) and being handed over (a burst, or stage 2 still busy): all in order.
func TestPipelineKeepsOrderWhenBatchesAlternate(t *testing.T) {
	_, _, aPod, bPod, br := twoTaps(t, true, 0)
	var calls atomic.Int64
	br.SetFilterHandler(func(*packet.EthernetFrame, bridge.Endpoint) bool {
		if calls.Add(1)%40 == 0 {
			for end := time.Now().Add(300 * time.Microsecond); time.Now().Before(end); {
			}
		}
		return true
	})
	const groups = 600
	total := 0
	for g := 0; g < groups; g++ {
		total += 1 + g%3*10
	}
	go func() {
		seq := uint32(1000)
		for g := 0; g < groups; g++ {
			n := 1 + g%3*10
			if _, err := aPod.Write(podRecord(seq, n)); err != nil {
				return
			}
			seq += uint32(n * segSize)
			if g%10 >= 5 {
				time.Sleep(time.Millisecond) // the reader catches up: what comes next is alone
			}
		}
	}()
	before := Pipeline()
	readSegments(t, bPod, 1000, total, false)
	after := Pipeline()
	inline, queued := after.ReadInline-before.ReadInline, after.ReadQueued-before.ReadQueued
	t.Logf("read: %d inline, %d queued; write: %d inline, %d queued", inline, queued, after.WriteInline-before.WriteInline, after.WriteQueued-before.WriteQueued)
	if inline == 0 || queued == 0 {
		t.Errorf("%d batches inline and %d queued: the test did not mix them", inline, queued)
	}
}

// A lone batch, with the stage after it idle, is run in place by the goroutine that has
// it, and so is the write that it makes: nothing is handed over.
func TestLoneBatchIsRunInline(t *testing.T) {
	_, _, aPod, bPod, _ := twoTaps(t, true, 0)
	before := Pipeline()
	for i := 0; i < 5; i++ { // one at a time: each is delivered before the next is sent
		if _, err := aPod.Write(podRecord(1000+uint32(i*segSize), 1)); err != nil {
			t.Fatal(err)
		}
		readSegments(t, bPod, 1000+uint32(i*segSize), 1, false)
	}
	after := Pipeline()
	if got := after.ReadInline - before.ReadInline; got != 5 {
		t.Errorf("%d batches run inline, want 5", got)
	}
	if got := after.WriteInline - before.WriteInline; got != 5 {
		t.Errorf("%d writes made inline, want 5", got)
	}
	if after.ReadQueued != before.ReadQueued || after.WriteQueued != before.WriteQueued {
		t.Errorf("something was handed over: %+v -> %+v", before, after)
	}
}

// A bridge that is slow holds up a reader that runs the batch itself as it does the queue:
// the pod's frames wait in its TAP, none is dropped, and they arrive in order.
func TestInlineReaderWaitsForTheBridge(t *testing.T) {
	_, _, aPod, bPod, br := twoTaps(t, true, 0)
	gate, entered := make(chan struct{}), make(chan struct{}, 100)
	br.SetFilterHandler(func(*packet.EthernetFrame, bridge.Endpoint) bool { entered <- struct{}{}; <-gate; return true })
	time.Sleep(20 * time.Millisecond) // both read loops wait in a read, each with a batch
	held, before := inFlight.Load(), Pipeline()
	const frames = 30
	if _, err := aPod.Write(podRecord(1000, 1)); err != nil {
		t.Fatal(err)
	}
	<-entered // the reader is in the bridge with the first, having found nothing else waiting
	for i := 1; i < frames; i++ {
		if _, err := aPod.Write(podRecord(1000+uint32(i*segSize), 1)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := Pipeline(); got.ReadInline-before.ReadInline != 1 || got.ReadQueued != before.ReadQueued {
		t.Errorf("the reader went on while the bridge waited: %+v -> %+v", before, got)
	}
	if len(entered) != 0 || inFlight.Load() != held {
		t.Errorf("%d more frames in the bridge, %d more batches held", len(entered), inFlight.Load()-held)
	}
	close(gate)
	readSegments(t, bPod, 1000, frames, false)
}

// While the bridge is slow and frames are waiting, the reader hands batches over until the
// queue is full and then stops, with the rest still in the pod's TAP: nothing is dropped,
// and it all arrives in order once the bridge goes on.
func TestReadStageWaitsForTheBridge(t *testing.T) {
	_, _, aPod, bPod, br, start := passTaps(t, passOpts{pipeline: true})
	gate := make(chan struct{})
	br.SetFilterHandler(func(*packet.EthernetFrame, bridge.Endpoint) bool { <-gate; return true })
	const frames = 100
	for i := 0; i < frames; i++ { // waiting before the read loops start, so that the first batch is handed over
		if _, err := aPod.Write(podRecord(1000+uint32(i*segSize), 1)); err != nil {
			t.Fatal(err)
		}
	}
	held, before := inFlight.Load(), Pipeline()
	start()
	waitUntil(t, "the read stage to wait", func() bool { return Pipeline().ReadQueueFull > before.ReadQueueFull })
	time.Sleep(50 * time.Millisecond)
	// The bridge holds one batch, the queue eight, and the reader one that it cannot hand
	// on; the receiver's read loop waits with one of its own.
	if got := inFlight.Load() - held; got != readQueueLen+3 {
		t.Errorf("%d batches in the pipeline while the bridge waits, want %d", got, readQueueLen+3)
	}
	close(gate)
	readSegments(t, bPod, 1000, frames, false)
}

// While the TAP is slow to take writes, the bridge's delivery waits when the queue to the
// writer is full: no frame is lost or reordered, and the delivery goes on once the pod reads.
func TestWriteStageWaitsForTheTAP(t *testing.T) {
	_, _, _, bPod, br := twoTaps(t, true, 16<<10)
	const frames = 500
	before := Pipeline()
	done := make(chan struct{})
	go func() { // as the overlay's worker delivers
		defer close(done)
		x := br.NewBatch()
		defer x.Release()
		for i := 0; i < frames; i++ {
			if err := x.InjectFrame(flowFrame(1000+uint32(i*segSize), true)); err != nil {
				return // the endpoint is closed: the test failed
			}
		}
		_ = x.Flush() // one batch: every write but the last is handed to the writer
	}()
	waitUntil(t, "the delivery to wait", func() bool { return Pipeline().WriteQueueFull > before.WriteQueueFull })
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the delivery finished though the TAP takes nothing")
	default:
	}
	readSegments(t, bPod, 1000, frames, true)
	<-done
}

// Deliveries of one to five frames that nothing joins, with pauses: all the writes of a
// batch of several go to the writer goroutine, the one of a lone frame is made in place
// when the writer is idle, so the TAP gets both kinds, in the order of the frames.
func TestWritesKeepOrderWhenTheyAlternate(t *testing.T) {
	_, _, _, bPod, br := twoTaps(t, true, 0)
	const frames = 3000
	before := Pipeline()
	go func() { // as the overlay's worker delivers
		x := br.NewBatch()
		defer x.Release()
		for i, batch := 0, 0; i < frames; batch++ {
			for k := 0; k < 1+batch%5 && i < frames; k++ {
				_ = x.InjectFrame(flowFrame(1000+uint32(i*segSize), true))
				i++
			}
			_ = x.Flush()
			for end := time.Now().Add(100 * time.Microsecond); time.Now().Before(end); { // the writer catches up
			}
		}
	}()
	readSegments(t, bPod, 1000, frames, true)
	after := Pipeline()
	inline, queued := after.WriteInline-before.WriteInline, after.WriteQueued-before.WriteQueued
	t.Logf("write: %d inline, %d queued", inline, queued)
	if inline == 0 || queued == 0 {
		t.Errorf("%d writes inline and %d queued: the test did not mix them", inline, queued)
	}
}

// A writer released with a frame held drops it: the writer's next user does not write it,
// nor join frames to it.
func TestReleasedWriterDropsItsFrame(t *testing.T) {
	ep, pod := pipeTap(t, "b", 5, false, 0)
	for i := 0; i < 20; i++ { // the pool hands the same writer back, usually
		w := ep.NewWriter()
		_ = w.Add(flowFrame(1000, false))
		w.Release()
		w = ep.NewWriter()
		_ = w.Add(flowFrame(1000+segSize, false)) // would join the dropped frame
		_ = w.Flush()
		w.Release()
		rec := make([]byte, 70000)
		n, err := pod.Read(rec)
		if err != nil {
			t.Fatal(err)
		}
		if h := packet.ParseVnetHeader(rec); h.GSOType != packet.VnetGSONone || n != packet.VnetHeaderLen+14+40+segSize {
			t.Fatalf("the pod got %d bytes with %+v: the dropped frame was written or joined", n, h)
		}
	}
}

// Endpoints that are created, loaded, and removed leave no goroutine and no pooled buffer
// behind, and nothing is written to a pod after its endpoint was removed; with passthrough too,
// where the TSO segments of the sender are written whole and queue behind the overlay's frames.
func TestPipelinesStopWhileBusy(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%v", passthrough), func(t *testing.T) {
			before, held := goroutines(), inFlight.Load()
			for cycle := 0; cycle < 25; cycle++ {
				a, b, aPod, bPod, br, start := passTaps(t, passOpts{pipeline: true, passthrough: passthrough, sndbuf: 16 << 10})
				start()
				var wg sync.WaitGroup
				stop := make(chan struct{})
				wg.Add(3)
				go func() { // the pod sends as fast as it can
					defer wg.Done()
					rec := podRecord(1000, 11)
					for {
						if _, err := aPod.Write(rec); err != nil {
							return
						}
					}
				}()
				go func() { // the overlay delivers to the other pod, which reads now and then
					defer wg.Done()
					x := br.NewBatch()
					defer x.Release()
					for i := uint32(0); ; i++ {
						select {
						case <-stop:
							return
						default:
						}
						// Once the receiver is removed its frames are flooded, to nobody: no error.
						if err := x.InjectFrame(flowFrame(1000+i*segSize, false)); err != nil {
							return
						}
						if err := x.Flush(); err != nil {
							return
						}
					}
				}()
				go func() {
					defer wg.Done()
					buf := make([]byte, 70000)
					for {
						if _, err := bPod.Read(buf); err != nil {
							return
						}
						time.Sleep(time.Duration(cycle%3) * 100 * time.Microsecond)
					}
				}()
				time.Sleep(time.Duration(5+cycle%7) * time.Millisecond)

				_ = br.RemoveEndpoint(b.ID()) // closes the receiver while the writer queue is full
				if err := b.Write([]byte("x")); err == nil {
					t.Fatal("write to a closed endpoint succeeded")
				}
				_ = br.RemoveEndpoint(a.ID())
				close(stop)
				wg.Wait()
				_ = a.Close()
				_ = b.Close()
				_ = aPod.Close()
				_ = bPod.Close()
			}
			settle(t, before)
			if got := inFlight.Load(); got != held {
				t.Errorf("%d pooled buffers were not returned", got-held)
			}
		})
	}
}

// After Close the writer writes nothing more, whatever is still queued (that a writer cannot
// flush to a closed endpoint is TestClosedEndpointRefusesWrites).
func TestNoWriteAfterClose(t *testing.T) {
	before := goroutines()
	ep, pod := pipeTap(t, "b", 5, true, 0)
	for i := 0; i < writeQueueLen; i++ {
		w := ep.NewWriter()
		_ = w.Add(flowFrame(1000+uint32(i*segSize), true))
		_ = w.Flush()
		w.Release()
	}
	_ = ep.Close()
	settle(t, before) // the writer is gone
	// What the writer had written before Close can be read; nothing is written after.
	n := 0
	buf := make([]byte, 70000)
	for {
		_ = pod.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		if _, err := pod.Read(buf); err != nil {
			break
		}
		n++
	}
	if n > writeQueueLen {
		t.Errorf("%d frames written, %d queued", n, writeQueueLen)
	}
	if got := inFlight.Load(); got != 0 {
		t.Errorf("%d buffers not returned", got)
	}
}

// The frames that plaidd completed itself are passed to the bridge as Trusted, and no
// others are, with the pipeline and without.
func TestOnlyCompletedFramesAreTrusted(t *testing.T) {
	for _, pipeline := range []bool{false, true} {
		t.Run(fmt.Sprintf("pipeline=%v", pipeline), func(t *testing.T) {
			_, _, aPod, bPod, br := twoTaps(t, pipeline, 0)
			var mu sync.Mutex
			var trusted []bool
			br.SetFilterHandler(func(f *packet.EthernetFrame, _ bridge.Endpoint) bool {
				mu.Lock()
				trusted = append(trusted, f.Trusted)
				mu.Unlock()
				return true
			})
			plain := podRecord(1000, 1) // as the pod's stack made it: nothing left to do
			needs := podRecord(1000+segSize, 1)
			packet.VnetHeader{Flags: packet.VnetNeedsCsum, CsumStart: 34, CsumOffset: 16}.Put(needs)
			for _, rec := range [][]byte{plain, needs, podRecord(1000+2*segSize, 3)} {
				if _, err := aPod.Write(rec); err != nil {
					t.Fatal(err)
				}
			}
			readSegments(t, bPod, 1000, 5, false)
			mu.Lock()
			defer mu.Unlock()
			if fmt.Sprint(trusted) != "[false true true true true]" {
				t.Errorf("trusted: %v, want the first frame untrusted and the four others trusted", trusted)
			}
		})
	}
}

// The pipelines allocate no more per frame than the loops they replace.
func TestPipelinesAddNoAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("pools drop buffers at random under the race detector")
	}
	measure := func(pipeline bool) float64 {
		_, _, aPod, bPod, _ := twoTaps(t, pipeline, 0)
		rec, buf := podRecord(1000, 40), make([]byte, 70000)
		run := func() {
			if _, err := aPod.Write(rec); err != nil {
				t.Fatal(err)
			}
			_ = bPod.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := bPod.Read(buf); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 20; i++ {
			run() // the pools fill
		}
		return testing.AllocsPerRun(100, run)
	}
	plain, piped := measure(false), measure(true)
	t.Logf("allocations for a super-frame of 40 segments through the bridge: one goroutine each way %.0f, pipelines %.0f", plain, piped)
	if piped > plain {
		t.Errorf("the pipelines allocate %.0f, the loops %.0f", piped, plain)
	}

	// A write through the writer goroutine allocates nothing.
	ep, pod := pipeTap(t, "w", 6, true, 0)
	var frames []*packet.EthernetFrame
	for i := 0; i < 40; i++ {
		frames = append(frames, flowFrame(1000+uint32(i*segSize), false))
	}
	buf := make([]byte, 70000)
	write := func() {
		w := ep.NewWriter()
		for _, f := range frames {
			_ = w.Add(f)
		}
		_ = w.Flush()
		w.Release()
		_ = pod.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := pod.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if got := testing.AllocsPerRun(50, write); got != 0 {
		t.Errorf("a writer with a writer goroutine allocates %.0f for 40 segments", got)
	}
}

// BenchmarkBridgeThroughput is the path a stream takes through plaidd's two TAP
// endpoints in one process: a pod's super-frames are read and cut, run through the bridge,
// joined again and written to the other pod, over socket pairs that stand for the TAPs.
func BenchmarkBridgeThroughput(b *testing.B) {
	for _, pipeline := range []bool{false, true} {
		b.Run(fmt.Sprintf("pipeline=%v", pipeline), func(b *testing.B) {
			_, _, aPod, bPod, _ := twoTaps(b, pipeline, 0)
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

// BenchmarkBridgeLatency is a frame at a time, as an ACK-clocked stream sends it: one
// small frame crosses the bridge and the sender waits for it to arrive before the next. A
// hand-off to another goroutine shows here as the wake-up it costs.
func BenchmarkBridgeLatency(b *testing.B) {
	for _, pipeline := range []bool{false, true} {
		b.Run(fmt.Sprintf("pipeline=%v", pipeline), func(b *testing.B) {
			_, _, aPod, bPod, _ := twoTaps(b, pipeline, 0)
			rec, buf := podRecord(1000, 1), make([]byte, 70000)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := aPod.Write(rec); err != nil {
					b.Fatal(err)
				}
				if _, err := bPod.Read(buf); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
