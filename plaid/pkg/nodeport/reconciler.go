// Package nodeport publishes Kubernetes NodePort Services on the physical host. A NodePort
// is a port of every node that reaches the Service. Here the physical host's port is a
// listener that slirp4netns keeps for the bubble (asked for through its API socket), which
// hands connections to the bubble's uplink address at the same port; a bubble-local
// destination NAT then turns that into the Service's cluster IP and port, and the
// Service engine does the rest (backend choice, return translation). Nothing in the
// engine knows about NodePorts.
//
// The Reconciler owns the forwards and the rules, and nothing else. A forward is its own if
// it has the shape of one it would make: the host address, the bubble's uplink address as
// guest, the same port on both sides, inside the NodePort range. The fixed forwards of the
// deployment (the API and kubelet ports) and anything else slirp4netns has are never touched,
// and ownership needs no memory: a restarted daemon finds its own forwards again.
package nodeport

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"plaid/pkg/slirpapi"
)

// Mapping is a NodePort to publish: the node port, and the Service address and port it is
// translated to.
type Mapping struct {
	Proto       string // "tcp" or "udp"
	Port        int    // the node port
	ClusterIP   [4]byte
	ServicePort uint16
	// Ref is what events about this mapping are about (the Service); the reconciler only
	// hands it back to the Reporter.
	Ref any
}

type mappingKey struct {
	proto string
	port  int
}

func (m Mapping) key() mappingKey { return mappingKey{m.Proto, m.Port} }

// Forwarder is slirp4netns's API as far as the reconciler uses it.
type Forwarder interface {
	ListHostFwd(ctx context.Context) ([]slirpapi.Entry, error)
	AddHostFwd(ctx context.Context, proto, hostAddr string, hostPort int, guestAddr string, guestPort int) (int, error)
	RemoveHostFwd(ctx context.Context, id int) error
}

// Rule is one destination NAT of the bubble's kernel: traffic that came in on the uplink for
// the node port is sent to the Service.
type Rule struct {
	Proto    string
	Port     int
	DestIP   [4]byte
	DestPort uint16
}

// Rules applies the complete set of rules, replacing the previous set atomically.
type Rules interface {
	Apply(ctx context.Context, rules []Rule) error
}

// Reporter receives events about Services.
type Reporter interface {
	Event(ref any, warning bool, reason, message string)
}

// Events the reconciler reports.
const (
	ReasonBindFailed   = "NodePortBindFailed"
	ReasonPublishError = "NodePortPublishFailed"
	ReasonOutOfRange   = "NodePortOutsideRange"
	ReasonPublished    = "NodePortPublished"
)

// Config configures a reconciler.
type Config struct {
	HostAddr  string // the physical host's address the forwards listen on
	GuestAddr string // the bubble's uplink address they forward to
	RangeLow  int    // the NodePort range (default 30000-32767, Kubernetes' own)
	RangeHigh int
	// RetryMin and RetryMax bound the backoff for a port that could not be published.
	RetryMin, RetryMax time.Duration
	// Resync is how often the forwards are compared with what they should be, to repair drift.
	Resync time.Duration
	Now    func() time.Time
}

// PortStatus is the state of one node port.
type PortStatus struct {
	Proto    string
	Port     int
	Exposed  bool
	Reason   string // why it is not
	Attempts int
}

// Status summarizes the reconciler.
type Status struct {
	Desired   int
	Exposed   int
	Failed    []PortStatus
	Passes    uint64
	LastError string // of the last pass, empty if it went through
	Removed   uint64 // forwards removed that were ours and no longer wanted
	Repaired  uint64 // forwards re-made that had disappeared
}

type portState struct {
	exposed   bool
	reason    string
	attempts  int
	nextRetry time.Time
	reported  string // the last failure reported as an event
	everFail  bool
	added     bool // a forward was made by this reconciler at some point
}

// Reconciler keeps slirp4netns's forwards and the kernel's rules in line with the desired mappings.
type Reconciler struct {
	cfg Config
	fwd Forwarder
	rul Rules
	rep Reporter

	mu       sync.Mutex
	desired  map[mappingKey]Mapping
	state    map[mappingKey]*portState
	applied  []Rule
	haveRule bool
	status   Status
	kick     chan struct{}
}

// New creates a reconciler.
func New(cfg Config, fwd Forwarder, rules Rules, rep Reporter) *Reconciler {
	if cfg.RangeLow == 0 && cfg.RangeHigh == 0 {
		cfg.RangeLow, cfg.RangeHigh = 30000, 32767
	}
	if cfg.RetryMin <= 0 {
		cfg.RetryMin = 2 * time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = time.Minute
	}
	if cfg.Resync <= 0 {
		cfg.Resync = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Reconciler{cfg: cfg, fwd: fwd, rul: rules, rep: rep,
		desired: map[mappingKey]Mapping{}, state: map[mappingKey]*portState{}, kick: make(chan struct{}, 1)}
}

// SetDesired installs the complete set of mappings to publish and asks for a pass.
func (r *Reconciler) SetDesired(m []Mapping) {
	r.mu.Lock()
	r.desired = make(map[mappingKey]Mapping, len(m))
	for _, x := range m {
		r.desired[x.key()] = x
	}
	r.mu.Unlock()
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Status returns the current state.
func (r *Reconciler) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.status
	st.Failed = append([]PortStatus(nil), st.Failed...)
	return st
}

// Run reconciles until the context ends: when the desired set changes, when a failed port is
// due for a retry, and at every resync.
func (r *Reconciler) Run(ctx context.Context) {
	for {
		r.Reconcile(ctx)
		wait := r.nextWake()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-r.kick:
			t.Stop()
			// let a burst of changes settle
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		case <-t.C:
		}
	}
}

func (r *Reconciler) nextWake() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	wake := r.cfg.Resync
	now := r.cfg.Now()
	for _, st := range r.state {
		if !st.exposed && !st.nextRetry.IsZero() {
			if d := st.nextRetry.Sub(now); d < wake {
				wake = d
			}
		}
	}
	if wake < 100*time.Millisecond {
		wake = 100 * time.Millisecond
	}
	return wake
}

func (r *Reconciler) owned(e slirpapi.Entry) bool {
	return (e.Proto == "tcp" || e.Proto == "udp") &&
		e.HostAddr == r.cfg.HostAddr && e.GuestAddr == r.cfg.GuestAddr &&
		e.HostPort == e.GuestPort && e.HostPort >= r.cfg.RangeLow && e.HostPort <= r.cfg.RangeHigh
}

func (r *Reconciler) inRange(port int) bool { return port >= r.cfg.RangeLow && port <= r.cfg.RangeHigh }

// Reconcile makes one pass: it lists the forwards, removes the ones that are ours and no
// longer wanted, sets the kernel's rules, and makes the forwards that are missing (those
// that failed wait for their backoff).
func (r *Reconciler) Reconcile(ctx context.Context) {
	r.mu.Lock()
	desired := make([]Mapping, 0, len(r.desired))
	for _, m := range r.desired {
		desired = append(desired, m)
	}
	r.mu.Unlock()
	sort.Slice(desired, func(i, j int) bool {
		if desired[i].Port != desired[j].Port {
			return desired[i].Port < desired[j].Port
		}
		return desired[i].Proto < desired[j].Proto
	})

	var wanted []Mapping
	for _, m := range desired {
		if !r.inRange(m.Port) {
			r.event(m, true, ReasonOutOfRange, fmt.Sprintf("node port %d is outside the range %d-%d that Plaid publishes; it is not published", m.Port, r.cfg.RangeLow, r.cfg.RangeHigh), "range")
			continue
		}
		wanted = append(wanted, m)
	}
	wantedSet := map[mappingKey]Mapping{}
	for _, m := range wanted {
		wantedSet[m.key()] = m
	}

	actual, err := r.fwd.ListHostFwd(ctx)
	if err != nil {
		r.finish(err, 0, 0, wanted)
		return
	}
	have := map[mappingKey]bool{}
	var removed, repaired uint64
	var passErr error
	for _, e := range actual {
		if !r.owned(e) {
			continue
		}
		k := mappingKey{e.Proto, e.HostPort}
		if _, want := wantedSet[k]; want {
			have[k] = true
			continue
		}
		if err := r.fwd.RemoveHostFwd(ctx, e.ID); err != nil {
			passErr = fmt.Errorf("removing the forward of %s port %d: %w", e.Proto, e.HostPort, err)
			continue
		}
		removed++
	}

	if err := r.applyRules(ctx, wanted); err != nil {
		passErr = err
	}

	now := r.cfg.Now()
	for _, m := range wanted {
		k := m.key()
		r.mu.Lock()
		st := r.state[k]
		if st == nil {
			st = &portState{}
			r.state[k] = st
		}
		r.mu.Unlock()
		if have[k] {
			r.mu.Lock()
			st.exposed, st.reason, st.attempts, st.nextRetry = true, "", 0, time.Time{}
			r.mu.Unlock()
			continue
		}
		if st.exposed {
			repaired++ // it was there and is gone
		}
		if !st.nextRetry.IsZero() && now.Before(st.nextRetry) {
			continue
		}
		_, err := r.fwd.AddHostFwd(ctx, m.Proto, r.cfg.HostAddr, m.Port, r.cfg.GuestAddr, m.Port)
		r.mu.Lock()
		if err == nil {
			wasFailing := st.everFail
			st.exposed, st.reason, st.attempts, st.nextRetry, st.added = true, "", 0, time.Time{}, true
			st.reported, st.everFail = "", false
			r.mu.Unlock()
			if wasFailing {
				r.event(m, false, ReasonPublished, fmt.Sprintf("%s node port %d is published on %s", m.Proto, m.Port, r.cfg.HostAddr), "")
			}
			continue
		}
		st.exposed = false
		st.attempts++
		st.reason = err.Error()
		st.everFail = true
		delay := r.cfg.RetryMin << uint(min(st.attempts-1, 16))
		if delay > r.cfg.RetryMax || delay <= 0 {
			delay = r.cfg.RetryMax
		}
		st.nextRetry = now.Add(delay)
		r.mu.Unlock()
		reason, msg := ReasonPublishError, fmt.Sprintf("%s node port %d could not be published on %s: %v", m.Proto, m.Port, r.cfg.HostAddr, err)
		if ae, ok := err.(*slirpapi.APIError); ok && ae.IsBindError() {
			reason = ReasonBindFailed
			msg = fmt.Sprintf("%s node port %d cannot be bound on %s: another process most likely has it (Plaid will retry, and does not use another port)", m.Proto, m.Port, r.cfg.HostAddr)
		}
		r.event(m, true, reason, msg, err.Error()) // a failed port is the port's state, not the pass's error
	}
	r.finish(passErr, removed, repaired, wanted)
}

// applyRules installs the rules for the wanted mappings if they differ from what is applied.
func (r *Reconciler) applyRules(ctx context.Context, wanted []Mapping) error {
	rules := make([]Rule, 0, len(wanted))
	for _, m := range wanted {
		rules = append(rules, Rule{Proto: m.Proto, Port: m.Port, DestIP: m.ClusterIP, DestPort: m.ServicePort})
	}
	r.mu.Lock()
	same := r.haveRule && len(rules) == len(r.applied)
	if same {
		for i := range rules {
			if rules[i] != r.applied[i] {
				same = false
				break
			}
		}
	}
	r.mu.Unlock()
	if same || r.rul == nil {
		return nil
	}
	if err := r.rul.Apply(ctx, rules); err != nil {
		return fmt.Errorf("setting the destination NAT rules: %w", err)
	}
	r.mu.Lock()
	r.applied, r.haveRule = rules, true
	r.mu.Unlock()
	return nil
}

func (r *Reconciler) finish(err error, removed, repaired uint64, wanted []Mapping) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Passes++
	r.status.Removed += removed
	r.status.Repaired += repaired
	r.status.LastError = ""
	if err != nil {
		r.status.LastError = err.Error()
	}
	r.status.Desired = len(wanted)
	r.status.Exposed = 0
	r.status.Failed = nil
	for _, m := range wanted {
		k := m.key()
		st := r.state[k]
		if st == nil {
			continue
		}
		if st.exposed {
			r.status.Exposed++
		} else if st.reason != "" {
			r.status.Failed = append(r.status.Failed, PortStatus{Proto: m.Proto, Port: m.Port, Reason: st.reason, Attempts: st.attempts})
		}
	}
	for k := range r.state { // forget what is no longer desired at all
		if _, ok := r.desired[k]; !ok {
			delete(r.state, k)
		}
	}
}

// event reports once per distinct failure of a port (detail), so a retry does not repeat it.
func (r *Reconciler) event(m Mapping, warning bool, reason, msg, detail string) {
	if r.rep == nil {
		return
	}
	if warning {
		r.mu.Lock()
		st := r.state[m.key()]
		if st == nil {
			st = &portState{}
			r.state[m.key()] = st
		}
		if st.reported == reason+detail {
			r.mu.Unlock()
			return
		}
		st.reported = reason + detail
		r.mu.Unlock()
	}
	r.rep.Event(m.Ref, warning, reason, msg)
}
