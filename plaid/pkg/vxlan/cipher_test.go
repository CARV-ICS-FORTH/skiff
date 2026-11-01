package vxlan

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"plaid/pkg/packet"
)

var testKey = []byte(strings.Repeat("k", overlayKeyLen))

// testSubnet is the node of the tests' ciphers.
var testSubnet = &net.IPNet{IP: net.IPv4(10, 244, 2, 0).To4(), Mask: net.CIDRMask(24, 32)}

func testCipher(t testing.TB, key []byte) *overlayCipher {
	t.Helper()
	c, err := newOverlayCipher(key, testSubnet)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// sealedDatagram is what Send makes of a frame.
func sealedDatagram(c *overlayCipher, frame []byte) []byte {
	b := packet.AppendVXLANHeader(nil, packet.VXLANFlags, 7)
	b = append(b, make([]byte, overlayRoomLen)...)
	b = append(b, frame...)
	return c.seal(b, 0, c.local)
}

func TestCipherRoundTrip(t *testing.T) {
	c := testCipher(t, testKey)
	frame := []byte("a frame of the pods' network, of no particular length")
	sealed := sealedDatagram(c, frame)
	if strings.Contains(string(sealed), string(frame)) {
		t.Fatal("the frame is on the wire as it is")
	}
	if len(sealed) != packet.VXLANHeaderLen+overlayCipherBytes+len(frame) {
		t.Fatalf("%d bytes", len(sealed))
	}
	opened, ok := testCipher(t, testKey).open(sealed)
	if !ok {
		t.Fatal("it does not open")
	}
	if string(opened[packet.VXLANHeaderLen:]) != string(frame) || opened[0] != packet.VXLANFlags {
		t.Fatalf("opened as %q", opened)
	}
}

func TestCipherRefuses(t *testing.T) {
	c := testCipher(t, testKey)
	rx := testCipher(t, testKey)
	frame := []byte("a frame")
	for name, edit := range map[string]func([]byte){
		"a changed header": func(b []byte) { b[4] ^= 1 },
		"a changed name":   func(b []byte) { b[packet.VXLANHeaderLen] ^= 1 },
		"a changed time":   func(b []byte) { b[packet.VXLANHeaderLen+4] ^= 1 },
		"a changed node":   func(b []byte) { b[packet.VXLANHeaderLen+8] ^= 1 },
		"a changed nonce":  func(b []byte) { b[overlayAuthedLen] ^= 1 },
		"a changed frame":  func(b []byte) { b[packet.VXLANHeaderLen+overlayRoomLen] ^= 1 },
		"a changed tag":    func(b []byte) { b[len(b)-1] ^= 1 },
	} {
		b := sealedDatagram(c, frame)
		edit(b)
		if _, ok := rx.open(b); ok {
			t.Errorf("%s was taken", name)
		}
	}
	if _, ok := testCipher(t, []byte(strings.Repeat("o", overlayKeyLen))).open(sealedDatagram(c, frame)); ok {
		t.Error("another key took it")
	}
	if _, ok := rx.open(make([]byte, 20)); ok {
		t.Error("a short datagram was taken")
	}
}

func TestCipherRefusesWhatItHasSeen(t *testing.T) {
	c := testCipher(t, testKey)
	rx := testCipher(t, testKey)
	first := sealedDatagram(c, []byte("first"))
	replay := append([]byte(nil), first...)
	if _, ok := rx.open(first); !ok {
		t.Fatal("the first time")
	}
	if _, ok := rx.open(replay); ok {
		t.Error("the second time")
	}
	// Out of order within the window is fine.
	later := sealedDatagram(c, []byte("later"))
	second := sealedDatagram(c, []byte("second"))
	if _, ok := rx.open(second); !ok {
		t.Error("the later one")
	}
	if _, ok := rx.open(later); !ok {
		t.Error("the one that came behind it")
	}
	// A sender that started again has a prefix of its own, and its old one stays known.
	restarted := testCipher(t, testKey)
	if _, ok := rx.open(sealedDatagram(restarted, []byte("again"))); !ok {
		t.Error("after a restart")
	}
	old := sealedDatagram(c, []byte("old"))
	oldAgain := append([]byte(nil), old...)
	if _, ok := rx.open(old); !ok {
		t.Error("the first prefix again")
	}
	if _, ok := rx.open(oldAgain); ok {
		t.Error("what it saw of the first prefix")
	}
}

// What was sealed for one node is not taken by another, whatever address it comes from.
func TestCipherRefusesWhatIsForAnotherNode(t *testing.T) {
	tx, other := testCipher(t, testKey), testCipher(t, testKey)
	other.local = subnetName(&net.IPNet{IP: net.IPv4(10, 244, 3, 0)})
	sealed := sealedDatagram(tx, []byte("for the node of 10.244.2.0"))
	if _, ok := other.open(append([]byte(nil), sealed...)); ok {
		t.Error("another node took it")
	}
	if _, ok := testCipher(t, testKey).open(sealed); !ok {
		t.Error("the node it is for did not")
	}
}

// What a sender sent before its last start is refused however many starts ago that was, and a
// start within the same second is not. One sender does not affect another.
func TestCipherRefusesAnEarlierStart(t *testing.T) {
	first := testCipher(t, testKey)
	first.started = 1000
	rx := testCipher(t, testKey)
	captured := sealedDatagram(first, []byte("from the first start"))
	if _, ok := rx.open(append([]byte(nil), captured...)); !ok {
		t.Fatal("the first start")
	}
	unseen := sealedDatagram(first, []byte("never arrived"))
	for i := uint32(1); i <= 2*replayPrefixes; i++ {
		later := testCipher(t, testKey)
		later.started = 1000 + i
		if _, ok := rx.open(sealedDatagram(later, []byte("a later start"))); !ok {
			t.Fatalf("start %d", i)
		}
		if _, ok := rx.open(append([]byte(nil), captured...)); ok {
			t.Fatalf("after %d starts, what it saw of the first was taken", i)
		}
		if _, ok := rx.open(append([]byte(nil), unseen...)); ok {
			t.Fatalf("after %d starts, what it had not seen of the first was taken", i)
		}
	}
	same := testCipher(t, testKey)
	same.started = 1000 + 2*replayPrefixes
	if _, ok := rx.open(sealedDatagram(same, []byte("the same second"))); !ok {
		t.Error("a start in the same second")
	}
	other := testCipher(t, testKey)
	other.local, other.started = first.local+256, 5
	wire := packet.AppendVXLANHeader(nil, packet.VXLANFlags, 7)
	wire = append(append(wire, make([]byte, overlayRoomLen)...), "another node"...)
	if _, ok := rx.open(other.seal(wire, 0, rx.local)); !ok {
		t.Error("another sender, with an earlier start")
	}
}

func TestReplayWindow(t *testing.T) {
	w := &replayWindow{}
	for _, c := range []struct {
		count uint64
		want  bool
	}{
		{1, true}, {1, false}, {3, true}, {2, true}, {3, false},
		{replayWindowBits + 10, true},
		{5, false}, // behind the window
		{replayWindowBits + 9, true},
		{replayWindowBits + 10, false},
		{5*replayWindowBits + 1, true}, // past the whole window
		{replayWindowBits + 11, false},
		{5*replayWindowBits + 1 - 64, true},
	} {
		if got := w.fresh(c.count); got != c.want {
			t.Errorf("%d: %v", c.count, got)
		}
	}
	// Every counter once, in steps across words: none is taken for one already seen.
	w = &replayWindow{}
	for i := uint64(1); i < 3*replayWindowBits; i += 3 {
		if !w.fresh(i) {
			t.Fatalf("%d was taken for seen", i)
		}
	}
}

func TestReadKey(t *testing.T) {
	dir := t.TempDir()
	for text, ok := range map[string]bool{strings.Repeat("ab", overlayKeyLen) + "\n": true, "abcd": false, strings.Repeat("zz", overlayKeyLen): false} {
		path := filepath.Join(dir, "key")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadKey(path); (err == nil) != ok {
			t.Errorf("%q: %v", text, err)
		}
	}
}

// An engine with a key sends what opens with the key, and leaves out what passes it by.
func TestEngineEncryptsWithAKey(t *testing.T) {
	f := newFixture(t, OverlayConfig{Key: testKey, LocalSubnet: testSubnet, Super: true, RDMA: &RDMAConfig{}}, 1)
	if !f.oe.cfg.Super || f.oe.cfg.RDMA != nil {
		t.Errorf("super-frames %v, RDMA %v", f.oe.cfg.Super, f.oe.cfg.RDMA)
	}
	frame := testFrame(0)
	frame.Payload = make([]byte, 100)
	if err := f.oe.Send(frame, net.ParseIP("10.244.2.10")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	n, err := f.sinks[0].Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != packet.VXLANHeaderLen+overlayCipherBytes+packet.EthernetHeaderLen+100 {
		t.Errorf("%d bytes on the wire", n)
	}
	opened, ok := testCipher(t, testKey).open(buf[:n])
	if !ok {
		t.Fatal("what it sent does not open")
	}
	if len(opened) != packet.VXLANHeaderLen+packet.EthernetHeaderLen+100 {
		t.Errorf("opened to %d bytes", len(opened))
	}
}

// A key that cannot be used does not leave an engine that sends in the clear.
func TestEngineRefusesABadKey(t *testing.T) {
	oe := NewOverlayEngine(OverlayConfig{BindAddress: "127.0.0.1", Key: []byte("short"), LocalSubnet: testSubnet}, NewRouteTable(), nil)
	if err := oe.Start(t.Context()); err == nil {
		oe.Close()
		t.Fatal("it started")
	}
	if err := oe.Adopt(t.Context(), nil, nil); err == nil {
		t.Fatal("it took the sockets")
	}
	if _, err := newOverlayCipher(testKey, nil); err == nil {
		t.Error("a key without the node's subnet")
	}
}

// BenchmarkOpen is the receiving side of a datagram with the key; seal-only is the part of it that
// makes the datagram.
func BenchmarkOpen(b *testing.B) {
	for _, size := range []int{1400, 8900} {
		b.Run("encrypted/"+strconv.Itoa(size), func(b *testing.B) {
			c, rx := testCipher(b, testKey), testCipher(b, testKey)
			frame := make([]byte, size)
			work := make([]byte, 0, size+64)
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				work = append(work[:0], sealedDatagram(c, frame)...)
				if _, ok := rx.open(work); !ok {
					b.Fatal("it does not open")
				}
			}
		})
		b.Run("seal-only/"+strconv.Itoa(size), func(b *testing.B) {
			c := testCipher(b, testKey)
			frame := make([]byte, size)
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sealedDatagram(c, frame)
			}
		})
	}
}
