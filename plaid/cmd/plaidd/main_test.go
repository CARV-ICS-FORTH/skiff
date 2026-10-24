package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	"plaid/pkg/api"
	"plaid/pkg/k8s"
	"plaid/pkg/tap"
	"plaid/pkg/vxlan"
)

func TestNewPlaidDaemonWithoutKubernetes(t *testing.T) {
	cfg := Config{
		SocketPath:        "/tmp/test-plaidd.sock",
		NodeCIDR:          "10.244.5.0/24",
		DisableKubernetes: true,
		ClusterCIDR:       "10.244.0.0/16",
		GatewayIP:         "10.244.5.1",
		GatewayMAC:        "02:00:00:00:00:01",
		VXLANPort:         8472,
		VXLANVNI:          1,
		EnableSlirp:       false,
		Routes:            "10.244.6.0/24=192.168.1.60",
	}

	daemon, err := NewPlaidDaemon(cfg)
	if err != nil {
		t.Fatalf("NewPlaidDaemon without Kubernetes: %v", err)
	}

	if daemon.k8sCtrl != nil {
		t.Errorf("expected k8sCtrl to be nil when Kubernetes is disabled")
	}

	if daemon.cfg.NodeCIDR != "10.244.5.0/24" {
		t.Errorf("expected NodeCIDR 10.244.5.0/24, got %s", daemon.cfg.NodeCIDR)
	}

	// Verify route was parsed
	routes := daemon.routes.Routes()
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}
	if routes[0].Subnet.String() != "10.244.6.0/24" {
		t.Errorf("expected route subnet 10.244.6.0/24, got %s", routes[0].Subnet.String())
	}
	if routes[0].RemoteHostIP.String() != "192.168.1.60" {
		t.Errorf("expected route host 192.168.1.60, got %s", routes[0].RemoteHostIP.String())
	}

	listed, err := daemon.HandleListRoutes()
	if err != nil || len(listed) != 1 || listed[0].Subnet != "10.244.6.0/24" || listed[0].RemoteHostIP != "192.168.1.60" {
		t.Errorf("listed routes: %v, %v", listed, err)
	}

	status, err := daemon.HandleGetStatus()
	if err != nil {
		t.Fatalf("HandleGetStatus failed: %v", err)
	}
	if status.Mode != "no-kubernetes" {
		t.Errorf("expected status mode 'no-kubernetes', got %s", status.Mode)
	}
}

func TestPlaidDaemonK8sRouteCallbacks(t *testing.T) {
	cfg := Config{
		SocketPath:        "/tmp/test-plaidd-k8s.sock",
		NodeCIDR:          "10.244.1.0/24", // the node network is given; the connector is attached below
		DisableKubernetes: true,
		ClusterCIDR:       "10.244.0.0/16",
		GatewayMAC:        "02:00:00:00:00:01",
		VXLANPort:         8472,
		VXLANVNI:          1,
		EnableSlirp:       false,
	}

	daemon, err := NewPlaidDaemon(cfg)
	if err != nil {
		t.Fatalf("NewPlaidDaemon failed: %v", err)
	}

	// Simulate attaching k8s controller
	fakeClient := fake.NewSimpleClientset()
	daemon.k8sCtrl = k8s.NewController(fakeClient, "local-node")

	status, err := daemon.HandleGetStatus()
	if err != nil {
		t.Fatalf("HandleGetStatus failed: %v", err)
	}
	if status.Mode != "kubernetes" {
		t.Errorf("expected status mode 'kubernetes', got %s", status.Mode)
	}

	// Test dynamically adding route via k8s callback
	gwMAC, _ := net.ParseMAC(daemon.cfg.GatewayMAC)
	_, sn, _ := net.ParseCIDR("10.244.2.0/24")
	hostIP := net.ParseIP("192.168.64.20")

	routeEntry := &vxlan.RouteEntry{
		Subnet:       sn,
		RemoteHostIP: hostIP,
		VtepMAC:      gwMAC,
		VNI:          uint32(daemon.cfg.VXLANVNI),
		Port:         daemon.cfg.VXLANPort,
	}
	daemon.routes.AddRoute(routeEntry)

	routes := daemon.routes.Routes()
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}

	daemon.routes.RemoveRoute("10.244.2.0/24")
	if len(daemon.routes.Routes()) != 0 {
		t.Errorf("expected 0 routes after remove, got %d", len(daemon.routes.Routes()))
	}
}

// An AddEndpoint request with the files of several queues makes one endpoint with that many
// queues, which the status counts; a request that is refused closes all of them.
func TestHandleAddEndpointTakesEveryQueue(t *testing.T) {
	daemon, err := NewPlaidDaemon(tapConfig(func(c *Config) { c.SocketPath = filepath.Join(t.TempDir(), "plaidd.sock") }))
	if err != nil {
		t.Fatal(err)
	}
	// queues makes n socket pairs and returns the descriptors of one end, for the request,
	// and the other ends, which see the first closed.
	queues := func(n int) (fds []int, peers []*os.File) {
		for i := 0; i < n; i++ {
			p, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Skipf("no socket pair: %v", err)
			}
			fds, peers = append(fds, p[0]), append(peers, os.NewFile(uintptr(p[1]), "peer"))
		}
		return
	}
	closed := func(peers []*os.File) bool {
		for _, p := range peers {
			_ = p.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := p.Read(make([]byte, 10)); err != io.EOF {
				return false
			}
		}
		return true
	}

	fds, peers := queues(3)
	if err := daemon.HandleAddEndpoint(&api.Request{PodID: "pod", PodName: "pod", IP: "10.244.5.7", MAC: "02:00:00:00:00:07"}, fds); err != nil {
		t.Fatal(err)
	}
	ep, ok := daemon.bridge.GetEndpoint("pod")
	if !ok || ep.(*tap.TapEndpoint).Queues() != 3 {
		t.Fatalf("endpoint %v, %v: want one with 3 queues", ep, ok)
	}
	if st := daemon.tapPipeline(); st.Endpoints != 1 || st.Queues != 3 {
		t.Errorf("status: %d endpoints, %d queues; want 1 and 3", st.Endpoints, st.Queues)
	}
	if err := daemon.HandleRemoveEndpoint(&api.Request{PodID: "pod"}); err != nil {
		t.Fatal(err)
	}
	if !closed(peers) {
		t.Error("removing the endpoint left a queue open")
	}

	fds, peers = queues(3)
	if err := daemon.HandleAddEndpoint(&api.Request{PodID: "bad", IP: "192.0.2.1", MAC: "02:00:00:00:00:08"}, fds); err == nil {
		t.Fatal("an endpoint outside the node range was accepted")
	}
	if !closed(peers) {
		t.Error("a refused request left a queue open")
	}
	if err := daemon.HandleAddEndpoint(&api.Request{PodID: "bad", IP: "10.244.5.8", MAC: "nonsense"}, fds[:0]); err == nil {
		t.Fatal("a request without a file descriptor was accepted")
	}
}

// An address that is not 6 bytes long (net.ParseMAC takes 8 and 20) is refused, and the
// descriptors of the request are closed.
func TestHandleAddEndpointRefusesAMACThatIsNot6Bytes(t *testing.T) {
	daemon, err := NewPlaidDaemon(tapConfig(func(c *Config) { c.SocketPath = filepath.Join(t.TempDir(), "plaidd.sock") }))
	if err != nil {
		t.Fatal(err)
	}
	p, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no socket pair: %v", err)
	}
	peer := os.NewFile(uintptr(p[1]), "peer")
	defer peer.Close()
	if err := daemon.HandleAddEndpoint(&api.Request{PodID: "bad", IP: "10.244.5.8", MAC: "02:00:00:00:00:00:00:08"}, []int{p[0]}); err == nil {
		t.Fatal("an 8-byte MAC was accepted")
	}
	if _, ok := daemon.bridge.GetEndpoint("bad"); ok {
		t.Error("a refused request left an endpoint")
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 10)); err != io.EOF {
		t.Errorf("the refused request left its descriptor open: %v", err)
	}
}
