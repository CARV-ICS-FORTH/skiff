package direct

import "net/netip"

// Config is what a supervisor needs to know.
type Config struct {
	// Ports are the ports the container marks: connections to a Service port in the set,
	// and binds of a port in it, take the direct path. (One list serves both roles; they are
	// different things in general, the Service's port and the container's.)
	Ports Ports
	// PodIP is the pod's address, which the container is told for its own sockets.
	PodIP netip.Addr
	// Factory makes the host sockets.
	Factory *FactoryClient
	// Resolver finds the backends of Service addresses.
	Resolver Resolver
	// Logf, when set, receives a line for every decision (the container's own output is
	// not the place for them).
	Logf func(format string, args ...any)
}
