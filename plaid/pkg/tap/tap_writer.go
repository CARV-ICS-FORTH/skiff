package tap

import (
	"sync"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
)

// tapWriter is the bridge.FrameWriter of a TAP with offloads: it holds the frames a
// batch delivers in a Coalescer, which joins consecutive TCP segments of a flow, and
// writes each result with one write.
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
	err := w.Flush()
	if serr := w.c.Start(frame); err == nil {
		err = serr
	}
	return err
}

// Flush writes the frame held.
func (w *tapWriter) Flush() error {
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
	_, err := w.t.file.Write(b)
	return err
}

func (w *tapWriter) Release() {
	w.c.Discard() // a frame held and not flushed is dropped, not given to the writer's next user
	w.t = nil
	writerPool.Put(w)
}
