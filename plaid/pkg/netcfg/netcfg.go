// Package netcfg holds the network settings the Plaid programs must agree on: the
// defaults of a standalone deployment and the way each setting is looked up (the
// program's own flag, then the environment, then the default). plaidd, plaid,
// plaidtainer and plaidctl all take their values from here, so a setting cannot
// have one meaning in one tool and another in the next.
//
// Inside a Skiff cluster nothing here is a default in effect: the deployment
// configuration (scripts/skiff.sh) states every value explicitly to plaidd,
// and the other tools learn them from plaidd.
package netcfg

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

const (
	// DefaultSocketPath is where plaidd serves its API unless told otherwise.
	DefaultSocketPath = "/run/plaid/plaidd.sock"

	// DefaultMTU is the standalone pod MTU: a 1500 byte link less the 50 bytes of
	// VXLAN encapsulation.
	DefaultMTU = 1450
	MinMTU     = 576
	MaxMTU     = 65535

	// Standalone defaults of the cluster and Service networks.
	DefaultClusterCIDR = "10.244.0.0/16"
	DefaultServiceCIDR = "10.43.0.0/16"

	// EnvSocket and EnvMTU are the variables every Plaid tool reads.
	EnvSocket = "PLAID_SOCKET_PATH"
	EnvMTU    = "PLAID_MTU"
)

// SocketPath is the API socket in effect: the explicit setting, else the
// environment's, else the default.
func SocketPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv(EnvSocket); env != "" {
		return env
	}
	return DefaultSocketPath
}

// MTUFromEnv returns the MTU the environment asks for, or 0 when it asks for none (or
// for something that is not a positive number).
func MTUFromEnv() int {
	if env := os.Getenv(EnvMTU); env != "" {
		if mtu, err := strconv.Atoi(env); err == nil && mtu > 0 {
			return mtu
		}
	}
	return 0
}

// EffectiveMTU is the MTU in effect for a setting made explicitly (0: none): the
// explicit one, else the environment's, else the default.
func EffectiveMTU(explicit int) int {
	if explicit > 0 {
		return explicit
	}
	if mtu := MTUFromEnv(); mtu > 0 {
		return mtu
	}
	return DefaultMTU
}

// ValidateMTU refuses an MTU no interface could have.
func ValidateMTU(mtu int) error {
	if mtu < MinMTU || mtu > MaxMTU {
		return fmt.Errorf("invalid MTU %d: must be between %d and %d", mtu, MinMTU, MaxMTU)
	}
	return nil
}

// ValidateResolver checks the resolver plaidd advertises against the Service
// network: inside an integrated cluster, the resolver is the cluster's DNS
// Service, an address of the Service network.
func ValidateResolver(resolver string, service *net.IPNet) error {
	ip := net.ParseIP(resolver)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("invalid resolver %q: not an IPv4 address", resolver)
	}
	if service != nil && !service.Contains(ip) {
		return fmt.Errorf("resolver %s is outside the Service CIDR %s", resolver, service)
	}
	return nil
}

// StandaloneResolver is the resolver a standalone container gets from slirp when
// nothing says otherwise: the third address of the gateway's network (10.0.2.3 for
// a gateway of 10.0.2.2). It applies to slirp mode only; in a cluster the resolver
// is always stated.
func StandaloneResolver(gateway string) string {
	ip := net.ParseIP(gateway).To4()
	if ip == nil {
		return ""
	}
	dns := make(net.IP, len(ip))
	copy(dns, ip)
	dns[3] = 3
	return dns.String()
}
