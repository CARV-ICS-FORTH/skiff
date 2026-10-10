package tap

import (
	"sync"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
)

// tapWriter is the bridge.FrameWriter of a TAP with offloads: it holds the frames a
// batch delivers in a Coalescer, which joins consecutive TCP segments of a flow, and
// writes each result with one write: by the goroutine that delivers, or, if the endpoint
// has a writer goroutine, by that (see TapEndpoint.enqueue).
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
func (w *tapWriter) Add(frame *packet.EthernetFrame) error {
	if w.c.Merge(frame) {
		return nil
	}
	err := w.flush(true)
	if serr := w.c.Start(frame); err == nil {
		err = serr
	}
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
	if len(frame.Payload) <= w.t.uplinkMTU && packet.CompleteSuper(h, frame.Payload) == nil {
		h = packet.VnetHeader{}
		stats.joinUplink.Add(1)
	} else {
		stats.passSuper.Add(1)
		stats.passBytes.Add(uint64(packet.EthernetHeaderLen + len(frame.Payload)))
	}
	if w.t.wq != nil {
		if w.t.wpending.Load() != 0 {
			pb := packet.GetBuffer()
			pb.B = pb.B[:packet.VnetHeaderLen]
			h.Put(pb.B)
			var merr error
			if pb.B, merr = frame.MarshalTo(pb.B); merr != nil {
				pb.Release()
				return merr
			}
			if qerr := w.t.enqueue(pb); err == nil {
				err = qerr
			}
			return err
		}
		stats.writeInline.Add(1)
	}
	if werr := w.t.vw.writeSuper(h, frame); err == nil {
		err = werr
	}
	return err
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
	if w.t.wq != nil {
		if more || w.t.wpending.Load() != 0 {
			return w.t.enqueue(w.c.Detach())
		}
		stats.writeInline.Add(1)
	}
	_, err := w.t.file.Write(b)
	return err
}

func (w *tapWriter) Release() {
	w.c.Discard() // a frame held and not flushed is dropped, not given to the writer's next user
	w.t = nil
	writerPool.Put(w)
}
