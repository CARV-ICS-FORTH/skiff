package packet

import "sync"

// bufferCap fits the largest thing in a Buffer: a TAP read (a virtio header and a
// frame of up to 64 KiB), a VXLAN header and such a frame, or a Coalescer's frame.
const bufferCap = VnetHeaderLen + EthernetHeaderLen + 65535

// Buffer is a reusable scratch slice for encoding one frame. It is for writes that
// finish before they return, so that a frame is not copied into a new allocation.
//
// A Buffer comes from a pool (GetBuffer), or from memory that its owner manages, such as
// the registered memory the HCA receives RDMA messages into (NewOwnedBuffer): releasing it
// then gives it back to its owner instead of the pool. Whoever holds a buffer does not
// need to know which it is.
type Buffer struct {
	B       []byte
	held    bool
	pooled  *pooledBuffer // the pool's, nil for an owned buffer
	release func(*Buffer) // an owned buffer's owner takes it back
}

// pooledBuffer is a pool's buffer with its array, so that a new one is one allocation.
type pooledBuffer struct {
	Buffer
	mem [bufferCap]byte
}

var bufferPool = sync.Pool{New: func() any {
	pb := new(pooledBuffer)
	pb.B = pb.mem[:0]
	pb.pooled = pb
	return pb
}}

// GetBuffer returns an empty Buffer that nobody else holds until it is released.
func GetBuffer() *Buffer {
	pb := bufferPool.Get().(*pooledBuffer)
	pb.held = true
	return &pb.Buffer
}

// NewOwnedBuffer makes a Buffer over memory that its owner manages: Release calls release,
// which takes the buffer back, instead of returning it to the pool. It is not held until
// Hold; an owner makes it once for each piece of its memory and hands it out again and again.
func NewOwnedBuffer(release func(*Buffer)) *Buffer {
	return &Buffer{release: release}
}

// Hold marks an owned buffer as handed out, its slice set by the owner, so that the next
// Release gives it back once.
func (b *Buffer) Hold(data []byte) {
	b.B = data
	b.held = true
}

// Release hands the buffer back; its slice must not be used afterwards. Releasing a
// buffer that is not held does nothing, so it cannot be handed out twice.
func (b *Buffer) Release() {
	if !b.held {
		return
	}
	b.held = false
	if b.release != nil {
		b.release(b)
		return
	}
	if cap(b.B) > bufferCap {
		return // grew past the standard size: let it go rather than keep it
	}
	b.B = b.B[:0]
	bufferPool.Put(b.pooled)
}
