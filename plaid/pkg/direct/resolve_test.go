package direct

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
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

func newTestResolver(f *fakePlaid) (*APIResolver, *time.Time) {
	now := time.Unix(1000, 0)
	r := NewAPIResolver(f, netip.MustParseAddr("10.0.1.11"), DefaultPortBase)
	r.now = func() time.Time { return now }
	r.pick = func(int) int { return 0 }
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
	r, _ := newTestResolver(f)
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
	r, _ := newTestResolver(f)
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
	r, _ := newTestResolver(f)
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

func TestResolveRefreshesAndSurvivesPlaidd(t *testing.T) {
	f := &fakePlaid{status: exampleStatus(), routes: []api.RouteInfo{{Subnet: "10.244.5.0/24", RemoteHostIP: "10.0.1.13"}}}
	r, now := newTestResolver(f)
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
	r, _ := newTestResolver(f)
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
	r, now := newTestResolver(f)
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
