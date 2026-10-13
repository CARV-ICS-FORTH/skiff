package tap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"plaid/pkg/api"
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
// it is to another TAP with offloads, to a node that takes it over the overlay, or to the
// Service engine, is not cut. The bubble's TAP can have an MTU above the pods' (SetUplinkMTU),
// and the MSS of the TCP handshakes that cross it is changed (setMSS) for what slirp4netns
// is cheapest to feed. A super-frame that the overlay put together is queued for the writer
// in its own buffer (AddSuperBuffer). A pod's TAP can have several queues (see
// CreateAndConfigureTapQueues), each with its own reader, processing stage and writer, and
// the endpoint is still one pod to the bridge (one MAC, one IP): a frame for the pod is
// written to the queue that the hash of its flow picks (see queueFor), so that a flow's
// frames are written in order. The README (Offloads, Queues of a pod's TAP) describes the
// whole.
type TapEndpoint struct {
	epID       string
	epName     string
	epIP       net.IP
	epMAC      net.HardwareAddr
	queues     []*tapQueue // at least one
	closeOnce  sync.Once
	closedChan chan struct{}

	vnet        bool // frames have a virtio header
	offload     bool // and the pod may use offloads
	passthrough bool // and its TSO segments go whole where the bridge can
	podMTU      int  // the MTU of the pods; with uplinkMTU, the largest IP packet the bridge takes
	uplinkMTU   int  // the MTU of this TAP if it is above the pods', otherwise 0

	pipeline bool // the read loop is two stages

	readHook func(queue int, frames [][]byte) // called with what a read leaves, before it is processed; the tests set it
}

// A tapQueue is one queue of the endpoint's TAP: a file of its own, and the state of the
// stages that read it and write to it.
type tapQueue struct {
	t    *TapEndpoint
	idx  int
	file *os.File
	vw   *vnetWriter // writes frames with an empty header; nil without vnet
	rb   readBatch   // what ReadFrames reads into

	probe *pollProbe          // tells whether another frame is waiting in the TAP
	wq    chan *packet.Buffer // the frames for the writer goroutine, with their virtio headers; nil if it is not running

	// The batches handed to the processing stage and the writes handed to the writer
	// goroutine that are not finished yet. What goes in place may go only when its count
	// is zero: see readLoop and enqueue.
	rpending, wpending atomic.Int64

	writes atomic.Uint64 // the writes to the TAP
}

// MaxQueues is the most queues a TAP is given: more than a pod's stack and the cores of a
// node can use, and what the API takes in one request.
const MaxQueues = api.MaxEndpointFDs

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
	JoinedForUplink   uint64 // pods' TSO segments written to the bubble as one packet
	UplinkSuperFrames uint64 // large TCP frames from the bubble made TSO segments
	MSSRaised         uint64 // handshakes written to the bubble with their MSS raised to the uplink's
	MSSLowered        uint64 // handshakes from the bubble written to pods with their MSS lowered
	Fragmented        uint64 // packets from the bubble fragmented to the pod MTU
	Fragments         uint64 // the fragments they were cut into
	OversizeDropped   uint64 // packets from the bubble above the pod MTU that were dropped
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
	readSuper, readSegments, readDropped, writeSuper, writeSegments, passSuper, passBytes  atomic.Uint64
	joinUplink, uplinkSuper, fragmented, fragments, oversizeDropped, mssRaised, mssLowered atomic.Uint64
	readInline, readQueued, readQueueFull, writeInline, writeQueued, writeQueueFull        atomic.Uint64
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
		JoinedForUplink:   stats.joinUplink.Load(),
		UplinkSuperFrames: stats.uplinkSuper.Load(),
		MSSRaised:         stats.mssRaised.Load(),
		MSSLowered:        stats.mssLowered.Load(),
		Fragmented:        stats.fragmented.Load(),
		Fragments:         stats.fragments.Load(),
		OversizeDropped:   stats.oversizeDropped.Load(),
	}
}

var offloadWarning sync.Once

// NewTapEndpoint creates an Endpoint backed by an open TAP file descriptor. With
// offload, it asks the TAP for checksum and TCP segmentation offloads if it has a
// virtio header; if the kernel refuses, it says so once and goes on without.
func NewTapEndpoint(id, name string, ip net.IP, mac net.HardwareAddr, file *os.File, offload bool) *TapEndpoint {
	return NewTapEndpointQueues(id, name, ip, mac, []*os.File{file}, offload)
}

// NewTapEndpointQueues is NewTapEndpoint for a TAP with several queues, which files are
// the files of, all attached to the one device. The endpoint owns them.
func NewTapEndpointQueues(id, name string, ip net.IP, mac net.HardwareAddr, files []*os.File, offload bool) *TapEndpoint {
	t := &TapEndpoint{
		epID:       id,
		epName:     name,
		epIP:       ip,
		epMAC:      mac,
		closedChan: make(chan struct{}),
	}
	// The virtio header and the offloads are settings of the device, not of a queue: they
	// are read and set through the first file.
	hdr := hasVnetHdr(files[0])
	for i, f := range files {
		q := &tapQueue{t: t, idx: i, file: f}
		if hdr {
			q.vw = newVnetWriter(f)
		}
		t.queues = append(t.queues, q)
	}
	t.vnet = t.queues[0].vw != nil
	if t.vnet && offload {
		if err := enableOffloads(files[0]); err != nil {
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

// Queues returns the number of queues of the TAP.
func (t *TapEndpoint) Queues() int { return len(t.queues) }

// Offload reports whether the TAP has its offloads on.
func (t *TapEndpoint) Offload() bool { return t.offload }

// SetPipeline makes the endpoint's read loops two stages and, if it has offloads, gives
// each queue a goroutine that writes what the bridge delivers to it. It is called before
// the endpoint is added to a bridge or its read loops are started, and only once. Close
// stops the goroutines.
func (t *TapEndpoint) SetPipeline() {
	t.pipeline = true
	if t.offload {
		for _, q := range t.queues {
			q.wq = make(chan *packet.Buffer, writeQueueLen)
			go q.writeLoop()
		}
	}
}

// SetPassthrough makes the endpoint pass the TSO segments of its pod whole to the TAPs
// with offloads that the bridge delivers them to, instead of cutting them. It has an
// effect only if the endpoint has offloads. It is called before the read loop is started.
func (t *TapEndpoint) SetPassthrough() { t.passthrough = t.offload }

// SetUplinkMTU tells the endpoint that its TAP has the MTU uplink, above the pod MTU pod
// (it does nothing if it is not). It is called before the read loop is started.
func (t *TapEndpoint) SetUplinkMTU(pod, uplink int) {
	if uplink > pod {
		t.podMTU, t.uplinkMTU = pod, uplink
	}
}

// Pipeline reports whether the endpoint was given a pipeline.
func (t *TapEndpoint) Pipeline() bool { return t.pipeline }

// queueFor returns the queue that a frame is written to: the one that the hash of its flow
// picks, the first for what has no flow (see packet.FlowIndex).
func (t *TapEndpoint) queueFor(etherType uint16, ip []byte) *tapQueue {
	return t.queues[packet.FlowIndex(etherType, ip, len(t.queues))]
}

// queueForFrame is queueFor for an Ethernet frame as bytes.
func (t *TapEndpoint) queueForFrame(frame []byte) *tapQueue {
	if len(t.queues) == 1 || len(frame) < packet.EthernetHeaderLen {
		return t.queues[0]
	}
	return t.queueFor(binary.BigEndian.Uint16(frame[12:]), frame[packet.EthernetHeaderLen:])
}

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

	// The caller's frame may go to other endpoints as well (a flood): it is not changed.
	if t.uplinkMTU > 0 {
		if c, ok := packet.RaiseSYNMSSCopy(frame, t.uplinkMTU); ok {
			stats.mssRaised.Add(1)
			frame = c
		}
	}
	q := t.queueForFrame(frame)
	q.writes.Add(1)
	if q.vw != nil {
		return q.vw.write(frame)
	}
	_, err := q.file.Write(frame)
	return err
}

// podSYNMTU is the MTU whose MSS (1460, or 1440 for IPv6) a pod with offloads is told its
// peer in the bubble takes. slirp4netns has no TCP window scaling (a window of 64 KB), and
// a pod that sends segments of its own large MTU puts many single segments into it besides
// its TSO segments; at 1460 the pod sends the same bytes as TSO segments, which are joined
// into one packet each for the bubble.
const podSYNMTU = 1500

// setMSS changes the MSS of a TCP handshake in frame, which is about to be written to this
// TAP and is this writer's own copy, if it should be told another one; src is where the
// frame came in, nil if it is not known. The MSS the two ends announce to each other is
// not what crosses a wire, as plaidd joins and cuts the segments, so it can choose it:
//
// The bubble is told that the pod takes segments of the uplink MTU, whoever the handshake
// is from, so that its stack sends large packets, which become TSO segments of the pod's
// MSS (fitPodMTU): nothing larger than the pod MTU reaches a pod.
//
// A pod with offloads is told no more than podSYNMTU allows if the handshake is from the
// bubble. A pod without offloads is not (it would send that many more packets), nor is one
// on another node (the overlay carries the pods' segments as they are, and never gets here).
//
// The checksum is complete on every path here: a segment whose sum the pod left to us is
// completed when it is read, and a TSO segment, whose sum is partial, is never a handshake.
func (t *TapEndpoint) setMSS(frame []byte, src bridge.Endpoint) {
	switch {
	case t.uplinkMTU > 0:
		if packet.RaiseSYNMSS(frame, t.uplinkMTU) {
			stats.mssRaised.Add(1)
		}
	case t.offload:
		if b, ok := src.(*TapEndpoint); ok && b.uplinkMTU > 0 && packet.LowerSYNMSS(frame, podSYNMTU) {
			stats.mssLowered.Add(1)
		}
	}
}

// ReadFrames waits for the pod to send on its first queue and returns the Ethernet frames
// of what it sent: one, or the segments of a TSO segment. They are complete, with their
// checksums done, and valid until the next call. A packet that the TAP's virtio header
// does not describe is dropped and counted: the result is empty and not an error.
func (t *TapEndpoint) ReadFrames() ([][]byte, error) {
	q := t.queues[0]
	return q.readFrames(&q.rb, nil)
}

// readFrames is ReadFrames into the buffers of rb, which also records whether the frames'
// checksums are known to be right: they are if the pod left them to us and we did them.
// If b is not nil and the endpoint passes TSO segments through, a segment that b would
// deliver whole is not cut: the result is the segment, and rb says so (see forward).
func (q *tapQueue) readFrames(rb *readBatch, b *bridge.Bridge) ([][]byte, error) {
	t := q.t
	if rb.rd == nil {
		// A frame of up to 65535 bytes, which holds the largest TSO segment and the
		// bubble's largest packet (an uplink MTU of 65520) with its Ethernet header.
		rb.rd = make([]byte, packet.VnetHeaderLen+65535)
	}
	rb.trusted, rb.super = false, false
	n, err := q.file.Read(rb.rd)
	if err != nil || n == 0 {
		return nil, err
	}
	h, frame := packet.VnetHeader{}, rb.rd[:n]
	if t.vnet {
		if n < packet.VnetHeaderLen {
			stats.readDropped.Add(1)
			return nil, nil
		}
		h, frame = packet.ParseVnetHeader(rb.rd), rb.rd[packet.VnetHeaderLen:n]
	}
	if t.podMTU > 0 && len(frame) > packet.EthernetHeaderLen+t.podMTU {
		var frames [][]byte
		var done bool
		if h, frames, done = t.fitPodMTU(rb, h, frame); done {
			return frames, nil
		}
	}
	if !t.vnet && h.GSOType == packet.VnetGSONone {
		rb.one[0] = frame
		return rb.one[:], nil
	}
	if b != nil && t.passthrough && h.IsTSO() && packet.CheckTSO(h, frame) == nil && b.PassesWhole(t, frame) {
		rb.one[0], rb.h, rb.super = frame, h, true
		return rb.one[:], nil
	}
	return t.cut(rb, h, frame), nil
}

// fitPodMTU deals with a frame that is larger than the pod MTU, which only the bubble's
// TAP can return: a TCP frame is made a TSO segment of pod-sized segments (h is the header
// to cut it with), and the others are fragmented, which is then done: frames are the
// fragments, none if the packet was dropped.
func (t *TapEndpoint) fitPodMTU(rb *readBatch, h packet.VnetHeader, frame []byte) (_ packet.VnetHeader, frames [][]byte, done bool) {
	th, err := packet.TSOFor(h, frame, t.podMTU)
	switch {
	case err == nil:
		if h.GSOType == packet.VnetGSONone {
			stats.uplinkSuper.Add(1)
		}
		return th, nil, false
	case !errors.Is(err, packet.ErrNotTCP):
		stats.readDropped.Add(1)
		return h, nil, true
	}
	frames, err = rb.seg.Fragment(h, frame, t.podMTU)
	switch {
	case err == nil:
		stats.fragmented.Add(1)
		stats.fragments.Add(uint64(len(frames)))
	case errors.Is(err, packet.ErrTooLarge):
		stats.oversizeDropped.Add(1)
	default:
		stats.readDropped.Add(1)
	}
	return h, frames, true
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

// StartReadLoop starts the goroutines reading Ethernet frames from the TAP device and processing them through the Bridge.
// If the TAP fails it removes the endpoint from the bridge.
func (t *TapEndpoint) StartReadLoop(ctx context.Context, b *bridge.Bridge) {
	t.StartReadLoopFunc(ctx, b, func(error) { _ = b.RemoveEndpoint(t.epID) })
}

// StartReadLoopFunc is StartReadLoop with fail called, once, with the error that ended a
// loop unless the endpoint was closed or ctx is done. There is a loop for each queue of
// the TAP, and they run alone: the frames of a flow come in on one queue, in order, and
// the frames of different flows have no order.
func (t *TapEndpoint) StartReadLoopFunc(ctx context.Context, b *bridge.Bridge, fail func(error)) {
	var once sync.Once
	failOnce := func(err error) { once.Do(func() { fail(err) }) }
	for _, q := range t.queues {
		var rq chan *readBatch
		if t.pipeline {
			rq = make(chan *readBatch, readQueueLen)
			q.probe = newPollProbe(q.file)
			go q.processLoop(ctx, b, rq)
		}
		go q.readLoop(ctx, b, failOnce, rq)
	}
}

func (t *TapEndpoint) stopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return t.errClosed() != nil
	}
}

// readLoop reads the queue until it fails or stops. With rq it hands batches to processLoop
// (waiting when rq is full, so that a slow bridge holds up the pod's TAP and no frame is
// dropped here) and closes rq when it returns.
//
// It runs a batch itself unless frames are waiting in the TAP, in which case the next read
// can overlap with the processing, or earlier batches are still with processLoop. The
// second condition keeps the order. rpending counts the batches handed over and not yet
// processed: it is raised here, by the only goroutine that hands over, before a batch goes
// into q, and lowered by processLoop after the batch is processed. So if it is zero here,
// every batch read before this one has been processed, and none is handed over until this
// goroutine does it, after this batch: this one runs alone, between its predecessors and
// its successors. Otherwise the batch goes into q behind the others.
func (q *tapQueue) readLoop(ctx context.Context, b *bridge.Bridge, fail func(error), rq chan<- *readBatch) {
	t := q.t
	var rb *readBatch // the batch that the next read goes to; with rq, one that this goroutine holds
	if rq != nil {
		defer func() {
			close(rq)
			if rb != nil {
				putReadBatch(rb)
			}
		}()
	}
	for !t.stopped(ctx) {
		if rq == nil {
			rb = &q.rb
		} else if rb == nil {
			rb = getReadBatch()
		}
		frames, err := q.readFrames(rb, b)
		rb.frames = frames
		if t.readHook != nil && len(frames) != 0 {
			t.readHook(q.idx, frames)
		}
		switch {
		case err != nil:
			if !t.stopped(ctx) {
				// TAP device closed or failed (e.g. netns deleted).
				fail(err)
			}
			return
		case len(frames) == 0:
		case rq == nil:
			t.forward(b, rb)
		case q.rpending.Load() == 0 && !q.probe.poll():
			stats.readInline.Add(1)
			t.forward(b, rb)
		default:
			q.rpending.Add(1)
			stats.readQueued.Add(1)
			select {
			case rq <- rb:
			default:
				stats.readQueueFull.Add(1)
				select {
				case rq <- rb:
				case <-t.closedChan:
					q.rpending.Add(-1)
					return
				case <-ctx.Done():
					q.rpending.Add(-1)
					return
				}
			}
			rb = nil
		}
	}
}

// processLoop runs the batches from readLoop through the bridge, one after the other. A
// batch that comes after the endpoint stopped is dropped. It returns when rq is closed
// and empty.
func (q *tapQueue) processLoop(ctx context.Context, b *bridge.Bridge, rq <-chan *readBatch) {
	for rb := range rq {
		if !q.t.stopped(ctx) {
			q.t.forward(b, rb)
		}
		q.rpending.Add(-1) // only now: see readLoop
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

// enqueue hands a buffer holding a frame with its virtio header to the queue's writer
// goroutine and takes over its release. If the queue is full it waits, which holds up the caller
// until the TAP has taken some of what is queued: for frames from the overlay that is one of
// its workers, and that worker's receive queue absorbs the burst meanwhile and counts what
// it has no room for, as for any slow bridge. Nothing is dropped here.
//
// wpending counts the writes handed over and not yet finished: raised here before the
// buffer goes into the queue, lowered by the writer after the write (or when the buffer is
// released unwritten). A caller that writes itself does so only when it is zero, and then
// every write this caller handed over earlier is finished: a caller's writes stay in the
// order it made them. The writes of different callers have no order, which they never
// had: a flow's frames all come from one caller at a time (the overlay's workers are
// callers too, and the sockets the kernel spreads a node's overlay traffic over keep each
// flow on one of them).
func (q *tapQueue) enqueue(pb *packet.Buffer) error {
	t := q.t
	inFlight.Add(1)
	q.wpending.Add(1)
	stats.writeQueued.Add(1)
	select {
	case q.wq <- pb:
	default:
		stats.writeQueueFull.Add(1)
		select {
		case q.wq <- pb:
		case <-t.closedChan:
			q.releaseWrite(pb)
			return t.errClosed()
		}
	}
	// The endpoint may have been closed, and the writer have drained the queue and
	// stopped, before the send: then nobody else would release what was queued.
	if err := t.errClosed(); err != nil {
		q.drainWrites()
		return err
	}
	return nil
}

func (q *tapQueue) releaseWrite(pb *packet.Buffer) {
	pb.Release()
	inFlight.Add(-1)
	q.wpending.Add(-1)
}

// drainWrites releases what is queued for the writer without writing it.
func (q *tapQueue) drainWrites() {
	for {
		select {
		case pb := <-q.wq:
			q.releaseWrite(pb)
		default:
			return
		}
	}
}

// write writes a frame with its virtio header to the queue.
func (q *tapQueue) write(b []byte) error {
	q.writes.Add(1)
	_, err := q.file.Write(b)
	return err
}

// writeLoop writes the queued frames to the TAP in the order they were queued, until the
// endpoint is closed. Nothing is written after that: the frames left are released.
func (q *tapQueue) writeLoop() {
	for {
		select {
		case pb := <-q.wq:
			select {
			case <-q.t.closedChan:
			default:
				_ = q.write(pb.B)
			}
			q.releaseWrite(pb)
		case <-q.t.closedChan:
			q.drainWrites()
			return
		}
	}
}

// Close closes the TAP device's files and stops the endpoint's goroutines: the writers, and
// the stages of the read loops once their reads return (they do at once for a pollable
// file; one that blocks in the kernel returns it with the next frame or error). Frames
// queued for the TAP and not yet written are dropped.
func (t *TapEndpoint) Close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.closedChan)
		for _, q := range t.queues {
			if q.file != nil {
				err = errors.Join(err, q.file.Close())
			}
		}
	})
	return err
}
