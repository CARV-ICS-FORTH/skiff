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

// The host ports of the direct path. A pod's server listens on the host at a port that
// follows from the pod's address and its list of listen ports, so that every supervisor can
// compute where a pod is served and none has to be told (no allocator, nothing to keep). A pod
// owns PodPortsPerPod host ports, from
//
//	first = base + (pod address - first address of the node's pod network) * 16
//
// and each of its listen ports has one of them, its slot (see Ports.Slots): port mod 16, unless
// another port of the list with the same remainder is lower, in which case one of the slots that
// no port of the list falls on. A node's pod network of up to a /24 gives its pods 4096 ports
// from base. A port whose remainder no lower listed port shares is at first + port mod 16, which
// a client computes from the address and the port alone; for the others it needs the server
// pod's list, which plaidd reads from the pods' annotations (APIResolver).
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

// Slots gives each port of the set its slot among a pod's host ports: the lowest port of those
// that are equal modulo PodPortsPerPod has its remainder, and the others, in order, the
// remainders that no port of the set has, in order. Distinct ports have distinct slots. A set
// of more than PodPortsPerPod ports leaves the highest of those that share a remainder without
// one: they are listed in order, and are not served on the host.
func (p Ports) Slots() (slots map[uint16]int, unserved []uint16) {
	sorted := make([]uint16, 0, len(p))
	for port := range p {
		sorted = append(sorted, port)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	slots = make(map[uint16]int, len(p))
	var taken [PodPortsPerPod]bool
	var displaced []uint16
	for _, port := range sorted {
		if r := int(port % PodPortsPerPod); !taken[r] {
			taken[r] = true
			slots[port] = r
		} else {
			displaced = append(displaced, port)
		}
	}
	free := 0
	for _, port := range displaced {
		for free < PodPortsPerPod && taken[free] {
			free++
		}
		if free == PodPortsPerPod {
			unserved = append(unserved, port)
			continue
		}
		taken[free] = true
		slots[port] = free
	}
	return slots, unserved
}

// FirstHostPort is the first of the host ports of a pod on a node whose pods have nodeNet,
// from base. It says false for a pod outside nodeNet and for a pod network larger than a
// /24, which has more pods than there are ports for.
func FirstHostPort(base int, nodeNet netip.Prefix, pod netip.Addr) (uint16, bool) {
	a := pod.Unmap()
	if !nodeNet.IsValid() || !nodeNet.Addr().Is4() || nodeNet.Bits() < 24 || !a.Is4() || !nodeNet.Contains(a) || CheckPortBase(base) != nil {
		return 0, false
	}
	first, addr := nodeNet.Masked().Addr().As4(), a.As4()
	index := int(addr[3]) - int(first[3]) // a /24 or smaller: only the last byte differs
	return uint16(base + index*PodPortsPerPod), true
}

// HostPort is the host port that serves port pod of a pod whose listen ports are listen, on a
// node whose pods have nodeNet, from base (see FirstHostPort). A nil listen is a pod whose list
// is not known: the port is taken for one that shares its remainder with no other, at its
// remainder. A port that is not in a known list has none.
func HostPort(base int, nodeNet netip.Prefix, pod netip.AddrPort, listen Ports) (uint16, bool) {
	first, ok := FirstHostPort(base, nodeNet, pod.Addr())
	if !ok {
		return 0, false
	}
	slot := int(pod.Port() % PodPortsPerPod)
	if listen != nil {
		slots, _ := listen.Slots()
		if slot, ok = slots[pod.Port()]; !ok {
			return 0, false
		}
	}
	return first + uint16(slot), true
}
