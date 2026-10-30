package main

import (
	"fmt"
	"sort"
	"sync"
	"syscall"

	"plaid/pkg/api"
)

// What the supervisors of the direct path (plaid/pkg/direct, which plaidtainer runs a
// container under) tell plaidd of what they have done, so that `plaidctl status` shows
// the direct path of the node: a supervisor pushes its counters when they have changed, and
// once more when it ends.

const (
	// maxDirectSupervisors, maxDirectName and maxDirectReasons bound what a report, from
	// anyone who can reach the API socket, can make plaidd keep.
	maxDirectSupervisors = 4096
	maxDirectName        = 256
	maxDirectReasons     = 64
)

// directBook is the supervisors' last reports. The zero value is ready.
type directBook struct {
	mu      sync.Mutex
	running map[string]api.DirectContainer // by name and process
	ended   int
	done    api.DirectCounters // the counters of those that ended
	// alive says whether a process is there; syscall.Kill(pid, 0) if not set.
	alive func(pid int) bool
}

func (b *directBook) isAlive(pid int) bool {
	if b.alive != nil {
		return b.alive(pid)
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// retire moves a supervisor that has ended, with its last counters, from those that run to
// those that ended. The caller holds the lock.
func (b *directBook) retire(key string, counters api.DirectCounters) {
	delete(b.running, key)
	b.ended++
	b.done.Add(counters)
}

// report keeps a supervisor's counters (they are its totals, so a report replaces the
// last), or, if it has ended, adds them to those of the supervisors that did.
func (b *directBook) report(r *api.DirectReport) error {
	if r.Supervisor == "" || len(r.Supervisor) > maxDirectName || r.PID <= 0 {
		return fmt.Errorf("a report of the direct path names its supervisor and its process")
	}
	if len(r.Counters.Fallbacks) > maxDirectReasons || len(r.Counters.Rejected) > maxDirectReasons {
		return fmt.Errorf("a report of the direct path has at most %d reasons of each kind", maxDirectReasons)
	}
	for _, counts := range []map[string]uint64{r.Counters.Fallbacks, r.Counters.Rejected} {
		for reason := range counts {
			if len(reason) > maxDirectName {
				return fmt.Errorf("a reason of a report of the direct path is longer than %d", maxDirectName)
			}
		}
	}
	key := fmt.Sprintf("%s#%d", r.Supervisor, r.PID)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, known := b.running[key]; !known && !r.Ended && len(b.running) >= maxDirectSupervisors {
		return fmt.Errorf("plaidd keeps the reports of at most %d supervisors", maxDirectSupervisors)
	}
	if r.Ended {
		b.retire(key, r.Counters)
		return nil
	}
	if b.running == nil {
		b.running = make(map[string]api.DirectContainer)
	}
	b.running[key] = api.DirectContainer{Name: r.Supervisor, PID: r.PID, Counters: r.Counters}
	return nil
}

// status sums what has been reported. A supervisor whose process is gone without its last
// report (it was killed) has ended: what it last reported is kept with the others'. It is
// nil if no supervisor has ever reported.
func (b *directBook) status() *api.DirectStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, c := range b.running {
		if !b.isAlive(c.PID) {
			b.retire(key, c.Counters)
		}
	}
	if len(b.running) == 0 && b.ended == 0 {
		return nil
	}
	st := &api.DirectStatus{Running: len(b.running), Ended: b.ended}
	st.Total.Add(b.done)
	for _, c := range b.running {
		st.Total.Add(c.Counters)
		st.Containers = append(st.Containers, c)
	}
	sort.Slice(st.Containers, func(i, j int) bool {
		if st.Containers[i].Name != st.Containers[j].Name {
			return st.Containers[i].Name < st.Containers[j].Name
		}
		return st.Containers[i].PID < st.Containers[j].PID
	})
	return st
}

func (d *PlaidDaemon) HandleReportDirect(r *api.DirectReport) error { return d.direct.report(r) }
