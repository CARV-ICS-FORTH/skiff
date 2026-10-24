package direct

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"
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

// Ports of a pod that are equal modulo 16 are served at one host port: they are told apart
// from the ports that have one of their own, and the set is not changed.
func TestPortsThatShareAHostPort(t *testing.T) {
	p, _ := ParsePorts("8080,5201,8096,5202,40000")
	own, shared := p.Exclusive()
	if own.String() != "5201,5202" || fmt.Sprint(shared) != "[8080 8096 40000]" || p.String() != "5201,5202,8080,8096,40000" {
		t.Errorf("own %s, shared %v, set %s", own, shared, p)
	}
	for _, port := range []uint16{8080, 8096} {
		a, _ := HostPort(DefaultPortBase, netip.MustParsePrefix("10.244.1.0/24"), netip.AddrPortFrom(netip.MustParseAddr("10.244.1.9"), port))
		b, _ := HostPort(DefaultPortBase, netip.MustParsePrefix("10.244.1.0/24"), netip.AddrPortFrom(netip.MustParseAddr("10.244.1.9"), 8080))
		if a != b {
			t.Errorf("port %d is served at %d, 8080 at %d", port, a, b)
		}
	}
	if own, shared := (Ports{}).Exclusive(); len(own) != 0 || len(shared) != 0 {
		t.Errorf("the empty set: %v, %v", own, shared)
	}
}

func TestTableKeepsTheRecentlyUsed(t *testing.T) {
	tab := NewTable(2, 2)
	tab.Add(1, Socket{Kind: KindClient})
	tab.Add(2, Socket{Kind: KindAccepted})
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

// The connections that come and go do not push out a listener that nothing has asked
// about for a while; listeners are kept in a bounded table of their own.
func TestTableKeepsListenersWhileConnectionsChurn(t *testing.T) {
	tab := NewTable(2, 2)
	tab.Add(100, Socket{Kind: KindListener})
	for i := uint64(1); i <= 50; i++ {
		tab.Add(i, Socket{Kind: KindAccepted})
	}
	if s, ok := tab.Get(100); !ok || s.Kind != KindListener {
		t.Errorf("the listener was pushed out by connections: %v, %v", s, ok)
	}
	tab.Add(101, Socket{Kind: KindListener})
	tab.Get(100) // 101 is the least recently used listener
	tab.Add(102, Socket{Kind: KindListener})
	if _, ok := tab.Get(101); ok {
		t.Error("a third listener did not push out the least recently used one")
	}
	if _, ok := tab.Get(100); !ok {
		t.Error("the listener that was used was dropped")
	}
	if tab.Len() != 4 {
		t.Errorf("%d entries, want 2 connections and 2 listeners", tab.Len())
	}
}

// fakeSockets is a liveness source for the table: the sockets that exist are the cookies in
// the set, and a call may be held until the test lets it go.
type fakeSockets struct {
	mu    sync.Mutex
	alive map[uint64]bool
	calls int           // lists given
	err   error         // what the next lists fail with
	gate  chan struct{} // when set, a list waits for it to be closed
	taken chan struct{} // when set, receives a value when a list has begun
}

func (f *fakeSockets) list() (map[uint64]struct{}, error) {
	f.mu.Lock()
	f.calls++
	gate, taken, err := f.gate, f.taken, f.err
	live := make(map[uint64]struct{})
	for c, ok := range f.alive {
		if ok {
			live[c] = struct{}{}
		}
	}
	f.mu.Unlock()
	if taken != nil {
		taken <- struct{}{}
	}
	if gate != nil {
		<-gate
	}
	return live, err
}

func (f *fakeSockets) called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// sweptTable makes a table that sweeps whenever it reaches 90% (no wait between sweeps), over
// the sockets that f says exist, and a function that waits for the sweep that is running.
func sweptTable(max int, f *fakeSockets, logf func(string, ...any)) (*Table, func()) {
	tab := NewTable(max, 2)
	tab.sweepGap = 0
	tab.SetLiveness(f.list, logf)
	return tab, func() {
		for {
			tab.mu.Lock()
			busy := tab.sweeping
			tab.mu.Unlock()
			if !busy {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// A connection that outlives any number of short ones stays in the table, because each
// time the table nears its size the entries of the sockets that are gone are dropped,
// and the LRU does not get to push the old one out.
func TestTableSweepKeepsLiveConnectionsAmongManyClosedOnes(t *testing.T) {
	f := &fakeSockets{alive: map[uint64]bool{1: true}}
	tab, wait := sweptTable(10, f, nil)
	tab.Add(1, Socket{Kind: KindClient, Peer: netip.MustParseAddrPort("10.244.1.9:80")})
	for c := uint64(100); c < 1100; c++ {
		tab.Add(c, Socket{Kind: KindAccepted})
		wait()
	}
	s, ok := tab.Get(1)
	if !ok || s.Kind != KindClient {
		t.Fatalf("the old connection was pushed out: %v, %v", s, ok)
	}
	if tab.Len() > 10 {
		t.Errorf("%d entries, the table holds 10", tab.Len())
	}
	if f.called() < 50 {
		t.Errorf("%d sweeps for 1000 connections in a table of 10", f.called())
	}
}

// Of the entries a sweep finds, those of sockets that exist stay, whatever their age, and
// those of sockets that do not are dropped; the listeners are not in it.
func TestTableSweepDropsOnlyWhatIsGone(t *testing.T) {
	f := &fakeSockets{alive: map[uint64]bool{2: true, 4: true, 6: true}}
	tab, wait := sweptTable(10, f, nil)
	tab.Add(100, Socket{Kind: KindListener}) // not a connection: not swept
	for c := uint64(1); c <= 9; c++ {        // the 9th takes the table to 90%
		tab.Add(c, Socket{Kind: KindAccepted})
	}
	wait()
	for c := uint64(1); c <= 9; c++ {
		_, ok := tab.Get(c)
		if want := c == 2 || c == 4 || c == 6; ok != want {
			t.Errorf("entry %d is in the table: %v, want %v", c, ok, want)
		}
	}
	if _, ok := tab.Get(100); !ok {
		t.Error("the listener was swept")
	}
	if tab.Len() != 4 {
		t.Errorf("%d entries, want 3 connections and the listener", tab.Len())
	}
}

// When every socket exists the sweep drops nothing, and the table still pushes out the
// least recently used to stay within its size.
func TestTableSweepLeavesLRUEvictionWhenAllAreAlive(t *testing.T) {
	f := &fakeSockets{alive: map[uint64]bool{}}
	for c := uint64(1); c <= 20; c++ {
		f.alive[c] = true
	}
	tab, wait := sweptTable(4, f, nil)
	for c := uint64(1); c <= 8; c++ {
		tab.Add(c, Socket{Kind: KindAccepted})
		wait()
	}
	if f.called() == 0 {
		t.Error("the table was full and nothing was swept")
	}
	if tab.Len() != 4 {
		t.Errorf("%d entries, want 4", tab.Len())
	}
	for c := uint64(1); c <= 8; c++ {
		if _, ok := tab.Get(c); ok != (c > 4) {
			t.Errorf("entry %d is in the table: %v", c, ok)
		}
	}
}

// A socket added while the list is being made may not be in it: it is kept.
func TestTableSweepKeepsWhatWasAddedDuringIt(t *testing.T) {
	f := &fakeSockets{alive: map[uint64]bool{}, gate: make(chan struct{}), taken: make(chan struct{}, 1)}
	tab, wait := sweptTable(10, f, nil)
	for c := uint64(1); c <= 9; c++ {
		tab.Add(c, Socket{Kind: KindAccepted})
	}
	<-f.taken
	tab.Add(50, Socket{Kind: KindAccepted}) // its socket is made after the list
	close(f.gate)
	wait()
	if _, ok := tab.Get(50); !ok {
		t.Error("an entry added during the sweep was dropped")
	}
	if tab.Len() != 1 {
		t.Errorf("%d entries, want only the new one", tab.Len())
	}
}

// One sweep runs at a time, and the next waits for the gap.
func TestTableSweepsOneAtATimeAndNotOften(t *testing.T) {
	f := &fakeSockets{alive: map[uint64]bool{}, gate: make(chan struct{}), taken: make(chan struct{}, 100)}
	tab, wait := sweptTable(4, f, nil)
	for c := uint64(1); c <= 50; c++ { // the sweep is held: the table fills and pushes out
		tab.Add(c, Socket{Kind: KindAccepted})
	}
	close(f.gate)
	wait()
	if n := f.called(); n != 1 {
		t.Errorf("%d lists made while one was running", n)
	}
	tab.mu.Lock()
	tab.sweepGap = time.Hour
	tab.mu.Unlock()
	for c := uint64(100); c <= 150; c++ {
		tab.Add(c, Socket{Kind: KindAccepted})
		wait()
	}
	if n := f.called(); n != 1 {
		t.Errorf("%d lists made, the second within the gap", n)
	}
}

// A list that cannot be had leaves the table as it is, with the least recently used pushed
// out as before, and is logged once.
func TestTableSweepFailureIsLoggedOnce(t *testing.T) {
	f := &fakeSockets{alive: map[uint64]bool{}, err: errors.New("netlink refused")}
	var lines []string
	tab, wait := sweptTable(4, f, func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	for c := uint64(1); c <= 30; c++ {
		tab.Add(c, Socket{Kind: KindAccepted})
		wait()
	}
	if f.called() < 2 {
		t.Errorf("%d lists, want the sweep tried again", f.called())
	}
	if len(lines) != 1 {
		t.Errorf("%d lines logged: %q", len(lines), lines)
	}
	if tab.Len() != 4 {
		t.Errorf("%d entries, want 4", tab.Len())
	}
	if _, ok := tab.Get(30); !ok {
		t.Error("the newest entry is not there")
	}
}
