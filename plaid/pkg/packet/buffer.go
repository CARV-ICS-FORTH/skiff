package packet

import "sync"

// bufferCap fits the largest thing in a Buffer: a TAP read (a virtio header and a
// frame of up to 64 KiB), a VXLAN header and such a frame, or a Coalescer's frame.
const bufferCap = VnetHeaderLen + EthernetHeaderLen + 65535

// Buffer is a reusable scratch slice for encoding one frame. It is for writes that
// finish before they return, so that a frame is not copied into a new allocation.
type Buffer struct {
	B    []byte
	held bool
	mem  [bufferCap]byte // B's array, so that a new Buffer is one allocation
}

var bufferPool = sync.Pool{New: func() any {
	b := new(Buffer)
	b.B = b.mem[:0]
	return b
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
