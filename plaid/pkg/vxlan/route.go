package vxlan

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
)

var (
	ErrRouteNotFound = errors.New("route not found")
)

// RouteEntry represents a remote node subnet destination.
type RouteEntry struct {
	Subnet       *net.IPNet
	RemoteHostIP net.IP
	VtepMAC      net.HardwareAddr
	VNI          uint32
	Port         int
}

// RouteTable manages CIDR routes for cross-host overlay forwarding.
type RouteTable struct {
	mu     sync.RWMutex
	routes []*RouteEntry
	// nodes holds the addresses of the routes' nodes, published whole: it is read for every
	// read from the overlay's sockets.
	nodes atomic.Pointer[map[netip.Addr]struct{}]
}

// NewRouteTable creates an empty RouteTable.
func NewRouteTable() *RouteTable {
	return &RouteTable{
		routes: make([]*RouteEntry, 0),
	}
}

// AddRoute adds or updates a route for a remote subnet.
func (rt *RouteTable) AddRoute(entry *RouteEntry) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	for i, r := range rt.routes {
		if r.Subnet.String() == entry.Subnet.String() {
			rt.routes[i] = entry
			rt.publishNodes()
			return
		}
	}
	rt.routes = append(rt.routes, entry)
	rt.publishNodes()
}

// RemoveRoute removes the route matching the given subnet CIDR string.
func (rt *RouteTable) RemoveRoute(subnetStr string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	for i, r := range rt.routes {
		if r.Subnet.String() == subnetStr {
			rt.routes = append(rt.routes[:i], rt.routes[i+1:]...)
			rt.publishNodes()
			return true
		}
	}
	return false
}

// publishNodes makes the addresses of the current routes the ones IsNode knows. The caller
// holds rt.mu.
func (rt *RouteTable) publishNodes() {
	nodes := make(map[netip.Addr]struct{}, len(rt.routes))
	for _, r := range rt.routes {
		if a, ok := netip.AddrFromSlice(r.RemoteHostIP); ok {
			nodes[a.Unmap()] = struct{}{}
		}
	}
	rt.nodes.Store(&nodes)
}

// IsNode reports whether a is the address of a node a route leads to.
func (rt *RouteTable) IsNode(a netip.Addr) bool {
	nodes := rt.nodes.Load()
	if nodes == nil {
		return false
	}
	_, ok := (*nodes)[a.Unmap()]
	return ok
}

// Lookup finds the RouteEntry matching a destination IP address.
func (rt *RouteTable) Lookup(dstIP net.IP) (*RouteEntry, error) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	for _, r := range rt.routes {
		if r.Subnet.Contains(dstIP) {
			return r, nil
		}
	}
	return nil, ErrRouteNotFound
}

// Routes returns a copy of all current routes.
func (rt *RouteTable) Routes() []*RouteEntry {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	res := make([]*RouteEntry, len(rt.routes))
	copy(res, rt.routes)
	return res
}
