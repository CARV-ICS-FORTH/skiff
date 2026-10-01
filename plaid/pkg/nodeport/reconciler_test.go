package nodeport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"plaid/pkg/slirpapi"
)

const (
	hostAddr  = "192.168.64.3"
	guestAddr = "10.0.2.100"
)

// fakeSlirp keeps forwards like slirp4netns, with ports held by others and errors to order.
type fakeSlirp struct {
	mu      sync.Mutex
	entries []slirpapi.Entry
	next    int
	taken   map[string]bool
	calls   []string
	listErr error
	addErr  error
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

func (f *fakeSlirp) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSlirp) resetCalls() { f.mu.Lock(); f.calls = nil; f.mu.Unlock() }

type fakeRules struct {
	mu    sync.Mutex
	sets  [][]Rule
	err   error
	order *[]string
}

func (r *fakeRules) Apply(_ context.Context, rules []Rule) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.sets = append(r.sets, append([]Rule(nil), rules...))
	if r.order != nil {
		*r.order = append(*r.order, "rules")
	}
	return nil
}

func (r *fakeRules) last() []Rule {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sets) == 0 {
		return nil
	}
	return r.sets[len(r.sets)-1]
}

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
	rl  *fakeRules
	rep *fakeReporter
	ck  *clock
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ck := &clock{t: time.Unix(1_000_000, 0)}
	s, rl, rep := newFakeSlirp(), &fakeRules{}, &fakeReporter{}
	r := New(Config{HostAddr: hostAddr, GuestAddr: guestAddr, Now: ck.Now}, s, rl, rep)
	return &rig{r: r, s: s, rl: rl, rep: rep, ck: ck}
}

func mp(proto string, port int, svc string) Mapping {
	return Mapping{Proto: proto, Port: port, ClusterIP: [4]byte{10, 43, 0, byte(port % 250)}, ServicePort: 80, Ref: svc}
}

func (g *rig) pass() { g.r.Reconcile(context.Background()) }

func TestPublishesWhatIsDesiredAndNothingElse(t *testing.T) {
	g := newRig(t)
	fixed := []slirpapi.Entry{
		{Proto: "tcp", HostAddr: hostAddr, HostPort: 10250, GuestAddr: guestAddr, GuestPort: 10250},    // the kubelet
		{Proto: "tcp", HostAddr: hostAddr, HostPort: 6443, GuestAddr: guestAddr, GuestPort: 6443},      // the API
		{Proto: "tcp", HostAddr: "127.0.0.1", HostPort: 31000, GuestAddr: guestAddr, GuestPort: 31000}, // another host address
		{Proto: "tcp", HostAddr: hostAddr, HostPort: 31001, GuestAddr: "10.0.2.77", GuestPort: 31001},  // another guest
		{Proto: "tcp", HostAddr: hostAddr, HostPort: 31002, GuestAddr: guestAddr, GuestPort: 9999},     // different ports
		{Proto: "tcp", HostAddr: hostAddr, HostPort: 12345, GuestAddr: guestAddr, GuestPort: 12345},    // outside the range
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
	st := g.r.Status()
	if st.Desired != 3 || st.Exposed != 3 || len(st.Failed) != 0 || st.LastError != "" {
		t.Errorf("status %+v", st)
	}
	if len(g.rl.last()) != 3 {
		t.Errorf("rules %+v", g.rl.last())
	}
	// Nothing changes on a second pass: one listing, no mutation, no new rules.
	g.s.resetCalls()
	sets := len(g.rl.sets)
	g.pass()
	if calls := g.s.callLog(); len(calls) != 1 || calls[0] != "list" {
		t.Errorf("calls %v", calls)
	}
	if len(g.rl.sets) != sets {
		t.Error("the rules were applied again")
	}
}

func TestRemovesWhatIsOursAndNoLongerWantedAndRepairsDrift(t *testing.T) {
	g := newRig(t)
	kubelet := g.s.preload(slirpapi.Entry{Proto: "tcp", HostAddr: hostAddr, HostPort: 10250, GuestAddr: guestAddr, GuestPort: 10250})
	_ = kubelet
	stale := slirpapi.Entry{Proto: "tcp", HostAddr: hostAddr, HostPort: 31500, GuestAddr: guestAddr, GuestPort: 31500}
	g.s.preload(stale) // ours by shape, from before: no memory of it is needed
	g.r.SetDesired([]Mapping{mp("tcp", 31080, "web")})
	g.pass()
	if got := strings.Join(g.s.ports(), " "); strings.Contains(got, "31500") || !strings.Contains(got, "10250") || !strings.Contains(got, "31080") {
		t.Errorf("forwards %s", got)
	}
	if g.r.Status().Removed != 1 {
		t.Errorf("status %+v", g.r.Status())
	}

	// Someone removes our forward behind our back: the next pass makes it again.
	g.s.mu.Lock()
	var id int
	for _, e := range g.s.entries {
		if e.HostPort == 31080 {
			id = e.ID
		}
	}
	g.s.mu.Unlock()
	if err := g.s.RemoveHostFwd(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	g.pass()
	if !strings.Contains(strings.Join(g.s.ports(), " "), "tcp/31080") || g.r.Status().Repaired != 1 {
		t.Errorf("forwards %v, status %+v", g.s.ports(), g.r.Status())
	}

	// A Service is removed: its forward and its rule go, the kubelet's stays.
	g.r.SetDesired(nil)
	g.pass()
	if got := g.s.ports(); len(got) != 1 || got[0] != "tcp/10250" {
		t.Errorf("forwards %v", got)
	}
	if len(g.rl.last()) != 0 {
		t.Errorf("rules %+v", g.rl.last())
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
	sets := len(g.rl.sets)
	g.s.listErr = errors.New("timed out")
	g.r.SetDesired(nil)
	g.s.resetCalls()
	g.pass()
	if calls := g.s.callLog(); len(calls) != 1 {
		t.Errorf("calls after a failed listing: %v", calls)
	}
	if !strings.Contains(g.r.Status().LastError, "timed out") || len(g.s.ports()) != 1 || len(g.rl.sets) != sets {
		t.Errorf("status %+v, forwards %v", g.r.Status(), g.s.ports())
	}
	g.s.listErr = nil
	g.pass()
	if len(g.s.ports()) != 0 || g.r.Status().LastError != "" {
		t.Errorf("after recovery: %v %+v", g.s.ports(), g.r.Status())
	}
}

func TestRulesAreSetBeforeForwardsAreMadeAndAfterTheyAreRemoved(t *testing.T) {
	g := newRig(t)
	var order []string
	g.rl.order = &order
	wrap := &orderedSlirp{fakeSlirp: g.s, order: &order}
	g.r = New(Config{HostAddr: hostAddr, GuestAddr: guestAddr, Now: g.ck.Now}, wrap, g.rl, g.rep)
	g.r.SetDesired([]Mapping{mp("tcp", 31080, "web")})
	g.pass()
	want := []string{"rules", "add"}
	if len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
		t.Errorf("adding: %v", order)
	}
	order = order[:0]
	g.r.SetDesired(nil)
	g.pass()
	if len(order) != 2 || order[0] != "remove" || order[1] != "rules" {
		t.Errorf("removing: %v", order)
	}
}

type orderedSlirp struct {
	*fakeSlirp
	order *[]string
}

func (o *orderedSlirp) AddHostFwd(ctx context.Context, p, h string, hp int, g string, gp int) (int, error) {
	*o.order = append(*o.order, "add")
	return o.fakeSlirp.AddHostFwd(ctx, p, h, hp, g, gp)
}

func (o *orderedSlirp) RemoveHostFwd(ctx context.Context, id int) error {
	*o.order = append(*o.order, "remove")
	return o.fakeSlirp.RemoveHostFwd(ctx, id)
}

func TestRulesFollowTheServiceAndAFailureToSetThemIsVisible(t *testing.T) {
	g := newRig(t)
	g.r.SetDesired([]Mapping{mp("tcp", 31080, "web")})
	g.pass()
	first := g.rl.last()
	if len(first) != 1 || first[0] != (Rule{Proto: "tcp", Port: 31080, DestIP: [4]byte{10, 43, 0, 80}, DestPort: 80}) {
		t.Fatalf("rules %+v", first)
	}
	// The Service is replaced (same node port, new address): the forward stays, the rule follows.
	m := mp("tcp", 31080, "web-new-uid")
	m.ClusterIP = [4]byte{10, 43, 7, 7}
	g.s.resetCalls()
	g.r.SetDesired([]Mapping{m})
	g.pass()
	if g.rl.last()[0].DestIP != m.ClusterIP {
		t.Errorf("rules %+v", g.rl.last())
	}
	for _, c := range g.s.callLog() {
		if c != "list" {
			t.Errorf("the forward was touched for a Service replacement: %v", g.s.callLog())
		}
	}
	g.rl.err = errors.New("iptables-restore failed")
	g.r.SetDesired([]Mapping{mp("tcp", 31080, "web"), mp("tcp", 31081, "web")})
	g.pass()
	if !strings.Contains(g.r.Status().LastError, "iptables-restore failed") {
		t.Errorf("status %+v", g.r.Status())
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
	ck := &clock{t: time.Now()}
	s, rl, rep := newFakeSlirp(), &fakeRules{}, &fakeReporter{}
	r := New(Config{HostAddr: hostAddr, GuestAddr: guestAddr, Resync: 50 * time.Millisecond, RetryMin: 20 * time.Millisecond}, s, rl, rep)
	_ = ck
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
