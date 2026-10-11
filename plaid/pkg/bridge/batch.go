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

// SuperWriter is a FrameWriter that can also take a TCP super-frame whole: a frame of up
// to 64 KiB that stands for several segments, as a TAP with offloads is given and hands
// over (see packet.VnetHeader).
type SuperWriter interface {
	FrameWriter
	// AddSuper writes frame, which is described by the virtio header h, after what the
	// writer holds. It has finished with frame when it returns: its payload is the
	// caller's read buffer, which the writer may change.
	AddSuper(h packet.VnetHeader, frame *packet.EthernetFrame) error
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
	sup     superFrame
	seg     packet.Segmenter // cuts a super-frame after the engine; its frames live until Flush
}

// superFrame is the state of a Batch that routes a TCP super-frame from src. Only a
// delivery to the one endpoint that can take it whole is allowed to take it; the places
// where the routing would do anything else with the frame (send it over the overlay, to
// the outbound handler, to everyone) leave it alone, so that the caller can cut it and
// route the segments. The Service engine is the exception: it translates the frame, and
// what comes out is routed the same way, except that the bridge cuts it itself if it
// cannot go whole (see serviceSuper).
type superFrame struct {
	on    bool
	dry   bool // only find out whether a delivery would take it, without delivering
	whole bool // a delivery took it (would have), the filter dropped it, or the engine has it
	src   Endpoint
	h     packet.VnetHeader
	raw   []byte // the Ethernet frame, to cut after the engine
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
	x.sup = superFrame{}
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
	if x.sup.on {
		return x.deliverSuper(ep, frame)
	}
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

// deliverSuper is deliver for the super-frame being routed, which only an endpoint that
// takes super-frames whole can have, and not the one it came from. (A super-frame the
// engine translated may go back to the pod it came from, but not whole: it is cut, and
// the segments take the path of every packet the engine emits.)
func (x *Batch) deliverSuper(ep Endpoint, frame *packet.EthernetFrame) error {
	s := &x.sup
	if ep == s.src {
		return nil
	}
	sw, ok := x.writer(ep).(SuperWriter)
	if !ok {
		return nil
	}
	s.whole = true
	if s.dry {
		return nil
	}
	return sw.AddSuper(s.h, frame)
}

// sendOverlay sends frame through the overlay: held in the batch until Flush.
func (x *Batch) sendOverlay(frame *packet.EthernetFrame, dstIP net.IP) error {
	if x.sup.on {
		return nil
	}
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
	return b.processFrames(srcEP, frames, false)
}

// ProcessTrustedFrames is ProcessFrames for frames whose transport checksums the caller
// knows to be right, such as frames it has just completed itself, so that a TAP that joins
// segments need not check them again.
func (b *Bridge) ProcessTrustedFrames(srcEP Endpoint, frames [][]byte) error {
	return b.processFrames(srcEP, frames, true)
}

// ProcessSuperFrame is ProcessFrame for raw, a TCP super-frame that the virtio header h
// describes, which the bridge delivers whole if its routing would deliver the frame, with
// no change but to its MAC addresses, to one endpoint that takes super-frames (see
// SuperWriter), or which the filter drops, or which is for the Service engine: the engine
// translates it as one packet, in place, and the bridge delivers it whole to one endpoint
// that takes super-frames or cuts it and routes the segments. Then whole is true and err
// is the error of the delivery. If not, nothing has been done with the frame, and it is
// for the caller to cut it and process the segments, which does what is to be done with
// them.
func (b *Bridge) ProcessSuperFrame(srcEP Endpoint, h packet.VnetHeader, raw []byte) (whole bool, err error) {
	return b.routeSuper(srcEP, h, raw, false)
}

// PassesWhole reports whether ProcessSuperFrame would take the frame whole now (deliver it,
// or give it to the Service engine), without the filter and without doing anything with
// it: the engine's state is not touched. The decision is the routing's, so that the
// caller can cut a frame that will not go whole where it is cheapest to.
func (b *Bridge) PassesWhole(srcEP Endpoint, raw []byte) bool {
	whole, _ := b.routeSuper(srcEP, packet.VnetHeader{}, raw, true)
	return whole
}

func (b *Bridge) routeSuper(srcEP Endpoint, h packet.VnetHeader, raw []byte, dry bool) (bool, error) {
	x := b.NewBatch()
	defer x.Release()
	x.sup = superFrame{on: true, dry: dry, src: srcEP, h: h, raw: raw}
	err := x.processFrame(srcEP, raw, false)
	if !x.sup.whole {
		return false, nil // the routing's errors come again with the segments
	}
	if ferr := x.Flush(); err == nil {
		err = ferr
	}
	return true, err
}

func (b *Bridge) processFrames(srcEP Endpoint, frames [][]byte, trusted bool) error {
	x := b.NewBatch()
	defer x.Release()
	var first error
	for _, raw := range frames {
		if err := x.processFrame(srcEP, raw, trusted); err != nil && first == nil {
			first = err
		}
	}
	if err := x.Flush(); err != nil && first == nil {
		first = err
	}
	return first
}
