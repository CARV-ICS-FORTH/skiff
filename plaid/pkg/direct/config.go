package direct

import (
	"net/netip"
	"time"

	"plaid/pkg/api"
)

// Config is what a supervisor needs to know.
type Config struct {
	// ConnectPorts are the destination ports the container dials that take the direct path:
	// the ports of the Services it connects to (not the targetPorts they lead to).
	ConnectPorts Ports
	// ListenPorts are the ports the container binds that take the direct path: its own
	// ports, the ones its Services' targetPorts name.
	ListenPorts Ports
	// PodIP is the pod's address, which the container is told for its own sockets.
	PodIP netip.Addr
	// Key authenticates the preambles and acks (DeriveKey): the same on every node of the
	// cluster, and not known to the pods.
	Key []byte
	// AckTimeout is how long a client waits for the server's ack before its connect is left
	// to the pod network (DefaultAckTimeout if zero).
	AckTimeout time.Duration
	// PreambleTimeout is how long a server's accept waits for the preamble of a connection
	// to a listener whose container took TCP_DEFER_ACCEPT off it, which holds the container's
	// accept (DefaultPreambleTimeout if zero). Other listeners do not wait.
	PreambleTimeout time.Duration
	// Factory makes the host sockets.
	Factory *FactoryClient
	// Resolver finds the backends of Service and pod addresses.
	Resolver Resolver
	// Report, when set, is given the counters (their totals so far) now and then when they
	// have changed, and the last time, with ended true, when the supervisor ends.
	Report func(counters api.DirectCounters, ended bool)
	// Logf, when set, receives a line for what the supervisor does that is not routine (the
	// container's own output is not the place for them): the first connections, the
	// fallbacks, the counters and where the time of a connect goes.
	Logf func(format string, args ...any)

	// beforeSend, for the tests, is called with the thread and the system call number of a held
	// call just before the supervisor answers it (an accept: between adding the descriptor and
	// the answer), and says whether the call is still held. Nil in a program.
	beforeSend func(tid, nr int, valid func() bool)
	// outOfReach, for the tests, says that the thread cannot be looked into, as a process that has
	// made itself not dumpable cannot be by a supervisor without CAP_SYS_PTRACE in its namespace
	// (a test's supervisor made that namespace, and has it).
	outOfReach func(tid int) bool
}
