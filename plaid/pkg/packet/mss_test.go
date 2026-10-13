package packet_test

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"

	"plaid/pkg/packet"
)

// checksumOK checks the TCP checksum of frame, an Ethernet frame over IPv4 or IPv6, from
// scratch.
func checksumOK(frame []byte, v6 bool) bool {
	ip := frame[14:]
	src, dst, tcp := ip[12:16], ip[16:20], ip[20:]
	if v6 {
		src, dst, tcp = ip[8:24], ip[24:40], ip[40:]
	}
	sum := uint32(refPseudo(src, dst, len(tcp))) + uint32(refSum(tcp))
	return sum&0xffff+sum>>16 == 0xffff
}

// synFrame builds an Ethernet frame with a TCP segment with the flags, the options (padded
// with end-of-list bytes to a multiple of four) and a few bytes of data, over IPv4 or IPv6,
// with addresses made from addr.
func synFrame(v6 bool, flags byte, opts []byte, addr byte) []byte {
	for len(opts)%4 != 0 {
		opts = append(opts, 0)
	}
	return build(spec{v6: v6, sport: 40000, seq: 1000, win: 65535, flags: flags, opts: opts, payload: []byte{1, 2, 3, 4}, dstMAC: 1, addr: addr})
}

func mssOpt(v uint16) []byte { return []byte{2, 4, byte(v >> 8), byte(v)} }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// mssOf is the value of the MSS option that the test put at offset off of the options.
func mssOf(frame []byte, v6 bool, off int) uint16 {
	l4 := 14 + 20
	if v6 {
		l4 = 14 + 40
	}
	return binary.BigEndian.Uint16(frame[l4+20+off+2:])
}

// The MSS option is found whatever comes before it, changed to the target (the MTU less
// the IP header and 20), and the checksum stays right: from scratch, over the whole
// segment, for options that put the two bytes on an even and on an odd offset. Both
// directions, both IP versions.
func TestSYNMSSRewrite(t *testing.T) {
	sackPerm, ts, ws := []byte{4, 2}, []byte{8, 10, 1, 2, 3, 4, 5, 6, 7, 8}, []byte{3, 3, 7}
	nop := []byte{1}
	for _, v6 := range []bool{false, true} {
		hdr, name := 40, "IPv4"
		if v6 {
			hdr, name = 60, "IPv6"
		}
		for _, tc := range []struct {
			name string
			opts func(v uint16) []byte
			off  int // of the MSS option among the options
		}{
			{"first", func(v uint16) []byte { return cat(mssOpt(v), sackPerm, ts, nop, ws) }, 0},
			{"after one NOP", func(v uint16) []byte { return cat(nop, mssOpt(v), sackPerm) }, 1},
			{"after two NOPs", func(v uint16) []byte { return cat(nop, nop, mssOpt(v)) }, 2},
			{"after three NOPs", func(v uint16) []byte { return cat(nop, nop, nop, mssOpt(v)) }, 3},
			{"after other options", func(v uint16) []byte { return cat(sackPerm, ts, mssOpt(v)) }, 12},
			{"after an odd option", func(v uint16) []byte { return cat(ws, mssOpt(v)) }, 3},
			{"the only option", mssOpt, 0},
		} {
			for _, flags := range []byte{packet.TCPSyn, packet.TCPSyn | packet.TCPAck} {
				for _, raise := range []bool{true, false} {
					from, mtu, want := uint16(1410), 65520, uint16(65520-hdr)
					change := packet.RaiseSYNMSS
					if !raise {
						from, mtu, want = 8910, 1500, uint16(1500-hdr)
						change = packet.LowerSYNMSS
					}
					frame := synFrame(v6, flags, tc.opts(from), 3)
					if !checksumOK(frame, v6) {
						t.Fatal("the frame built is wrong")
					}
					if !change(frame, mtu) {
						t.Errorf("%s %s flags %#x raise=%v: not changed", name, tc.name, flags, raise)
						continue
					}
					if got := mssOf(frame, v6, tc.off); got != want {
						t.Errorf("%s %s: MSS %d, want %d", name, tc.name, got, want)
					}
					if !checksumOK(frame, v6) {
						t.Errorf("%s %s flags %#x raise=%v: the TCP checksum is wrong afterwards", name, tc.name, flags, raise)
					}
					exp := synFrame(v6, flags, tc.opts(want), 3)
					if !bytes.Equal(frame, exp) {
						t.Errorf("%s %s: more than the option and the checksum changed", name, tc.name)
					}
				}
			}
		}
	}
}

// Whatever the addresses and the old and new values, the incremental checksum equals the
// one computed from scratch (the corner cases of ones-complement arithmetic included).
func TestSYNMSSChecksumAgainstScratch(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		v6 := i%2 == 1
		old := uint16(rnd.Intn(65536))
		opts := cat([]byte{1}, mssOpt(old), []byte{4, 2})
		if i%4 >= 2 {
			opts = cat(mssOpt(old), []byte{4, 2})
		}
		frame := synFrame(v6, packet.TCPSyn, opts, byte(rnd.Intn(256)))
		mtu := 576 + rnd.Intn(65000)
		hdr := 40
		if v6 {
			hdr = 60
		}
		changed := packet.RaiseSYNMSS(frame, mtu)
		if changed != (int(old) < mtu-hdr) {
			t.Fatalf("MSS %d, MTU %d: changed=%v", old, mtu, changed)
		}
		if !checksumOK(frame, v6) {
			t.Fatalf("v6=%v MSS %d to MTU %d: the checksum is wrong", v6, old, mtu)
		}
	}
}

// Frames that are not to be changed are left byte for byte: no MSS option, options that do
// not parse, not a SYN, an MSS that is there already, other protocols, short frames.
func TestSYNMSSLeavesOtherFramesAlone(t *testing.T) {
	sackPerm := []byte{4, 2}
	for _, v6 := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			flags byte
			opts  []byte
		}{
			{"no options", packet.TCPSyn, nil},
			{"no MSS option", packet.TCPSyn, cat(sackPerm, []byte{1, 3, 3, 7})},
			{"an ACK", packet.TCPAck, mssOpt(1410)},
			{"a RST", packet.TCPRst | packet.TCPAck, mssOpt(1410)},
			{"a data segment", packet.TCPAck | 0x08, cat([]byte{1, 1}, mssOpt(1410))},
			{"end of list before the MSS", packet.TCPSyn, cat([]byte{0}, mssOpt(1410))},
			{"option of length 0", packet.TCPSyn, cat([]byte{9, 0}, mssOpt(1410))},
			{"option of length 1", packet.TCPSyn, cat([]byte{9, 1}, mssOpt(1410))},
			{"option beyond the header", packet.TCPSyn, cat([]byte{9, 20}, mssOpt(1410))},
			{"a kind with no length", packet.TCPSyn, cat([]byte{1, 1, 1, 9})},
			{"an MSS option of length 3", packet.TCPSyn, []byte{2, 3, 5, 0xb4}},
			{"an MSS option that is cut", packet.TCPSyn, cat([]byte{1, 1}, []byte{2, 4, 5, 0})},
			{"an MSS above the target", packet.TCPSyn, mssOpt(65500)},
			{"an MSS at the target", packet.TCPSyn, mssOpt(65480)},
		} {
			frame := synFrame(v6, tc.flags, tc.opts, 3)
			if tc.name == "an MSS option that is cut" {
				l4 := 14 + 20
				if v6 {
					l4 = 14 + 40
				}
				frame[l4+12] = 6 << 4 // the header ends after "1 1 2 4": the option's value is past it
			}
			orig := append([]byte(nil), frame...)
			if packet.RaiseSYNMSS(frame, 65520) || !bytes.Equal(frame, orig) {
				t.Errorf("v6=%v %s: raised", v6, tc.name)
			}
		}
	}

	// Lowering leaves an MSS at or below the target.
	for _, v6 := range []bool{false, true} {
		for _, v := range []uint16{1400, 1460, 1440} {
			frame := synFrame(v6, packet.TCPSyn, mssOpt(v), 3)
			target := uint16(1460)
			if v6 {
				target = 1440
			}
			changed := packet.LowerSYNMSS(frame, 1500)
			if changed != (v > target) {
				t.Errorf("v6=%v MSS %d lowered to %d: changed=%v", v6, v, target, changed)
			}
		}
	}

	// Not TCP, not IP, too short, an IPv4 fragment: nothing.
	udp := synFrame(false, packet.TCPSyn, mssOpt(1410), 3)
	udp[14+9] = 17
	arp := synFrame(false, packet.TCPSyn, mssOpt(1410), 3)
	arp[12], arp[13] = 0x08, 0x06
	frag := synFrame(false, packet.TCPSyn, mssOpt(1410), 3)
	frag[14+6] = 0x20
	for name, f := range map[string][]byte{"UDP": udp, "ARP": arp, "a fragment": frag, "empty": nil, "short": make([]byte, 30), "an Ethernet header": make([]byte, 14)} {
		orig := append([]byte(nil), f...)
		if packet.RaiseSYNMSS(f, 65520) || packet.LowerSYNMSS(f, 576) || !bytes.Equal(f, orig) {
			t.Errorf("%s: changed", name)
		}
	}
}

// RaiseSYNMSSCopy leaves the frame it is given as it was and returns the changed copy; a
// frame it would not change comes back as it is, not copied.
func TestRaiseSYNMSSCopy(t *testing.T) {
	for _, v6 := range []bool{false, true} {
		frame := synFrame(v6, packet.TCPSyn, mssOpt(1410), 5)
		orig := append([]byte(nil), frame...)
		out, changed := packet.RaiseSYNMSSCopy(frame, 65520)
		if !changed || !bytes.Equal(frame, orig) || bytes.Equal(out, orig) {
			t.Fatalf("v6=%v: changed=%v, original kept=%v", v6, changed, bytes.Equal(frame, orig))
		}
		want := append([]byte(nil), orig...)
		if !packet.RaiseSYNMSS(want, 65520) || !bytes.Equal(out, want) {
			t.Errorf("v6=%v: the copy is not what the change in place gives", v6)
		}
		if out, changed := packet.RaiseSYNMSSCopy(frame, 576); changed || &out[0] != &frame[0] {
			t.Errorf("v6=%v: a frame left as it is was copied (changed=%v)", v6, changed)
		}
	}
}

// Changing a handshake allocates nothing, and looking at a frame that is not one costs no
// allocation either.
func TestSYNMSSDoesNotAllocate(t *testing.T) {
	syn := synFrame(false, packet.TCPSyn, cat([]byte{1}, mssOpt(1410), []byte{4, 2}), 3)
	ack := synFrame(false, packet.TCPAck, nil, 3)
	if n := testing.AllocsPerRun(100, func() {
		binary.BigEndian.PutUint16(syn[14+20+20+3:], 1410)
		packet.RaiseSYNMSS(syn, 65520)
		packet.LowerSYNMSS(ack, 1500)
	}); n != 0 {
		t.Errorf("%v allocations", n)
	}
}
