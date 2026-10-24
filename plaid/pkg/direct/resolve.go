package direct

import (
	"math/rand/v2"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"plaid/pkg/api"
)

// Backend is where a Service or pod address leads: the pod that will serve the connection, and the
// host socket address on its node that the connection goes to (see HostPort).
type Backend struct {
	Pod  netip.AddrPort // the pod's address and port
	Host netip.AddrPort // the node's address and the port the pod's server listens on there
}

// Resolver finds the backend of a Service or pod address. It says false for an address that
// is neither (or that has no backend that can take a connection), and the connect goes the
// way it would have without the supervisor.
type Resolver interface {
	Resolve(dst netip.AddrPort) (Backend, bool)
	// HostOf is the address of the node a pod is on, as the other nodes reach it.
	HostOf(pod netip.Addr) (netip.Addr, bool)
	// HostPort is the port on its node's address where the pod's port is served on the host
	// (see the package's HostPort); false if the pod has none.
	HostPort(pod netip.AddrPort) (uint16, bool)
}

// PlaidAPI is what the resolver asks of plaidd (api.Client).
type PlaidAPI interface {
	GetStatus() (*api.Response, error)
	ListServices() (*api.Response, error)
	ListRoutes() ([]api.RouteInfo, error)
}

// plaidd is asked for its tables in the background every refreshEvery, and not by the lookups
// of a connect or an accept, which wait for nothing but the first answer. A lookup that misses
// asks sooner (a Service made a moment ago), and waits for the answer, but plaidd is asked no
// more often than missRefresh for that, so that a container that connects to an address that
// is neither a Service's nor a pod's does not make plaidd list its table for every connect.
const (
	refreshEvery = 2 * time.Second
	missRefresh  = 250 * time.Millisecond
)

// APIResolver resolves Service and pod addresses with the Service table and the routes of plaidd.
// It keeps a copy of both, which it asks for again in the background (see Close).
type APIResolver struct {
	client PlaidAPI
	local  netip.Addr // this node's address, when it is not what the overlay is bound to
	base   int        // where the host ports start
	now    func() time.Time
	pick   func(n int) int
	every  time.Duration // how often the copy is made again in the background
	// Logf, when set, receives a line for a node whose pods have no host ports.
	Logf func(format string, args ...any)

	loaded    atomic.Pointer[tables] // the last answer of plaidd; nil before the first
	startOnce sync.Once
	stop      chan struct{}
	stopOnce  sync.Once

	mu     sync.Mutex // the asking, not the copy: no lookup takes it while plaidd is asked
	tried  time.Time  // when plaidd was last asked
	asking *question  // the asking that is on its way, if any

	logMu   sync.Mutex
	noPorts map[netip.Prefix]bool // the pod networks that were said to have no host ports
}

// tables is what plaidd said, which is not changed once it is published.
type tables struct {
	services map[netip.AddrPort][]netip.AddrPort // Service address to its eligible backends
	nodeNet  netip.Prefix                        // the pods of this node
	localIP  netip.Addr
	routes   []route
}

// question is an asking of plaidd that callers that come meanwhile wait for.
type question struct {
	done chan struct{}
	ok   bool
}

type route struct {
	subnet netip.Prefix
	host   netip.Addr
}

// NewAPIResolver makes a resolver that asks client. localHost is this node's address on
// the network the other nodes reach it by; if it is not valid the address plaidd's overlay
// socket is bound to is used. portBase is where the host ports start (CheckPortBase).
func NewAPIResolver(client PlaidAPI, localHost netip.Addr, portBase int) *APIResolver {
	return &APIResolver{client: client, local: localHost, base: portBase, now: time.Now, pick: rand.IntN,
		every: refreshEvery, stop: make(chan struct{})}
}

// Close ends the asking in the background.
func (r *APIResolver) Close() { r.stopOnce.Do(func() { close(r.stop) }) }

// Resolve implements Resolver. A Service address leads to one of the Service's eligible
// backends, picked at random for each connection. A pod's address leads to that pod, whose
// subnet is this node's or the node's it is routed to; the Service CIDR and the pod networks
// are different ranges, so the address says which it is. The connection goes to the pod's
// port, which the backend's target port or the destination itself names.
func (r *APIResolver) Resolve(dst netip.AddrPort) (Backend, bool) {
	b, known, ok := r.lookup(r.current(), dst)
	if !known && r.reload(missRefresh) {
		b, _, ok = r.lookup(r.current(), dst)
	}
	return b, ok
}

// lookup resolves dst from the copy of the tables. known says that dst is a Service's
// address or in a pod network the routes lead to; a refresh may change the answer for an
// address that is not.
func (r *APIResolver) lookup(t *tables, dst netip.AddrPort) (b Backend, known, ok bool) {
	if node, ok := t.nodeOf(dst.Addr()); ok {
		if port, ok := r.hostPort(node, dst); ok {
			return Backend{Pod: dst, Host: netip.AddrPortFrom(node.host, port)}, true, true
		}
		return Backend{}, true, false
	}
	backends, ok := t.services[dst]
	if !ok {
		return Backend{}, false, false
	}
	// Those on a node that is not known are left out; a Service whose backends are all
	// unknown is not one for the direct path.
	var usable []Backend
	for _, pod := range backends {
		if node, ok := t.nodeOf(pod.Addr()); ok {
			if port, ok := r.hostPort(node, pod); ok {
				usable = append(usable, Backend{Pod: pod, Host: netip.AddrPortFrom(node.host, port)})
			}
		}
	}
	if len(usable) == 0 {
		return Backend{}, true, false
	}
	return usable[r.pick(len(usable))], true, true
}

// HostOf implements Resolver.
func (r *APIResolver) HostOf(pod netip.Addr) (netip.Addr, bool) {
	node, ok := r.current().nodeOf(pod)
	if !ok && r.reload(missRefresh) {
		node, ok = r.current().nodeOf(pod)
	}
	return node.host, ok
}

// HostPort implements Resolver.
func (r *APIResolver) HostPort(pod netip.AddrPort) (uint16, bool) {
	node, ok := r.current().nodeOf(pod.Addr())
	if !ok && r.reload(missRefresh) {
		node, ok = r.current().nodeOf(pod.Addr())
	}
	if !ok {
		return 0, false
	}
	return r.hostPort(node, pod)
}

// nodeOf is the node a pod is on: its pod network and its address.
func (t *tables) nodeOf(pod netip.Addr) (route, bool) {
	if t.nodeNet.Contains(pod) {
		return route{t.nodeNet, t.localIP}, t.localIP.IsValid() && !t.localIP.IsUnspecified()
	}
	best := -1
	var node route
	for _, rt := range t.routes {
		if rt.subnet.Contains(pod) && rt.subnet.Bits() > best {
			best, node = rt.subnet.Bits(), rt
		}
	}
	return node, best >= 0
}

// hostPort is the host port of a pod on the node, and says (once for each pod network) when
// the node's pod network is too large to have one.
func (r *APIResolver) hostPort(node route, pod netip.AddrPort) (uint16, bool) {
	port, ok := HostPort(r.base, node.subnet, pod)
	if !ok && r.Logf != nil && node.subnet.Bits() < 24 {
		r.logMu.Lock()
		first := !r.noPorts[node.subnet]
		if first {
			if r.noPorts == nil {
				r.noPorts = make(map[netip.Prefix]bool)
			}
			r.noPorts[node.subnet] = true
		}
		r.logMu.Unlock()
		if first {
			r.Logf("no direct path to the pods of %s: a node's pod network is at most a /24 for host ports", node.subnet)
		}
	}
	return port, ok
}

// current is the copy of plaidd's tables. The first lookup waits for the first answer, and
// finds nothing if plaidd does not give one; the others wait for nothing.
func (r *APIResolver) current() *tables {
	r.startOnce.Do(func() { go r.refreshInBackground() })
	if t := r.loaded.Load(); t != nil {
		return t
	}
	r.reload(missRefresh)
	if t := r.loaded.Load(); t != nil {
		return t
	}
	return &tables{}
}

func (r *APIResolver) refreshInBackground() {
	tick := time.NewTicker(r.every)
	defer tick.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-tick.C:
			r.reload(0)
		}
	}
}

// reload asks plaidd for its tables and publishes them, unless it was asked less than minGap
// ago, and says whether it did. A caller that comes while plaidd is being asked waits for that
// answer and does not ask again. If plaidd cannot be asked the old copy stays.
func (r *APIResolver) reload(minGap time.Duration) bool {
	r.mu.Lock()
	if q := r.asking; q != nil {
		r.mu.Unlock()
		<-q.done
		return q.ok
	}
	now := r.now()
	if now.Sub(r.tried) < minGap {
		r.mu.Unlock()
		return false
	}
	r.tried = now
	q := &question{done: make(chan struct{})}
	r.asking = q
	r.mu.Unlock()

	t, ok := r.ask()
	if ok {
		r.loaded.Store(t)
	}
	r.mu.Lock()
	r.asking = nil
	r.mu.Unlock()
	q.ok = ok
	close(q.done)
	return ok
}

// ask makes plaidd's tables from its answers.
func (r *APIResolver) ask() (*tables, bool) {
	status, err := r.client.GetStatus()
	if err != nil {
		return nil, false
	}
	list, err := r.client.ListServices()
	if err != nil {
		return nil, false
	}
	routes, err := r.client.ListRoutes()
	if err != nil {
		return nil, false
	}
	t := &tables{services: make(map[netip.AddrPort][]netip.AddrPort)}
	for _, e := range list.Services {
		if !strings.EqualFold(e.Protocol, "tcp") {
			continue
		}
		vip, err := netip.ParseAddr(e.VIP)
		if err != nil {
			continue
		}
		key := netip.AddrPortFrom(vip, e.Port)
		for _, b := range e.Backends {
			if ip, err := netip.ParseAddr(b.IP); err == nil && b.Eligible {
				t.services[key] = append(t.services[key], netip.AddrPortFrom(ip, b.Port))
			}
		}
	}
	t.nodeNet, _ = netip.ParsePrefix(status.NodeCIDR)
	t.localIP = r.local
	if !t.localIP.IsValid() {
		if ap, err := netip.ParseAddrPort(status.OverlayAddress); err == nil {
			t.localIP = ap.Addr()
		}
	}
	for _, rt := range routes {
		subnet, err1 := netip.ParsePrefix(rt.Subnet)
		host, err2 := netip.ParseAddr(rt.RemoteHostIP)
		if err1 == nil && err2 == nil {
			t.routes = append(t.routes, route{subnet, host})
		}
	}
	return t, true
}
