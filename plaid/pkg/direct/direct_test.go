package direct

import (
	"net/netip"
	"syscall"
	"testing"
)

func TestPreambleRoundTrip(t *testing.T) {
	in := Preamble{
		Client:  netip.MustParseAddrPort("10.244.1.9:40458"),
		Backend: netip.MustParseAddrPort("10.244.5.4:5201"),
	}
	b, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:4]) != "DPv1" || len(b) != PreambleSize {
		t.Fatalf("unexpected wire form %q", b)
	}
	out, err := ParsePreamble(b[:])
	if err != nil || out != in {
		t.Fatalf("got %v, %v; want %v", out, err, in)
	}
}

func TestPreambleRefusals(t *testing.T) {
	if _, err := ParsePreamble([]byte("GET / HTTP/1.1\r\n")); err == nil {
		t.Error("a request line was taken for a preamble")
	}
	if _, err := ParsePreamble([]byte("DPv1")); err == nil {
		t.Error("a short preamble was accepted")
	}
	v6 := Preamble{Client: netip.MustParseAddrPort("[fd00::1]:1"), Backend: netip.MustParseAddrPort("10.0.0.1:1")}
	if _, err := v6.Marshal(); err == nil {
		t.Error("an IPv6 client address was marshalled")
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
