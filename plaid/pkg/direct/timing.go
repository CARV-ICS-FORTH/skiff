package direct

import (
	"fmt"
	"math/bits"
	"strings"
	"sync/atomic"
	"time"
)

// Where the time of a connect or an accept goes: the supervisor stamps a call at the phases
// of its decision, and keeps for each phase a histogram of how long it took. A stamp is a
// reading of the monotonic clock and an atomic add, a microsecond for a whole connect.

// phase is a part of a held call that is timed: the time from the previous stamp of the
// call to the stamp that names it.
type phase int

const (
	// connect
	phDispatch phase = iota // the call was received, and a goroutine runs for it
	phResolve               // the destination is read from the container and the Service is resolved
	phInspect               // the container's socket is duplicated and looked at
	phEpoll                 // the look at the container's epoll sets
	phState                 // the socket is checked to be unused
	phFactory               // a host socket from the factory
	phOptions               // the options are copied
	phDial                  // the TCP connection to the backend's node
	phPreamble              // the preamble is made and written
	phAck                   // the wait for the server's ack
	phAddfd                 // the host socket is put in the container's table
	phReply                 // the table of swapped sockets, the closes and the answer
	phConnect               // all of it, from the call's receipt

	// accept, from the call's receipt, then from the connection that was taken
	phAccept    // the wait for a connection (the call is held before there is one)
	phReadPre   // the preamble is read
	phVerify    // it is parsed and its MAC checked
	phChecks    // the time, peer, backend and client checks
	phAckWrite  // the client is still there and the ack is written
	phWriteAddr // the client's address is written to the container
	phAcceptFD  // the connection is put in the container's table and the call answered
	phAdmit     // all of it, from the connection that was taken
	numPhases
)

var phaseNames = [numPhases]string{
	phDispatch: "dispatch", phResolve: "resolve", phInspect: "inspect", phEpoll: "epoll", phState: "state",
	phFactory: "factory", phOptions: "options", phDial: "dial", phPreamble: "preamble", phAck: "ack",
	phAddfd: "addfd", phReply: "reply", phConnect: "total",
	phAccept: "wait", phReadPre: "read-preamble", phVerify: "verify", phChecks: "checks", phAckWrite: "write-ack",
	phWriteAddr: "addr", phAcceptFD: "addfd", phAdmit: "total",
}

// A histogram's buckets are eight to each power of two of the nanoseconds: a percentile
// is good to about 6 %.
const histBuckets = 8 + 61*8

type histogram struct {
	n       atomic.Uint64
	buckets [histBuckets]atomic.Uint64
}

func bucketOf(ns int64) int {
	if ns < 8 {
		return int(max(ns, 0))
	}
	e := bits.Len64(uint64(ns)) - 4
	return 8 + e*8 + int(uint64(ns)>>e)&7
}

// bucketLow is the smallest value of a bucket.
func bucketLow(b int) int64 {
	if b < 8 {
		return int64(b)
	}
	e := (b - 8) / 8
	return int64(8+(b-8)%8) << e
}

func (h *histogram) add(d time.Duration) {
	h.n.Add(1)
	h.buckets[bucketOf(int64(d))].Add(1)
}

// percentile is the p-th percentile (0 to 1) of what was added, as the low edge of its bucket.
func (h *histogram) percentile(p float64) time.Duration {
	n := h.n.Load()
	if n == 0 {
		return 0
	}
	want := uint64(float64(n-1)*p) + 1
	var seen uint64
	for b := range h.buckets {
		if seen += h.buckets[b].Load(); seen >= want {
			return time.Duration(bucketLow(b))
		}
	}
	return 0
}

// Timings holds the histograms of the phases.
type Timings struct {
	h [numPhases]histogram
}

// timeline stamps one held call. A call that is decided in the supervisor and gives the
// container its socket is the one worth timing: the stamps are kept with the call and
// recorded when it ends that way (finish); a call that is let through, or that fails,
// leaves no trace. The zero value, and nil, stamp nothing.
type timeline struct {
	t    *Timings
	t0   time.Time
	last time.Time
	d    [numPhases]time.Duration
}

func (t *Timings) begin() *timeline {
	now := time.Now()
	return &timeline{t: t, t0: now, last: now}
}

// mark ends phase p: it started at the previous mark.
func (tl *timeline) mark(p phase) {
	if tl == nil {
		return
	}
	now := time.Now()
	tl.d[p] = max(now.Sub(tl.last), 1)
	tl.last = now
}

// restart starts counting from now: the time the call has waited is not a phase.
func (tl *timeline) restart() {
	if tl != nil {
		tl.t0 = time.Now()
		tl.last = tl.t0
	}
}

// finish records the phases that were marked, and the whole call from the start (or the
// last restart) as phase p.
func (tl *timeline) finish(p phase) {
	if tl == nil {
		return
	}
	tl.d[p] = time.Since(tl.t0)
	for ph, d := range tl.d {
		if d > 0 {
			tl.t.h[ph].add(d)
		}
	}
	tl.d = [numPhases]time.Duration{}
}

// String is the phases of connect and of accept that were timed, a line for each: the
// number of calls, then each phase as its median and 90th percentile in microseconds.
func (t *Timings) String() string {
	line := func(name string, from, to phase) string {
		if t.h[to].n.Load() == 0 {
			return ""
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s n=%d, p50/p90 us:", name, t.h[to].n.Load())
		for p := from; p <= to; p++ {
			if t.h[p].n.Load() > 0 {
				fmt.Fprintf(&b, " %s %d/%d", phaseNames[p], t.h[p].percentile(0.5).Microseconds(), t.h[p].percentile(0.9).Microseconds())
			}
		}
		return b.String()
	}
	return strings.TrimSpace(line("connect", phDispatch, phConnect) + "\n" + line("accept", phAccept, phAdmit))
}
