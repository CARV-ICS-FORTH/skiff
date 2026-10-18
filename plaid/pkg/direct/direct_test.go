package direct

import (
	"net/netip"
	"syscall"
	"testing"
	"time"
)

var testKey = DeriveKey([]byte("the cluster's private key"))

func examplePreamble(t *testing.T) Preamble {
	t.Helper()
	p, err := NewPreamble(PreambleDirect, netip.MustParseAddrPort("10.244.1.9:40458"),
		netip.MustParseAddrPort("10.244.5.4:5201"), netip.MustParseAddrPort("10.0.1.11:40458"))
	if err != nil {
		t.Fatal(err)
	}
	p.Time = p.Time.Truncate(time.Second)
	return p
}

func TestPreambleRoundTrip(t *testing.T) {
	in := examplePreamble(t)
	b := in.Marshal(testKey)
	if string(b[:4]) != "DPv2" || len(b) != PreambleSize {
		t.Fatalf("unexpected wire form %q", b)
	}
	out, err := ParsePreamble(b[:], testKey)
	if err != nil || out.Kind != in.Kind || out.Client != in.Client || out.Backend != in.Backend ||
		out.Host != in.Host || !out.Time.Equal(in.Time) || out.Nonce != in.Nonce {
		t.Fatalf("got %+v, %v; want %+v", out, err, in)
	}
	// An address that is not IPv4 (a pod-local client on ::1) goes through as well.
	in.Client = netip.MustParseAddrPort("[::1]:40000")
	b = in.Marshal(testKey)
	if out, err := ParsePreamble(b[:], testKey); err != nil || out.Client != in.Client {
		t.Errorf("got %v, %v; want %v", out.Client, err, in.Client)
	}
}

// Every byte of the preamble is covered by its MAC, and the key is the cluster's.
func TestPreambleIsAuthenticated(t *testing.T) {
	b := examplePreamble(t).Marshal(testKey)
	for i := 4; i < PreambleSize; i++ {
		forged := b
		forged[i] ^= 1
		if _, err := ParsePreamble(forged[:], testKey); err != errBadMAC {
			t.Errorf("byte %d changed: %v", i, err)
		}
	}
	if _, err := ParsePreamble(b[:], DeriveKey([]byte("another cluster"))); err != errBadMAC {
		t.Errorf("another cluster's key: %v", err)
	}
	// Two preambles of the same connection differ: the ack is for this one.
	p, q := examplePreamble(t), examplePreamble(t)
	if p.Nonce == q.Nonce || p.Ack(testKey) == q.Ack(testKey) {
		t.Error("the nonce repeats")
	}
	if p.Ack(testKey) == p.Ack(DeriveKey([]byte("another cluster"))) {
		t.Error("the ack does not depend on the key")
	}
}

func TestPreambleRefusals(t *testing.T) {
	if _, err := ParsePreamble([]byte("GET / HTTP/1.1\r\n"), testKey); err == nil {
		t.Error("a request line was taken for a preamble")
	}
	if _, err := ParsePreamble([]byte("DPv2"), testKey); err == nil {
		t.Error("a short preamble was accepted")
	}
	b := examplePreamble(t).Marshal(testKey)
	b[3] = '1' // the first version's magic
	if _, err := ParsePreamble(b[:], testKey); err == nil {
		t.Error("an old magic was accepted")
	}
}

func TestSockaddr(t *testing.T) {
	ap := netip.MustParseAddrPort("10.244.5.4:5201")
	for _, family := range []int{syscall.AF_INET, syscall.AF_INET6} {
		b := encodeSockaddr(ap, family)
		want := sockaddrInetSize
		if family == syscall.AF_INET6 {
			want = sockaddrInet6Size
		}
		if len(b) != want {
			t.Errorf("family %d: %d bytes, want %d", family, len(b), want)
		}
		if got, ok := parseSockaddr(b); !ok || got != ap {
			t.Errorf("family %d: parsed %v, %v; want %v", family, got, ok, ap)
		}
	}
	// An IPv6 socket that is not an IPv4 address in disguise is not one we handle.
	b := encodeSockaddr(netip.MustParseAddrPort("[fd00::1]:80"), syscall.AF_INET6)
	if got, ok := parseSockaddr(b); !ok || got.Addr().Is4() {
		t.Errorf("a real IPv6 address came back as %v", got)
	}
	if _, ok := parseSockaddr([]byte{1, 0, 0, 0}); ok {
		t.Error("AF_UNIX was taken for an inet address")
	}
}

func TestPorts(t *testing.T) {
	p, err := ParsePorts(" 5203, 5201,,5201")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Has(5201) || !p.Has(5203) || p.Has(5202) || p.String() != "5201,5203" {
		t.Errorf("got %q", p)
	}
	for _, bad := range []string{"0", "70000", "http", "5201;5202"} {
		if _, err := ParsePorts(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if p, err := ParsePorts(""); err != nil || len(p) != 0 {
		t.Errorf("empty list: %v, %v", p, err)
	}
}

func TestTableKeepsTheRecentlyUsed(t *testing.T) {
	tab := NewTable(2)
	tab.Add(1, Socket{Kind: KindClient})
	tab.Add(2, Socket{Kind: KindListener})
	tab.Get(1) // 2 is now the least recently used
	tab.Add(3, Socket{Kind: KindAccepted})
	if _, ok := tab.Get(2); ok {
		t.Error("the least recently used entry stayed")
	}
	if s, ok := tab.Get(1); !ok || s.Kind != KindClient {
		t.Errorf("entry 1: %v, %v", s, ok)
	}
	if tab.Len() != 2 {
		t.Errorf("%d entries, want 2", tab.Len())
	}
	tab.Add(1, Socket{Kind: KindAccepted}) // replaced, not duplicated
	if s, _ := tab.Get(1); s.Kind != KindAccepted || tab.Len() != 2 {
		t.Errorf("replace: %v, %d entries", s, tab.Len())
	}
}
