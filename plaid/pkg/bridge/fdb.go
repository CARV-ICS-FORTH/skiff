package bridge

import (
	"net"
	"sync"
	"time"
)

// fdbEntry is an entry in the Forwarding Database. Its fields are read under the FDB's
// read lock and written under its write lock.
type fdbEntry struct {
	mac     net.HardwareAddr // a copy: the caller's may be a slice of a frame
	ep      Endpoint
	updated int64 // the FDB's clock when the entry was last refreshed
	static  bool
}

const DefaultMaxFDBEntries = 1024

// fdbRefresh is how stale the time of an entry that Learn sees again with the same endpoint
// may get before Learn stores the time anew: Learn is called for every frame, and storing
// takes the write lock. An entry is thus aged, and the oldest is evicted, to within this.
const fdbRefresh = time.Second

var fdbStart = time.Now()

// fdbClock is the FDB's clock, in nanoseconds from some start (one call of the runtime's
// monotonic clock, where time.Now makes two).
func fdbClock() int64 { return int64(time.Since(fdbStart)) }

// FDB is a concurrent-safe MAC forwarding database. Its table is keyed by the 6 bytes of a
// MAC address, so that looking one up does not allocate; an address of another length,
// which no Ethernet frame carries, is never stored or found.
type FDB struct {
	mu         sync.RWMutex
	entries    map[[6]byte]*fdbEntry
	ttl        time.Duration
	refresh    int64        // a refresh younger than this is not stored again: fdbRefresh, or less for a short ttl
	now        func() int64 // a variable so that tests can move it
	maxEntries int
}

// NewFDB creates a new FDB with the given entry expiration TTL (e.g. 5 minutes).
func NewFDB(ttl time.Duration) *FDB {
	refresh := fdbRefresh
	if ttl > 0 {
		refresh = min(refresh, ttl/4) // an entry in use must not age
	}
	return &FDB{
		entries:    make(map[[6]byte]*fdbEntry),
		ttl:        ttl,
		refresh:    int64(refresh),
		now:        fdbClock,
		maxEntries: DefaultMaxFDBEntries,
	}
}

// SetMaxEntries configures the maximum number of entries allowed in the FDB.
func (f *FDB) SetMaxEntries(max int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maxEntries = max
}

// get returns the entry for mac, or nil. The caller holds the lock.
func (f *FDB) get(mac net.HardwareAddr) *fdbEntry {
	if len(mac) != 6 {
		return nil
	}
	return f.entries[[6]byte(mac)]
}

// put stores e for e.mac. The caller holds the write lock.
func (f *FDB) put(e *fdbEntry) {
	if len(e.mac) == 6 {
		f.entries[[6]byte(e.mac)] = e
	}
}

// remove deletes the entry for mac. The caller holds the write lock.
func (f *FDB) remove(mac net.HardwareAddr) {
	if len(mac) == 6 {
		delete(f.entries, [6]byte(mac))
	}
}

// each calls fn for every entry; fn may remove the entry it is given.
func (f *FDB) each(fn func(e *fdbEntry)) {
	for _, e := range f.entries {
		fn(e)
	}
}

// expired reports whether e has aged out at time now.
func (f *FDB) expired(e *fdbEntry, now int64) bool {
	return !e.static && f.ttl > 0 && now-e.updated > int64(f.ttl)
}

// EvictExpired removes all non-static entries that have exceeded TTL. Returns number of evicted entries.
func (f *FDB) EvictExpired() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.evictExpiredLocked()
}

func (f *FDB) evictExpiredLocked() int {
	now, evicted := f.now(), 0
	f.each(func(e *fdbEntry) {
		if f.expired(e, now) {
			f.remove(e.mac)
			evicted++
		}
	})
	return evicted
}

// Len returns the current number of entries in the FDB.
func (f *FDB) Len() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.entries)
}

// Learn registers or refreshes a MAC address mapping to an endpoint. It is called for
// every frame, and the entry almost always exists, with the same endpoint and a time
// that is recent enough: then it only reads, under the read lock, so that the frames of
// many streams do not wait for each other here. A new entry, a changed endpoint and a time
// that has become stale take the write lock.
func (f *FDB) Learn(mac net.HardwareAddr, ep Endpoint) {
	f.mu.RLock()
	e := f.get(mac)
	known := e != nil && e.ep == ep && (e.static || f.now()-e.updated < f.refresh)
	f.mu.RUnlock()
	if known {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if e := f.get(mac); e != nil {
		e.ep = ep
		e.updated = now
		return
	}

	// If table reached max capacity, evict expired entries first
	if f.maxEntries > 0 && len(f.entries) >= f.maxEntries {
		f.evictExpiredLocked()
	}

	// If still at capacity, evict oldest non-static entry
	if f.maxEntries > 0 && len(f.entries) >= f.maxEntries {
		var oldest *fdbEntry
		f.each(func(e *fdbEntry) {
			if !e.static && (oldest == nil || e.updated < oldest.updated) {
				oldest = e
			}
		})
		if oldest != nil {
			f.remove(oldest.mac)
		}
	}

	f.put(&fdbEntry{mac: append(net.HardwareAddr(nil), mac...), ep: ep, updated: now})
}

// SetStatic sets a static (non-aging) MAC mapping.
func (f *FDB) SetStatic(mac net.HardwareAddr, ep Endpoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.put(&fdbEntry{mac: append(net.HardwareAddr(nil), mac...), ep: ep, updated: f.now(), static: true})
}

// Lookup finds the endpoint associated with a MAC address. Returns nil if not found or expired.
func (f *FDB) Lookup(mac net.HardwareAddr) Endpoint {
	f.mu.RLock()
	defer f.mu.RUnlock()

	e := f.get(mac)
	if e == nil || f.expired(e, f.now()) {
		return nil
	}
	return e.ep
}

// Delete removes a MAC entry from the FDB.
func (f *FDB) Delete(mac net.HardwareAddr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remove(mac)
}

// DeleteByEndpoint removes all entries pointing to a given endpoint ID.
func (f *FDB) DeleteByEndpoint(epID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.each(func(e *fdbEntry) {
		if e.ep != nil && e.ep.ID() == epID {
			f.remove(e.mac)
		}
	})
}
