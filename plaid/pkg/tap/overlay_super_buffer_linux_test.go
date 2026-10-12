//go:build linux

package tap

import (
	"bytes"
	"testing"
	"time"

	"net"

	"plaid/pkg/packet"
)

// bufferOf is the pooled buffer a lane puts a super-frame together in: room for the virtio
// header, then the frame. rec is a pod's record (podRecordTo): the header it describes the
// frame with, and the frame.
func bufferOf(rec []byte) (packet.VnetHeader, *packet.Buffer) {
	pb := packet.GetBuffer()
	pb.B = append(pb.B[:0], make([]byte, packet.VnetHeaderLen)...)
	pb.B = append(pb.B, rec[packet.VnetHeaderLen:]...)
	return packet.ParseVnetHeader(rec), pb
}

// A super-frame that the overlay put together in a buffer is written by the TAP's writer
// goroutine from that buffer: the caller queues it and goes on, nothing is copied, the
// virtio header and the MAC addresses the bridge set are filled in, and the pod gets the
// frame whole.
func TestRebuiltSuperFrameIsWrittenByTheWriterFromTheBuffer(t *testing.T) {
	t.Run("through the bridge", func(t *testing.T) {
		br, pod := workerPod(t, true, true)
		rec := podRecordTo(1000, 11, net.HardwareAddr{2, 0, 0, 0, 0, 1}, podB)
		h, pb := bufferOf(rec)
		pipes, offloads := Pipeline(), Offloads()

		x := br.NewBatch()
		if err := x.InjectSuper(h, pb); err != nil {
			t.Fatal(err)
		}
		_ = x.Flush()
		x.Release()

		if p := Pipeline(); p.WriteQueued != pipes.WriteQueued+1 || p.WriteInline != pipes.WriteInline {
			t.Errorf("the write was not queued for the writer goroutine: %+v -> %+v", pipes, p)
		}
		if d := delta(Offloads(), offloads); d.PassedSuperFrames != 1 || d.WriteSuperFrames != 0 {
			t.Errorf("counters %+v", d)
		}
		got := read(t, pod)
		want := append([]byte(nil), rec...)
		copy(want[packet.VnetHeaderLen:], []byte{2, 0, 0, 0, 0, 5}) // the pod's MAC; the source stays
		if !bytes.Equal(got, want) {
			t.Errorf("the pod got %d bytes, or not the record with the destination MAC set", len(got))
		}
		waitUntil(t, "the buffer to be released", func() bool { return inFlight.Load() == 0 })
	})

	t.Run("the buffer queued is the buffer given", func(t *testing.T) {
		ep, pod := pipeTap(t, "b", 5, false, 0)
		ep.wq = make(chan *packet.Buffer, 4) // a queue with no goroutine, so that the test sees what is queued
		w := ep.NewWriter().(*tapWriter)
		defer w.Release()
		rec := podRecordTo(1000, 11, net.HardwareAddr{2, 0, 0, 0, 0, 1}, podB)
		h, pb := bufferOf(rec)
		fr, _ := packet.ParseEthernet(pb.B[packet.VnetHeaderLen:])
		fr.DstMAC, fr.SrcMAC = ep.MAC(), net.HardwareAddr{2, 0, 0, 0, 7, 7}

		taken, err := w.AddSuperBuffer(h, fr, pb)
		if !taken || err != nil {
			t.Fatalf("taken=%v, %v", taken, err)
		}
		select {
		case q := <-ep.wq:
			if q != pb {
				t.Fatal("what was queued is not the buffer that was given")
			}
			want := append([]byte(nil), rec...)
			copy(want[packet.VnetHeaderLen:], ep.MAC())
			copy(want[packet.VnetHeaderLen+6:], []byte{2, 0, 0, 0, 7, 7})
			if !bytes.Equal(q.B, want) {
				t.Error("the queued record is not the header, the MACs and the frame")
			}
			ep.releaseWrite(q)
		default:
			t.Fatal("nothing was queued")
		}
		_ = pod
		if inFlight.Load() != 0 {
			t.Errorf("%d buffers not returned", inFlight.Load())
		}
	})

	t.Run("an endpoint with no writer goroutine does not take it", func(t *testing.T) {
		ep, _ := pipeTap(t, "b", 5, false, 0)
		w := ep.NewWriter().(*tapWriter)
		defer w.Release()
		rec := podRecordTo(1000, 3, net.HardwareAddr{2, 0, 0, 0, 0, 1}, podB)
		h, pb := bufferOf(rec)
		defer pb.Release()
		fr, _ := packet.ParseEthernet(pb.B[packet.VnetHeaderLen:])
		fr.DstMAC = ep.MAC()
		if taken, err := w.AddSuperBuffer(h, fr, pb); taken || err != nil {
			t.Errorf("taken=%v, %v", taken, err)
		}
	})
}

// The frame order of a destination is kept: a joined frame the writer holds, and the writes
// already queued, go before a rebuilt super-frame, and what is delivered after goes after.
func TestRebuiltSuperFrameKeepsTheOrderOfItsDestination(t *testing.T) {
	br, pod := workerPod(t, true, true)
	const before, after = 5, 5
	x := br.NewBatch()
	for i := 0; i < before; i++ {
		_ = x.InjectFrame(flowFrame(1000+uint32(i*segSize), true))
	}
	rec := podRecordTo(1000+before*segSize, 11, net.HardwareAddr{2, 0, 0, 0, 0, 1}, podB)
	h, pb := bufferOf(rec)
	if err := x.InjectSuper(h, pb); err != nil { // the same batch: the segments are held by the writer
		t.Fatal(err)
	}
	for i := 0; i < after; i++ {
		_ = x.InjectFrame(flowFrame(1000+uint32((before+11+i)*segSize), true))
	}
	_ = x.Flush()
	x.Release()
	readSegments(t, pod, 1000, before+11+after, true)
	waitUntil(t, "the buffers to be released", func() bool { return inFlight.Load() == 0 })
}

// The buffer comes back on every path: the writer wrote it, the write failed, the endpoint
// was closed with the frame queued, or closed before.
func TestRebuiltSuperFrameBufferIsReturnedOnEveryPath(t *testing.T) {
	queue := func(t *testing.T, ep *TapEndpoint) (bool, error) {
		w := ep.NewWriter().(*tapWriter)
		defer w.Release()
		rec := podRecordTo(1000, 11, net.HardwareAddr{2, 0, 0, 0, 0, 1}, podB)
		h, pb := bufferOf(rec)
		fr, _ := packet.ParseEthernet(pb.B[packet.VnetHeaderLen:])
		fr.DstMAC = ep.MAC()
		taken, err := w.AddSuperBuffer(h, fr, pb)
		if !taken {
			pb.Release() // the caller's, as the bridge does
		}
		return taken, err
	}

	t.Run("written", func(t *testing.T) {
		ep, pod := pipeTap(t, "b", 5, true, 0)
		if taken, err := queue(t, ep); !taken || err != nil {
			t.Fatal(taken, err)
		}
		read(t, pod)
		waitUntil(t, "the buffer to be released", func() bool { return inFlight.Load() == 0 && ep.wpending.Load() == 0 })
	})
	t.Run("the write fails", func(t *testing.T) {
		ep, pod := pipeTap(t, "b", 5, true, 0)
		_ = pod.Close() // the TAP's peer is gone: writes fail
		for i := 0; i < 3; i++ {
			if taken, err := queue(t, ep); !taken || err != nil {
				t.Fatal(taken, err)
			}
		}
		waitUntil(t, "the buffers to be released", func() bool { return inFlight.Load() == 0 && ep.wpending.Load() == 0 })
	})
	t.Run("the endpoint is closed with frames queued", func(t *testing.T) {
		before := goroutines()
		ep, pod := pipeTap(t, "b", 5, true, 16<<10)
		filler := fillTAP(t, ep) // the TAP is full: the writer is stuck with the first frame
		for i := 0; i < 6; i++ {
			if taken, err := queue(t, ep); !taken || err != nil {
				t.Fatal(taken, err)
			}
		}
		if ep.wpending.Load() < 2 {
			t.Fatalf("%d writes queued", ep.wpending.Load())
		}
		_ = ep.Close()
		settle(t, before)
		waitUntil(t, "the buffers to be released", func() bool { return inFlight.Load() == 0 })
		_, _ = filler, pod
	})
	t.Run("the endpoint is closed", func(t *testing.T) {
		ep, _ := pipeTap(t, "b", 5, true, 0)
		_ = ep.Close()
		if taken, err := queue(t, ep); taken || err == nil {
			t.Errorf("taken=%v, %v", taken, err)
		}
		if inFlight.Load() != 0 {
			t.Errorf("%d buffers not returned", inFlight.Load())
		}
	})
	t.Run("the bridge releases what no writer takes", func(t *testing.T) {
		br, pod := workerPod(t, true, false) // no pipeline: no writer goroutine
		rec := podRecordTo(1000, 11, net.HardwareAddr{2, 0, 0, 0, 0, 1}, podB)
		h, pb := bufferOf(rec)
		pipes := Pipeline()
		x := br.NewBatch()
		if err := x.InjectSuper(h, pb); err != nil {
			t.Fatal(err)
		}
		_ = x.Flush()
		x.Release()
		if p := Pipeline(); p.WriteQueued != pipes.WriteQueued {
			t.Error("a write was queued without a writer goroutine")
		}
		got := read(t, pod)
		if packet.ParseVnetHeader(got) != h || len(got) != len(rec) {
			t.Error("the pod did not get the frame written in place")
		}
	})
}

// Handing a buffer to the writer allocates nothing, and the pod gets every frame.
func TestRebuiltSuperFrameHandOverAllocatesNothing(t *testing.T) {
	ep, pod := pipeTap(t, "b", 5, true, 0)
	w := ep.NewWriter().(*tapWriter)
	defer w.Release()
	rec := podRecordTo(1000, 11, net.HardwareAddr{2, 0, 0, 0, 0, 1}, podB)
	h, _ := bufferOf(rec)
	fr, _ := packet.ParseEthernet(rec[packet.VnetHeaderLen:])
	fr.DstMAC = ep.MAC()
	in := make([]byte, 70000)
	run := func() {
		pb := packet.GetBuffer()
		pb.B = append(append(pb.B[:0], make([]byte, packet.VnetHeaderLen)...), rec[packet.VnetHeaderLen:]...)
		fr.Payload = pb.B[packet.VnetHeaderLen+packet.EthernetHeaderLen:]
		if taken, err := w.AddSuperBuffer(h, fr, pb); !taken || err != nil {
			panic("not taken")
		}
		_ = pod.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := pod.Read(in); err != nil {
			panic(err)
		}
	}
	run()
	allocs := testing.AllocsPerRun(50, run)
	t.Logf("%.1f allocations to hand over and write a frame", allocs)
	if allocs != 0 && !raceEnabled {
		t.Errorf("%.1f allocations, want 0", allocs)
	}
	waitUntil(t, "the buffers to be released", func() bool { return inFlight.Load() == 0 })
}
