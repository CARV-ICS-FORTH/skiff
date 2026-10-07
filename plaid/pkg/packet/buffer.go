package packet

import "sync"

// bufferCap fits a VXLAN header and the largest frame a TAP read returns.
const bufferCap = VXLANHeaderLen + 65535

// Buffer is a reusable scratch slice for encoding one frame. It is for writes that
// finish before they return, so that a frame is not copied into a new allocation.
type Buffer struct {
	B    []byte
	held bool
}

var bufferPool = sync.Pool{New: func() any {
	return &Buffer{B: make([]byte, 0, bufferCap)}
}}

// GetBuffer returns an empty Buffer that nobody else holds until it is released.
func GetBuffer() *Buffer {
	b := bufferPool.Get().(*Buffer)
	b.held = true
	return b
}

// Release hands the buffer back; its slice must not be used afterwards. Releasing a
// buffer that is not held does nothing, so it cannot be handed out twice.
func (b *Buffer) Release() {
	if !b.held {
		return
	}
	b.held = false
	if cap(b.B) > bufferCap {
		return // grew past the standard size: let it go rather than keep it
	}
	b.B = b.B[:0]
	bufferPool.Put(b)
}
