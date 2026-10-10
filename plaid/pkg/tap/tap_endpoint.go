package tap

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
)

// TapEndpoint implements bridge.Endpoint for a pod's TAP interface file descriptor. With offloads
// on (NewTapEndpoint) the pod's stack hands over TCP segments of up to 64 KiB with their
// checksums not done: the endpoint cuts them into complete MTU-sized frames as it reads, so that
// the bridge sees only those, and joins consecutive segments of a flow that the bridge delivers
// together (NewWriter) into one segment for the pod. A TAP without a virtio header is read and
// written as plain frames. With SetPipeline the reading and processing, and the writing, are
// split between goroutines; with SetPassthrough a TSO segment that the bridge would deliver as
// it is to another TAP with offloads is not cut. The README (Offloads) describes the whole.
type TapEndpoint struct {
	epID       string
	epName     string
	epIP       net.IP
	epMAC      net.HardwareAddr
	file       *os.File
	closeOnce  sync.Once
	closedChan chan struct{}

	vnet        bool        // frames have a virtio header
	offload     bool        // and the pod may use offloads
	passthrough bool        // and its TSO segments go whole where the bridge can
	vw          *vnetWriter // writes frames with an empty header; nil without vnet
	rb          readBatch   // what ReadFrames reads into

	pipeline bool                // the read loop is two stages
	probe    *pollProbe          // tells whether another frame is waiting in the TAP
	wq       chan *packet.Buffer // the frames for the writer goroutine, with their virtio headers; nil if it is not running

	// The batches handed to the processing stage and the writes handed to the writer
	// goroutine that are not finished yet. What goes in place may go only when its count
	// is zero: see readLoop and enqueue.
	rpending, wpending atomic.Int64
}

// The sizes of the queues between the stages. A read batch and a write each hold up to 64
// KiB. The queues are deep enough for the stages to run a few batches apart, which hides
// the jitter between them, and shallow enough that a stream holds little behind the slower
// stage (at most 512 KiB to the bridge and 1 MiB to the TAP): more would add delay, not
// throughput.
const (
	readQueueLen  = 8
	writeQueueLen = 16
)

// A readBatch is what one read of the TAP leaves: the buffer it went to, and the frames
// cut from it, which point into that buffer or into the segmenter's.
type readBatch struct {
	rd      []byte
	seg     packet.Segmenter
	one     [1][]byte
	frames  [][]byte
	trusted bool // plaidd has completed the frames' checksums
	super   bool // frames is the one TSO segment that the virtio header h describes, not cut
	h       packet.VnetHeader
}

var readBatchPool = sync.Pool{New: func() any { return new(readBatch) }}

// inFlight counts the pooled read batches and write buffers that the pipelines hold; it
// is zero when all of them have been returned. The tests use it.
var inFlight atomic.Int64

func getReadBatch() *readBatch {
	inFlight.Add(1)
	return readBatchPool.Get().(*readBatch)
}

func putReadBatch(rb *readBatch) {
	inFlight.Add(-1)
	readBatchPool.Put(rb)
}

// OffloadStats counts the work of the offloads of all TAP endpoints.
type OffloadStats struct {
	ReadSuperFrames   uint64 // TSO segments the pods handed over
	ReadSegments      uint64 // the frames they were cut into
	ReadDropped       uint64 // frames whose virtio header did not describe them, dropped
	WriteSuperFrames  uint64 // writes that joined several segments
	WriteSegments     uint64 // the segments in them
	PassedSuperFrames uint64 // TSO segments written whole to another TAP, not cut
	PassedBytes       uint64 // the bytes of their frames
}

// PipelineStats counts how often a stage of a pipeline had to wait for the next one.
type PipelineStats struct {
	ReadInline     uint64 // batches the read stage ran through the bridge itself
	ReadQueued     uint64 // batches it handed to the processing stage
	ReadQueueFull  uint64 // of those, how many found the queue to it full
	WriteInline    uint64 // writes that the bridge's delivery made itself
	WriteQueued    uint64 // writes it handed to the TAP's writer
	WriteQueueFull uint64 // of those, how many found the queue to the writer full
}

var stats struct {
	readSuper, readSegments, readDropped, writeSuper, writeSegments, passSuper, passBytes atomic.Uint64
	readInline, readQueued, readQueueFull, writeInline, writeQueued, writeQueueFull       atomic.Uint64
}

// Pipeline returns the counters of the TAP pipelines.
func Pipeline() PipelineStats {
	return PipelineStats{
		ReadInline: stats.readInline.Load(), ReadQueued: stats.readQueued.Load(), ReadQueueFull: stats.readQueueFull.Load(),
		WriteInline: stats.writeInline.Load(), WriteQueued: stats.writeQueued.Load(), WriteQueueFull: stats.writeQueueFull.Load(),
	}
}

// Offloads returns the counters of the TAP offloads.
func Offloads() OffloadStats {
	return OffloadStats{
		ReadSuperFrames:   stats.readSuper.Load(),
		ReadSegments:      stats.readSegments.Load(),
		ReadDropped:       stats.readDropped.Load(),
		WriteSuperFrames:  stats.writeSuper.Load(),
		WriteSegments:     stats.writeSegments.Load(),
		PassedSuperFrames: stats.passSuper.Load(),
		PassedBytes:       stats.passBytes.Load(),
	}
}

var offloadWarning sync.Once

// NewTapEndpoint creates an Endpoint backed by an open TAP file descriptor. With
// offload, it asks the TAP for checksum and TCP segmentation offloads if it has a
// virtio header; if the kernel refuses, it says so once and goes on without.
func NewTapEndpoint(id, name string, ip net.IP, mac net.HardwareAddr, file *os.File, offload bool) *TapEndpoint {
	t := &TapEndpoint{
		epID:       id,
		epName:     name,
		epIP:       ip,
		epMAC:      mac,
		file:       file,
		closedChan: make(chan struct{}),
	}
	if hasVnetHdr(file) {
		t.vw = newVnetWriter(file)
	}
	t.vnet = t.vw != nil
	if t.vnet && offload {
		if err := enableOffloads(file); err != nil {
			offloadWarning.Do(func() {
				fmt.Printf("[plaidd/tap] TAP offloads are not available (%v): continuing without them\n", err)
			})
		} else {
			t.offload = true
		}
	}
	return t
}

func (t *TapEndpoint) ID() string            { return t.epID }
func (t *TapEndpoint) Name() string          { return t.epName }
func (t *TapEndpoint) IP() net.IP            { return t.epIP }
func (t *TapEndpoint) MAC() net.HardwareAddr { return t.epMAC }
func (t *TapEndpoint) File() *os.File        { return t.file }

// Offload reports whether the TAP has its offloads on.
func (t *TapEndpoint) Offload() bool { return t.offload }

// SetPipeline makes the endpoint's read loop two stages and, if it has offloads, gives it
// a goroutine that writes what the bridge delivers to it. It is called before the
// endpoint is added to a bridge or its read loop is started, and only once. Close stops
// the goroutines.
func (t *TapEndpoint) SetPipeline() {
	t.pipeline = true
	if t.offload {
		t.wq = make(chan *packet.Buffer, writeQueueLen)
		go t.writeLoop()
	}
}

// SetPassthrough makes the endpoint pass the TSO segments of its pod whole to the TAPs
// with offloads that the bridge delivers them to, instead of cutting them. It has an
// effect only if the endpoint has offloads. It is called before the read loop is started.
func (t *TapEndpoint) SetPassthrough() { t.passthrough = t.offload }

// Pipeline reports whether the endpoint was given a pipeline.
func (t *TapEndpoint) Pipeline() bool { return t.pipeline }

// errClosed returns the error for a write to a closed endpoint, or nil if it is open.
func (t *TapEndpoint) errClosed() error {
	select {
	case <-t.closedChan:
		return fmt.Errorf("endpoint %s is closed", t.epID)
	default:
		return nil
	}
}

// Write sends an Ethernet frame into the TAP device (received inside the pod netns).
func (t *TapEndpoint) Write(frame []byte) error {
	if err := t.errClosed(); err != nil {
		return err
	}

	if t.vw != nil {
		return t.vw.write(frame)
	}
	_, err := t.file.Write(frame)
	return err
}

// ReadFrames waits for the pod to send and returns the Ethernet frames of what it sent:
// one, or the segments of a TSO segment. They are complete, with their checksums done,
// and valid until the next call. A packet that the TAP's virtio header does not describe
// is dropped and counted: the result is empty and not an error.
func (t *TapEndpoint) ReadFrames() ([][]byte, error) { return t.readFrames(&t.rb, nil) }

// readFrames is ReadFrames into the buffers of rb, which also records whether the frames'
// checksums are known to be right: they are if the pod left them to us and we did them.
// If b is not nil and the endpoint passes TSO segments through, a segment that b would
// deliver whole is not cut: the result is the segment, and rb says so (see forward).
func (t *TapEndpoint) readFrames(rb *readBatch, b *bridge.Bridge) ([][]byte, error) {
	if rb.rd == nil {
		rb.rd = make([]byte, packet.VnetHeaderLen+65535)
	}
	rb.trusted, rb.super = false, false
	n, err := t.file.Read(rb.rd)
	if err != nil || n == 0 {
		return nil, err
	}
	if !t.vnet {
		rb.one[0] = rb.rd[:n]
		return rb.one[:], nil
	}
	if n < packet.VnetHeaderLen {
		stats.readDropped.Add(1)
		return nil, nil
	}
	h, frame := packet.ParseVnetHeader(rb.rd), rb.rd[packet.VnetHeaderLen:n]
	if b != nil && t.passthrough && h.IsTSO() && packet.CheckTSO(h, frame) == nil && b.PassesWhole(t, frame) {
		rb.one[0], rb.h, rb.super = frame, h, true
		return rb.one[:], nil
	}
	return t.cut(rb, h, frame), nil
}

// cut is the Segmenter's cut of a read, with the counters.
func (t *TapEndpoint) cut(rb *readBatch, h packet.VnetHeader, frame []byte) [][]byte {
	frames, err := rb.seg.Segment(h, frame)
	switch {
	case err != nil:
		stats.readDropped.Add(1)
	case h.GSOType != packet.VnetGSONone:
		stats.readSuper.Add(1)
		stats.readSegments.Add(uint64(len(frames)))
	}
	rb.trusted = err == nil && (h.GSOType != packet.VnetGSONone || h.Flags&packet.VnetNeedsCsum != 0)
	return frames
}

// StartReadLoop starts a goroutine reading Ethernet frames from the TAP device and processing them through the Bridge.
// If the TAP fails it removes the endpoint from the bridge.
func (t *TapEndpoint) StartReadLoop(ctx context.Context, b *bridge.Bridge) {
	t.StartReadLoopFunc(ctx, b, func(error) { _ = b.RemoveEndpoint(t.epID) })
}

// StartReadLoopFunc is StartReadLoop with fail called, once, with the error that ended the
// loop unless the endpoint was closed or ctx is done.
func (t *TapEndpoint) StartReadLoopFunc(ctx context.Context, b *bridge.Bridge, fail func(error)) {
	var q chan *readBatch
	if t.pipeline {
		q = make(chan *readBatch, readQueueLen)
		t.probe = newPollProbe(t.file)
		go t.processLoop(ctx, b, q)
	}
	go t.readLoop(ctx, b, fail, q)
}

func (t *TapEndpoint) stopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return t.errClosed() != nil
	}
}

// readLoop reads the TAP until it fails or stops. With q it hands batches to processLoop
// (waiting when q is full, so that a slow bridge holds up the pod's TAP and no frame is
// dropped here) and closes q when it returns.
//
// It runs a batch itself unless frames are waiting in the TAP, in which case the next read
// can overlap with the processing, or earlier batches are still with processLoop. The
// second condition keeps the order. rpending counts the batches handed over and not yet
// processed: it is raised here, by the only goroutine that hands over, before a batch goes
// into q, and lowered by processLoop after the batch is processed. So if it is zero here,
// every batch read before this one has been processed, and none is handed over until this
// goroutine does it, after this batch: this one runs alone, between its predecessors and
// its successors. Otherwise the batch goes into q behind the others.
func (t *TapEndpoint) readLoop(ctx context.Context, b *bridge.Bridge, fail func(error), q chan<- *readBatch) {
	var rb *readBatch // the batch that the next read goes to; with q, one that this goroutine holds
	if q != nil {
		defer func() {
			close(q)
			if rb != nil {
				putReadBatch(rb)
			}
		}()
	}
	for !t.stopped(ctx) {
		if q == nil {
			rb = &t.rb
		} else if rb == nil {
			rb = getReadBatch()
		}
		frames, err := t.readFrames(rb, b)
		rb.frames = frames
		switch {
		case err != nil:
			if !t.stopped(ctx) {
				// TAP device closed or failed (e.g. netns deleted).
				fail(err)
			}
			return
		case len(frames) == 0:
		case q == nil:
			t.forward(b, rb)
		case t.rpending.Load() == 0 && !t.probe.poll():
			stats.readInline.Add(1)
			t.forward(b, rb)
		default:
			t.rpending.Add(1)
			stats.readQueued.Add(1)
			select {
			case q <- rb:
			default:
				stats.readQueueFull.Add(1)
				select {
				case q <- rb:
				case <-t.closedChan:
					t.rpending.Add(-1)
					return
				case <-ctx.Done():
					t.rpending.Add(-1)
					return
				}
			}
			rb = nil
		}
	}
}

// processLoop runs the batches from readLoop through the bridge, one after the other. A
// batch that comes after the endpoint stopped is dropped. It returns when q is closed
// and empty.
func (t *TapEndpoint) processLoop(ctx context.Context, b *bridge.Bridge, q <-chan *readBatch) {
	for rb := range q {
		if !t.stopped(ctx) {
			t.forward(b, rb)
		}
		t.rpending.Add(-1) // only now: see readLoop
		putReadBatch(rb)
	}
}

// forward processes the frames of a read through the bridge. A TSO segment that was not
// cut is delivered whole; if the bridge will not now (readFrames asked a moment ago, and
// something changed), it is cut here and its segments go through the bridge.
func (t *TapEndpoint) forward(b *bridge.Bridge, rb *readBatch) {
	if rb.super {
		if whole, _ := b.ProcessSuperFrame(t, rb.h, rb.frames[0]); whole {
			return
		}
		rb.frames = t.cut(rb, rb.h, rb.frames[0])
	}
	if rb.trusted {
		_ = b.ProcessTrustedFrames(t, rb.frames)
	} else {
		_ = b.ProcessFrames(t, rb.frames)
	}
}

// enqueue hands a buffer holding a frame with its virtio header to the writer goroutine
// and takes over its release. If the queue is full it waits, which holds up the caller
// until the TAP has taken some of what is queued: for frames from the overlay that is its
// worker, and the overlay's receive queue absorbs the burst meanwhile and counts what it
// has no room for, as for any slow bridge. Nothing is dropped here.
//
// wpending counts the writes handed over and not yet finished: raised here before the
// buffer goes into the queue, lowered by the writer after the write (or when the buffer is
// released unwritten). A caller that writes itself does so only when it is zero, and then
// every write this caller handed over earlier is finished: a caller's writes stay in the
// order it made them. The writes of different callers have no order, which they never
// had: a flow's frames all come from one caller at a time.
func (t *TapEndpoint) enqueue(pb *packet.Buffer) error {
	inFlight.Add(1)
	t.wpending.Add(1)
	stats.writeQueued.Add(1)
	select {
	case t.wq <- pb:
	default:
		stats.writeQueueFull.Add(1)
		select {
		case t.wq <- pb:
		case <-t.closedChan:
			t.releaseWrite(pb)
			return t.errClosed()
		}
	}
	// The endpoint may have been closed, and the writer have drained the queue and
	// stopped, before the send: then nobody else would release what was queued.
	if err := t.errClosed(); err != nil {
		t.drainWrites()
		return err
	}
	return nil
}

func (t *TapEndpoint) releaseWrite(pb *packet.Buffer) {
	pb.Release()
	inFlight.Add(-1)
	t.wpending.Add(-1)
}

// drainWrites releases what is queued for the writer without writing it.
func (t *TapEndpoint) drainWrites() {
	for {
		select {
		case pb := <-t.wq:
			t.releaseWrite(pb)
		default:
			return
		}
	}
}

// writeLoop writes the queued frames to the TAP in the order they were queued, until the
// endpoint is closed. Nothing is written after that: the frames left are released.
func (t *TapEndpoint) writeLoop() {
	for {
		select {
		case pb := <-t.wq:
			select {
			case <-t.closedChan:
			default:
				_, _ = t.file.Write(pb.B)
			}
			t.releaseWrite(pb)
		case <-t.closedChan:
			t.drainWrites()
			return
		}
	}
}

// Close closes the TAP device file and stops the endpoint's goroutines: the writer, and
// the stages of the read loop once their read returns (it does at once for a pollable
// file; one that blocks in the kernel returns it with the next frame or error). Frames
// queued for the TAP and not yet written are dropped.
func (t *TapEndpoint) Close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.closedChan)
		if t.file != nil {
			err = t.file.Close()
		}
	})
	return err
}
