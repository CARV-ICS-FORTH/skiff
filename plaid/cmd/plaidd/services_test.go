package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"plaid/pkg/api"
	"plaid/pkg/k8s"
)

func tapDaemon(t *testing.T, mutate func(*Config)) *PlaidDaemon {
	t.Helper()
	cfg := tapConfig(func(c *Config) {
		c.DisableKubernetes = true
		if mutate != nil {
			mutate(c)
		}
	})
	d, err := NewPlaidDaemon(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestServiceAddressDefaultsToDotTwoAndIsChecked(t *testing.T) {
	_, node, _ := net.ParseCIDR("10.244.5.0/24")
	for given, want := range map[string]string{"": "10.244.5.2", "10.244.5.3": "10.244.5.3", "10.244.5.2": "10.244.5.2"} {
		got, err := serviceAddress(given, node)
		if err != nil || got.String() != want {
			t.Errorf("serviceAddress(%q) = %v, %v; want %s", given, got, err, want)
		}
	}
	for _, bad := range []string{"10.244.5.1", "10.244.5.4", "10.244.5.0", "10.244.6.2", "fd00::2", "nonsense"} {
		if _, err := serviceAddress(bad, node); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTapModeHasAServiceEngineAndOtherModesDoNot(t *testing.T) {
	d := tapDaemon(t, nil)
	if d.services == nil || d.services.SNATAddr().String() != "10.244.5.2" || d.cfg.SNATAddress != "10.244.5.2" {
		t.Fatalf("engine %v, SNAT %q", d.services, d.cfg.SNATAddress)
	}
	if !d.services.Owns([4]byte{10, 43, 0, 1}) || !d.services.Owns([4]byte{10, 244, 5, 2}) {
		t.Error("the engine does not own the Service range and its own address")
	}
	if d.bridge == nil {
		t.Fatal("no bridge")
	}
	status, _ := d.HandleGetStatus()
	if status.ServiceAddress != "10.244.5.2" || status.ServiceCIDR != "10.43.0.0/16" || status.ServiceManager != "api" || status.ServiceEngine == nil || status.ServiceEngine.Synced {
		t.Errorf("status %+v %+v", status, status.ServiceEngine)
	}

	slirp, err := NewPlaidDaemon(Config{SocketPath: "/tmp/x.sock", NodeCIDR: "10.244.5.0/24", DisableKubernetes: true, ClusterCIDR: "10.244.0.0/16", GatewayMAC: "02:00:00:00:00:01", GatewayMode: "slirp", VXLANPort: 8472, VXLANVNI: 1})
	if err != nil {
		t.Fatal(err)
	}
	if slirp.services != nil {
		t.Error("slirp mode has a Service engine")
	}
	if err := slirp.HandleSetServices(&api.Request{}); err == nil {
		t.Error("the table was set on a daemon without an engine")
	}
}

func TestDisablingKubernetesNeedsTheNodeNetwork(t *testing.T) {
	cfg := tapConfig(func(c *Config) { c.DisableKubernetes = true; c.NodeCIDR = "" })
	if _, err := NewPlaidDaemon(cfg); err == nil {
		t.Error("--disable-kubernetes without --node-cidr was accepted")
	}
}

func entry(id, vip string, port uint16, proto string, backends ...api.BackendEntry) api.ServiceEntry {
	return api.ServiceEntry{ID: id, VIP: vip, Port: port, Protocol: proto, Backends: backends}
}

func TestTheTableIsSetAtomicallyAndChecked(t *testing.T) {
	d := tapDaemon(t, nil)
	good := []api.ServiceEntry{
		entry("web", "10.43.0.50", 80, "tcp", api.BackendEntry{IP: "10.244.5.9", Port: 8080, Eligible: true}),
		entry("dns", "10.43.0.10", 53, "UDP", api.BackendEntry{IP: "10.244.6.4", Port: 53}),
	}
	if err := d.HandleSetServices(&api.Request{Services: good}); err != nil {
		t.Fatal(err)
	}
	list, err := d.HandleListServices()
	if err != nil || len(list.Services) != 2 || list.Services[0].VIP != "10.43.0.10" || list.Services[0].Protocol != "udp" || list.Services[1].Backends[0].Eligible != true {
		t.Fatalf("listing %+v %v", list, err)
	}
	if list.ServiceEngine == nil || !list.ServiceEngine.Synced || list.ServiceEngine.Services != 2 || list.ServiceEngine.Backends != 2 {
		t.Errorf("stats %+v", list.ServiceEngine)
	}

	bad := map[string][]api.ServiceEntry{
		"outside the range": {entry("x", "10.99.0.1", 80, "tcp")},
		"not an address":    {entry("x", "web", 80, "tcp")},
		"port 0":            {entry("x", "10.43.0.1", 0, "tcp")},
		"protocol":          {entry("x", "10.43.0.1", 80, "sctp")},
		"duplicate":         {entry("a", "10.43.0.1", 80, "tcp"), entry("b", "10.43.0.1", 80, "TCP")},
		"backend address":   {entry("x", "10.43.0.1", 80, "tcp", api.BackendEntry{IP: "fd00::1", Port: 80})},
		"backend port":      {entry("x", "10.43.0.1", 80, "tcp", api.BackendEntry{IP: "10.244.5.9"})},
		"one bad of many":   {entry("ok", "10.43.0.2", 80, "tcp"), entry("x", "10.43.0.1", 80, "nope")},
	}
	for name, table := range bad {
		if err := d.HandleSetServices(&api.Request{Services: table}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// None of the refused tables changed anything.
	if list, _ := d.HandleListServices(); len(list.Services) != 2 {
		t.Errorf("a refused table was applied: %+v", list.Services)
	}
	if err := d.HandleSetServices(&api.Request{}); err != nil {
		t.Fatal(err)
	}
	if list, _ := d.HandleListServices(); len(list.Services) != 0 {
		t.Error("an empty table did not clear the engine")
	}
}

func TestTheTableBelongsToKubernetesWhenItIsActive(t *testing.T) {
	d := tapDaemon(t, nil)
	d.k8sCtrl = k8s.NewController(fake.NewSimpleClientset(), "n")
	if err := d.HandleSetServices(&api.Request{Services: []api.ServiceEntry{entry("x", "10.43.0.1", 80, "tcp")}}); err == nil {
		t.Error("the table was written through the API while Kubernetes manages it")
	}
	if st, _ := d.HandleGetStatus(); st.ServiceManager != "kubernetes" {
		t.Errorf("manager %q", st.ServiceManager)
	}
}

func TestTheTableIsSetAndListedThroughTheSocket(t *testing.T) {
	d := tapDaemon(t, nil)
	dir, err := os.MkdirTemp("/tmp", "ps")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "p.sock")
	d.apiServer = api.NewServer(path, d, d, d)
	d.apiServer.SetServiceHandler(d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.apiServer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer d.apiServer.Close()
	d.apiServer.MarkReady()

	client := api.NewClient(path)
	if err := client.SetServices([]api.ServiceEntry{entry("web", "10.43.0.50", 80, "tcp", api.BackendEntry{IP: "10.244.5.9", Port: 8080, Eligible: true})}); err != nil {
		t.Fatal(err)
	}
	resp, err := client.ListServices()
	if err != nil || len(resp.Services) != 1 || resp.Services[0].ID != "web" || resp.ServiceEngine.Services != 1 {
		t.Fatalf("%+v %v", resp, err)
	}
	if err := client.SetServices([]api.ServiceEntry{entry("x", "1.1.1.1", 80, "tcp")}); err == nil {
		t.Error("a bad table was accepted through the socket")
	}
	st, err := client.GetStatus()
	if err != nil || st.ServiceEngine == nil || st.ServiceEngine.Services != 1 {
		t.Errorf("status %+v %v", st, err)
	}
}

func TestNodePortsNeedTheTapGatewayAValidHostAddressAndRange(t *testing.T) {
	with := func(c *Config) {
		c.NodePortSlirpSocket = "/tmp/slirp.sock"
		c.NodePortHostAddress = "192.168.64.3"
		c.NodePortRange = "30000-32767"
	}
	d := tapDaemon(t, with)
	if d.nodePorts == nil || d.nodePortRules == nil {
		t.Fatal("no NodePort reconciler with valid settings")
	}
	st, _ := d.HandleGetStatus()
	if st.NodePorts == nil || st.NodePorts.Desired != 0 || st.NodePorts.Exposed != 0 {
		t.Errorf("status %+v", st.NodePorts)
	}
	without := tapDaemon(t, nil)
	if without.nodePorts != nil {
		t.Error("NodePorts are published without being asked to")
	}
	if st, _ := without.HandleGetStatus(); st.NodePorts != nil {
		t.Error("NodePort status without NodePorts")
	}
	for name, mutate := range map[string]func(*Config){
		"not IPv4":        func(c *Config) { with(c); c.NodePortHostAddress = "fd00::1" },
		"no address":      func(c *Config) { with(c); c.NodePortHostAddress = "" },
		"bad range":       func(c *Config) { with(c); c.NodePortRange = "30000" },
		"reversed range":  func(c *Config) { with(c); c.NodePortRange = "32767-30000" },
		"range too large": func(c *Config) { with(c); c.NodePortRange = "30000-70000" },
		"slirp gateway":   func(c *Config) { with(c); c.GatewayMode = "slirp" },
	} {
		cfg := tapConfig(func(c *Config) { c.DisableKubernetes = true; mutate(c) })
		if _, err := NewPlaidDaemon(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
