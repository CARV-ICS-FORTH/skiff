package direct

import (
	"errors"
	"net/netip"
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
	r := NewAPIResolver(f, netip.MustParseAddr("10.0.1.11"))
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
	want := Backend{Pod: netip.MustParseAddrPort("10.244.5.4:5201"), Host: netip.MustParseAddrPort("10.0.1.13:5201")}
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
	// The pod is on this node, the node's own address; the port is the Service's target port.
	if !ok || got.Pod.String() != "10.244.1.7:8080" || got.Host.String() != "10.0.1.11:8080" {
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
