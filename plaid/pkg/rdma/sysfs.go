package rdma

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Port names a port of an RDMA device.
type Port struct {
	Device string // as the kernel names it, e.g. mlx5_0
	Num    int    // from 1
}

func (p Port) String() string { return fmt.Sprintf("%s:%d", p.Device, p.Num) }

// ParsePort reads "DEVICE" or "DEVICE:PORT" (port 1 when not given).
func ParsePort(s string) (Port, error) {
	dev, num, found := strings.Cut(s, ":")
	if dev == "" || strings.ContainsAny(dev, "/ ") {
		return Port{}, fmt.Errorf("%q is not an RDMA device (DEVICE or DEVICE:PORT, e.g. mlx5_0 or mlx5_0:1)", s)
	}
	p := Port{Device: dev, Num: 1}
	if found {
		n, err := strconv.Atoi(num)
		if err != nil || n < 1 || n > 255 {
			return Port{}, fmt.Errorf("%q: the port of an RDMA device is a number from 1", s)
		}
		p.Num = n
	}
	return p, nil
}

// SysRoot is where sysfs is; tests point it elsewhere.
const SysRoot = "/sys"

// ErrNoPort is what Choose returns when the host has no port to use.
var ErrNoPort = errors.New("no active InfiniBand port")

// Choose picks the port the overlay uses: the one behind the network interface iface (the
// cluster's network, which on an InfiniBand cluster is usually IPoIB), if it is an active
// InfiniBand port; otherwise the first active InfiniBand port, by device name and port.
// It reads sysfs under root (SysRoot), which a Skiff bubble sees as the host's. why says how
// the port was chosen, for the startup output.
func Choose(root, iface string) (p Port, why string, err error) {
	if iface != "" {
		if ip, ierr := InterfacePort(root, iface); ierr == nil {
			ll, st := portLinkLayer(root, ip), portState(root, ip)
			if ll == LinkInfiniBand && st == PortActive {
				return ip, "the port of " + iface, nil
			}
		}
	}
	ports, err := ActivePorts(root, LinkInfiniBand)
	if err != nil {
		return Port{}, "", err
	}
	if len(ports) == 0 {
		return Port{}, "", ErrNoPort
	}
	why = "the first active InfiniBand port"
	if iface != "" {
		why += " (" + iface + " is not on one)"
	}
	return ports[0], why, nil
}

// InterfacePort is the RDMA port behind a network interface: an IPoIB interface, or the
// Ethernet interface of a RoCE port, links to its device in sysfs, and its dev_port is the
// port's number less one (as the IPoIB driver sets it; 0 when the device has one port).
func InterfacePort(root, iface string) (Port, error) {
	if iface == "" || strings.ContainsAny(iface, "/") {
		return Port{}, fmt.Errorf("no interface %q", iface)
	}
	devs, err := os.ReadDir(filepath.Join(root, "class/net", iface, "device/infiniband"))
	if err != nil || len(devs) == 0 {
		return Port{}, fmt.Errorf("%s has no RDMA device", iface)
	}
	p := Port{Device: devs[0].Name(), Num: 1}
	if n, err := strconv.Atoi(readSys(filepath.Join(root, "class/net", iface, "dev_port"))); err == nil && n >= 0 {
		p.Num = n + 1
	}
	if _, err := os.Stat(filepath.Join(root, "class/infiniband", p.Device, "ports", strconv.Itoa(p.Num))); err != nil {
		return Port{}, fmt.Errorf("%s: %s has no port %d", iface, p.Device, p.Num)
	}
	return p, nil
}

// ActivePorts lists the active ports of a link layer, by device name and port.
func ActivePorts(root string, ll LinkLayer) ([]Port, error) {
	devs, err := os.ReadDir(filepath.Join(root, "class/infiniband"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Port
	for _, d := range devs {
		ports, err := os.ReadDir(filepath.Join(root, "class/infiniband", d.Name(), "ports"))
		if err != nil {
			continue
		}
		for _, pe := range ports {
			n, err := strconv.Atoi(pe.Name())
			if err != nil {
				continue
			}
			p := Port{Device: d.Name(), Num: n}
			if portLinkLayer(root, p) == ll && portState(root, p) == PortActive {
				out = append(out, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Device != out[j].Device {
			return out[i].Device < out[j].Device
		}
		return out[i].Num < out[j].Num
	})
	return out, nil
}

// PortRate is a port's data rate in Gbit/s from sysfs ("56 Gb/sec (4X FDR)"), 0 if unknown.
func PortRate(root string, p Port) int {
	f := strings.Fields(readSys(filepath.Join(root, "class/infiniband", p.Device, "ports", strconv.Itoa(p.Num), "rate")))
	if len(f) == 0 {
		return 0
	}
	r, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return int(r)
}

// portLinkLayer reads a port's link layer; a port that does not say is InfiniBand (older
// kernels have no link_layer file, and only InfiniBand ports then).
func portLinkLayer(root string, p Port) LinkLayer {
	dir := filepath.Join(root, "class/infiniband", p.Device, "ports", strconv.Itoa(p.Num))
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	switch readSys(filepath.Join(dir, "link_layer")) {
	case "Ethernet":
		return LinkEthernet
	default:
		return LinkInfiniBand
	}
}

var stateRE = regexp.MustCompile(`^(\d+):`)

// portState reads a port's state ("4: ACTIVE"), 0 if it cannot.
func portState(root string, p Port) PortState {
	m := stateRE.FindStringSubmatch(readSys(filepath.Join(root, "class/infiniband", p.Device, "ports", strconv.Itoa(p.Num), "state")))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return PortState(n)
}

func readSys(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
