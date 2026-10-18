package direct

import (
	"net/netip"
	"time"
)

// Config is what a supervisor needs to know.
type Config struct {
	// Ports are the ports the container marks: connections to a Service port in the set,
	// and binds of a port in it, take the direct path. (One list serves both roles; they are
	// different things in general, the Service's port and the container's.)
	Ports Ports
	// PodIP is the pod's address, which the container is told for its own sockets.
	PodIP netip.Addr
	// Key authenticates the preambles and acks (DeriveKey): the same on every node of the
	// cluster, and not known to the pods.
	Key []byte
	// AckTimeout is how long a client waits for the server's ack before its connect is left
	// to the pod network (DefaultAckTimeout if zero).
	AckTimeout time.Duration
	// Factory makes the host sockets.
	Factory *FactoryClient
	// Resolver finds the backends of Service addresses.
	Resolver Resolver
	// Logf, when set, receives a line for every decision (the container's own output is
	// not the place for them).
	Logf func(format string, args ...any)
}
