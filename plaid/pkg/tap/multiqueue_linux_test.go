//go:build linux

package tap

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
	"plaid/pkg/packet/packettest"
)

// flowSegment is a TCP segment of the flow from 10.244.1.4:sport to 10.244.1.5:80, from the
// sender to the receiver of twoTaps, with n bytes of payload that depend on seq, and checksums done.
func flowSegment(sport uint16, seq uint32, n int) *packet.EthernetFrame {
	f := tcpFrame(seq, n, 0x10)
	f.DstMAC, f.SrcMAC = net.HardwareAddr{2, 0, 0, 0, 0, 5}, net.HardwareAddr{2, 0, 0, 0, 0, 4}
	binary.BigEndian.PutUint16(f.Payload[20:], sport)
	sumFrame(f.Payload)
	return f
}

// flowRecord is what a pod's stack writes to its TAP for n consecutive segments of a flow:
// one frame, or a super-frame (see record).
func flowRecord(sport uint16, seq uint32, n int) []byte {
	return record(n, func(i int) *packet.EthernetFrame { return flowSegment(sport, seq+uint32(i*segSize), segSize) })
}

// Which queue a frame is written to depends on its flow alone: the same for the segments of a
// flow whatever their size, flags and sequence numbers, and for the super-frame that joins
// them; the first for frames that have no flow (ARP) or that are too short; and the only one
// for a TAP with one.
func TestQueueChoiceFollowsTheFlow(t *testing.T) {
	ep, _ := pipeTapQueues(t, "q", 9, false, 0, 4)
	one, _ := pipeTapQueues(t, "o", 10, false, 0, 1)
	used := map[int]bool{}
	for sport := uint16(40000); sport < 40032; sport++ {
		want := ep.queueForFrame(mustMarshal(flowSegment(sport, 1000, segSize))).idx
		used[want] = true
		for _, seq := range []uint32{0, 1000 + segSize, 1 << 31} {
			for _, n := range []int{0, 1, 500, segSize} {
				if got := ep.queueForFrame(mustMarshal(flowSegment(sport, seq, n))).idx; got != want {
					t.Fatalf("port %d: queue %d for a segment of %d bytes at %d, queue %d for the first", sport, got, n, seq, want)
				}
			}
		}
		f := flowSegment(sport, 1000, segSize)
		if got := ep.queueFor(f.EtherType, f.Payload).idx; got != want {
			t.Errorf("port %d: queue %d for the frame's parts, %d for its bytes", sport, got, want)
		}
		// The record of a super-frame of several segments holds the flow's first headers.
		rec := flowRecord(sport, 1000, 6)
		if got := ep.queueForFrame(rec[packet.VnetHeaderLen:]).idx; got != want {
			t.Errorf("port %d: queue %d for a super-frame, %d for its segments", sport, got, want)
		}
		if got := one.queueForFrame(mustMarshal(f)).idx; got != 0 {
			t.Errorf("a TAP with one queue chose queue %d", got)
		}
	}
	if len(used) != 4 {
		t.Errorf("32 flows used %d of 4 queues", len(used))
	}
	arp := make([]byte, 42)
	binary.BigEndian.PutUint16(arp[12:], packet.EtherTypeARP)
	for name, frame := range map[string][]byte{"ARP": arp, "a short frame": arp[:10], "no frame": nil} {
		if got := ep.queueForFrame(frame).idx; got != 0 {
			t.Errorf("%s went to queue %d", name, got)
		}
	}
}

// One flow's frames are written to one queue, in order, whatever the other flows do, and
// the flows are spread over the queues: eight flows, sent by a pod over the queues of its
// own TAP, cross the bridge to a pod with four, as single frames and as super-frames, with
// and without the pipeline and passthrough. Each flow arrives complete and in order on the
// queue its hash picks.
func TestQueuesKeepAFlowInOrder(t *testing.T) {
	for _, c := range []struct{ pipeline, passthrough bool }{{false, false}, {true, false}, {true, true}} {
		t.Run(fmt.Sprintf("pipeline=%v passthrough=%v", c.pipeline, c.passthrough), func(t *testing.T) {
			const flows, queues, groups = 8, 4, 150
			br := bridge.NewBridge(bridge.BridgeConfig{})
			a, aPods := pipeTapQueues(t, "a", 4, c.pipeline, 0, queues)
			b, bPods := pipeTapQueues(t, "b", 5, c.pipeline, 0, queues)
			var aReads, bReads mqReads
			a.readHook, b.readHook = aReads.hook, bReads.hook
			for _, ep := range []*TapEndpoint{a, b} {
				if c.passthrough {
					ep.SetPassthrough()
				}
				if err := br.AddEndpoint(ep); err != nil {
					t.Fatal(err)
				}
				ep.StartReadLoop(context.Background(), br)
			}
			t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

			// The segments of a group of g: one frame, or a super-frame of six.
			size := func(g int) int { return 1 + g%2*5 }
			total := 0
			for g := 0; g < groups; g++ {
				total += size(g)
			}
			sport := func(f int) uint16 { return uint16(40000 + f) }
			// The sender's stack puts a flow on one queue of its TAP, as the driver would.
			var writers sync.WaitGroup
			for q := 0; q < queues; q++ {
				writers.Add(1)
				go func() {
					defer writers.Done()
					seq := make([]uint32, flows)
					for f := range seq {
						seq[f] = 1000
					}
					for g := 0; g < groups; g++ {
						for f := q; f < flows; f += queues {
							n := size(g)
							if _, err := aPods[q].Write(flowRecord(sport(f), seq[f], n)); err != nil {
								return
							}
							seq[f] += uint32(n * segSize)
						}
					}
				}()
			}

			// What the receiver's queues should deliver: the flows whose hash picks them.
			want := make([]int, queues)
			ownerOf := map[uint16]int{}
			for f := 0; f < flows; f++ {
				q := b.queueFor(packet.EtherTypeIPv4, flowSegment(sport(f), 1000, segSize).Payload).idx
				ownerOf[sport(f)] = q
				want[q] += total
			}
			var readers sync.WaitGroup
			for q := 0; q < queues; q++ {
				readers.Add(1)
				go func() {
					defer readers.Done()
					buf := make([]byte, 70000)
					next := map[uint16]uint32{}
					for got := 0; got < want[q]; {
						_ = bPods[q].SetReadDeadline(time.Now().Add(10 * time.Second))
						m, err := bPods[q].Read(buf)
						if err != nil {
							t.Errorf("queue %d, after %d of %d segments: %v", q, got, want[q], err)
							return
						}
						_, frames, err := cutRecord(buf[:m])
						if err != nil {
							t.Errorf("queue %d: %v", q, err)
							return
						}
						for _, fr := range frames {
							ip := fr[packet.EthernetHeaderLen:]
							port, seq := binary.BigEndian.Uint16(ip[20:]), binary.BigEndian.Uint32(ip[24:])
							if owner, ok := ownerOf[port]; !ok || owner != q {
								t.Errorf("queue %d got a segment of the flow from port %d, which is queue %d's", q, port, owner)
								return
							}
							if next[port] == 0 {
								next[port] = 1000
							}
							if seq != next[port] {
								t.Errorf("queue %d: flow %d has sequence %d, want %d: order was lost", q, port, seq, next[port])
								return
							}
							next[port] += segSize
							got++
						}
					}
				}()
			}
			writers.Wait()
			readers.Wait()

			read, written, counts := queuesUsed(b, &bReads)
			t.Logf("receiver:%s", counts)
			if written < 2 {
				t.Errorf("the receiver was written to on %d queues", written)
			}
			read, _, counts = queuesUsed(a, &aReads)
			t.Logf("sender:%s", counts)
			if read < 2 {
				t.Errorf("the sender was read on %d queues", read)
			}
		})
	}
}

// An endpoint with a pipeline starts the goroutines it needs and no others (for each queue,
// a reader, and with a pipeline a processor and a writer too); one without starts the readers.
// Every queue is closed with the endpoint, and the goroutines of every queue end.
func TestCloseEndsEveryQueue(t *testing.T) {
	for _, queues := range []int{1, 4} {
		for _, pipeline := range []bool{false, true} {
			t.Run(fmt.Sprintf("queues=%d/pipeline=%v", queues, pipeline), func(t *testing.T) {
				before := goroutines()
				ep, pods := pipeTapQueues(t, "x", 9, pipeline, 0, queues)
				ep.StartReadLoop(context.Background(), bridge.NewBridge(bridge.BridgeConfig{}))
				want := queues // a reader for each queue
				if pipeline {
					want = queues * 3 // and a processor and a writer
				}
				if got := goroutines() - before; got != want {
					t.Errorf("%d goroutines, want %d", got, want)
				}
				if (ep.queues[0].wq != nil) != pipeline || ep.Pipeline() != pipeline {
					t.Errorf("writer queue %v", ep.queues[0].wq != nil)
				}
				if err := ep.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
				settle(t, before)
				for i, pod := range pods { // the pod's end of a socket pair sees the TAP's end closed
					_ = pod.SetReadDeadline(time.Now().Add(time.Second))
					if _, err := pod.Read(make([]byte, 100)); err != io.EOF {
						t.Errorf("queue %d: the file was not closed: %v", i, err)
					}
				}
			})
		}
	}
}

// A frame for a pod whose TAP has several queues goes to the queue of its flow also when it
// is written alone, not by a batch's writer.
func TestWriteChoosesTheQueueOfTheFlow(t *testing.T) {
	ep, pods := pipeTapQueues(t, "w", 5, false, 0, 4)
	seen := map[int]bool{}
	for sport := uint16(40000); sport < 40016; sport++ {
		f := flowSegment(sport, 1000, 100)
		if err := ep.Write(mustMarshal(f)); err != nil {
			t.Fatal(err)
		}
		q := ep.queueFor(f.EtherType, f.Payload).idx
		seen[q] = true
		_ = pods[q].SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 2000)
		n, err := pods[q].Read(buf)
		if err != nil || n != packet.VnetHeaderLen+packet.EthernetHeaderLen+len(f.Payload) {
			t.Fatalf("port %d: nothing on queue %d: %d bytes, %v", sport, q, n, err)
		}
		if err := packettest.Verify(buf[packet.VnetHeaderLen+packet.EthernetHeaderLen : n]); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) < 3 {
		t.Errorf("16 flows were written to %d queues", len(seen))
	}
}
