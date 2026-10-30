package direct

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"plaid/pkg/api"
)

type fakePlaid struct {
	status   api.Response
	services []api.ServiceEntry
	routes   []api.RouteInfo
	err      error
	asked    int
}

func (f *fakePlaid) GetStatus() (*api.Response, error) {
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	r := f.status
	return &r, nil
}

func (f *fakePlaid) ListServices() (*api.Response, error) {
	return &api.Response{Services: f.services}, f.err
}

func (f *fakePlaid) ListRoutes() ([]api.RouteInfo, error) { return f.routes, f.err }

func newTestResolver(t testing.TB, f *fakePlaid) (*APIResolver, *time.Time) {
	now := time.Unix(1000, 0)
	r := NewAPIResolver(f, netip.MustParseAddr("10.0.1.11"), DefaultPortBase)
	r.now = func() time.Time { return now }
	r.pick = func(int) int { return 0 }
	t.Cleanup(r.Close)
	return r, &now
}

func exampleStatus() api.Response {
	return api.Response{NodeCIDR: "10.244.1.0/24", OverlayAddress: "0.0.0.0:8472"}
}

func TestResolveBackendOnAnotherNode(t *testing.T) {
	f := &fakePlaid{
		status: exampleStatus(),
		services: []api.ServiceEntry{{
			VIP: "10.43.0.77", Port: 5201, Protocol: "tcp",
			Backends: []api.BackendEntry{{IP: "10.244.5.4", Port: 5201, Eligible: true}},
		}},
		routes: []api.RouteInfo{{Subnet: "10.244.5.0/24", RemoteHostIP: "10.0.1.13"}, {Subnet: "10.244.0.0/16", RemoteHostIP: "10.0.9.9"}},
	}
	r, _ := newTestResolver(t, f)
	got, ok := r.Resolve(netip.MustParseAddrPort("10.43.0.77:5201"))
	// 20000 + 4*16 + 5201 mod 16
	want := Backend{Pod: netip.MustParseAddrPort("10.244.5.4:5201"), Host: netip.MustParseAddrPort("10.0.1.13:20065")}
	if !ok || got != want {
		t.Fatalf("got %v, %v; want the longest route's node: %v", got, ok, want)
	}
}

func TestResolveUsesTargetPortAndLocalNode(t *testing.T) {
	f := &fakePlaid{
		status: exampleStatus(),
		services: []api.ServiceEntry{{
			VIP: "10.43.0.80", Port: 80, Protocol: "tcp",
			Backends: []api.BackendEntry{{IP: "10.244.1.7", Port: 8080, Eligible: true}},
		}},
	}
	r, _ := newTestResolver(t, f)
	got, ok := r.Resolve(netip.MustParseAddrPort("10.43.0.80:80"))
	// The pod is on this node, the node's own address; the pod's port is the Service's target
	// port, and the host port follows from the pod's address and that: 20000 + 7*16 + 8080 mod 16.
	if !ok || got.Pod.String() != "10.244.1.7:8080" || got.Host.String() != "10.0.1.11:20112" {
		t.Fatalf("got %v, %v", got, ok)
	}
}

func TestResolveSkipsWhatCannotTakeAConnection(t *testing.T) {
	f := &fakePlaid{
		status: exampleStatus(),
		services: []api.ServiceEntry{
			{VIP: "10.43.0.1", Port: 53, Protocol: "udp", Backends: []api.BackendEntry{{IP: "10.244.5.4", Port: 53, Eligible: true}}},
			{VIP: "10.43.0.2", Port: 80, Protocol: "tcp", Backends: []api.BackendEntry{{IP: "10.244.5.4", Port: 80, Eligible: false}}},
			{VIP: "10.43.0.3", Port: 80, Protocol: "tcp", Backends: []api.BackendEntry{{IP: "10.244.9.9", Port: 80, Eligible: true}}}, // no route
			{VIP: "10.43.0.4", Port: 80, Protocol: "tcp", Backends: []api.BackendEntry{
				{IP: "10.244.9.9", Port: 80, Eligible: true}, {IP: "10.244.5.4", Port: 80, Eligible: true}}},
		},
		routes: []api.RouteInfo{{Subnet: "10.244.5.0/24", RemoteHostIP: "10.0.1.13"}},
	}
	r, _ := newTestResolver(t, f)
	for _, dst := range []string{"10.43.0.1:53", "10.43.0.2:80", "10.43.0.3:80", "10.43.0.99:80", "10.43.0.4:81"} {
		if b, ok := r.Resolve(netip.MustParseAddrPort(dst)); ok {
			t.Errorf("%s resolved to %v", dst, b)
		}
	}
	// The backend on an unknown node is left out, not the Service.
	if b, ok := r.Resolve(netip.MustParseAddrPort("10.43.0.4:80")); !ok || b.Pod.String() != "10.244.5.4:80" {
		t.Errorf("10.43.0.4:80: %v, %v", b, ok)
	}
}

func TestResolvePodAddress(t *testing.T) {
	f := &fakePlaid{
		status: exampleStatus(),
		services: []api.ServiceEntry{{VIP: "10.43.0.80", Port: 80, Protocol: "tcp",
			Backends: []api.BackendEntry{{IP: "10.244.5.4", Port: 8080, Eligible: true}}}},
		routes: []api.RouteInfo{{Subnet: "10.244.5.0/24", RemoteHostIP: "10.0.1.13"}, {Subnet: "10.250.0.0/16", RemoteHostIP: "10.0.9.9"}},
	}
	r, now := newTestResolver(t, f)
	for dst, want := range map[string]Backend{
		// A pod of another node: its own port, on the node it is routed to (20000 + 4*16 + 8080 mod 16).
		"10.244.5.4:8080": {Pod: netip.MustParseAddrPort("10.244.5.4:8080"), Host: netip.MustParseAddrPort("10.0.1.13:20064")},
		// A pod of this node: on the node's own address (20000 + 7*16 + 5201 mod 16).
		"10.244.1.7:5201": {Pod: netip.MustParseAddrPort("10.244.1.7:5201"), Host: netip.MustParseAddrPort("10.0.1.11:20113")},
	} {
		if got, ok := r.Resolve(netip.MustParseAddrPort(dst)); !ok || got != want {
			t.Errorf("%s: %v, %v; want %v", dst, got, ok, want)
		}
	}
	// A Service is still resolved to its backend's target port.
	if got, ok := r.Resolve(netip.MustParseAddrPort("10.43.0.80:80")); !ok || got.Pod.String() != "10.244.5.4:8080" {
		t.Errorf("the Service: %v, %v", got, ok)
	}
	// Addresses that are not a pod's: the node's own, a network no route leads to, a route
	// that is too wide for host ports (a pod network larger than a /24). Only the first two
	// may be a Service or a pod that is new, so only they make plaidd list its tables again.
	*now = now.Add(missRefresh)
	asked := f.asked
	if got, ok := r.Resolve(netip.MustParseAddrPort("10.250.3.4:5201")); ok || f.asked != asked {
		t.Errorf("a pod of a wide route: %v, %v, %d requests", got, ok, f.asked-asked)
	}
	for _, dst := range []string{"10.0.1.11:5201", "10.244.9.9:5201"} {
		*now = now.Add(missRefresh)
		asked = f.asked
		if got, ok := r.Resolve(netip.MustParseAddrPort(dst)); ok || f.asked != asked+1 {
			t.Errorf("%s: %v, %v, %d requests", dst, got, ok, f.asked-asked)
		}
	}
}

func TestResolvePodOfANewNode(t *testing.T) {
	f := &fakePlaid{status: exampleStatus()}
	r, now := newTestResolver(t, f)
	dst := netip.MustParseAddrPort("10.244.6.4:5201")
	if _, ok := r.Resolve(dst); ok {
		t.Fatal("resolved before the node existed")
	}
	// The node joins: a miss asks again, but not before missRefresh has passed.
	f.routes = []api.RouteInfo{{Subnet: "10.244.6.0/24", RemoteHostIP: "10.0.1.14"}}
	*now = now.Add(missRefresh)
	if got, ok := r.Resolve(dst); !ok || got.Host.String() != "10.0.1.14:20065" {
		t.Errorf("after the node joined: %v, %v", got, ok)
	}
	asked := f.asked
	if _, ok := r.Resolve(dst); !ok || f.asked != asked {
		t.Errorf("a hit asked plaidd: %d requests", f.asked-asked)
	}
}

func TestResolveRefreshesAndSurvivesPlaidd(t *testing.T) {
	f := &fakePlaid{status: exampleStatus(), routes: []api.RouteInfo{{Subnet: "10.244.5.0/24", RemoteHostIP: "10.0.1.13"}}}
	r, now := newTestResolver(t, f)
	vip := netip.MustParseAddrPort("10.43.0.5:80")
	if _, ok := r.Resolve(vip); ok {
		t.Fatal("resolved before the Service existed")
	}
	// The Service appears: a miss asks again, but not before missRefresh has passed.
	f.services = []api.ServiceEntry{{VIP: "10.43.0.5", Port: 80, Protocol: "tcp", Backends: []api.BackendEntry{{IP: "10.244.5.4", Port: 80, Eligible: true}}}}
	if _, ok := r.Resolve(vip); ok {
		t.Error("asked plaidd again at once for a miss")
	}
	*now = now.Add(missRefresh)
	if _, ok := r.Resolve(vip); !ok {
		t.Error("a miss did not refresh the tables")
	}
	// A hit within refreshAfter asks nothing.
	asked := f.asked
	if _, ok := r.Resolve(vip); !ok || f.asked != asked {
		t.Errorf("a hit asked plaidd: %d requests", f.asked-asked)
	}
	// plaidd goes away: the last tables are still good.
	f.err = errors.New("plaidd is not there")
	*now = now.Add(10 * time.Second)
	if _, ok := r.Resolve(vip); !ok {
		t.Error("the copy was dropped when plaidd could not be asked")
	}
}

func TestHostOfPods(t *testing.T) {
	f := &fakePlaid{
		status: exampleStatus(),
		routes: []api.RouteInfo{{Subnet: "10.244.5.0/24", RemoteHostIP: "10.0.1.13"}},
	}
	r, _ := newTestResolver(t, f)
	for pod, want := range map[string]string{"10.244.1.7": "10.0.1.11", "10.244.5.4": "10.0.1.13"} {
		if got, ok := r.HostOf(netip.MustParseAddr(pod)); !ok || got.String() != want {
			t.Errorf("%s: %v, %v; want %s", pod, got, ok, want)
		}
	}
	if got, ok := r.HostOf(netip.MustParseAddr("10.244.9.9")); ok {
		t.Errorf("a pod of no known node is on %v", got)
	}
}

func TestHostPort(t *testing.T) {
	pod := func(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }
	net24 := netip.MustParsePrefix("10.244.5.0/24")
	for _, tc := range []struct {
		base int
		net  netip.Prefix
		pod  string
		want uint16
	}{
		{20000, net24, "10.244.5.0:0", 20000},
		{20000, net24, "10.244.5.4:5201", 20065},
		{20000, net24, "10.244.5.4:5217", 20065}, // equal modulo 16: the same host port
		{20000, net24, "10.244.5.4:5202", 20066},
		{20000, net24, "10.244.5.255:65535", 24095}, // the last of 4096
		{30000, netip.MustParsePrefix("10.244.5.128/25"), "10.244.5.130:80", 30032},
		{20000, netip.MustParsePrefix("10.244.5.4/24"), "10.244.5.4:80", 20064}, // the network's host bits are ignored
	} {
		if got, ok := HostPort(tc.base, tc.net, pod(tc.pod)); !ok || got != tc.want {
			t.Errorf("%d %v %s: %d, %v; want %d", tc.base, tc.net, tc.pod, got, ok, tc.want)
		}
	}
	for name, c := range map[string]struct {
		base int
		net  netip.Prefix
		pod  string
	}{
		"a /23 has more pods than ports": {20000, netip.MustParsePrefix("10.244.4.0/23"), "10.244.4.4:80"},
		"a pod of another network":       {20000, net24, "10.244.6.4:80"},
		"a base that runs out of ports":  {61441, net24, "10.244.5.4:80"},
		"a base in the system's ports":   {80, net24, "10.244.5.4:80"},
		"no network":                     {20000, netip.Prefix{}, "10.244.5.4:80"},
	} {
		if got, ok := HostPort(c.base, c.net, pod(c.pod)); ok {
			t.Errorf("%s: %d", name, got)
		}
	}
	if CheckPortBase(DefaultPortBase) != nil || CheckPortBase(61440) != nil || CheckPortBase(61441) == nil {
		t.Error("CheckPortBase")
	}
}

func TestResolveSaysOnceThatAPodNetworkHasNoHostPorts(t *testing.T) {
	f := &fakePlaid{
		status: exampleStatus(),
		services: []api.ServiceEntry{{VIP: "10.43.0.6", Port: 80, Protocol: "tcp",
			Backends: []api.BackendEntry{{IP: "10.244.8.4", Port: 80, Eligible: true}}}},
		routes: []api.RouteInfo{{Subnet: "10.244.8.0/22", RemoteHostIP: "10.0.1.13"}},
	}
	r, now := newTestResolver(t, f)
	var lines []string
	r.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	for i := 0; i < 3; i++ {
		if b, ok := r.Resolve(netip.MustParseAddrPort("10.43.0.6:80")); ok {
			t.Fatalf("resolved to %v on a node with a /22", b)
		}
		*now = now.Add(time.Second)
	}
	if _, ok := r.HostPort(netip.MustParseAddrPort("10.244.8.4:80")); ok {
		t.Error("a host port in a /22")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "10.244.8.0/22") {
		t.Errorf("log: %q", lines)
	}
}

// gatedPlaid answers like fakePlaid, safely for goroutines, and can hold its answers back.
type gatedPlaid struct {
	status api.Response
	asked  atomic.Int32 // requests of GetStatus, the first of the three of a refresh

	mu       sync.Mutex
	services []api.ServiceEntry
	gate     chan struct{} // when set, the answers wait until it is closed
}

func (p *gatedPlaid) GetStatus() (*api.Response, error) {
	p.asked.Add(1)
	p.mu.Lock()
	gate := p.gate
	p.mu.Unlock()
	if gate != nil {
		<-gate
	}
	r := p.status
	return &r, nil
}

func (p *gatedPlaid) ListServices() (*api.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &api.Response{Services: append([]api.ServiceEntry(nil), p.services...)}, nil
}

func (p *gatedPlaid) ListRoutes() ([]api.RouteInfo, error) { return nil, nil }

func (p *gatedPlaid) hold() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gate = make(chan struct{})
	return p.gate
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s did not happen", what)
}

func plaidWith(vip string) *gatedPlaid {
	return &gatedPlaid{status: exampleStatus(), services: []api.ServiceEntry{{VIP: vip, Port: 80, Protocol: "tcp",
		Backends: []api.BackendEntry{{IP: "10.244.1.7", Port: 80, Eligible: true}}}}}
}

// A lookup of an address the tables have does not wait for plaidd, which a refresh in the
// background is asking (slowly); the lookups that miss wait for the one asking, and do not
// ask again.
func TestLookupsDoNotWaitForARefresh(t *testing.T) {
	p := plaidWith("10.43.0.5")
	r := NewAPIResolver(p, netip.MustParseAddr("10.0.1.11"), DefaultPortBase)
	t.Cleanup(r.Close)
	vip := netip.MustParseAddrPort("10.43.0.5:80")
	if _, ok := r.Resolve(vip); !ok {
		t.Fatal("not resolved")
	}
	time.Sleep(missRefresh + 50*time.Millisecond) // a miss may ask now
	gate := p.hold()
	go r.reload(0) // what the background does every refreshEvery
	waitUntil(t, "the refresh", func() bool { return p.asked.Load() == 2 })

	start := time.Now()
	for i := 0; i < 100; i++ {
		if _, ok := r.Resolve(vip); !ok {
			t.Fatal("a hit was lost during the refresh")
		}
		if _, ok := r.HostOf(netip.MustParseAddr("10.244.1.7")); !ok {
			t.Fatal("a pod was lost during the refresh")
		}
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("200 lookups took %v while plaidd was being asked", took)
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Resolve(netip.MustParseAddrPort("10.43.0.99:80")) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if got := p.asked.Load(); got != 2 {
		t.Errorf("plaidd was asked %d times, want the one refresh that was on its way and no more", got)
	}
}

// The tables are asked for again without a lookup, and the answer is the next lookup's.
func TestTablesAreRefreshedInTheBackground(t *testing.T) {
	p := plaidWith("10.43.0.5")
	r := NewAPIResolver(p, netip.MustParseAddr("10.0.1.11"), DefaultPortBase)
	r.every = 10 * time.Millisecond
	t.Cleanup(r.Close)
	if _, ok := r.Resolve(netip.MustParseAddrPort("10.43.0.5:80")); !ok {
		t.Fatal("not resolved")
	}
	p.mu.Lock()
	p.services = append(p.services, plaidWith("10.43.0.6").services...)
	p.mu.Unlock()
	waitUntil(t, "the refresh", func() bool {
		_, ok := r.loaded.Load().services[netip.MustParseAddrPort("10.43.0.6:80")]
		return ok
	})
}
