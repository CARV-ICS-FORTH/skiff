package direct

import (
	"net/netip"
	"time"

	"plaid/pkg/api"
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
	// Report, when set, is given the counters (their totals so far) now and then when they
	// have changed, and the last time, with ended true, when the supervisor ends.
	Report func(counters api.DirectCounters, ended bool)
	// Logf, when set, receives a line for what the supervisor does that is not routine (the
	// container's own output is not the place for them): the first connections, the
	// fallbacks, the counters and where the time of a connect goes.
	Logf func(format string, args ...any)
}
