package packet_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"testing"

	"plaid/pkg/packet"
)

// refSum is the ones-complement sum of data as a loop over 16-bit words, which is the
// definition: the code under test adds eight bytes at a time.
func refSum(data []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(data[i])<<8 | uint32(data[i+1])
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	return uint16(sum)
}

// refPseudo is the folded sum of the pseudo-header of a TCP segment of n bytes.
func refPseudo(src, dst []byte, n int) uint16 {
	h := append(append(append([]byte(nil), src...), dst...), 0, 6, byte(n>>8), byte(n))
	return refSum(h)
}

// A flow is the identity of a test stream; spec is one TCP segment of it.
type spec struct {
	v6         bool
	sport      uint16
	seq, ack   uint32
	win        uint16
	flags      byte
	opts       []byte
	id         uint16
	payload    []byte
	ipOpts     []byte // IPv4 options
	dstMAC     byte
	addr       byte // not 0: the addresses are made from it (see addrs)
	badCsum    bool
	fragOffset uint16
}

func addrs(v6 bool) (src, dst []byte) {
	if v6 {
		return []byte{0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4}, []byte{0xfd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}
	}
	return []byte{10, 244, 1, 4}, []byte{10, 244, 2, 5}
}

// addrs returns the source and destination addresses of s: the default ones, or, if s.addr
// is set, others that depend on it.
func (s spec) addrs() (src, dst []byte) {
	if s.addr == 0 {
		return addrs(s.v6)
	}
	if s.v6 {
		a := make([]byte, 32)
		for i := range a {
			a[i] = s.addr + byte(8+i)
		}
		return a[:16], a[16:]
	}
	return []byte{10, 244, s.addr, 4}, []byte{10, 244, 7, s.addr}
}

// build returns the Ethernet frame for s with its IP and TCP checksums complete, as a
// peer's stack would send it.
func build(s spec) []byte {
	src, dst := s.addrs()
	tcpLen := 20 + len(s.opts) + len(s.payload)
	f := []byte{2, 0, 0, 0, 0, s.dstMAC, 2, 0, 0, 0, 0, 1, 8, 0}
	if s.v6 {
		f[12], f[13] = 0x86, 0xdd
		f = append(f, 0x60, 0, 0, 0, byte(tcpLen>>8), byte(tcpLen), 6, 64)
		f = append(append(f, src...), dst...)
	} else {
		ihl := 20 + len(s.ipOpts)
		f = append(f, byte(0x40|ihl/4), 0, byte((ihl+tcpLen)>>8), byte(ihl+tcpLen), byte(s.id>>8), byte(s.id), 0x40|byte(s.fragOffset>>8), byte(s.fragOffset), 64, 6, 0, 0)
		f = append(append(f, src...), dst...)
		f = append(f, s.ipOpts...)
		binary.BigEndian.PutUint16(f[24:], ^refSum(f[14:14+ihl]))
	}
	l4 := len(f)
	f = append(f, byte(s.sport>>8), byte(s.sport), 0x1f, 0x90)
	f = binary.BigEndian.AppendUint32(f, s.seq)
	f = binary.BigEndian.AppendUint32(f, s.ack)
	f = append(f, byte((20+len(s.opts))/4)<<4, s.flags, byte(s.win>>8), byte(s.win), 0, 0, 0, 0)
	f = append(append(f, s.opts...), s.payload...)
	sum := uint32(refPseudo(src, dst, tcpLen)) + uint32(refSum(f[l4:]))
	sum = sum&0xffff + sum>>16
	csum := ^uint16(sum)
	if s.badCsum {
		csum ^= 0x0101
	}
	binary.BigEndian.PutUint16(f[l4+16:], csum)
	return f
}

// kernelSuper returns what a TAP hands over for a TSO segment: the headers of s, all of
// its payload, the total length of all of it and, in the checksum field, the sum of the
// pseudo-header.
func kernelSuper(s spec, mss int) (packet.VnetHeader, []byte) {
	f := build(s)
	src, dst := s.addrs()
	l3 := 14
	ihl := 40
	if !s.v6 {
		ihl = 20 + len(s.ipOpts)
	}
	l4 := l3 + ihl
	tcpLen := len(f) - l4
	if s.v6 {
		binary.BigEndian.PutUint16(f[l3+4:], uint16(tcpLen))
	} else {
		binary.BigEndian.PutUint16(f[l3+10:], 0)
		binary.BigEndian.PutUint16(f[l3+10:], ^refSum(f[l3:l3+ihl]))
	}
	binary.BigEndian.PutUint16(f[l4+16:], refPseudo(src, dst, tcpLen))
	h := packet.VnetHeader{Flags: packet.VnetNeedsCsum, GSOType: packet.VnetGSOTCPv4, GSOSize: uint16(mss), CsumStart: uint16(l4), CsumOffset: 16}
	if s.v6 {
		h.GSOType = packet.VnetGSOTCPv6
	}
	h.HdrLen = uint16(l4 + 20 + len(s.opts))
	return h, f
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

// checkFrame verifies a complete frame independently of the code that made it: lengths,
// the IPv4 header checksum and the TCP checksum. It returns the TCP header and payload.
func checkFrame(t testing.TB, f []byte, v6 bool) (tcp, payload []byte) {
	t.Helper()
	src, dst := addrs(v6)
	ip := f[14:]
	ihl := 40
	if v6 {
		if got := int(binary.BigEndian.Uint16(ip[4:])); got != len(ip)-40 {
			t.Fatalf("IPv6 payload length %d, frame has %d", got, len(ip)-40)
		}
	} else {
		ihl = int(ip[0]&0xf) * 4
		if got := int(binary.BigEndian.Uint16(ip[2:])); got != len(ip) {
			t.Fatalf("IPv4 total length %d, frame has %d", got, len(ip))
		}
		if refSum(ip[:ihl]) != 0xffff {
			t.Fatalf("IPv4 header checksum is wrong")
		}
	}
	tcp = ip[ihl:]
	if got := uint32(refPseudo(src, dst, len(tcp))) + uint32(refSum(tcp)); got != 0xffff && got != 0x1fffe {
		t.Fatalf("TCP checksum is wrong (sum %#x)", got)
	}
	return tcp, tcp[int(tcp[12]>>4)*4:]
}

var timestamps = []byte{1, 1, 8, 10, 0, 0, 0, 1, 0, 0, 0, 2}

func TestSegment(t *testing.T) {
	const mss = 1398
	for _, tc := range []struct {
		name    string
		s       spec
		mss     int
		payload int
		ecn     bool // the kernel's type for a CWR segment: TCP type and the ECN bit
	}{
		{"IPv4 three segments, the last short", spec{flags: 0x10, id: 100}, mss, 2*mss + 500, false},
		{"IPv4 exact multiple", spec{flags: 0x10, id: 7}, mss, 3 * mss, false},
		{"IPv4 with timestamps", spec{flags: 0x10, opts: timestamps, id: 0xfffe}, mss - 12, 5*(mss-12) + 1, false},
		{"IPv4 jumbo", spec{flags: 0x10, id: 1}, 8898, 7 * 8898, false},
		{"IPv4 one segment", spec{flags: 0x19, id: 1}, mss, 100, false},
		{"IPv4 PSH, FIN and CWR", spec{flags: 0x10 | 0x08 | 0x01 | 0x80, id: 1}, mss, 3*mss + 1, false},
		{"IPv4 CWR, ECN type", spec{flags: 0x10 | 0x08 | 0x80, id: 9}, mss, 4*mss + 7, true},
		{"IPv6 CWR, ECN type", spec{v6: true, flags: 0x10 | 0x80}, mss, 3 * mss, true},
		{"IPv4 options", spec{flags: 0x10, ipOpts: []byte{1, 1, 1, 0}, id: 1}, mss, 2*mss + 3, false},
		{"IPv6", spec{v6: true, flags: 0x18, opts: timestamps}, mss - 12, 4*(mss-12) + 9, false},
		{"IPv6 PSH, FIN and CWR", spec{v6: true, flags: 0x10 | 0x08 | 0x01 | 0x80}, mss, 2 * mss, false},
		{"largest", spec{flags: 0x10}, 1, 1400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.s.seq, tc.s.ack, tc.s.win, tc.s.sport = 0xfffff000, 77, 500, 4000
			tc.s.payload = randomBytes(tc.payload)
			h, super := kernelSuper(tc.s, tc.mss)
			if tc.ecn {
				h.GSOType |= packet.VnetGSOECN
			}
			orig := super[:14:14]

			var sg packet.Segmenter
			frames, err := sg.Segment(h, super)
			if err != nil {
				t.Fatal(err)
			}
			want := (tc.payload + tc.mss - 1) / tc.mss
			if len(frames) != want {
				t.Fatalf("%d frames, want %d", len(frames), want)
			}
			var joined []byte
			hdr := len(frames[0]) - min(tc.mss, tc.payload)
			for i, f := range frames {
				tcp, data := checkFrame(t, f, tc.s.v6)
				joined = append(joined, data...)
				if !bytes.Equal(f[:14], orig[:14]) {
					t.Errorf("segment %d: Ethernet header changed", i)
				}
				if i < want-1 && len(data) != tc.mss || len(data) == 0 || len(data) > tc.mss {
					t.Errorf("segment %d carries %d bytes, want %d (last: the rest)", i, len(data), tc.mss)
				}
				if got, w := binary.BigEndian.Uint32(tcp[4:]), tc.s.seq+uint32(i*tc.mss); got != w {
					t.Errorf("segment %d: seq %#x, want %#x", i, got, w)
				}
				wantFlags := tc.s.flags
				if i < want-1 {
					wantFlags &^= 0x01 | 0x08
				}
				if i > 0 {
					wantFlags &^= 0x80
				}
				if tcp[13] != wantFlags {
					t.Errorf("segment %d: flags %#x, want %#x", i, tcp[13], wantFlags)
				}
				if !tc.s.v6 {
					if id := binary.BigEndian.Uint16(f[14+4:]); id != tc.s.id+uint16(i) {
						t.Errorf("segment %d: IP id %d, want %d", i, id, tc.s.id+uint16(i))
					}
				}
				if len(f)-len(data) != hdr {
					t.Errorf("segment %d has headers of %d bytes, the first has %d", i, len(f)-len(data), hdr)
				}
				if got := binary.BigEndian.Uint32(tcp[8:]); got != tc.s.ack {
					t.Errorf("segment %d: acknowledgment %d, want %d", i, got, tc.s.ack)
				}
			}
			if !bytes.Equal(joined, tc.s.payload) {
				t.Error("the segments' payload is not the original payload")
			}
		})
	}
}

func TestSegmentPassesOtherFramesThrough(t *testing.T) {
	var sg packet.Segmenter
	f := build(spec{flags: 0x10, payload: []byte("data"), seq: 1})
	out, err := sg.Segment(packet.VnetHeader{}, f)
	if err != nil || len(out) != 1 || &out[0][0] != &f[0] {
		t.Fatalf("a plain frame is returned as it is: %v %v", out, err)
	}

	// NEEDS_CSUM without GSO: the checksum is completed in place, for TCP and for UDP.
	s := spec{flags: 0x10, payload: []byte("hello, world"), seq: 5, ack: 6, win: 9, sport: 1}
	h, partial := kernelSuper(s, 0)
	h.GSOType, h.GSOSize = packet.VnetGSONone, 0
	out, err = sg.Segment(h, partial)
	if err != nil || len(out) != 1 {
		t.Fatal(out, err)
	}
	checkFrame(t, out[0], false)
	if want := build(s); !bytes.Equal(out[0], want) {
		t.Errorf("completed frame differs from the one built with the checksum done")
	}

	// UDP: the field holds the pseudo-header's sum; a result of zero is sent as 0xffff.
	for _, forceZero := range []bool{false, true} {
		src, dst := addrs(false)
		udp := append([]byte{0x30, 0x39, 0x00, 0x35, 0, 20, 0, 0}, randomBytes(12)...)
		if forceZero {
			udp[len(udp)-2], udp[len(udp)-1] = 0, 0
		}
		pseudo := append(append(append([]byte(nil), src...), dst...), 0, 17, 0, 20)
		binary.BigEndian.PutUint16(udp[6:], refSum(pseudo))
		if forceZero { // make the sum over everything 0xffff, so that the complement is zero
			binary.BigEndian.PutUint16(udp[len(udp)-2:], 0xffff^refSum(udp))
		}
		fr := append([]byte{2, 0, 0, 0, 0, 1, 2, 0, 0, 0, 0, 1, 8, 0, 0x45, 0, 0, 40, 0, 0, 0x40, 0, 64, 17, 0, 0}, append(append(src, dst...), udp...)...)
		binary.BigEndian.PutUint16(fr[24:], ^refSum(fr[14:34]))
		h := packet.VnetHeader{Flags: packet.VnetNeedsCsum, CsumStart: 34, CsumOffset: 6}
		out, err := sg.Segment(h, fr)
		if err != nil {
			t.Fatal(err)
		}
		got := binary.BigEndian.Uint16(out[0][34+6:])
		sum := uint32(refSum(pseudo)) + uint32(refSum(append(append([]byte(nil), out[0][34:34+6]...), out[0][34+8:]...)))
		sum = sum&0xffff + sum>>16
		if want := ^uint16(sum); want != 0 && got != want || want == 0 && got != 0xffff {
			t.Errorf("UDP checksum %#x, want %#x (forced zero: %v)", got, want, forceZero)
		}
	}
}

func TestSegmentRejectsWhatItCannotDescribe(t *testing.T) {
	base := spec{flags: 0x10, seq: 1, sport: 1, payload: randomBytes(3000)}
	good := func() (packet.VnetHeader, []byte) { return kernelSuper(base, 1000) }
	for _, tc := range []struct {
		name   string
		change func(h *packet.VnetHeader, f *[]byte)
	}{
		{"UDP GSO (UFO)", func(h *packet.VnetHeader, f *[]byte) { h.GSOType = 3 }},
		{"UDP segmentation", func(h *packet.VnetHeader, f *[]byte) { h.GSOType = 5 }},
		{"ECN bit on no GSO type", func(h *packet.VnetHeader, f *[]byte) { h.GSOType = packet.VnetGSOECN }},
		{"ECN bit on a UDP type", func(h *packet.VnetHeader, f *[]byte) { h.GSOType = 5 | packet.VnetGSOECN }},
		{"no segment size", func(h *packet.VnetHeader, f *[]byte) { h.GSOSize = 0 }},
		{"no checksum flag", func(h *packet.VnetHeader, f *[]byte) { h.Flags = 0 }},
		{"checksum start", func(h *packet.VnetHeader, f *[]byte) { h.CsumStart++ }},
		{"checksum offset", func(h *packet.VnetHeader, f *[]byte) { h.CsumOffset = 6 }},
		{"IPv6 type on IPv4", func(h *packet.VnetHeader, f *[]byte) { h.GSOType = packet.VnetGSOTCPv6 }},
		{"VLAN", func(h *packet.VnetHeader, f *[]byte) { (*f)[12], (*f)[13] = 0x81, 0 }},
		{"fragment", func(h *packet.VnetHeader, f *[]byte) { (*f)[14+6] |= 0x20 }},
		{"not TCP", func(h *packet.VnetHeader, f *[]byte) { (*f)[14+9] = 17 }},
		{"truncated", func(h *packet.VnetHeader, f *[]byte) { *f = (*f)[:50] }},
		{"TCP header past the frame", func(h *packet.VnetHeader, f *[]byte) { (*f)[14+20+12] = 15 << 4; *f = (*f)[:14+20+40] }},
		{"TCP header too short", func(h *packet.VnetHeader, f *[]byte) { (*f)[14+20+12] = 4 << 4 }},
		{"no payload", func(h *packet.VnetHeader, f *[]byte) { *f = (*f)[:14+20+20] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, f := good()
			tc.change(&h, &f)
			var sg packet.Segmenter
			out, err := sg.Segment(h, f)
			if !errors.Is(err, packet.ErrOffload) || len(out) != 0 {
				t.Errorf("got %d frames and error %v, want ErrOffload", len(out), err)
			}
			// What a TAP may take whole is what Segment cuts.
			h, f = good()
			tc.change(&h, &f)
			if h.IsTSO() && packet.CheckTSO(h, f) == nil {
				t.Error("IsTSO and CheckTSO accept it")
			}
		})
	}
	h, f := good()
	if !h.IsTSO() || packet.CheckTSO(h, f) != nil {
		t.Error("a TSO segment from the kernel is not accepted")
	}
	var sg packet.Segmenter
	if _, err := sg.Segment(packet.VnetHeader{Flags: packet.VnetNeedsCsum, CsumStart: 34, CsumOffset: 16}, make([]byte, 40)); !errors.Is(err, packet.ErrOffload) {
		t.Errorf("a checksum field past the frame: %v", err)
	}
}

// The segment size a frame may state is at least the smallest MSS of its IP version, and
// the frame stands for at most 128 segments.
func TestCheckTSOSize(t *testing.T) {
	for _, v6 := range []bool{false, true} {
		least := packet.MinTSOSizeV4
		if v6 {
			least = packet.MinTSOSizeV6
		}
		for _, tc := range []struct {
			size, payload int
			ok            bool
		}{
			{least - 1, 3000, false},
			{least, 3000, true},
			{least + 1, 3000, true},
			{1, 3000, false},
			{65535, 3000, true},
			{least, packet.MaxTSOSegments * least, true},    // 128 segments, the last one full
			{least, packet.MaxTSOSegments*least + 1, false}, // 129
			{least, packet.MaxTSOSegments*least - 1, true},  // 128, the last one short
			{least, (packet.MaxTSOSegments + 1) * least, false},
			{1460, packet.MaxTSOSegments * 1460, true},
			{1460, packet.MaxTSOSegments*1460 + 1, false},
		} {
			name := fmt.Sprintf("v6=%v size=%d payload=%d", v6, tc.size, tc.payload)
			h, f := kernelSuper(spec{v6: v6, flags: 0x10, seq: 1, sport: 1, payload: randomBytes(tc.payload)}, tc.size)
			if h.GSOSize = uint16(tc.size); packet.CheckTSOSize(h, f) == nil != tc.ok || h.TSOSizeOK(tc.payload) != tc.ok {
				t.Errorf("%s: CheckTSOSize %v, TSOSizeOK %v, want ok=%v", name, packet.CheckTSOSize(h, f), h.TSOSizeOK(tc.payload), tc.ok)
			}
			if err := packet.CheckTSOSize(h, f); err != nil && !errors.Is(err, packet.ErrOffload) {
				t.Errorf("%s: %v is not ErrOffload", name, err)
			}
			// CheckTSO is not about the size.
			if packet.CheckTSO(h, f) != nil {
				t.Errorf("%s: CheckTSO refuses it", name)
			}
		}
	}
	// The ECN bit does not change the IP version, and a frame CheckTSO refuses is refused.
	h, f := kernelSuper(spec{v6: true, flags: 0x10, seq: 1, sport: 1, payload: randomBytes(3000)}, 1220)
	h.GSOType |= packet.VnetGSOECN
	if packet.CheckTSOSize(h, f) != nil {
		t.Error("an IPv6 frame of 1220 with the ECN bit is refused")
	}
	h.GSOSize = 1219
	if packet.CheckTSOSize(h, f) == nil {
		t.Error("an IPv6 frame of 1219 with the ECN bit is accepted")
	}
	h.GSOSize = 0
	if packet.CheckTSOSize(h, f) == nil || h.TSOSizeOK(3000) {
		t.Error("a segment size of 0 is accepted")
	}
}

func TestChecksum16MatchesTheDefinition(t *testing.T) {
	// The worked example of RFC 1071 section 3.
	if got := packet.Checksum16([]byte{0x00, 0x01, 0xf2, 0x03, 0xf4, 0xf5, 0xf6, 0xf7}); got != ^uint16(0xddf2) {
		t.Errorf("the example of RFC 1071: %#x", got)
	}
	r := rand.New(rand.NewSource(1))
	for n := 0; n < 300; n++ {
		b := make([]byte, n)
		r.Read(b)
		if n%3 == 0 {
			for i := range b {
				b[i] = 0xff // the carries
			}
		}
		for off := 0; off < 2 && off <= n; off++ {
			if got, want := packet.Checksum16(b[off:]), ^refSum(b[off:]); got != want {
				t.Fatalf("length %d offset %d: %#x, want %#x", n-off, off, got, want)
			}
		}
	}
	if got := packet.Checksum16(randomBytes(65535)); got != ^refSum(randomBytes(65535)) {
		t.Errorf("65535 bytes: %#x", got)
	}
}

// frames cuts s into segments of mss bytes, as a peer sends them: complete frames.
func frames(s spec, mss, n int) []*packet.EthernetFrame {
	var out []*packet.EthernetFrame
	payload := s.payload
	for i := 0; i < n; i++ {
		seg := s
		seg.payload = payload[i*mss : min((i+1)*mss, len(payload))]
		seg.seq = s.seq + uint32(i*mss)
		seg.id = s.id + uint16(i)
		raw := build(seg)
		f, err := packet.ParseEthernet(raw)
		if err != nil {
			panic(err)
		}
		out = append(out, f)
	}
	return out
}

// run offers frames to a Coalescer the way a writer does and returns what is written.
func run(frames ...*packet.EthernetFrame) (out [][]byte, segs []int) {
	var c packet.Coalescer
	flush := func() {
		if c.Pending() {
			b, n := c.Take()
			out, segs = append(out, append([]byte(nil), b...)), append(segs, n)
		}
	}
	for _, f := range frames {
		if c.Merge(f) {
			continue
		}
		flush()
		if err := c.Start(f); err != nil {
			panic(err)
		}
	}
	flush()
	return out, segs
}

func TestCoalesce(t *testing.T) {
	const mss = 1398
	base := spec{flags: 0x10, seq: 1000, ack: 5, win: 200, sport: 4000, id: 10}
	for _, v6 := range []bool{false, true} {
		t.Run("v6="+strconv.FormatBool(v6), func(t *testing.T) {
			s := base
			s.v6 = v6
			s.payload = randomBytes(5*mss + 100)
			fs := frames(s, mss, 6)
			out, segs := run(fs...)
			if len(out) != 1 || segs[0] != 6 {
				t.Fatalf("%d writes with %v segments, want one with 6", len(out), segs)
			}
			b := out[0]
			h := packet.ParseVnetHeader(b)
			frame := b[packet.VnetHeaderLen:]
			l4 := 14 + 20
			gso := byte(packet.VnetGSOTCPv4)
			if v6 {
				l4, gso = 14+40, packet.VnetGSOTCPv6
			}
			want := packet.VnetHeader{Flags: packet.VnetNeedsCsum, GSOType: gso, HdrLen: uint16(l4 + 20), GSOSize: mss, CsumStart: uint16(l4), CsumOffset: 16}
			if h != want {
				t.Errorf("header %+v, want %+v", h, want)
			}
			// The checksum field holds the pseudo-header's sum, and with the rest of the
			// segment the sum is what completes it.
			src, dst := addrs(v6)
			tcp := frame[l4:]
			if got, w := binary.BigEndian.Uint16(tcp[16:]), refPseudo(src, dst, len(tcp)); got != w {
				t.Errorf("checksum field %#x, want the pseudo-header's sum %#x", got, w)
			}
			if v6 {
				if int(binary.BigEndian.Uint16(frame[14+4:])) != len(tcp) {
					t.Error("IPv6 payload length")
				}
			} else if int(binary.BigEndian.Uint16(frame[14+2:])) != len(frame)-14 || refSum(frame[14:34]) != 0xffff {
				t.Error("IPv4 length or header checksum")
			}
			// Cutting it again gives the frames it was made of.
			var sg packet.Segmenter
			back, err := sg.Segment(h, frame)
			if err != nil || len(back) != 6 {
				t.Fatalf("segmenting the super-frame: %d frames, %v", len(back), err)
			}
			for i, f := range back {
				orig, _ := fs[i].Marshal()
				if !bytes.Equal(f, orig) {
					t.Errorf("segment %d differs from the original", i)
				}
			}
		})
	}
}

func TestCoalesceRules(t *testing.T) {
	const mss = 1000
	base := spec{flags: 0x10, seq: 1000, ack: 5, win: 200, sport: 4000, id: 10, payload: randomBytes(4 * mss)}
	// two builds a pair of segments from base and lets change alter the second.
	two := func(change func(s *spec)) []*packet.EthernetFrame {
		fs := frames(base, mss, 2)
		s2 := base
		s2.seq += mss
		s2.payload = base.payload[mss : 2*mss]
		s2.id++
		if change != nil {
			change(&s2)
		}
		f, _ := packet.ParseEthernet(build(s2))
		return []*packet.EthernetFrame{fs[0], f}
	}
	for _, tc := range []struct {
		name  string
		fs    []*packet.EthernetFrame
		wants []int // segments in each write
	}{
		{"in order", two(nil), []int{2}},
		{"a gap", two(func(s *spec) { s.seq++ }), []int{1, 1}},
		{"a retransmission", two(func(s *spec) { s.seq -= mss }), []int{1, 1}},
		{"another port", two(func(s *spec) { s.sport++ }), []int{1, 1}},
		{"another acknowledgment", two(func(s *spec) { s.ack++ }), []int{1, 1}},
		{"another window", two(func(s *spec) { s.win-- }), []int{1, 1}},
		{"other options", two(func(s *spec) { s.opts = timestamps }), []int{1, 1}},
		{"another destination MAC", two(func(s *spec) { s.dstMAC = 9 }), []int{1, 1}},
		{"SYN", two(func(s *spec) { s.flags = 0x12 }), []int{1, 1}},
		{"FIN", two(func(s *spec) { s.flags = 0x11 }), []int{1, 1}},
		{"RST", two(func(s *spec) { s.flags = 0x14 }), []int{1, 1}},
		{"URG", two(func(s *spec) { s.flags = 0x30 }), []int{1, 1}},
		{"CWR", two(func(s *spec) { s.flags = 0x90 }), []int{1, 1}},
		{"ECE", two(func(s *spec) { s.flags = 0x50 }), []int{1, 1}},
		{"no ACK", two(func(s *spec) { s.flags = 0 }), []int{1, 1}},
		{"PSH on the second", two(func(s *spec) { s.flags = 0x18 }), []int{2}},
		{"longer than the first", two(func(s *spec) { s.payload = append(s.payload, 1) }), []int{1, 1}},
		{"no payload", two(func(s *spec) { s.payload = nil }), []int{1, 1}},
		{"a bad checksum", two(func(s *spec) { s.badCsum = true }), []int{1, 1}},
		{"IPv4 options", two(func(s *spec) { s.ipOpts = []byte{1, 1, 1, 0} }), []int{1, 1}},
		{"a fragment", two(func(s *spec) { s.fragOffset = 0x2000 }), []int{1, 1}},
		{"IPv6 after IPv4", two(func(s *spec) { s.v6 = true }), []int{1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, segs := run(tc.fs...)
			if fmt.Sprint(segs) != fmt.Sprint(tc.wants) {
				t.Errorf("writes of %v segments, want %v", segs, tc.wants)
			}
			// Whatever is not merged is written as it was, with an empty header.
			if len(out) == 2 {
				for i, b := range out {
					orig, _ := tc.fs[i].Marshal()
					if h := packet.ParseVnetHeader(b); h != (packet.VnetHeader{}) || !bytes.Equal(b[packet.VnetHeaderLen:], orig) {
						t.Errorf("write %d is not the frame as it was, with an empty header", i)
					}
				}
			}
		})
	}

	t.Run("a bad checksum in the first", func(t *testing.T) {
		bad := base
		bad.badCsum = true
		fs := frames(base, mss, 3)
		fs[0] = frames(bad, mss, 1)[0]
		_, segs := run(fs...)
		if fmt.Sprint(segs) != "[1 2]" {
			t.Errorf("writes of %v segments, want [1 2]: the bad one alone, the good two together", segs)
		}
	})
	t.Run("a short segment ends the super-frame", func(t *testing.T) {
		s := base
		s.payload = base.payload[:2*mss+500]
		fs := frames(s, mss, 3)
		more := base
		more.seq += uint32(len(s.payload))
		more.payload = base.payload[:mss]
		fs = append(fs, frames(more, mss, 1)...)
		if _, segs := run(fs...); fmt.Sprint(segs) != "[3 1]" {
			t.Errorf("writes of %v segments, want [3 1]", segs)
		}
	})
	t.Run("PSH ends it and is kept", func(t *testing.T) {
		fs := frames(base, mss, 4)
		fs[2], _ = packet.ParseEthernet(build(spec{flags: 0x18, seq: base.seq + 2*mss, ack: base.ack, win: base.win, sport: base.sport, id: base.id + 2, payload: base.payload[2*mss : 3*mss]}))
		out, segs := run(fs...)
		if fmt.Sprint(segs) != "[3 1]" {
			t.Fatalf("writes of %v segments, want [3 1]", segs)
		}
		if flags := out[0][packet.VnetHeaderLen+14+20+13]; flags != 0x18 {
			t.Errorf("flags of the super-frame %#x, want ACK|PSH", flags)
		}
	})
	t.Run("not TCP", func(t *testing.T) {
		arp := &packet.EthernetFrame{DstMAC: fs0().DstMAC, SrcMAC: fs0().SrcMAC, EtherType: packet.EtherTypeARP, Payload: make([]byte, 28)}
		udp := fs0()
		udp.Payload = append([]byte(nil), udp.Payload...)
		udp.Payload[9] = 17
		if _, segs := run(fs0(), arp, fs0(), udp, fs0()); fmt.Sprint(segs) != "[1 1 1 1 1]" {
			t.Errorf("writes of %v segments", segs)
		}
	})
	t.Run("the size of an IP packet", func(t *testing.T) {
		s := base
		s.payload = randomBytes(66 * mss)
		fs := frames(s, mss, 66)
		out, segs := run(fs...)
		if len(segs) != 2 || segs[0] != 65 || segs[1] != 1 {
			t.Fatalf("writes of %v segments, want [65 1]", segs)
		}
		if got := len(out[0]) - packet.VnetHeaderLen - 14; got > 65535 {
			t.Errorf("the super-frame's IP packet is %d bytes", got)
		}
	})
	t.Run("a frame that cannot be encoded", func(t *testing.T) {
		var c packet.Coalescer
		if err := c.Start(&packet.EthernetFrame{EtherType: 0x800}); err == nil || c.Pending() {
			t.Errorf("Start accepted a frame without addresses: %v", err)
		}
	})
}

// A frame marked Trusted is merged without its TCP checksum being checked; one that is
// not is refused if the checksum is wrong. For frames with right checksums, the result is
// the same either way.
func TestCoalesceTrusted(t *testing.T) {
	const mss = 1000
	base := spec{flags: 0x10, seq: 1000, ack: 5, win: 200, sport: 4000, id: 10, payload: randomBytes(4 * mss)}
	mark := func(fs []*packet.EthernetFrame, trusted bool) []*packet.EthernetFrame {
		for _, f := range fs {
			f.Trusted = trusted
		}
		return fs
	}
	// corrupt flips a payload byte of segment i, which leaves its checksum wrong.
	corrupt := func(fs []*packet.EthernetFrame, i int) []*packet.EthernetFrame {
		fs[i].Payload[len(fs[i].Payload)-1] ^= 0xff
		return fs
	}

	t.Run("right checksums: the same output", func(t *testing.T) {
		verified, vsegs := run(mark(frames(base, mss, 4), false)...)
		trusted, tsegs := run(mark(frames(base, mss, 4), true)...)
		if fmt.Sprint(vsegs) != "[4]" || fmt.Sprint(tsegs) != "[4]" || !bytes.Equal(verified[0], trusted[0]) {
			t.Errorf("verified %v, trusted %v segments, equal=%v", vsegs, tsegs, bytes.Equal(verified[0], trusted[0]))
		}
	})
	for _, bad := range []int{0, 2} { // the first segment is checked when a second joins, the others as they join
		t.Run(fmt.Sprintf("segment %d wrong", bad), func(t *testing.T) {
			_, segs := run(mark(corrupt(frames(base, mss, 4), bad), false)...)
			if len(segs) < 2 {
				t.Errorf("a segment with a wrong checksum was merged: writes of %v segments", segs)
			}
			out, segs := run(mark(corrupt(frames(base, mss, 4), bad), true)...)
			if fmt.Sprint(segs) != "[4]" {
				t.Errorf("trusted segments were not merged: writes of %v segments", segs)
			}
			want, _ := run(mark(frames(base, mss, 4), true)...)
			want[0][packet.VnetHeaderLen+14+20+20+(bad+1)*mss-1] ^= 0xff // the flipped byte, in the merged payload
			if !bytes.Equal(out[0], want[0]) {
				t.Error("the merged output is not the segments' bytes")
			}
		})
	}
	t.Run("trust is per frame", func(t *testing.T) {
		// The first segment is verified (and fine), and so is the third, which is not.
		fs := corrupt(frames(base, mss, 4), 2)
		fs[0].Trusted, fs[1].Trusted, fs[3].Trusted = false, true, true
		if _, segs := run(fs...); len(segs) < 2 {
			t.Errorf("an untrusted segment with a wrong checksum was merged: %v", segs)
		}
	})
}

func fs0() *packet.EthernetFrame {
	return frames(spec{flags: 0x10, seq: 1, payload: randomBytes(100), sport: 1}, 100, 1)[0]
}

func benchSpec(mss, n int) spec {
	return spec{flags: 0x10, seq: 1, ack: 5, win: 200, sport: 4000, opts: timestamps, payload: randomBytes(mss * n)}
}

func BenchmarkSegment(b *testing.B) {
	for _, mss := range []int{8898, 1398} {
		b.Run(strconv.Itoa(mss), func(b *testing.B) {
			n := 65000 / mss
			h, super := kernelSuper(benchSpec(mss, n), mss)
			orig := append([]byte(nil), super...)
			var sg packet.Segmenter
			b.ReportAllocs()
			b.SetBytes(int64(len(super)))
			for i := 0; i < b.N; i++ {
				copy(super, orig)
				if _, err := sg.Segment(h, super); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkCoalesce(b *testing.B) {
	for _, mss := range []int{8898, 1398} {
		for _, trusted := range []bool{false, true} {
			b.Run(fmt.Sprintf("%d/trusted=%v", mss, trusted), func(b *testing.B) {
				n := 65000 / mss
				fs := frames(benchSpec(mss, n), mss, n)
				var c packet.Coalescer
				total := 0
				for _, f := range fs {
					f.Trusted = trusted
					total += len(f.Payload) + 14
				}
				b.ReportAllocs()
				b.SetBytes(int64(total))
				for i := 0; i < b.N; i++ {
					for _, f := range fs {
						if !c.Merge(f) {
							if c.Pending() {
								c.Take()
							}
							if err := c.Start(f); err != nil {
								b.Fatal(err)
							}
						}
					}
					c.Take()
				}
			})
		}
	}
}
