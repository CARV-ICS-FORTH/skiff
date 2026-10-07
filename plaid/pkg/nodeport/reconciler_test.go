package nodeport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"plaid/pkg/slirpapi"
)

const (
	hostAddr = "192.168.64.3"
	// uplink is the bubble's own address on the slirp4netns network: where the fixed forwards
	// of the deployment (the API and the kubelet) point. It is not an address of the Service range.
	uplink = "10.0.2.100"
)

var serviceNet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("10.43.0.0/16")
	return n
}()

// fakeSlirp keeps forwards like slirp4netns, with ports held by others and errors to order.
type fakeSlirp struct {
	mu        sync.Mutex
	entries   []slirpapi.Entry
	next      int
	taken     map[string]bool
	calls     []string
	listErr   error
	addErr    error
	removeErr error
}

func newFakeSlirp() *fakeSlirp { return &fakeSlirp{next: 1, taken: map[string]bool{}} }

func (f *fakeSlirp) preload(e slirpapi.Entry) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.ID = f.next
	f.next++
	f.entries = append(f.entries, e)
	return e.ID
}

func (f *fakeSlirp) ListHostFwd(context.Context) ([]slirpapi.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "list")
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]slirpapi.Entry(nil), f.entries...), nil
}

func (f *fakeSlirp) AddHostFwd(_ context.Context, proto, host string, hp int, guest string, gp int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("add %s %d", proto, hp))
	if f.addErr != nil {
		return 0, f.addErr
	}
	if f.taken[fmt.Sprintf("%s:%d", proto, hp)] {
		return 0, &slirpapi.APIError{Desc: "bad request: add_hostfwd: slirp_add_hostfwd failed"}
	}
	for _, e := range f.entries {
		if e.Proto == proto && e.HostPort == hp {
			return 0, &slirpapi.APIError{Desc: "bind failed: Address already in use"}
		}
	}
	id := f.next
	f.next++
	f.entries = append(f.entries, slirpapi.Entry{ID: id, Proto: proto, HostAddr: host, HostPort: hp, GuestAddr: guest, GuestPort: gp})
	return id, nil
}

func (f *fakeSlirp) RemoveHostFwd(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("remove %d", id))
	if f.removeErr != nil {
		return f.removeErr
	}
	for i, e := range f.entries {
		if e.ID == id {
			f.entries = append(f.entries[:i], f.entries[i+1:]...)
			return nil
		}
	}
	return errors.New("no such id")
}

func (f *fakeSlirp) ports() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.entries {
		out = append(out, fmt.Sprintf("%s/%d", e.Proto, e.HostPort))
	}
	return out
}

// forward returns the entry for a protocol and host port.
func (f *fakeSlirp) forward(proto string, port int) (slirpapi.Entry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.entries {
		if e.Proto == proto && e.HostPort == port {
			return e, true
		}
	}
	return slirpapi.Entry{}, false
}

func (f *fakeSlirp) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSlirp) resetCalls() { f.mu.Lock(); f.calls = nil; f.mu.Unlock() }

type event struct {
	ref     any
	warning bool
	reason  string
	message string
}

type fakeReporter struct {
	mu     sync.Mutex
	events []event
}

func (f *fakeReporter) Event(ref any, warning bool, reason, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event{ref, warning, reason, message})
}

func (f *fakeReporter) reasons() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.events {
		out = append(out, e.reason)
	}
	return out
}

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

type rig struct {
	r   *Reconciler
	s   *fakeSlirp
	rep *fakeReporter
	ck  *clock
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ck := &clock{t: time.Unix(1_000_000, 0)}
	s, rep := newFakeSlirp(), &fakeReporter{}
	r := New(Config{HostAddr: hostAddr, ServiceCIDR: serviceNet, Now: ck.Now}, s, rep)
	return &rig{r: r, s: s, rep: rep, ck: ck}
}

// mp is a mapping of a node port to a Service whose cluster IP and port follow from the node port.
func mp(proto string, port int, svc string) Mapping {
	return Mapping{Proto: proto, Port: port, ClusterIP: [4]byte{10, 43, 0, byte(port % 250)}, ServicePort: 80, Ref: svc}
}

// forwardOf is the forward the reconciler makes for a mapping.
func forwardOf(m Mapping) slirpapi.Entry {
	guest, port := m.guest()
	return slirpapi.Entry{Proto: m.Proto, HostAddr: hostAddr, HostPort: m.Port, GuestAddr: guest, GuestPort: port}
}

func (g *rig) pass() { g.r.Reconcile(context.Background()) }

func TestPublishesWhatIsDesiredAndNothingElse(t *testing.T) {
	g := newRig(t)
	fixed := []slirpapi.Entry{
		{Proto: "tcp", HostAddr: hostAddr, HostPort: 10250, GuestAddr: uplink, GuestPort: 10250},      // the kubelet
		{Proto: "tcp", HostAddr: hostAddr, HostPort: 6443, GuestAddr: uplink, GuestPort: 6443},        // the API
		{Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 31000, GuestAddr: "10.43.0.9", GuestPort: 80}, // another host address
		{Proto: "tcp", HostAddr: hostAddr, HostPort: 31001, GuestAddr: "10.0.2.77", GuestPort: 80},    // a guest outside the Service range
		{Proto: "tcp", HostAddr: hostAddr, HostPort: 31002, GuestAddr: uplink, GuestPort: 31002},      // the bubble's own address, not a Service's
		{Proto: "tcp", HostAddr: hostAddr, HostPort: 12345, GuestAddr: "10.43.0.9", GuestPort: 80},    // outside the port range
	}
	for _, e := range fixed {
		g.s.preload(e)
	}
	g.r.SetDesired([]Mapping{mp("tcp", 31080, "web"), mp("udp", 31080, "web"), mp("tcp", 31443, "web")})
	g.pass()
	got := strings.Join(g.s.ports(), " ")
	for _, want := range []string{"tcp/10250", "tcp/6443", "tcp/31000", "tcp/31001", "tcp/31002", "tcp/12345", "tcp/31080", "udp/31080", "tcp/31443"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s missing from %s", want, got)
		}
	}
	if len(g.s.ports()) != 9 {
		t.Errorf("forwards: %v", g.s.ports())
	}
	// A forward goes from the host's node port to the Service's cluster IP and port.
	for _, m := range []Mapping{mp("tcp", 31080, "web"), mp("udp", 31080, "web"), mp("tcp", 31443, "web")} {
		e, ok := g.s.forward(m.Proto, m.Port)
		want := forwardOf(m)
		if !ok || e.HostAddr != want.HostAddr || e.GuestAddr != want.GuestAddr || e.GuestPort != want.GuestPort {
			t.Errorf("forward of %s %d: %+v, want %+v", m.Proto, m.Port, e, want)
		}
	}
	if e, _ := g.s.forward("tcp", 31080); e.GuestAddr != "10.43.0.80" || e.GuestPort != 80 {
		t.Errorf("the forward of node port 31080 points at %s:%d", e.GuestAddr, e.GuestPort)
	}
	st := g.r.Status()
	if st.Desired != 3 || st.Exposed != 3 || len(st.Failed) != 0 || st.LastError != "" {
		t.Errorf("status %+v", st)
	}
	// Nothing changes on a second pass: one listing, no mutation.
	g.s.resetCalls()
	g.pass()
	if calls := g.s.callLog(); len(calls) != 1 || calls[0] != "list" {
		t.Errorf("calls %v", calls)
	}
}

func TestRemovesWhatIsOursAndNoLongerWantedAndRepairsDrift(t *testing.T) {
	g := newRig(t)
	g.s.preload(slirpapi.Entry{Proto: "tcp", HostAddr: hostAddr, HostPort: 10250, GuestAddr: uplink, GuestPort: 10250}) // the kubelet's
	// Ours by shape, from before: no memory of it is needed.
	g.s.preload(slirpapi.Entry{Proto: "tcp", HostAddr: hostAddr, HostPort: 31500, GuestAddr: "10.43.0.9", GuestPort: 80})
	g.r.SetDesired([]Mapping{mp("tcp", 31080, "web")})
	g.pass()
	if got := strings.Join(g.s.ports(), " "); strings.Contains(got, "31500") || !strings.Contains(got, "10250") || !strings.Contains(got, "31080") {
		t.Errorf("forwards %s", got)
	}
	if g.r.Status().Removed != 1 {
		t.Errorf("status %+v", g.r.Status())
	}

	// Someone removes our forward behind our back: the next pass makes it again.
	e, _ := g.s.forward("tcp", 31080)
	if err := g.s.RemoveHostFwd(context.Background(), e.ID); err != nil {
		t.Fatal(err)
	}
	g.pass()
	if !strings.Contains(strings.Join(g.s.ports(), " "), "tcp/31080") || g.r.Status().Repaired != 1 {
		t.Errorf("forwards %v, status %+v", g.s.ports(), g.r.Status())
	}

	// A Service is removed: its forward goes, the kubelet's stays.
	g.r.SetDesired(nil)
	g.pass()
	if got := g.s.ports(); len(got) != 1 || got[0] != "tcp/10250" {
		t.Errorf("forwards %v", got)
	}
}

func TestAForwardThatPointsAtAnotherServiceIsMadeAgain(t *testing.T) {
	g := newRig(t)
	g.r.SetDesired([]Mapping{mp("tcp", 31080, "web")})
	g.pass()
	first, _ := g.s.forward("tcp", 31080)

	// The Service is replaced (same node port, new cluster IP): the forward is made again, as
	// a replacement and not as drift or as a removal.
	m := mp("tcp", 31080, "web-new-uid")
	m.ClusterIP = [4]byte{10, 43, 7, 7}
	g.s.resetCalls()
	g.r.SetDesired([]Mapping{m})
	g.pass()
	if calls := strings.Join(g.s.callLog(), ","); calls != fmt.Sprintf("list,remove %d,add tcp 31080", first.ID) {
		t.Errorf("calls %s", calls)
	}
	if e, ok := g.s.forward("tcp", 31080); !ok || e.GuestAddr != "10.43.7.7" || e.GuestPort != 80 {
		t.Errorf("forward %+v", e)
	}

	// The same for another port of the same Service.
	m.ServicePort = 8080
	g.r.SetDesired([]Mapping{m})
	g.pass()
	if e, _ := g.s.forward("tcp", 31080); e.GuestPort != 8080 {
		t.Errorf("forward %+v", e)
	}
	st := g.r.Status()
	if st.Removed != 0 || st.Repaired != 0 || st.Exposed != 1 || st.LastError != "" || len(g.s.ports()) != 1 {
		t.Errorf("status %+v, forwards %v", st, g.s.ports())
	}

	// Nothing more to do on the next pass.
	g.s.resetCalls()
	g.pass()
	if calls := g.s.callLog(); len(calls) != 1 || calls[0] != "list" {
		t.Errorf("calls %v", calls)
	}
}

func TestAForwardThatCannotBeRemovedIsNotMadeAgainInThatPass(t *testing.T) {
	g := newRig(t)
	g.s.preload(slirpapi.Entry{Proto: "tcp", HostAddr: hostAddr, HostPort: 31080, GuestAddr: "10.43.9.9", GuestPort: 80})
	g.s.removeErr = errors.New("slirp4netns: busy")
	g.r.SetDesired([]Mapping{mp("tcp", 31080, "web")})
	g.s.resetCalls()
	g.pass()
	for _, c := range g.s.callLog() {
		if strings.HasPrefix(c, "add") {
			t.Errorf("a forward was made over one that could not be removed: %v", g.s.callLog())
		}
	}
	if !strings.Contains(g.r.Status().LastError, "removing the forward of tcp port 31080") {
		t.Errorf("status %+v", g.r.Status())
	}
	if r := g.rep.reasons(); len(r) != 0 {
		t.Errorf("a bind failure was reported for it: %v", r)
	}

	// The next pass, when slirp4netns lets go, makes it.
	g.s.removeErr = nil
	g.pass()
	if e, _ := g.s.forward("tcp", 31080); e.GuestAddr != "10.43.0.80" || g.r.Status().LastError != "" {
		t.Errorf("forward %+v, status %+v", e, g.r.Status())
	}
}

func TestAPortSomethingElseHoldsIsAFailureOfThatPortAlone(t *testing.T) {
	g := newRig(t)
	g.s.taken["tcp:31080"] = true
	web, other := "svc/web", "svc/other"
	g.r.SetDesired([]Mapping{mp("tcp", 31080, web), mp("udp", 31080, web), mp("tcp", 31443, other)})
	g.pass()

	st := g.r.Status()
	if st.Desired != 3 || st.Exposed != 2 || len(st.Failed) != 1 || st.Failed[0].Port != 31080 || st.Failed[0].Proto != "tcp" || st.Failed[0].Attempts != 1 {
		t.Fatalf("status %+v", st)
	}
	if got := strings.Join(g.s.ports(), " "); !strings.Contains(got, "udp/31080") || !strings.Contains(got, "tcp/31443") || strings.Contains(got, "tcp/31080") {
		t.Errorf("forwards %s", got)
	}
	if ev := g.rep.events; len(ev) != 1 || ev[0].reason != ReasonBindFailed || !ev[0].warning || ev[0].ref != web {
		t.Fatalf("events %+v", ev)
	}
	if !strings.Contains(g.rep.events[0].message, "tcp node port 31080") || !strings.Contains(g.rep.events[0].message, "does not use another port") {
		t.Errorf("message %q", g.rep.events[0].message)
	}
	// No other port was chosen.
	if len(g.s.ports()) != 2 {
		t.Errorf("forwards %v", g.s.ports())
	}

	// Not before the backoff; then again (and the event is not repeated for the same failure).
	g.s.resetCalls()
	g.pass()
	for _, c := range g.s.callLog() {
		if strings.HasPrefix(c, "add") {
			t.Errorf("a retry before its backoff: %v", g.s.callLog())
		}
	}
	g.ck.t = g.ck.t.Add(3 * time.Second)
	g.pass()
	if st := g.r.Status(); len(st.Failed) != 1 || st.Failed[0].Attempts != 2 {
		t.Errorf("after the retry: %+v", st)
	}
	if len(g.rep.events) != 1 {
		t.Errorf("the same failure was reported again: %v", g.rep.reasons())
	}

	// The holder lets go: the port is published on the next retry and the event says so.
	delete(g.s.taken, "tcp:31080")
	g.ck.t = g.ck.t.Add(time.Minute)
	g.pass()
	if st := g.r.Status(); st.Exposed != 3 || len(st.Failed) != 0 {
		t.Errorf("after the holder went: %+v", st)
	}
	if r := g.rep.reasons(); len(r) != 2 || r[1] != ReasonPublished || g.rep.events[1].warning {
		t.Errorf("events %v", r)
	}
}

func TestBackoffGrowsAndIsBounded(t *testing.T) {
	g := newRig(t)
	g.s.taken["tcp:31080"] = true
	g.r.SetDesired([]Mapping{mp("tcp", 31080, "web")})
	var waits []time.Duration
	for i := 0; i < 12; i++ {
		g.pass()
		g.r.mu.Lock()
		waits = append(waits, g.r.state[mappingKey{"tcp", 31080}].nextRetry.Sub(g.ck.t))
		g.r.mu.Unlock()
		g.ck.t = g.ck.t.Add(waits[len(waits)-1])
	}
	if waits[0] != 2*time.Second || waits[1] != 4*time.Second || waits[2] != 8*time.Second {
		t.Errorf("waits %v", waits)
	}
	if last := waits[len(waits)-1]; last != time.Minute {
		t.Errorf("the backoff was not capped: %v", waits)
	}
}

func TestAnotherErrorIsReportedAsSuch(t *testing.T) {
	g := newRig(t)
	g.s.addErr = errors.New("slirp4netns: the API is unreachable")
	g.r.SetDesired([]Mapping{mp("tcp", 31080, "web")})
	g.pass()
	if r := g.rep.reasons(); len(r) != 1 || r[0] != ReasonPublishError {
		t.Errorf("events %v", r)
	}
}

func TestAFailingListingChangesNothing(t *testing.T) {
	g := newRig(t)
	g.r.SetDesired([]Mapping{mp("tcp", 31080, "web")})
	g.pass()
	g.s.listErr = errors.New("timed out")
	g.r.SetDesired(nil)
	g.s.resetCalls()
	g.pass()
	if calls := g.s.callLog(); len(calls) != 1 {
		t.Errorf("calls after a failed listing: %v", calls)
	}
	if !strings.Contains(g.r.Status().LastError, "timed out") || len(g.s.ports()) != 1 {
		t.Errorf("status %+v, forwards %v", g.r.Status(), g.s.ports())
	}
	g.s.listErr = nil
	g.pass()
	if len(g.s.ports()) != 0 || g.r.Status().LastError != "" {
		t.Errorf("after recovery: %v %+v", g.s.ports(), g.r.Status())
	}
}

func TestPortsOutsideTheRangeAreNotPublished(t *testing.T) {
	g := newRig(t)
	g.r.SetDesired([]Mapping{mp("tcp", 80, "web"), mp("tcp", 31080, "web")})
	g.pass()
	g.pass()
	if got := g.s.ports(); len(got) != 1 || got[0] != "tcp/31080" {
		t.Errorf("forwards %v", got)
	}
	if r := g.rep.reasons(); len(r) != 1 || r[0] != ReasonOutOfRange {
		t.Errorf("events %v (once, not per pass)", r)
	}
	if st := g.r.Status(); st.Desired != 1 {
		t.Errorf("status %+v", st)
	}
}

func TestRunReconcilesOnChangeAndOnRetryAndStops(t *testing.T) {
	s, rep := newFakeSlirp(), &fakeReporter{}
	r := New(Config{HostAddr: hostAddr, ServiceCIDR: serviceNet, Resync: 50 * time.Millisecond, RetryMin: 20 * time.Millisecond}, s, rep)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	wait := func(what string, cond func() bool) {
		for i := 0; i < 300; i++ {
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	s.mu.Lock()
	s.taken["tcp:31080"] = true
	s.mu.Unlock()
	r.SetDesired([]Mapping{mp("tcp", 31080, "web"), mp("tcp", 31081, "web")})
	wait("the free port", func() bool { return len(s.ports()) == 1 })
	s.mu.Lock()
	delete(s.taken, "tcp:31080")
	s.mu.Unlock()
	wait("the retry", func() bool { return len(s.ports()) == 2 })
	// Drift: a forward disappears and the resync repairs it.
	s.mu.Lock()
	id := s.entries[0].ID
	s.mu.Unlock()
	_ = s.RemoveHostFwd(context.Background(), id)
	wait("the repair", func() bool { return len(s.ports()) == 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}
