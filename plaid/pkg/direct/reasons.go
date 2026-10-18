package direct

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// reasonLogEvery is how often a reason that keeps happening is worth a line in the log.
const reasonLogEvery = 10 * time.Second

// Reasons counts what the supervisor did not do, by why: a direct connection that was left
// to the overlay, a connection to a listener that was turned away.
type Reasons struct {
	mu     sync.Mutex
	counts map[string]uint64
	logged map[string]time.Time
}

// Add counts one for the reason, and says whether it should be logged: the first time, and
// then at most every reasonLogEvery, so that a client that keeps failing does not fill the log.
func (r *Reasons) Add(reason string) (total uint64, log bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts, r.logged = make(map[string]uint64), make(map[string]time.Time)
	}
	r.counts[reason]++
	now := time.Now()
	if last, ok := r.logged[reason]; ok && now.Sub(last) < reasonLogEvery {
		return r.counts[reason], false
	}
	r.logged[reason] = now
	return r.counts[reason], true
}

// Snapshot is the counts so far.
func (r *Reasons) Snapshot() map[string]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]uint64, len(r.counts))
	for k, v := range r.counts {
		out[k] = v
	}
	return out
}

// String is the counts as "reason=n" words, in the order of the reasons.
func (r *Reasons) String() string {
	counts := r.Snapshot()
	words := make([]string, 0, len(counts))
	for reason, n := range counts {
		words = append(words, fmt.Sprintf("%s=%d", reason, n))
	}
	sort.Strings(words)
	return strings.Join(words, " ")
}
