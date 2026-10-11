//go:build linux

package tap

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
)

// workerPod is the pod 10.244.1.5 behind a TAP of the given kind, on a bridge that is
// handed frames the way the overlay's workers hand them (see bridge.Batch.InjectFrame).
func workerPod(t *testing.T, offload, pipeline bool) (*bridge.Bridge, *os.File) {
	t.Helper()
	ep, pod := socketTap(t, tapOpts{name: "b", id: 5, vnet: offload, offload: offload, pipeline: pipeline})
	br := bridge.NewBridge(bridge.BridgeConfig{})
	if err := br.AddEndpoint(ep); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ep.Close() })
	return br, pod
}

// workerSegment is segment seq of TCP flow `flow` to the pod: the flow is told by its source port.
func workerSegment(flow int, seq uint32) *packet.EthernetFrame {
	f := flowFrame(seq, false)
	binary.BigEndian.PutUint16(f.Payload[20:], uint16(20000+flow))
	sumFrame(f.Payload)
	return f
}

// readWorkerFlows reads the pod's TAP until n segments have arrived and returns, for each
// flow, the sequence numbers in the order they arrived, after checking each segment.
func readWorkerFlows(t *testing.T, pod *os.File, vnet bool, n int) map[int][]uint32 {
	t.Helper()
	flows := map[int][]uint32{}
	buf := make([]byte, 70000)
	for got := 0; got < n; {
		_ = pod.SetReadDeadline(time.Now().Add(10 * time.Second))
		m, err := pod.Read(buf)
		if err != nil {
			t.Fatalf("after %d of %d segments: %v", got, n, err)
		}
		rec := buf[:m]
		if !vnet {
			rec = append(make([]byte, packet.VnetHeaderLen), rec...)
		}
		_, frames := recordFrames(t, rec)
		for _, f := range frames {
			ip := f[14:]
			flow, seq := int(binary.BigEndian.Uint16(ip[20:]))-20000, binary.BigEndian.Uint32(ip[24:])
			if len(f) != 14+40+segSize {
				t.Fatalf("segment of flow %d, sequence %d has %d bytes", flow, seq, len(f))
			}
			for i, c := range ip[40:] {
				if c != byte(seq)+byte(i) {
					t.Fatalf("flow %d, sequence %d: byte %d is %#x: frames were mixed", flow, seq, i, c)
				}
			}
			flows[flow] = append(flows[flow], seq)
			got++
		}
	}
	return flows
}

// Several overlay workers deliver frames of different flows to one pod at once, each with
// batches of its own, as the engine's receive sockets do: every frame is written once and
// the frames of each flow reach the pod in order. (Run with -race: that is the point.)
func TestOverlayWorkersDeliverToOnePodAtOnce(t *testing.T) {
	const workers, flowsEach, rounds, perBatch = 4, 2, 60, 10
	for _, tc := range []struct{ offload, pipeline bool }{{true, true}, {true, false}, {false, true}, {false, false}} {
		t.Run(fmt.Sprintf("offload=%v pipeline=%v", tc.offload, tc.pipeline), func(t *testing.T) {
			br, pod := workerPod(t, tc.offload, tc.pipeline)
			offloads, pipeline := Offloads(), Pipeline()
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					next := make([]uint32, flowsEach) // the next sequence number of each of the worker's flows
					for i := range next {
						next[i] = 1000
					}
					inject := func(x *bridge.Batch, i int) {
						if err := x.InjectFrame(workerSegment(w*flowsEach+i, next[i])); err != nil {
							t.Error(err)
						}
						next[i] += segSize
					}
					for r := 0; r < rounds; r++ {
						x := br.NewBatch()
						for k := 0; k < perBatch; k++ {
							if r%2 == 0 {
								inject(x, 0) // a stretch of one flow, which the TAP joins
							} else {
								inject(x, k%flowsEach) // the worker's flows alternate
							}
						}
						if err := x.Flush(); err != nil {
							t.Error(err)
						}
						x.Release()
					}
				}()
			}
			total := workers * rounds * perBatch
			flows := readWorkerFlows(t, pod, tc.offload, total)
			wg.Wait()

			for w := 0; w < workers; w++ {
				for i := 0; i < flowsEach; i++ {
					seqs := flows[w*flowsEach+i]
					for j, s := range seqs {
						if want := 1000 + uint32(j)*segSize; s != want {
							t.Fatalf("flow %d: segment %d has sequence %d, want %d: a frame was lost, repeated or reordered", w*flowsEach+i, j, s, want)
						}
					}
				}
			}
			count := 0
			for _, seqs := range flows {
				count += len(seqs)
			}
			if count != total {
				t.Errorf("%d segments arrived, want %d", count, total)
			}
			o, p := Offloads(), Pipeline()
			t.Logf("super-frames written %d (%d segments); writes inline %d, queued %d",
				o.WriteSuperFrames-offloads.WriteSuperFrames, o.WriteSegments-offloads.WriteSegments, p.WriteInline-pipeline.WriteInline, p.WriteQueued-pipeline.WriteQueued)
			if tc.offload && o.WriteSuperFrames == offloads.WriteSuperFrames {
				t.Error("no frames were joined: the test did not exercise the coalescing writers")
			}
			waitUntil(t, "the write buffers to be released", func() bool { return inFlight.Load() == 0 })
		})
	}
}
