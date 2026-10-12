package vxlan

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"plaid/pkg/packet"
)

// tsoFrame is the Ethernet frame of a TCP super-frame with n bytes of payload, IPv4 or
// IPv6, and the virtio header that describes it, as a pod's stack hands it over: the
// segments are those of a pod MTU of mtu, with the timestamp option (12 bytes), and the
// checksum field holds only a pseudo-header's sum. The payload depends on seed.
func tsoFrame(v6 bool, n, mtu int, seed byte) (packet.VnetHeader, []byte) {
	const tcpLen = 32
	ihl, etherType, gso := 20, packet.EtherTypeIPv4, uint8(packet.VnetGSOTCPv4)
	if v6 {
		ihl, etherType, gso = 40, packet.EtherTypeIPv6, packet.VnetGSOTCPv6
	}
	f := make([]byte, packet.EthernetHeaderLen+ihl+tcpLen+n)
	copy(f[0:], []byte{2, 0, 0, 0, 0, 2, 2, 0, 0, 0, 0, 1})
	binary.BigEndian.PutUint16(f[12:], uint16(etherType))
	ip := f[packet.EthernetHeaderLen:]
	if v6 {
		ip[0], ip[6], ip[7] = 0x60, packet.IPProtocolTCP, 64
		binary.BigEndian.PutUint16(ip[4:], uint16(tcpLen+n))
		copy(ip[8:], []byte{0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 4})
		copy(ip[24:], []byte{0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 4})
	} else {
		ip[0], ip[8], ip[9] = 0x45, 64, packet.IPProtocolTCP
		binary.BigEndian.PutUint16(ip[2:], uint16(ihl+tcpLen+n))
		binary.BigEndian.PutUint16(ip[6:], 0x4000)
		copy(ip[12:], []byte{10, 244, 1, 4, 10, 244, 2, 4})
		binary.BigEndian.PutUint16(ip[10:], packet.Checksum16(ip[:ihl]))
	}
	tcp := ip[ihl:]
	binary.BigEndian.PutUint16(tcp[0:], 4000)
	binary.BigEndian.PutUint16(tcp[2:], 80)
	binary.BigEndian.PutUint32(tcp[4:], 1000)
	tcp[12], tcp[13] = tcpLen/4<<4, packet.TCPAck
	binary.BigEndian.PutUint16(tcp[14:], 500)
	binary.BigEndian.PutUint16(tcp[16:], 0x1234) // the sum of the pseudo-header, to be completed
	copy(tcp[20:], []byte{1, 1, 8, 10, 0, 0, 0, 1, 0, 0, 0, 2})
	for i := range tcp[tcpLen:] {
		tcp[tcpLen+i] = byte(i*7) + seed
	}
	return packet.VnetHeader{
		Flags: packet.VnetNeedsCsum, GSOType: gso, HdrLen: uint16(packet.EthernetHeaderLen + ihl + tcpLen),
		GSOSize: uint16(mtu - ihl - tcpLen), CsumStart: uint16(packet.EthernetHeaderLen + ihl), CsumOffset: 16,
	}, f
}

// chunkDatagram is the size of the datagrams a super-frame is chunked into on a link of the
// usual MTU: the VXLAN header, the Ethernet header and a segment of 1450 bytes.
const chunkDatagram = packet.VXLANHeaderLen + packet.EthernetHeaderLen + 1450

// refChunks are the datagrams of a super-frame as the package comment describes them,
// written out independently of the code that makes and parses them: of d bytes but for the
// last, from the VXLAN header on, with the VXLAN header's flags flags.
func refChunks(flags byte, vni, id uint32, h packet.VnetHeader, frame []byte, d int) [][]byte {
	var out [][]byte
	be := binary.BigEndian
	for off := 0; off < len(frame); {
		b := []byte{flags | packet.VXLANFlags | packet.VXLANFlagChunk, 0, 0, 0, byte(vni >> 16), byte(vni >> 8), byte(vni), 0}
		b = append(b, 1, 0, 0, 0)
		b = be.AppendUint32(b, id)
		b = be.AppendUint32(b, uint32(len(frame)))
		b = be.AppendUint32(b, uint32(off))
		if off == 0 {
			b = append(b, h.Flags, h.GSOType)
			b = be.AppendUint16(b, h.HdrLen)
			b = be.AppendUint16(b, h.GSOSize)
			b = be.AppendUint16(b, h.CsumStart)
			b = be.AppendUint16(b, h.CsumOffset)
		}
		n := min(d-len(b), len(frame)-off)
		out = append(out, append(b, frame[off:off+n]...))
		off += n
	}
	return out
}

// superSink records what is handed to the bridge, as copies, from any goroutine.
type superSink struct {
	mu     sync.Mutex
	frames []*packet.EthernetFrame
	supers []recordedSuper
}

type recordedSuper struct {
	h     packet.VnetHeader
	frame []byte
}

func (s *superSink) InjectFrame(f *packet.EthernetFrame) error {
	c := *f
	c.DstMAC, c.SrcMAC = append(net.HardwareAddr(nil), f.DstMAC...), append(net.HardwareAddr(nil), f.SrcMAC...)
	c.Payload = append([]byte(nil), f.Payload...) // valid only during the call
	s.mu.Lock()
	s.frames = append(s.frames, &c)
	s.mu.Unlock()
	return nil
}

func (s *superSink) InjectSuper(h packet.VnetHeader, buf *packet.Buffer) error {
	s.mu.Lock()
	s.supers = append(s.supers, recordedSuper{h, append([]byte(nil), buf.B[packet.VnetHeaderLen:]...)})
	s.mu.Unlock()
	buf.Release()
	return nil
}

func (s *superSink) counts() (frames, supers int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames), len(s.supers)
}

// testLane is a receive lane of an engine that has the feature, whose clock the test
// moves.
type testLane struct {
	*rxLane
	now int64
}

func newTestLane() *testLane {
	oe := NewOverlayEngine(OverlayConfig{Super: true}, NewRouteTable(), nil)
	return &testLane{rxLane: &rxLane{oe: oe}, now: int64(time.Hour)}
}

func (l *testLane) feed(sink FrameInjector, src netip.AddrPort, datagrams ...[]byte) {
	for _, d := range datagrams {
		l.process(sink, d, src, l.now)
	}
}

func source(i int) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(1 + i/100)}), uint16(40000+i%100))
}

func (l *testLane) counters() Stats { return l.oe.Stats() }

// Frames of 1 byte to 64 KiB of payload, IPv4 and IPv6, at the MTUs of 1450 and 8950,
// come out as they went in, with the same virtio header.
func TestChunksRebuildTheFrame(t *testing.T) {
	for _, v6 := range []bool{false, true} {
		for _, mtu := range []int{1450, 8950} {
			mss := mtu - 52
			if v6 {
				mss = mtu - 72
			}
			d := packet.VXLANHeaderLen + packet.EthernetHeaderLen + mtu
			for _, n := range []int{1, 2, mss - 1, mss, mss + 1, 2 * mss, 3*mss + 7, 20000, 65400} {
				name := fmt.Sprintf("v6=%v mtu=%d payload=%d", v6, mtu, n)
				h, frame := tsoFrame(v6, n, mtu, byte(n))
				l, sink := newTestLane(), &superSink{}
				datagrams := refChunks(packet.VXLANFlagSuperOK, 1, 7, h, frame, d)
				for i, dg := range datagrams {
					if len(dg) > d || i < len(datagrams)-1 && len(dg) != d {
						t.Fatalf("%s: datagram %d of %d is %d bytes, want %d", name, i, len(datagrams), len(dg), d)
					}
				}
				l.feed(sink, source(0), datagrams...)
				if len(sink.supers) != 1 || len(sink.frames) != 0 {
					t.Fatalf("%s: %d super-frames and %d frames delivered", name, len(sink.supers), len(sink.frames))
				}
				if got := sink.supers[0]; got.h != h || !bytes.Equal(got.frame, frame) {
					t.Fatalf("%s: the frame or its header changed: %+v, want %+v", name, got.h, h)
				}
				if st := l.counters(); st.SuperRx != 1 || st.SuperPartial != 0 || st.SuperInvalid != 0 || l.delivered.Load() != 1 {
					t.Errorf("%s: counters %+v", name, st)
				}
				if len(l.ra.es) != 0 {
					t.Errorf("%s: %d reassemblies left", name, len(l.ra.es))
				}
			}
		}
	}
}

// The first bytes of a small frame, written out: the format is the one the package comment
// describes.
func TestChunkWireFormat(t *testing.T) {
	h := packet.VnetHeader{Flags: 1, GSOType: 0x81, HdrLen: 0x0102, GSOSize: 0x0304, CsumStart: 0x0506, CsumOffset: 0x0708}
	frame := bytes.Repeat([]byte{0xee}, 100)
	got := refChunks(0, 0x010203, 0x0a0b0c0d, h, frame, 80)
	want := []byte{
		0x18, 0, 0, 0, 1, 2, 3, 0, // VXLAN: I and the chunk bit, VNI
		1, 0, 0, 0, // version
		0x0a, 0x0b, 0x0c, 0x0d, // id
		0, 0, 0, 100, // length of the frame
		0, 0, 0, 0, // offset
		1, 0x81, 1, 2, 3, 4, 5, 6, 7, 8, // the virtio header
	}
	if !bytes.HasPrefix(got[0], want) || len(got[0]) != 80 {
		t.Fatalf("first datagram % x", got[0])
	}
	// And the code that parses it reads the same.
	c, ok := parseChunk(got[0])
	if !ok || c.id != 0x0a0b0c0d || c.total != 100 || c.offset != 0 || c.h != h || len(c.data) != 80-len(want) {
		t.Errorf("parsed %+v, %v", c, ok)
	}
	if b := appendChunkHeader(nil, 0x0a0b0c0d, 100, 0, h); !bytes.Equal(b, want[8:]) {
		t.Errorf("header % x, want % x", b, want[8:])
	}
	// The first datagram of 80 bytes has 46 bytes of the frame (80 less the headers).
	if c, ok := parseChunk(got[1]); !ok || c.offset != 46 || len(c.data) != 54 || len(got[1]) != 8+16+54 {
		t.Errorf("second chunk %+v, %v", c, ok)
	}
}

// A frame with one chunk missing is dropped and counted once, and the next frame from the
// same source is intact.
func TestLostChunkDropsTheFrameOnly(t *testing.T) {
	h1, f1 := tsoFrame(false, 9000, 1450, 1)
	h2, f2 := tsoFrame(false, 8000, 1450, 2)
	one := refChunks(0, 1, 1, h1, f1, chunkDatagram)
	two := refChunks(0, 1, 2, h2, f2, chunkDatagram)
	if len(one) < 5 {
		t.Fatal("test frame too small")
	}
	for lost := range one {
		l, sink := newTestLane(), &superSink{}
		l.feed(sink, source(0), one[:lost]...)
		l.feed(sink, source(0), one[lost+1:]...)
		if len(sink.supers) != 0 {
			t.Fatalf("chunk %d lost: the frame was delivered", lost)
		}
		l.feed(sink, source(0), two...)
		if len(sink.supers) != 1 || sink.supers[0].h != h2 || !bytes.Equal(sink.supers[0].frame, f2) {
			t.Fatalf("chunk %d lost: the next frame did not arrive intact (%d delivered)", lost, len(sink.supers))
		}
		if st := l.counters(); st.SuperPartial != 1 || st.SuperInvalid != 0 || st.SuperRx != 1 {
			t.Errorf("chunk %d lost: counters %+v, want the one frame dropped", lost, st)
		}
	}
}

// seq joins runs of datagrams.
func seq(runs ...[][]byte) (out [][]byte) {
	for _, r := range runs {
		out = append(out, r...)
	}
	return out
}

// A chunk out of its place, or twice, drops the frame.
func TestReorderedOrDuplicatedChunkDropsTheFrame(t *testing.T) {
	h, f := tsoFrame(true, 9000, 1450, 1)
	h2, f2 := tsoFrame(true, 3000, 1450, 2)
	c := refChunks(0, 1, 1, h, f, chunkDatagram)
	next := refChunks(0, 1, 2, h2, f2, chunkDatagram)
	wrongTotal := append([]byte(nil), c[1]...)
	binary.BigEndian.PutUint32(wrongTotal[8+8:], uint32(len(f)+1))
	last := c[len(c)-1:]
	for _, tc := range []struct {
		name   string
		chunks [][]byte
		want   [][]byte // the frames that arrive
	}{
		{"swapped", seq(c[:1], c[2:3], c[1:2], c[3:], next), [][]byte{f2}},
		{"duplicated", seq(c[:2], c[1:], next), [][]byte{f2}},
		{"another total", seq(c[:1], [][]byte{wrongTotal}, c[2:], next), [][]byte{f2}},
		// The first chunk again starts the frame again, which then completes.
		{"first twice", seq(c[:1], c, next), [][]byte{f, f2}},
		// The frame is complete when the last chunk comes, so a second last chunk is the
		// tail of a frame that is lost.
		{"last twice", seq(c, last, next), [][]byte{f, f2}},
	} {
		l, sink := newTestLane(), &superSink{}
		l.feed(sink, source(0), tc.chunks...)
		if len(sink.supers) != len(tc.want) {
			t.Errorf("%s: %d frames delivered, want %d", tc.name, len(sink.supers), len(tc.want))
			continue
		}
		for i, w := range tc.want {
			if !bytes.Equal(sink.supers[i].frame, w) {
				t.Errorf("%s: frame %d is not the one sent", tc.name, i)
			}
		}
		if st := l.counters(); st.SuperPartial != 1 {
			t.Errorf("%s: %d frames counted as dropped, want 1: %+v", tc.name, st.SuperPartial, st)
		}
	}
}

// Plain datagrams from a source with a frame in progress are delivered and leave it alone.
func TestPlainDatagramsDoNotDisturbAFrameInProgress(t *testing.T) {
	h, f := tsoFrame(false, 20000, 1450, 3)
	chunks := refChunks(packet.VXLANFlagSuperOK, 1, 5, h, f, chunkDatagram)
	l, sink := newTestLane(), &superSink{}
	for i, c := range chunks {
		l.feed(sink, source(0), c)
		if i < len(chunks)-1 {
			l.feed(sink, source(0), datagram(uint32(i), 600)) // from the same socket, a plain frame of another flow
		}
	}
	if len(sink.supers) != 1 || !bytes.Equal(sink.supers[0].frame, f) || len(sink.frames) != len(chunks)-1 {
		t.Fatalf("%d super-frames and %d frames delivered", len(sink.supers), len(sink.frames))
	}
	if st := l.counters(); st.SuperPartial != 0 || st.SuperInvalid != 0 || st.RxInvalid != 0 {
		t.Errorf("counters %+v", st)
	}
}

// Frames from several sources are put together at the same time, each from its own chunks.
func TestSourcesAreReassembledSeparately(t *testing.T) {
	const sources = 5
	l, sink := newTestLane(), &superSink{}
	var chunks [sources][][]byte
	var want [sources][]byte
	for i := range chunks {
		h, f := tsoFrame(i%2 == 1, 6000+1000*i, 1450, byte(i))
		want[i] = f
		chunks[i] = refChunks(0, 1, 1, h, f, chunkDatagram) // the same id: it is per source
	}
	for round := 0; ; round++ {
		any := false
		for i := range chunks {
			if round < len(chunks[i]) {
				l.feed(sink, source(i), chunks[i][round])
				any = true
			}
		}
		if !any {
			break
		}
	}
	if len(sink.supers) != sources {
		t.Fatalf("%d frames delivered", len(sink.supers))
	}
	for _, got := range sink.supers {
		ok := false
		for _, w := range want {
			ok = ok || bytes.Equal(got.frame, w)
		}
		if !ok {
			t.Errorf("a frame of %d bytes is none of those sent", len(got.frame))
		}
	}
	if st := l.counters(); st.SuperPartial != 0 || st.SuperInvalid != 0 {
		t.Errorf("counters %+v", st)
	}
}

// A lane holds frames from at most maxReassemblies sources: the oldest is dropped, and
// counted, for a new one.
func TestOldestReassemblyIsEvicted(t *testing.T) {
	const extra = 5
	l, sink := newTestLane(), &superSink{}
	var chunks [maxReassemblies + extra][][]byte
	for i := range chunks {
		h, f := tsoFrame(false, 4000, 1450, byte(i))
		chunks[i] = refChunks(0, 1, 1, h, f, chunkDatagram)
		l.now += int64(time.Millisecond)
		l.feed(sink, source(i), chunks[i][0]) // only the first chunk
		if len(l.ra.es) > maxReassemblies {
			t.Fatalf("%d reassemblies after %d sources", len(l.ra.es), i+1)
		}
	}
	if st := l.counters(); st.SuperEvicted != extra {
		t.Errorf("%d evicted, want %d", st.SuperEvicted, extra)
	}
	for i := len(chunks) - 1; i >= 0; i-- {
		l.feed(sink, source(i), chunks[i][1:]...)
	}
	// The first ones were evicted, so their later chunks are those of lost frames (which
	// do not take the place of a frame in progress); the others complete.
	if len(sink.supers) != maxReassemblies {
		t.Errorf("%d frames delivered, want %d", len(sink.supers), maxReassemblies)
	}
	if st := l.counters(); st.SuperPartial != extra {
		t.Errorf("%d frames counted as lost, want %d: %+v", st.SuperPartial, extra, st)
	}
	if len(l.ra.es) > maxReassemblies {
		t.Errorf("%d reassemblies", len(l.ra.es))
	}
}

// A frame that is not finished in a second is dropped, at the next activity on the lane.
func TestOldFrameIsDroppedAtTheNextActivity(t *testing.T) {
	h, f := tsoFrame(false, 9000, 1450, 1)
	c := refChunks(0, 1, 1, h, f, chunkDatagram)
	l, sink := newTestLane(), &superSink{}
	l.feed(sink, source(0), c[:3]...)
	l.now += int64(900 * time.Millisecond)
	l.feed(sink, source(1), refChunks(0, 1, 1, h, f, chunkDatagram)[0])
	if st := l.counters(); st.SuperPartial != 0 {
		t.Fatalf("dropped after 0.9 s: %+v", st)
	}
	l.now += int64(200 * time.Millisecond) // 1.1 s since the first chunk
	l.feed(sink, source(2), refChunks(0, 1, 1, h, f, chunkDatagram)[0])
	if st := l.counters(); st.SuperPartial != 1 {
		t.Fatalf("after 1.1 s: %+v, want the frame of source 0 dropped", st)
	}
	l.feed(sink, source(0), c[3:]...) // what is left of it
	if len(sink.supers) != 0 {
		t.Fatalf("the old frame was delivered")
	}
	if st := l.counters(); st.SuperPartial != 1 {
		t.Errorf("the frame was counted again: %+v", st)
	}
	// And the same activity from the source itself.
	l2, sink2 := newTestLane(), &superSink{}
	l2.feed(sink2, source(0), c[:3]...)
	l2.now += int64(1100 * time.Millisecond)
	l2.feed(sink2, source(0), c[3:]...)
	if len(sink2.supers) != 0 || l2.counters().SuperPartial != 1 {
		t.Errorf("a frame that took 1.1 s: %d delivered, %+v", len(sink2.supers), l2.counters())
	}
}

// Chunks and frames that are not valid are counted and never panic, whatever they hold.
func TestMalformedChunksAreCounted(t *testing.T) {
	h, f := tsoFrame(false, 3000, 1450, 1)
	good := refChunks(0, 1, 1, h, f, chunkDatagram)
	first := func() []byte { return append([]byte(nil), good[0]...) }
	mid := func() []byte { return append([]byte(nil), good[1]...) }
	be := binary.BigEndian
	type tc struct {
		name string
		dg   []byte
		// which counter: invalid chunk (superInvalid), plain datagram invalid (rxInvalid), or none (stray: partial)
		kind string
	}
	var cases []tc
	add := func(name, kind string, dg []byte) { cases = append(cases, tc{name, dg, kind}) }

	add("empty", "rx", nil)
	add("only the VXLAN header", "chunk", good[0][:8])
	add("short chunk header", "chunk", good[0][:8+15])
	add("no data", "chunk", good[0][:8+16])
	add("first chunk without all of the virtio header", "chunk", good[0][:8+16+9])
	add("first chunk with no data", "chunk", good[0][:8+16+10])
	b := first()
	b[8] = 2
	add("version 2", "chunk", b)
	b = first()
	b[8] = 0
	add("version 0", "chunk", b)
	for i := 9; i < 12; i++ {
		b = first()
		b[i] = 1
		add(fmt.Sprintf("byte %d not zero", i-8), "chunk", b)
	}
	b = first()
	be.PutUint32(b[16:], 0)
	add("length 0", "chunk", b)
	b = first()
	be.PutUint32(b[16:], maxSuperFrame+1)
	add("length above the maximum", "chunk", b)
	b = first()
	be.PutUint32(b[16:], 0xffffffff)
	add("length 4 GiB", "chunk", b)
	b = first()
	be.PutUint32(b[16:], 100)
	add("data past the end of the frame", "chunk", b)
	b = mid()
	be.PutUint32(b[20:], 0xffffffff)
	add("offset 4 GiB", "chunk", b)
	b = first()
	b[0] = packet.VXLANFlagChunk // no I flag
	add("chunk without the I flag", "rx", b)

	// Complete frames that are not what the virtio header says.
	whole := func(name string, mod func(h *packet.VnetHeader, f []byte)) {
		h, f := tsoFrame(false, 3000, 1450, 1)
		mod(&h, f)
		add(name, "chunk", refChunks(0, 1, 1, h, f, 10000)[0])
	}
	whole("no checksum offload", func(h *packet.VnetHeader, f []byte) { h.Flags = 0 })
	whole("no GSO", func(h *packet.VnetHeader, f []byte) { h.GSOType = 0 })
	whole("UDP GSO", func(h *packet.VnetHeader, f []byte) { h.GSOType = 3 })
	whole("GSO size 0", func(h *packet.VnetHeader, f []byte) { h.GSOSize = 0 })
	whole("checksum start wrong", func(h *packet.VnetHeader, f []byte) { h.CsumStart++ })
	whole("checksum offset wrong", func(h *packet.VnetHeader, f []byte) { h.CsumOffset = 17 })
	whole("header length past the frame", func(h *packet.VnetHeader, f []byte) { h.HdrLen = 9999 })
	whole("IPv6 type for an IPv4 frame", func(h *packet.VnetHeader, f []byte) { h.GSOType = packet.VnetGSOTCPv6 })
	whole("not TCP", func(h *packet.VnetHeader, f []byte) { f[14+9] = packet.IPProtocolUDP })
	whole("IP options", func(h *packet.VnetHeader, f []byte) { f[14] = 0x46 })
	whole("a fragment", func(h *packet.VnetHeader, f []byte) { f[14+6] = 0x20 })
	{
		h, f := tsoFrame(false, 1, 1450, 1)
		f[14+20+12] = 0xf0
		add("TCP header longer than the frame", "chunk", refChunks(0, 1, 1, h, f, 10000)[0])
	}
	whole("TCP header shorter than 20 bytes", func(h *packet.VnetHeader, f []byte) { f[14+20+12] = 0x10 })

	for _, c := range cases {
		l, sink := newTestLane(), &superSink{}
		l.feed(sink, source(0), c.dg)
		st := l.counters()
		var counted bool
		switch c.kind {
		case "chunk":
			counted = st.SuperInvalid == 1 && st.RxInvalid == 0
		case "rx":
			counted = st.RxInvalid == 1 && st.SuperInvalid == 0
		}
		if !counted || len(sink.supers) != 0 || len(sink.frames) != 0 || len(l.ra.es) != 0 && c.kind != "stray" {
			t.Errorf("%s: counters %+v, %d super-frames, %d frames, %d reassemblies", c.name, st, len(sink.supers), len(sink.frames), len(l.ra.es))
		}
	}

	// A chunk from the middle of nothing is a lost frame, counted once for all its chunks.
	l, sink := newTestLane(), &superSink{}
	l.feed(sink, source(0), good[1], good[2])
	if st := l.counters(); st.SuperPartial != 1 || st.SuperInvalid != 0 || len(sink.supers) != 0 {
		t.Errorf("stray chunks: %+v", st)
	}

	// Mutations of valid datagrams, and of frames that are valid, never panic.
	rng := rand.New(rand.NewSource(1))
	l, sink = newTestLane(), &superSink{}
	for i := 0; i < 20000; i++ {
		src := append([]byte(nil), good[rng.Intn(len(good))]...)
		for k := rng.Intn(4); k >= 0 && len(src) > 0; k-- {
			switch rng.Intn(3) {
			case 0:
				src[rng.Intn(len(src))] = byte(rng.Intn(256))
			case 1:
				src = src[:rng.Intn(len(src)+1)]
			case 2:
				src[rng.Intn(min(len(src), 40))] ^= 1 << rng.Intn(8)
			}
		}
		l.now += int64(rng.Intn(int(300 * time.Millisecond)))
		l.feed(sink, source(rng.Intn(80)), src)
		sink.supers, sink.frames = sink.supers[:0], sink.frames[:0]
		if len(l.ra.es) > maxReassemblies {
			t.Fatalf("%d reassemblies", len(l.ra.es))
		}
	}
	// And the lane still works.
	l.now += int64(10 * time.Second)
	l.feed(sink, source(200), refChunks(0, 1, 1, h, f, chunkDatagram)...)
	if len(sink.supers) != 1 || !bytes.Equal(sink.supers[0].frame, f) {
		t.Error("the lane does not work after the mutated datagrams")
	}
}

// The segment size that a rebuilt frame states is the sending node's to choose, and the
// frame is cut by it: below the smallest MSS of the frame's IP version (536, 1220) the
// frame is invalid, counted and dropped, and so is one for more than 128 segments. The
// second limit cannot be reached with a frame that a chunk header may state (at 536 the
// largest holds 123 segments); packet's test covers it, and the largest frame at the
// smallest size is accepted here.
func TestRebuiltFrameWithATinySegmentSizeIsInvalid(t *testing.T) {
	for _, tc := range []struct {
		v6      bool
		size, n int
		ok      bool
	}{
		{false, 535, 3000, false},
		{false, 536, 3000, true},
		{false, 1, 3000, false},
		{false, 536, 65483, true}, // the largest IPv4 frame: 123 segments
		{false, 535, 65483, false},
		{true, 1219, 3000, false},
		{true, 1220, 3000, true},
		{true, 1, 3000, false},
		{true, 1220, 65535 - 72, true}, // 54 segments
		{true, 1219, 65535 - 72, false},
		{false, 1219, 3000, true}, // the IPv6 limit is not the IPv4 one
	} {
		name := fmt.Sprintf("v6=%v size=%d payload=%d", tc.v6, tc.size, tc.n)
		h, f := tsoFrame(tc.v6, tc.n, 1450, 1)
		h.GSOSize = uint16(tc.size)
		l, sink := newTestLane(), &superSink{}
		l.feed(sink, source(0), refChunks(packet.VXLANFlagSuperOK, 1, 7, h, f, 1500)...)
		st := l.counters()
		want := Stats{SuperRx: 1}
		if !tc.ok {
			want = Stats{SuperInvalid: 1}
		}
		if len(sink.supers) != btoi(tc.ok) || len(sink.frames) != 0 || st.SuperRx != want.SuperRx || st.SuperInvalid != want.SuperInvalid ||
			st.SuperPartial != 0 || st.RxInvalid != 0 || l.delivered.Load() != uint64(btoi(tc.ok)) || len(l.ra.es) != 0 {
			t.Errorf("%s: %d super-frames delivered, counters %+v", name, len(sink.supers), st)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The mark: a datagram with it makes its sender a peer for two seconds; one without it, or
// from an engine that does not send, does not.
func TestPeersExpireAfterTwoSeconds(t *testing.T) {
	l := newTestLane()
	a, b := source(0), source(300)
	marked := func(flags byte) []byte {
		d := datagram(1, 200)
		d[0] |= flags
		return d
	}
	l.feed(&superSink{}, a, marked(packet.VXLANFlagSuperOK))
	l.feed(&superSink{}, b, marked(0))
	fresh := func(a netip.AddrPort) bool { return l.oe.peers.fresh(a.Addr(), l.now) }
	if !fresh(a) || fresh(b) || l.counters().SuperPeers != 1 {
		t.Fatalf("peers: a %v, b %v, %d known", fresh(a), fresh(b), l.counters().SuperPeers)
	}
	at := l.now
	l.now = at + int64(1999*time.Millisecond)
	if !fresh(a) {
		t.Error("expired before two seconds")
	}
	l.now = at + int64(2001*time.Millisecond)
	if fresh(a) {
		t.Error("still a peer after two seconds")
	}
	l.oe.clock = func() int64 { return l.now }
	if n := l.counters().SuperPeers; n != 0 {
		t.Errorf("%d peers known after they expired", n)
	}
	// Seen again, it is one again.
	l.feed(&superSink{}, a, marked(packet.VXLANFlagSuperOK))
	if !fresh(a) {
		t.Error("not a peer after another datagram with the mark")
	}

	// An engine that does not send super-frames does not keep the table.
	off := &rxLane{oe: NewOverlayEngine(OverlayConfig{}, NewRouteTable(), nil)}
	off.process(&superSink{}, marked(packet.VXLANFlagSuperOK), a, l.now)
	if off.oe.peers.fresh(a.Addr(), l.now) {
		t.Error("an engine without the feature noted a peer")
	}
}

// The marks and the chunk bit are reserved bits that an engine without the feature
// ignores: its parser reads the datagram as it reads it without them.
func TestMarkedDatagramsParseLikePlainOnes(t *testing.T) {
	d := datagram(7, 500)
	plain, err := packet.ParseVXLAN(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, mark := range []byte{packet.VXLANFlagSuperOK, packet.VXLANFlagChunk, packet.VXLANFlagSuperOK | packet.VXLANFlagChunk} {
		m := append([]byte(nil), d...)
		m[0] |= mark
		got, err := packet.ParseVXLAN(m)
		if err != nil {
			t.Fatalf("mark %#x: %v", mark, err)
		}
		if got.Header.VNI != plain.Header.VNI || got.Header.Flags != plain.Header.Flags|mark ||
			!bytes.Equal(got.InnerFrame.DstMAC, plain.InnerFrame.DstMAC) || !bytes.Equal(got.InnerFrame.SrcMAC, plain.InnerFrame.SrcMAC) ||
			got.InnerFrame.EtherType != plain.InnerFrame.EtherType || !bytes.Equal(got.InnerFrame.Payload, plain.InnerFrame.Payload) {
			t.Errorf("mark %#x: parsed differently: %v", mark, got)
		}
	}
}

// An engine without the feature handles marked plain datagrams as it handles the others:
// it injects the frame and counts nothing else (that the frame is what ParseVXLAN makes of
// a marked datagram is TestMarkedDatagramsParseLikePlainOnes).
func TestEngineWithoutTheFeatureIgnoresTheMark(t *testing.T) {
	plain := &rxLane{oe: NewOverlayEngine(OverlayConfig{}, NewRouteTable(), nil)}
	src := source(0)
	for _, mark := range []byte{0, packet.VXLANFlagSuperOK} {
		sink := &superSink{}
		d := datagram(9, 400)
		d[0] |= mark
		plain.process(sink, d, src, 1)
		if len(sink.frames) != 1 || !sink.frames[0].Trusted || binary.BigEndian.Uint32(sink.frames[0].Payload) != 9 {
			t.Fatalf("mark %#x: %d frames injected", mark, len(sink.frames))
		}
	}
	if st := plain.oe.Stats(); st.RxInvalid != 0 || st.SuperInvalid != 0 || st.SuperPeers != 0 || plain.delivered.Load() != 2 {
		t.Errorf("counters %+v", st)
	}
}

// Receiving a super-frame, from the datagrams to the bridge, which takes the buffer it was put
// together in and releases it, allocates nothing, with the sender's mark on the datagrams or not.
func TestReceivingASuperFrameAllocatesNothing(t *testing.T) {
	h, f := tsoFrame(false, 60000, 1450, 1)
	for _, flags := range []byte{packet.VXLANFlagSuperOK, 0} {
		t.Run(fmt.Sprintf("flags %#x", flags), func(t *testing.T) {
			datagrams := refChunks(flags, 1, 1, h, f, chunkDatagram)
			l := newTestLane()
			sink := &countingSink{}
			src := source(0)
			run := func() {
				for _, dg := range datagrams {
					l.process(sink, dg, src, l.now)
				}
			}
			run()
			allocs := testing.AllocsPerRun(50, run)
			t.Logf("%d datagrams a frame: %.1f allocations; %d frames", len(datagrams), allocs, sink.n)
			if allocs != 0 && !raceEnabled {
				t.Errorf("%.1f allocations for a super-frame, want 0", allocs)
			}
			if sink.n < 51 {
				t.Errorf("%d frames delivered", sink.n)
			}
		})
	}
}

type countingSink struct{ n int }

func (*countingSink) InjectFrame(*packet.EthernetFrame) error { return nil }
func (s *countingSink) InjectSuper(_ packet.VnetHeader, buf *packet.Buffer) error {
	s.n++
	buf.Release()
	return nil
}

// bufferSink takes the buffer a frame was put together in and keeps it until the test lets
// it go, as a writer goroutine that has not written the frame yet does.
type bufferSink struct {
	held []*packet.Buffer
	hs   []packet.VnetHeader
	fail bool
}

func (*bufferSink) InjectFrame(*packet.EthernetFrame) error { return nil }
func (s *bufferSink) InjectSuper(h packet.VnetHeader, buf *packet.Buffer) error {
	if s.fail {
		buf.Release()
		return fmt.Errorf("refused")
	}
	s.held, s.hs = append(s.held, buf), append(s.hs, h)
	return nil
}

// A sink that takes buffers is given the one the frame was put together in: room for the
// virtio header first, then the frame, nothing copied again, and the lane does not use it
// afterwards. The next frames do not reuse it while the sink holds it.
func TestLaneHandsOverTheBuffer(t *testing.T) {
	l, sink := newTestLane(), &bufferSink{}
	var want [][]byte
	for i := 0; i < 5; i++ {
		h, f := tsoFrame(i%2 == 1, 20000+i*1000, 1450, byte(i))
		want = append(want, f)
		l.feed(sink, source(0), refChunks(0, 1, uint32(i), h, f, chunkDatagram)...)
		if len(sink.held) != i+1 || sink.hs[i] != h {
			t.Fatalf("frame %d: %d buffers handed over", i, len(sink.held))
		}
	}
	for i, buf := range sink.held {
		if len(buf.B) != packet.VnetHeaderLen+len(want[i]) || !bytes.Equal(buf.B[packet.VnetHeaderLen:], want[i]) {
			t.Errorf("frame %d: the buffer holds %d bytes, or not the frame after the room for the header", i, len(buf.B))
		}
	}
	if st := l.counters(); st.SuperRx != 5 || st.RxDropped != 0 {
		t.Errorf("counters %+v", st)
	}
	for _, buf := range sink.held {
		buf.Release()
	}

	// A refusal is the sink's to release; the lane counts it.
	h, f := tsoFrame(false, 3000, 1450, 1)
	refusing := &bufferSink{fail: true}
	l.feed(refusing, source(0), refChunks(0, 1, 9, h, f, chunkDatagram)...)
	if st := l.counters(); st.RxDropped != 1 || st.SuperRx != 5 {
		t.Errorf("a refused frame: %+v", st)
	}
}
