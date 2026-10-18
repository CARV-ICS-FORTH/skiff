package direct

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// Ports is a set of TCP ports.
type Ports map[uint16]struct{}

// ParsePorts reads a list of ports separated by commas ("5201,5203"); the empty string is
// the empty set.
func ParsePorts(s string) (Ports, error) {
	ports := Ports{}
	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.ParseUint(field, 10, 16)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("invalid port %q", field)
		}
		ports[uint16(n)] = struct{}{}
	}
	return ports, nil
}

// Has says whether the port is in the set.
func (p Ports) Has(port uint16) bool {
	_, ok := p[port]
	return ok
}

// String is the set as ParsePorts reads it.
func (p Ports) String() string {
	list := make([]int, 0, len(p))
	for port := range p {
		list = append(list, int(port))
	}
	sort.Ints(list)
	parts := make([]string, len(list))
	for i, port := range list {
		parts[i] = strconv.Itoa(port)
	}
	return strings.Join(parts, ",")
}

// The host ports of the direct path. A pod's server listens on the host at a port that
// follows from the pod's address and its own port, so that every supervisor can compute
// where a pod is served and none has to be told (no allocator, nothing to keep):
//
//	host port = base + (pod address - first address of the node's pod network) * 16 + pod port mod 16
//
// A node's pod network of up to a /24 gives its pods 4096 ports from base. Two ports of one
// pod that are equal modulo 16 get the same host port; the second bind finds it in use and
// stays a socket of the pod network.
const (
	// DefaultPortBase is where the host ports start: below the ephemeral range of Linux
	// (32768 and up), where the nodes of the test cluster have nothing listening.
	DefaultPortBase = 20000
	// PodPortsPerPod is how many host ports a pod's address owns; HostPorts how many a node's.
	PodPortsPerPod = 16
	HostPorts      = 256 * PodPortsPerPod
)

// CheckPortBase says whether the host ports from base fit in the port numbers.
func CheckPortBase(base int) error {
	if base < 1024 || base+HostPorts > 65536 {
		return fmt.Errorf("the port base %d must be between 1024 and %d", base, 65536-HostPorts)
	}
	return nil
}

// HostPort is the host port that serves port pod of a pod on a node whose pods have
// nodeNet, from base. It says false for a pod outside nodeNet and for a pod network larger
// than a /24, which has more pods than there are ports for.
func HostPort(base int, nodeNet netip.Prefix, pod netip.AddrPort) (uint16, bool) {
	a := pod.Addr().Unmap()
	if !nodeNet.IsValid() || !nodeNet.Addr().Is4() || nodeNet.Bits() < 24 || !a.Is4() || !nodeNet.Contains(a) || CheckPortBase(base) != nil {
		return 0, false
	}
	first, addr := nodeNet.Masked().Addr().As4(), a.As4()
	index := int(addr[3]) - int(first[3]) // a /24 or smaller: only the last byte differs
	return uint16(base + index*PodPortsPerPod + int(pod.Port())%PodPortsPerPod), true
}
