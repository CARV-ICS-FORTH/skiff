package packet_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"plaid/pkg/packet"
)

// A plain TCP frame above the pod MTU, with complete checksums as the bubble's kernel
// forwards it, gets the header of a TSO segment, and cut by it makes frames that fit the
// MTU, in order, with right checksums (checked independently of the code that makes them).
// A segment that the kernel handed over keeps its header, with the MSS of the MTU.
func TestTSOFor(t *testing.T) {
	const mtu, total = 1450, 60000
	data := randomBytes(total)
	for _, tc := range []struct {
		name string
		s    spec
		opts int // bytes of TCP options
		ihl  int
	}{
		{"IPv4", spec{flags: packet.TCPAck, payload: data, seq: 7}, 0, 20},
		{"IPv4 with timestamps", spec{flags: packet.TCPAck, payload: data, opts: timestamps, seq: 7}, len(timestamps), 20},
		{"IPv4 with IP options", spec{flags: packet.TCPAck, payload: data, ipOpts: []byte{1, 1, 1, 1}, seq: 7}, 0, 24},
		{"IPv6", spec{v6: true, flags: packet.TCPAck, payload: data, seq: 7}, 0, 40},
		{"PSH and FIN", spec{flags: packet.TCPAck | 0x08 | packet.TCPFin, payload: data, seq: 7}, 0, 20},
		{"CWR", spec{flags: packet.TCPAck | 0x80, payload: data, seq: 7}, 0, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := build(tc.s)
			h, err := packet.TSOFor(packet.VnetHeader{}, frame, mtu)
			if err != nil {
				t.Fatal(err)
			}
			l4 := 14 + tc.ihl
			want := packet.VnetHeader{
				Flags: packet.VnetNeedsCsum, GSOType: packet.VnetGSOTCPv4, HdrLen: uint16(l4 + 20 + tc.opts),
				GSOSize: uint16(mtu - tc.ihl - 20 - tc.opts), CsumStart: uint16(l4), CsumOffset: 16,
			}
			if tc.s.v6 {
				want.GSOType = packet.VnetGSOTCPv6
			}
			if tc.s.flags&0x80 != 0 {
				want.GSOType |= packet.VnetGSOECN
			}
			if h != want {
				t.Fatalf("header %+v, want %+v", h, want)
			}
			if err := packet.CheckTSO(h, frame); err != nil {
				t.Fatalf("the checks the kernel's header passes: %v", err)
			}
			var sg packet.Segmenter
			segs, err := sg.Segment(h, frame)
			if err != nil {
				t.Fatal(err)
			}
			var got []byte
			for i, f := range segs {
				if len(f) > 14+mtu {
					t.Fatalf("segment %d has %d bytes of IP packet", i, len(f)-14)
				}
				_, payload := checkFrame(t, f, tc.s.v6)
				got = append(got, payload...)
			}
			if !bytes.Equal(got, data) {
				t.Error("the segments carry other bytes")
			}
		})
	}

	t.Run("a TSO segment keeps its header, with a smaller segment size", func(t *testing.T) {
		h, frame := kernelSuper(spec{flags: packet.TCPAck, payload: data}, 9000)
		got, err := packet.TSOFor(h, frame, mtu)
		if err != nil || got.GSOSize != mtu-40 {
			t.Fatalf("%+v, %v", got, err)
		}
		got.GSOSize = h.GSOSize
		if got != h {
			t.Errorf("header changed beyond the size: %+v, was %+v", got, h)
		}
		h, frame = kernelSuper(spec{flags: packet.TCPAck, payload: data}, 1000) // smaller already
		if got, err = packet.TSOFor(h, frame, mtu); err != nil || got != h {
			t.Errorf("a size below the MSS was changed: %+v, %v", got, err)
		}
	})

	t.Run("a checksum for the kernel to complete is left to the cut", func(t *testing.T) {
		_, frame := kernelSuper(spec{flags: packet.TCPAck, payload: data}, 9000)
		h := packet.VnetHeader{Flags: packet.VnetNeedsCsum, CsumStart: 34, CsumOffset: 16}
		got, err := packet.TSOFor(h, frame, mtu)
		if err != nil || got.GSOType != packet.VnetGSOTCPv4 {
			t.Fatalf("%+v, %v", got, err)
		}
		var sg packet.Segmenter
		segs, err := sg.Segment(got, frame)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range segs {
			checkFrame(t, f, false)
		}
	})

	t.Run("what it refuses", func(t *testing.T) {
		bad := build(spec{flags: packet.TCPAck, payload: data, badCsum: true})
		if _, err := packet.TSOFor(packet.VnetHeader{}, bad, mtu); !errors.Is(err, packet.ErrOffload) {
			t.Errorf("a wrong checksum: %v", err)
		}
		// The kernel says it has checked it.
		if _, err := packet.TSOFor(packet.VnetHeader{Flags: packet.VnetDataValid}, bad, mtu); err != nil {
			t.Errorf("a checksum the kernel has checked: %v", err)
		}
		if _, err := packet.TSOFor(packet.VnetHeader{Flags: packet.VnetNeedsCsum, CsumStart: 20, CsumOffset: 16}, build(spec{payload: data}), mtu); !errors.Is(err, packet.ErrOffload) {
			t.Errorf("offsets that are not the TCP header's: %v", err)
		}
		udp := build(spec{payload: data})
		udp[14+9] = 17
		if _, err := packet.TSOFor(packet.VnetHeader{}, udp, mtu); !errors.Is(err, packet.ErrNotTCP) {
			t.Errorf("UDP: %v", err)
		}
		frag := build(spec{payload: data, fragOffset: 100})
		if _, err := packet.TSOFor(packet.VnetHeader{}, frag, mtu); !errors.Is(err, packet.ErrNotTCP) {
			t.Errorf("a fragment: %v", err)
		}
		padded := append(build(spec{payload: data}), 0, 0, 0, 0)
		if _, err := packet.TSOFor(packet.VnetHeader{}, padded, mtu); !errors.Is(err, packet.ErrNotTCP) {
			t.Errorf("a frame longer than its IP packet: %v", err)
		}
		h, frame := kernelSuper(spec{payload: data}, 9000)
		h.GSOType = 3 // UDP
		if _, err := packet.TSOFor(h, frame, mtu); !errors.Is(err, packet.ErrOffload) {
			t.Errorf("a GSO type that is not TCP: %v", err)
		}
	})
}

// CompleteSuper makes a TSO segment one packet whose lengths and checksums are right
// whatever the sender left in them, with the headers and flags it had.
func TestCompleteSuper(t *testing.T) {
	for _, s := range []spec{
		{flags: packet.TCPAck | 0x08, payload: randomBytes(50000)},
		{flags: packet.TCPAck | packet.TCPFin, opts: timestamps, payload: randomBytes(50001)},
		{flags: packet.TCPAck, ipOpts: []byte{1, 1, 1, 1}, payload: randomBytes(40000)},
		{v6: true, flags: packet.TCPAck | 0x80, payload: randomBytes(50000)},
	} {
		h, frame := kernelSuper(s, 1398)
		ip := frame[14:]
		want := append([]byte(nil), ip...)
		// What the sender leaves undone is not trusted: the lengths and checksums are
		// made wrong first.
		if s.v6 {
			binary.BigEndian.PutUint16(ip[4:], 1)
		} else {
			binary.BigEndian.PutUint16(ip[2:], 1)
			binary.BigEndian.PutUint16(ip[10:], 0x1234)
		}
		l4 := int(h.CsumStart) - 14
		binary.BigEndian.PutUint16(ip[l4+16:], 0x5678)
		if err := packet.CompleteSuper(h, ip); err != nil {
			t.Fatal(err)
		}
		checkFrame(t, frame, s.v6)
		if !bytes.Equal(ip[l4+4:], want[l4+4:l4+16]) && !bytes.Equal(ip[l4:l4+16], want[l4:l4+16]) {
			t.Errorf("the TCP header changed")
		}
		if !bytes.Equal(ip[l4+20:], want[l4+20:]) || ip[l4+13] != want[l4+13] {
			t.Errorf("the payload or the flags changed")
		}
	}
	h, frame := kernelSuper(spec{payload: randomBytes(5000)}, 1398)
	h.CsumStart = 6000
	if err := packet.CompleteSuper(h, frame[14:]); !errors.Is(err, packet.ErrOffload) {
		t.Errorf("offsets outside the packet: %v", err)
	}
}

// reassemble puts the fragments back together as a receiver does: it checks the headers,
// the offsets and the flags, and returns the packet.
func reassemble(t *testing.T, frames [][]byte, mtu int) (ip []byte, lastMore bool) {
	t.Helper()
	var data []byte
	for i, f := range frames {
		fip := f[14:]
		if len(fip) > mtu || int(binary.BigEndian.Uint16(fip[2:])) != len(fip) || refSum(fip[:20]) != 0xffff {
			t.Fatalf("fragment %d: %d bytes, total length %d, header sum %#x", i, len(fip), binary.BigEndian.Uint16(fip[2:]), refSum(fip[:20]))
		}
		ff := binary.BigEndian.Uint16(fip[6:])
		if int(ff&0x1fff)*8 != len(data)+int(binary.BigEndian.Uint16(frames[0][14+6:])&0x1fff)*8 {
			t.Fatalf("fragment %d at offset %d, expected %d", i, int(ff&0x1fff)*8, len(data))
		}
		if more := ff&0x2000 != 0; i < len(frames)-1 {
			if !more || len(fip[20:])%8 != 0 {
				t.Fatalf("fragment %d: more=%v, %d bytes", i, more, len(fip)-20)
			}
		} else {
			lastMore = more
		}
		if !bytes.Equal(fip[:2], frames[0][14:16]) || !bytes.Equal(fip[4:6], frames[0][18:20]) || !bytes.Equal(fip[8:10], frames[0][22:24]) || !bytes.Equal(fip[12:20], frames[0][26:34]) || !bytes.Equal(f[:14], frames[0][:14]) {
			t.Fatalf("fragment %d has other headers", i)
		}
		data = append(data, fip[20:]...)
	}
	ip = append(append([]byte(nil), frames[0][14:34]...), data...)
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	binary.BigEndian.PutUint16(ip[6:], 0)
	binary.BigEndian.PutUint16(ip[10:], 0)
	binary.BigEndian.PutUint16(ip[10:], ^refSum(ip[:20]))
	return ip, lastMore
}

func TestFragment(t *testing.T) {
	const mtu = 1450
	data := randomBytes(20000)
	// plain is a packet whose Don't Fragment flag is clear, as a datagram without it is.
	plain := func(s spec) []byte {
		f := build(s)
		f[14+6] &^= 0x40
		binary.BigEndian.PutUint16(f[14+10:], 0)
		binary.BigEndian.PutUint16(f[14+10:], ^refSum(f[14:34]))
		return f
	}
	var sg packet.Segmenter

	t.Run("reassembles to the packet", func(t *testing.T) {
		frame := plain(spec{flags: packet.TCPAck, payload: data})
		orig := append([]byte(nil), frame[14:]...)
		frags, err := sg.Fragment(packet.VnetHeader{}, frame, mtu)
		if err != nil {
			t.Fatal(err)
		}
		if want := (len(orig) - 20 + 1423) / 1424; len(frags) != want {
			t.Errorf("%d fragments, want %d", len(frags), want)
		}
		ip, lastMore := reassemble(t, frags, mtu)
		if lastMore || !bytes.Equal(ip, orig) {
			t.Errorf("reassembled packet differs (more fragments after the last: %v)", lastMore)
		}
	})

	t.Run("a fragment is fragmented again", func(t *testing.T) {
		frame := plain(spec{flags: packet.TCPAck, payload: data})
		binary.BigEndian.PutUint16(frame[14+6:], 0x2000|100) // More Fragments, offset 800 bytes
		binary.BigEndian.PutUint16(frame[14+10:], 0)
		binary.BigEndian.PutUint16(frame[14+10:], ^refSum(frame[14:34]))
		frags, err := sg.Fragment(packet.VnetHeader{}, frame, mtu)
		if err != nil {
			t.Fatal(err)
		}
		if _, lastMore := reassemble(t, frags, mtu); !lastMore {
			t.Error("the last fragment lost the More Fragments flag of the fragment it came from")
		}
	})

	t.Run("a checksum for the kernel to complete is completed", func(t *testing.T) {
		_, frame := kernelSuper(spec{flags: packet.TCPAck, payload: data}, 9000) // pseudo-header sum in the field
		frame[14+6] &^= 0x40
		binary.BigEndian.PutUint16(frame[14+10:], 0)
		binary.BigEndian.PutUint16(frame[14+10:], ^refSum(frame[14:34]))
		frags, err := sg.Fragment(packet.VnetHeader{Flags: packet.VnetNeedsCsum, CsumStart: 34, CsumOffset: 16}, frame, mtu)
		if err != nil {
			t.Fatal(err)
		}
		ip, _ := reassemble(t, frags, mtu)
		checkFrame(t, append(make([]byte, 14), ip...), false)
	})

	t.Run("what it refuses", func(t *testing.T) {
		for name, frame := range map[string][]byte{
			"Don't Fragment": build(spec{payload: data}),
			"IP options":     plain(spec{payload: data, ipOpts: []byte{1, 1, 1, 1}}),
			"IPv6":           build(spec{v6: true, payload: data}),
			"a short frame":  make([]byte, 20),
		} {
			if frags, err := sg.Fragment(packet.VnetHeader{}, frame, mtu); !errors.Is(err, packet.ErrTooLarge) || len(frags) != 0 {
				t.Errorf("%s: %d fragments, %v", name, len(frags), err)
			}
		}
		if _, err := sg.Fragment(packet.VnetHeader{GSOType: packet.VnetGSOTCPv4}, plain(spec{payload: data}), mtu); !errors.Is(err, packet.ErrOffload) {
			t.Errorf("a GSO frame: %v", err)
		}
	})
}
