package direct

import (
	"math/rand/v2"
	"net/netip"
	"strings"
	"sync"
	"time"

	"plaid/pkg/api"
)

// Backend is where a Service address leads: the pod that will serve the connection, and the
// host socket address on its node that the connection goes to.
type Backend struct {
	Pod  netip.AddrPort // the pod's address and port
	Host netip.AddrPort // the node's address and the port the pod's server listens on there
}

// Resolver finds the backend of a Service address. It says false for an address that is no
// Service's (or that has no backend that can take a connection), and the connect goes the
// way it would have without the supervisor.
type Resolver interface {
	Resolve(dst netip.AddrPort) (Backend, bool)
}

// PlaidAPI is what the resolver asks of plaidd (api.Client).
type PlaidAPI interface {
	GetStatus() (*api.Response, error)
	ListServices() (*api.Response, error)
	ListRoutes() ([]api.RouteInfo, error)
}

// refreshAfter is how old plaidd's tables may be when a Service address is looked up. A
// miss refreshes them sooner (a Service made a moment ago), but plaidd is asked no more
// often than missRefresh, so that a container that connects to an address that is no
// Service's does not make plaidd list its table for every connect.
const (
	refreshAfter = 2 * time.Second
	missRefresh  = 250 * time.Millisecond
)

// APIResolver resolves Service addresses with the Service table and the routes of plaidd.
// It keeps a copy of both for a short time.
type APIResolver struct {
	client PlaidAPI
	local  netip.Addr // this node's address, when it is not what the overlay is bound to
	now    func() time.Time
	pick   func(n int) int

	mu       sync.Mutex
	loaded   time.Time                           // when the copy was made
	tried    time.Time                           // when plaidd was last asked
	services map[netip.AddrPort][]netip.AddrPort // Service address to its eligible backends
	nodeNet  netip.Prefix                        // the pods of this node
	localIP  netip.Addr
	routes   []route
}

type route struct {
	subnet netip.Prefix
	host   netip.Addr
}

// NewAPIResolver makes a resolver that asks client. localHost is this node's address on
// the network the other nodes reach it by; if it is not valid the address plaidd's overlay
// socket is bound to is used.
func NewAPIResolver(client PlaidAPI, localHost netip.Addr) *APIResolver {
	return &APIResolver{client: client, local: localHost, now: time.Now, pick: rand.IntN}
}

// Resolve implements Resolver. The backend is one of the Service's eligible ones, picked
// at random for each connection; its pod is on this node (its subnet is the node's) or on
// the node its subnet is routed to, and the connection goes to the same port there.
func (r *APIResolver) Resolve(dst netip.AddrPort) (Backend, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.now().Sub(r.loaded) >= refreshAfter {
		r.load()
	}
	backends, ok := r.services[dst]
	if !ok && r.load() {
		backends, ok = r.services[dst]
	}
	if !ok {
		return Backend{}, false
	}
	// Those on a node that is not known are left out; a Service whose backends are all
	// unknown is not one for the direct path.
	var usable []Backend
	for _, pod := range backends {
		if host, ok := r.hostOf(pod.Addr()); ok {
			usable = append(usable, Backend{Pod: pod, Host: netip.AddrPortFrom(host, pod.Port())})
		}
	}
	if len(usable) == 0 {
		return Backend{}, false
	}
	return usable[r.pick(len(usable))], true
}

// hostOf is the address of the node a pod is on.
func (r *APIResolver) hostOf(pod netip.Addr) (netip.Addr, bool) {
	if r.nodeNet.Contains(pod) {
		return r.localIP, r.localIP.IsValid() && !r.localIP.IsUnspecified()
	}
	best := -1
	var host netip.Addr
	for _, rt := range r.routes {
		if rt.subnet.Contains(pod) && rt.subnet.Bits() > best {
			best, host = rt.subnet.Bits(), rt.host
		}
	}
	return host, best >= 0
}

// load replaces the copy with plaidd's tables, unless it asked a moment ago, and says
// whether it did. If plaidd cannot be asked the old copy stays.
func (r *APIResolver) load() bool {
	now := r.now()
	if now.Sub(r.tried) < missRefresh {
		return false
	}
	r.tried = now
	status, err := r.client.GetStatus()
	if err != nil {
		return false
	}
	list, err := r.client.ListServices()
	if err != nil {
		return false
	}
	routes, err := r.client.ListRoutes()
	if err != nil {
		return false
	}
	services := make(map[netip.AddrPort][]netip.AddrPort)
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
				services[key] = append(services[key], netip.AddrPortFrom(ip, b.Port))
			}
		}
	}
	r.nodeNet, _ = netip.ParsePrefix(status.NodeCIDR)
	r.localIP = r.local
	if !r.localIP.IsValid() {
		if ap, err := netip.ParseAddrPort(status.OverlayAddress); err == nil {
			r.localIP = ap.Addr()
		}
	}
	r.routes = r.routes[:0]
	for _, rt := range routes {
		subnet, err1 := netip.ParsePrefix(rt.Subnet)
		host, err2 := netip.ParseAddr(rt.RemoteHostIP)
		if err1 == nil && err2 == nil {
			r.routes = append(r.routes, route{subnet, host})
		}
	}
	r.services = services
	r.loaded = r.now()
	return true
}
