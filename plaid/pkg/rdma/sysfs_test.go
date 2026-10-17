package rdma

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// sysTree makes a sysfs of RDMA devices and network interfaces under a temporary root.
type sysTree struct {
	t    *testing.T
	root string
}

func newSysTree(t *testing.T) *sysTree { return &sysTree{t: t, root: t.TempDir()} }

func (s *sysTree) write(rel, content string) {
	s.t.Helper()
	p := filepath.Join(s.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// port adds a port of a device: its state ("4: ACTIVE") and link layer ("" for none).
func (s *sysTree) port(dev string, num int, state, ll string) {
	dir := filepath.Join("class/infiniband", dev, "ports", itoa(num))
	s.write(filepath.Join(dir, "state"), state)
	s.write(filepath.Join(dir, "rate"), "56 Gb/sec (4X FDR)")
	if ll != "" {
		s.write(filepath.Join(dir, "link_layer"), ll)
	}
}

// netdev adds a network interface of a device, as the IPoIB driver links them.
func (s *sysTree) netdev(iface, dev string, devPort int) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Join(s.root, "class/net", iface, "device/infiniband", dev), 0o755); err != nil {
		s.t.Fatal(err)
	}
	s.write(filepath.Join("class/net", iface, "dev_port"), itoa(devPort))
}

func itoa(n int) string { return string(rune('0' + n)) }

func TestParsePort(t *testing.T) {
	for in, want := range map[string]Port{"mlx5_0": {"mlx5_0", 1}, "mlx5_1:2": {"mlx5_1", 2}} {
		got, err := ParsePort(in)
		if err != nil || got != want {
			t.Errorf("ParsePort(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", ":1", "mlx5_0:", "mlx5_0:0", "mlx5_0:x", "a/b", "mlx5_0:300"} {
		if _, err := ParsePort(in); err == nil {
			t.Errorf("ParsePort(%q) accepted", in)
		}
	}
}

// The tie nodes' layout: a ConnectX-4 whose first function is InfiniBand (IPoIB on it) and
// whose second is Ethernet.
func tieNode(t *testing.T) *sysTree {
	s := newSysTree(t)
	s.port("mlx5_0", 1, "4: ACTIVE", "InfiniBand")
	s.port("mlx5_1", 1, "4: ACTIVE", "Ethernet")
	s.netdev("ibp97s0f0", "mlx5_0", 0)
	s.netdev("enp37s0f1", "mlx5_1", 0)
	return s
}

func TestChooseThePortOfTheClusterInterface(t *testing.T) {
	s := tieNode(t)
	p, why, err := Choose(s.root, "ibp97s0f0")
	if err != nil || p != (Port{"mlx5_0", 1}) || why != "the port of ibp97s0f0" {
		t.Fatalf("Choose = %v %q %v", p, why, err)
	}
	if r := PortRate(s.root, p); r != 56 {
		t.Errorf("rate %d, want 56", r)
	}
}

func TestChooseTheFirstActiveIBPortOtherwise(t *testing.T) {
	s := tieNode(t)
	// The cluster's network on the Ethernet function (RoCE is not taken), or on an
	// interface with no RDMA device, or none named: the InfiniBand port.
	for _, iface := range []string{"enp37s0f1", "eno1", ""} {
		p, why, err := Choose(s.root, iface)
		if err != nil || p != (Port{"mlx5_0", 1}) {
			t.Errorf("Choose(%q) = %v %q %v", iface, p, why, err)
		}
	}
	// Ports are taken in order, active ones only, of the InfiniBand link layer; a port
	// without link_layer (older kernels) is InfiniBand.
	s2 := newSysTree(t)
	s2.port("mlx5_2", 1, "4: ACTIVE", "")
	s2.port("mlx5_1", 1, "1: DOWN", "InfiniBand")
	s2.port("mlx5_0", 1, "4: ACTIVE", "Ethernet")
	ports, err := ActivePorts(s2.root, LinkInfiniBand)
	if err != nil || len(ports) != 1 || ports[0] != (Port{"mlx5_2", 1}) {
		t.Fatalf("ActivePorts = %v %v", ports, err)
	}
}

func TestChooseNothing(t *testing.T) {
	// No InfiniBand at all (the VMs), or none active.
	s := newSysTree(t)
	if _, _, err := Choose(s.root, "eth0"); !errors.Is(err, ErrNoPort) {
		t.Errorf("no /sys/class/infiniband: %v", err)
	}
	s.port("mlx5_0", 1, "1: DOWN", "InfiniBand")
	s.netdev("ib0", "mlx5_0", 0)
	if _, _, err := Choose(s.root, "ib0"); !errors.Is(err, ErrNoPort) {
		t.Errorf("a port that is down: %v", err)
	}
}

func TestInterfacePort(t *testing.T) {
	// A two-port HCA's second port: dev_port is the port less one.
	s := newSysTree(t)
	s.port("mlx4_0", 1, "4: ACTIVE", "InfiniBand")
	s.port("mlx4_0", 2, "4: ACTIVE", "InfiniBand")
	s.netdev("ib1", "mlx4_0", 1)
	if p, err := InterfacePort(s.root, "ib1"); err != nil || p != (Port{"mlx4_0", 2}) {
		t.Errorf("InterfacePort = %v %v", p, err)
	}
	s.netdev("ib9", "mlx4_0", 8)
	if _, err := InterfacePort(s.root, "ib9"); err == nil {
		t.Error("a port the device does not have was accepted")
	}
	if _, err := InterfacePort(s.root, "../x"); err == nil {
		t.Error("a path was accepted as an interface")
	}
}
