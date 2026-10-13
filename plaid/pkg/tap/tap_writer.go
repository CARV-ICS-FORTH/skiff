package tap

import (
	"sync"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
)

// tapWriter is the bridge.FrameWriter of a TAP with offloads: it holds the frames a
// batch delivers in a Coalescer, which joins consecutive TCP segments of a flow, and
// writes each result with one write: by the goroutine that delivers, or, if the endpoint
// has a writer goroutine, by that (see tapQueue.enqueue). A result is written to the queue
// that its flow picks (see TapEndpoint.queueFor); the frame held is of one flow, so a
// flow's frames are joined, and written, in the order they came in, whatever queues the
// frames of other flows between them go to.
type tapWriter struct {
	t *TapEndpoint
	c packet.Coalescer
}

var writerPool = sync.Pool{New: func() any { return new(tapWriter) }}

// NewWriter implements bridge.BatchEndpoint. Without offloads the TAP takes frames one
// at a time.
func (t *TapEndpoint) NewWriter() bridge.FrameWriter {
	if !t.offload {
		return nil
	}
	w := writerPool.Get().(*tapWriter)
	w.t = t
	return w
}

// Add holds frame, after writing the frame held before it unless frame joins that.
func (w *tapWriter) Add(frame *packet.EthernetFrame) error { return w.AddFrom(nil, frame) }

// AddFrom is Add for a frame that came in at src (see bridge.SourceWriter). A TCP
// handshake has its MSS changed in the copy that is held, never in the caller's frame,
// and never joins another frame: only ACKs do.
func (w *tapWriter) AddFrom(src bridge.Endpoint, frame *packet.EthernetFrame) error {
	if w.c.Merge(frame) {
		return nil
	}
	err := w.flush(true)
	if serr := w.c.Start(frame); err == nil {
		err = serr
	}
	w.t.setMSS(w.c.Held(), src)
	return err
}

// AddSuper writes a TSO segment from a pod, as the pod's stack made it (see
// bridge.SuperWriter), after the frame held. It is written in place, from the caller's
// buffer, unless writes are still with the writer goroutine: then it is copied and queued
// behind them, which keeps the order.
//
// A TAP with an MTU above the pods' (the bubble's) that the segment fits takes it as one
// ordinary packet: the buffer's headers and checksum are completed and it is written
// with an empty virtio header, so that its kernel forwards one packet and not the
// segments it would cut it into.
func (w *tapWriter) AddSuper(h packet.VnetHeader, frame *packet.EthernetFrame) error {
	err := w.flush(true)
	if cerr := w.t.errClosed(); cerr != nil {
		return cerr
	}
	h = w.superHeader(h, frame)
	q := w.t.queueFor(frame.EtherType, frame.Payload)
	if q.wq != nil {
		if q.wpending.Load() != 0 {
			pb := packet.GetBuffer()
			pb.B = pb.B[:packet.VnetHeaderLen]
			h.Put(pb.B)
			var merr error
			if pb.B, merr = frame.MarshalTo(pb.B); merr != nil {
				pb.Release()
				return merr
			}
			if qerr := q.enqueue(pb); err == nil {
				err = qerr
			}
			return err
		}
		stats.writeInline.Add(1)
	}
	q.writes.Add(1)
	if werr := q.vw.writeSuper(h, frame); err == nil {
		err = werr
	}
	return err
}

// superHeader is the virtio header a super-frame is written with: its own, or none if the
// TAP takes it as one ordinary packet (see AddSuper). It counts the frame.
func (w *tapWriter) superHeader(h packet.VnetHeader, frame *packet.EthernetFrame) packet.VnetHeader {
	if len(frame.Payload) <= w.t.uplinkMTU && packet.CompleteSuper(h, frame.Payload) == nil {
		stats.joinUplink.Add(1)
		return packet.VnetHeader{}
	}
	stats.passSuper.Add(1)
	stats.passBytes.Add(uint64(packet.EthernetHeaderLen + len(frame.Payload)))
	return h
}

// AddSuperBuffer is AddSuper for a frame that lies in pb (see bridge.BufferSuperWriter),
// which the writer goroutine writes from there: nothing is copied, and the goroutine that
// delivers goes on at once instead of waiting for the pod's stack to take the frame, which
// is where the time of a stream goes. It is always queued, behind the frame held and the
// writes already queued, so the order is that of AddSuper. If the queue is full it waits
// for the writer, as every queued write does (see TapEndpoint.enqueue). An endpoint with
// no writer goroutine (no pipeline) does not take it: the caller writes in place.
func (w *tapWriter) AddSuperBuffer(h packet.VnetHeader, frame *packet.EthernetFrame, pb *packet.Buffer) (bool, error) {
	q := w.t.queueFor(frame.EtherType, frame.Payload)
	if q.wq == nil || len(frame.DstMAC) != 6 || len(frame.SrcMAC) != 6 ||
		len(pb.B) != packet.VnetHeaderLen+packet.EthernetHeaderLen+len(frame.Payload) {
		return false, nil
	}
	err := w.flush(true)
	if cerr := w.t.errClosed(); cerr != nil {
		return false, cerr
	}
	w.superHeader(h, frame).Put(pb.B)
	// The Ethernet header holds the MAC addresses of the frame the overlay carried; the
	// bridge has set those of this one.
	copy(pb.B[packet.VnetHeaderLen:], frame.DstMAC)
	copy(pb.B[packet.VnetHeaderLen+6:], frame.SrcMAC)
	if qerr := q.enqueue(pb); err == nil {
		err = qerr
	}
	return true, err
}

// Flush writes the frame held.
func (w *tapWriter) Flush() error { return w.flush(false) }

// flush writes the frame held; more says that the caller has more frames to deliver
// at once (the frame that comes next is already in hand), which a writer goroutine can
// write the held one in the meantime.
func (w *tapWriter) flush(more bool) error {
	if !w.c.Pending() {
		return nil
	}
	if err := w.t.errClosed(); err != nil {
		w.c.Take()
		return err
	}
	b, segments := w.c.Take()
	if segments > 1 {
		stats.writeSuper.Add(1)
		stats.writeSegments.Add(uint64(segments))
	}
	q := w.t.queueForFrame(b[packet.VnetHeaderLen:])
	if q.wq != nil {
		if more || q.wpending.Load() != 0 {
			return q.enqueue(w.c.Detach())
		}
		stats.writeInline.Add(1)
	}
	return q.write(b)
}

func (w *tapWriter) Release() {
	w.c.Discard() // a frame held and not flushed is dropped, not given to the writer's next user
	w.t = nil
	writerPool.Put(w)
}
