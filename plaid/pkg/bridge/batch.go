package bridge

import (
	"fmt"
	"net"
	"sync"

	"plaid/pkg/packet"
)

// FrameWriter collects the frames that one Batch delivers to an endpoint and writes them
// when it is flushed, so that the endpoint can write several frames together.
type FrameWriter interface {
	// Add takes a copy of frame. It may write what it holds already, and returns that
	// write's error.
	Add(frame *packet.EthernetFrame) error
	// Flush writes what is held.
	Flush() error
	// Release returns the writer to its endpoint; it is not used afterwards.
	Release()
}

// BatchEndpoint is an Endpoint that gains from taking the frames of a batch together,
// such as a TAP that merges consecutive TCP segments into one write.
type BatchEndpoint interface {
	Endpoint
	// NewWriter returns a writer, or nil if the endpoint takes frames one at a time.
	NewWriter() FrameWriter
}

// OverlayBatch collects the frames of a batch bound for remote nodes, so that frames for
// one node can leave in one send. It keeps their order.
type OverlayBatch interface {
	Send(frame *packet.EthernetFrame, dstIP net.IP) error
	// Flush sends what is held.
	Flush() error
	Release()
}

// Batch is a run of frames that enter the bridge together, from one read of a TAP or one
// stretch of the overlay's receive queue. The bridge handles each frame as it comes, but
// what it sends out is held until Flush: frames to the overlay, and frames to endpoints
// that take them in batches. Frames reach each destination in the order they entered.
// A Batch is used by one goroutine at a time.
type Batch struct {
	b       *Bridge
	overlay OverlayBatch
	writers []batchWriter
}

type batchWriter struct {
	ep Endpoint
	w  FrameWriter
}

var batchPool = sync.Pool{New: func() any { return new(Batch) }}

// NewBatch returns an empty Batch; Release it when done.
func (b *Bridge) NewBatch() *Batch {
	x := batchPool.Get().(*Batch)
	x.b = b
	return x
}

// Release returns the batch for reuse. What was not flushed is dropped.
func (x *Batch) Release() {
	x.drop()
	x.b = nil
	batchPool.Put(x)
}

func (x *Batch) drop() {
	if x.overlay != nil {
		x.overlay.Release()
		x.overlay = nil
	}
	for i := range x.writers {
		x.writers[i].w.Release()
		x.writers[i] = batchWriter{}
	}
	x.writers = x.writers[:0]
}

// Flush sends and writes everything held and returns the first error.
func (x *Batch) Flush() error {
	var first error
	if x.overlay != nil {
		first = x.overlay.Flush()
	}
	for _, bw := range x.writers {
		if err := bw.w.Flush(); err != nil && first == nil {
			first = err
		}
	}
	x.drop()
	return first
}

// deliver hands frame to ep: held in the batch if ep takes batches, written now if not.
// raw, if not nil, is the frame still as it arrived, which an endpoint that does not batch
// can be given without encoding it again.
func (x *Batch) deliver(ep Endpoint, frame *packet.EthernetFrame, raw []byte) error {
	if w := x.writer(ep); w != nil {
		return w.Add(frame)
	}
	if raw != nil {
		return ep.Write(raw)
	}
	return writeFrame(ep, frame)
}

// writer returns the batch's writer for ep, or nil if ep takes frames one at a time.
func (x *Batch) writer(ep Endpoint) FrameWriter {
	for _, bw := range x.writers {
		if bw.ep == ep {
			return bw.w
		}
	}
	be, ok := ep.(BatchEndpoint)
	if !ok {
		return nil
	}
	w := be.NewWriter()
	if w != nil {
		x.writers = append(x.writers, batchWriter{ep, w})
	}
	return w
}

// sendOverlay sends frame through the overlay: held in the batch until Flush.
func (x *Batch) sendOverlay(frame *packet.EthernetFrame, dstIP net.IP) error {
	if x.overlay == nil {
		x.b.mu.RLock()
		newBatch := x.b.overlayBatcher
		x.b.mu.RUnlock()
		if newBatch == nil {
			return fmt.Errorf("overlay unavailable for %s", dstIP)
		}
		x.overlay = newBatch()
	}
	return x.overlay.Send(frame, dstIP)
}

// ProcessFrames is ProcessFrame for several frames from one source, which stay in
// order, with what they send out flushed once at the end. It returns the first error.
func (b *Bridge) ProcessFrames(srcEP Endpoint, frames [][]byte) error {
	x := b.NewBatch()
	defer x.Release()
	var first error
	for _, raw := range frames {
		if err := x.ProcessFrame(srcEP, raw); err != nil && first == nil {
			first = err
		}
	}
	if err := x.Flush(); err != nil && first == nil {
		first = err
	}
	return first
}
