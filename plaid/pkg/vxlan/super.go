package vxlan

import (
	"encoding/binary"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"plaid/pkg/packet"
)

// The chunk header is described in the package comment.
const (
	superVersion = 1
	chunkHdrLen  = 16
	// maxSuperFrame is the largest frame a chunk header may state: an Ethernet header and
	// the largest IP packet.
	maxSuperFrame = packet.EthernetHeaderLen + 65535
	// What a lane holds in reassembly at most, and how long a frame may take to arrive.
	maxReassemblies = 64
	reassemblyAge   = int64(time.Second)
	// How long a node's mark is good for. The marks come with every datagram it sends, and
	// a stream has an answer going the other way all the time, so a peer that has stopped
	// sending ceases to be one soon, and a short time bounds what is lost to a node that
	// restarted without the feature.
	peerAge = int64(2 * time.Second)
	// maxPeers bounds the table: a datagram's source address can be forged.
	maxPeers = 1024
)

// chunk is a parsed chunk header and the frame bytes that follow it.
type chunk struct {
	id, total, offset uint32
	h                 packet.VnetHeader // the first chunk's only
	data              []byte
}

// parseChunk parses the chunk that follows the VXLAN header in d, a whole datagram.
func parseChunk(d []byte) (c chunk, ok bool) {
	b := d[packet.VXLANHeaderLen:]
	if len(b) < chunkHdrLen+1 || b[0] != superVersion || b[1]|b[2]|b[3] != 0 {
		return c, false
	}
	be := binary.BigEndian
	c.id, c.total, c.offset = be.Uint32(b[4:]), be.Uint32(b[8:]), be.Uint32(b[12:])
	b = b[chunkHdrLen:]
	if c.offset == 0 {
		if len(b) < packet.VnetHeaderLen+1 {
			return c, false
		}
		c.h = packet.VnetHeader{
			Flags: b[0], GSOType: b[1], HdrLen: be.Uint16(b[2:]), GSOSize: be.Uint16(b[4:]),
			CsumStart: be.Uint16(b[6:]), CsumOffset: be.Uint16(b[8:]),
		}
		b = b[packet.VnetHeaderLen:]
	}
	c.data = b
	return c, c.total > 0 && c.total <= maxSuperFrame && uint64(c.offset)+uint64(len(b)) <= uint64(c.total)
}

// appendChunkHeader appends the chunk header for the bytes at offset of a frame of total
// bytes, with the virtio header h if offset is 0.
func appendChunkHeader(dst []byte, id uint32, total, offset int, h packet.VnetHeader) []byte {
	be := binary.BigEndian
	dst = append(dst, superVersion, 0, 0, 0)
	dst = be.AppendUint32(dst, id)
	dst = be.AppendUint32(dst, uint32(total))
	dst = be.AppendUint32(dst, uint32(offset))
	if offset == 0 {
		dst = append(dst, h.Flags, h.GSOType)
		dst = be.AppendUint16(dst, h.HdrLen)
		dst = be.AppendUint16(dst, h.GSOSize)
		dst = be.AppendUint16(dst, h.CsumStart)
		dst = be.AppendUint16(dst, h.CsumOffset)
	}
	return dst
}

// SuperInjector is an injector that takes a TCP super-frame that came through the overlay
// whole, in the buffer it was put together in, so that whoever writes it later does so
// from there, without a copy. The buffer holds packet.VnetHeaderLen bytes of room for the
// virtio header and then the Ethernet frame, and h is the virtio header that describes the
// frame (packet.CheckTSOSize has accepted them). The injector owns the buffer from the
// call on, whatever it returns, and releases it when it is done with it.
type SuperInjector interface {
	InjectSuper(h packet.VnetHeader, buf *packet.Buffer) error
}

// reassembly is the super-frame that one source is sending, as far as it has arrived, in
// buf after room for the virtio header. A
// source is one socket of a node (its address and port), whose datagrams reach the lane
// in the order they were sent, so that it has at most one frame in progress. When the
// frame is lost, buf is nil and the entry stays to swallow the chunks of it that follow,
// until the next frame starts, so that a lost frame is counted once.
type reassembly struct {
	src     netip.AddrPort
	id      uint32
	total   int
	h       packet.VnetHeader
	buf     *packet.Buffer
	started int64 // the clock when the frame's first chunk, or its loss, was seen
}

// reassembler is a lane's frames in progress. Its zero value is ready.
type reassembler struct {
	es []reassembly
}

// find returns the index of src's entry, or -1.
func (ra *reassembler) find(src netip.AddrPort) int {
	for i := range ra.es {
		if ra.es[i].src == src {
			return i
		}
	}
	return -1
}

// have is how many bytes of the frame have arrived.
func (e *reassembly) have() int { return len(e.buf.B) - packet.VnetHeaderLen }

// drop discards the frame e has and leaves e to swallow what is left of it.
func (e *reassembly) drop(now int64) {
	if e.buf != nil {
		e.buf.Release()
		e.buf = nil
	}
	e.started = now
}

// remove deletes the entry at index i, which does not keep its order.
func (ra *reassembler) remove(i int) {
	if e := &ra.es[i]; e.buf != nil {
		e.buf.Release()
	}
	last := len(ra.es) - 1
	ra.es[i] = ra.es[last]
	ra.es[last] = reassembly{}
	ra.es = ra.es[:last]
}

// release gives back the buffers of the frames in progress.
func (ra *reassembler) release() {
	for i := range ra.es {
		ra.es[i].drop(0)
	}
	ra.es = nil
}

// chunk handles a datagram with the chunk bit set, from src, at time now.
func (l *rxLane) chunk(sink FrameInjector, d []byte, src netip.AddrPort, now int64) {
	oe, ra := l.oe, &l.ra
	c, ok := parseChunk(d)
	if !ok {
		oe.superInvalid.Add(1)
		return
	}
	l.expire(now, src)
	i := ra.find(src)

	switch {
	case c.offset == 0:
		if i < 0 {
			i = l.add(src, true)
		} else if ra.es[i].buf != nil {
			oe.superPartial.Add(1) // the frame before it was not finished
		}
		e := &ra.es[i]
		e.drop(now)
		e.id, e.total, e.h = c.id, int(c.total), c.h
		e.buf = packet.GetBuffer()
		e.buf.B = e.buf.B[:packet.VnetHeaderLen] // room for the virtio header in front of the frame
	case i < 0 || ra.es[i].id != c.id:
		// A chunk from the middle of a frame whose start was lost, which is one frame lost
		// (and so is the one it found unfinished).
		if i < 0 {
			i = l.add(src, false)
		} else if ra.es[i].buf != nil {
			oe.superPartial.Add(1)
		}
		oe.superPartial.Add(1)
		if i >= 0 {
			ra.es[i].drop(now)
			ra.es[i].id = c.id
		}
		return
	case ra.es[i].buf == nil:
		return // the rest of a frame already lost
	case int(c.offset) != ra.es[i].have() || int(c.total) != ra.es[i].total:
		// A chunk is missing, repeated or out of its place, or does not belong to the frame.
		oe.superPartial.Add(1)
		ra.es[i].drop(now)
		return
	}
	e := &ra.es[i]
	e.buf.B = append(e.buf.B, c.data...)
	if e.have() < e.total {
		return
	}
	buf, h := e.buf, e.h
	e.buf = nil
	ra.remove(i)
	l.deliverSuper(sink, h, buf)
}

// add makes room for the frame of a new source: a source whose frame was lost goes first,
// then, if the frame is live (being received, not just remembered as lost), the oldest
// frame in progress, which is counted. It returns the index of the new entry, which is
// empty but for src, or -1 if the table is full of live frames and src's is only lost.
func (l *rxLane) add(src netip.AddrPort, live bool) int {
	ra := &l.ra
	if len(ra.es) >= maxReassemblies {
		victim := 0
		for i := range ra.es {
			switch e, v := &ra.es[i], &ra.es[victim]; {
			case e.buf == nil && v.buf != nil, (e.buf == nil) == (v.buf == nil) && e.started < v.started:
				victim = i
			}
		}
		if ra.es[victim].buf != nil {
			if !live {
				return -1
			}
			l.oe.superEvicted.Add(1)
		}
		ra.remove(victim)
	}
	ra.es = append(ra.es, reassembly{src: src})
	return len(ra.es) - 1
}

// expire drops the frames that have been in progress for more than reassemblyAge, and
// forgets the sources whose frame was lost that long ago. A dropped frame's entry stays
// for a while to swallow what is left of it, so that it is counted once; so does the
// entry of keep, the source of the chunk being handled.
func (l *rxLane) expire(now int64, keep netip.AddrPort) {
	ra := &l.ra
	for i := 0; i < len(ra.es); {
		e := &ra.es[i]
		switch {
		case now-e.started <= reassemblyAge:
		case e.buf != nil:
			l.oe.superPartial.Add(1)
			e.drop(now)
		case e.src == keep:
			e.started = now
		default:
			ra.remove(i)
			continue
		}
		i++
	}
}

// deliverSuper validates a complete frame, which lies in buf after the room for the virtio
// header, and gives it to the sink, which may keep the buffer. Otherwise it is released
// here.
func (l *rxLane) deliverSuper(sink FrameInjector, h packet.VnetHeader, buf *packet.Buffer) {
	oe, frame := l.oe, buf.B[packet.VnetHeaderLen:]
	if !h.IsTSO() || int(h.HdrLen) > len(frame) || packet.CheckTSOSize(h, frame) != nil {
		oe.superInvalid.Add(1)
		buf.Release()
		return
	}
	si, ok := sink.(SuperInjector)
	if !ok { // no injector: a plain FrameInjector without a batch, in a test
		buf.Release()
		return
	}
	if err := si.InjectSuper(h, buf); err != nil {
		oe.rxDropped.Add(1)
		return
	}
	oe.superRx.Add(1)
	l.delivered.Add(1)
}

// peers are the nodes that have said, with the mark in the VXLAN header of a datagram,
// that they take super-frames, and when they last did.
type peers struct {
	mu sync.RWMutex
	m  map[netip.Addr]*atomic.Int64 // the clock of the last datagram with the mark
}

// note records a datagram with the mark from a at time now.
func (p *peers) note(a netip.Addr, now int64) {
	p.mu.RLock()
	last := p.m[a]
	p.mu.RUnlock()
	if last != nil {
		last.Store(now)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = make(map[netip.Addr]*atomic.Int64)
	}
	if len(p.m) >= maxPeers {
		for k, v := range p.m {
			if now-v.Load() > peerAge {
				delete(p.m, k)
			}
		}
		if len(p.m) >= maxPeers {
			return
		}
	}
	last = new(atomic.Int64)
	last.Store(now)
	p.m[a] = last
}

// fresh reports whether a sent the mark within peerAge of now.
func (p *peers) fresh(a netip.Addr, now int64) bool {
	p.mu.RLock()
	last := p.m[a]
	p.mu.RUnlock()
	return last != nil && now-last.Load() <= peerAge
}

// count is how many peers are fresh.
func (p *peers) count(now int64) (n int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, last := range p.m {
		if now-last.Load() <= peerAge {
			n++
		}
	}
	return n
}
