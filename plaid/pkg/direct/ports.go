package direct

import (
	"fmt"
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
