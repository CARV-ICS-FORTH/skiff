package bridge

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFDBEvictExpired(t *testing.T) {
	ttl := 50 * time.Millisecond
	fdb := NewFDB(ttl)

	mac1, _ := net.ParseMAC("02:00:00:00:00:01")
	mac2, _ := net.ParseMAC("02:00:00:00:00:02")
	staticMAC, _ := net.ParseMAC("02:00:00:00:00:03")

	ep1 := newMockEndpoint("ep1", "tap1", "10.244.1.2", mac1.String())
	ep2 := newMockEndpoint("ep2", "tap2", "10.244.1.3", mac2.String())
	epStatic := newMockEndpoint("ep3", "tap3", "10.244.1.4", staticMAC.String())

	fdb.Learn(mac1, ep1)
	fdb.SetStatic(staticMAC, epStatic)

	// Immediate lookup succeeds
	if ep := fdb.Lookup(mac1); ep == nil || ep.ID() != "ep1" {
		t.Fatalf("expected to find ep1 immediately")
	}
	if ep := fdb.Lookup(staticMAC); ep == nil || ep.ID() != "ep3" {
		t.Fatalf("expected to find static ep3 immediately")
	}

	time.Sleep(70 * time.Millisecond)

	// Learn mac2 after sleep
	fdb.Learn(mac2, ep2)

	// Evict expired entries: mac1 should be evicted, mac2 and staticMAC should remain
	evicted := fdb.EvictExpired()
	if evicted != 1 {
		t.Errorf("expected 1 entry to be evicted, got %d", evicted)
	}

	if ep := fdb.Lookup(mac1); ep != nil {
		t.Errorf("expected mac1 to be evicted, but lookup returned %v", ep)
	}
	if ep := fdb.Lookup(mac2); ep == nil || ep.ID() != "ep2" {
		t.Errorf("expected mac2 to be found, got %v", ep)
	}
	if ep := fdb.Lookup(staticMAC); ep == nil || ep.ID() != "ep3" {
		t.Errorf("expected staticMAC to remain after TTL expiry")
	}
}

func TestFDBMaxEntriesBound(t *testing.T) {
	fdb := NewFDB(time.Hour)
	fdb.SetMaxEntries(3)

	for i := 1; i <= 5; i++ {
		mac, _ := net.ParseMAC(fmt.Sprintf("02:00:00:00:00:0%d", i))
		ep := newMockEndpoint(fmt.Sprintf("ep%d", i), fmt.Sprintf("tap%d", i), fmt.Sprintf("10.244.1.%d", i+1), mac.String())
		fdb.Learn(mac, ep)
		time.Sleep(2 * time.Millisecond)
	}

	if fdb.Len() > 3 {
		t.Errorf("expected FDB length to be bounded to 3, got %d", fdb.Len())
	}

	// Oldest entries (1 and 2) should have been evicted to respect maxEntries
	mac1, _ := net.ParseMAC("02:00:00:00:00:01")
	if ep := fdb.Lookup(mac1); ep != nil {
		t.Errorf("expected oldest entry mac1 to be evicted")
	}

	mac5, _ := net.ParseMAC("02:00:00:00:00:05")
	if ep := fdb.Lookup(mac5); ep == nil {
		t.Errorf("expected newest entry mac5 to be present")
	}
}

// fdbClockFor gives f a clock the test moves.
func fdbClockFor(f *FDB) *atomic.Int64 {
	var now atomic.Int64
	now.Store(int64(time.Hour))
	f.now = now.Load
	return &now
}

func macN(n int) net.HardwareAddr { return net.HardwareAddr{2, 0, 0, 0, byte(n >> 8), byte(n)} }

func epN(n int) Endpoint {
	return newMockEndpoint(fmt.Sprintf("ep%d", n), fmt.Sprintf("tap%d", n), "10.244.1.2", macN(n).String())
}

// Learn of an entry that exists with the same endpoint and was refreshed less than a
// second ago, or is static, only reads: it does not wait for the write lock. A new entry,
// a changed endpoint and a stale refresh do.
func TestFDBLearnOfAKnownEntryTakesNoWriteLock(t *testing.T) {
	f := NewFDB(5 * time.Minute)
	now := fdbClockFor(f)
	ep, other := epN(1), epN(2)
	f.Learn(macN(1), ep)
	f.SetStatic(macN(3), ep)

	// learns reports whether Learn returns while a reader holds the lock, which a write lock could not.
	learns := func(mac net.HardwareAddr, e Endpoint) (ok bool) {
		f.mu.RLock()
		done := make(chan struct{})
		go func() { f.Learn(mac, e); close(done) }()
		select {
		case <-done:
			ok = true
		case <-time.After(100 * time.Millisecond):
		}
		f.mu.RUnlock()
		<-done
		return ok
	}
	now.Add(int64(999 * time.Millisecond))
	if !learns(macN(1), ep) {
		t.Error("a known entry refreshed 999 ms ago takes the write lock")
	}
	if !learns(macN(3), ep) {
		t.Error("a static entry takes the write lock")
	}
	now.Add(int64(24 * time.Hour))
	if !learns(macN(3), ep) {
		t.Error("a static entry takes the write lock after a day")
	}
	if learns(macN(1), ep) {
		t.Error("an entry refreshed a day ago does not take the write lock")
	}
	if learns(macN(1), other) {
		t.Error("a changed endpoint does not take the write lock")
	}
	if learns(macN(4), ep) {
		t.Error("a new entry does not take the write lock")
	}
}

// The time of an entry is stored again once it is a second old, so that an entry that is
// in use never ages, and one that is not does, after the TTL.
func TestFDBRefreshAndAgeing(t *testing.T) {
	f := NewFDB(10 * time.Second)
	now := fdbClockFor(f)
	ep, other := epN(1), epN(2)
	start := now.Load()

	f.Learn(macN(1), ep)
	f.Learn(macN(2), ep)
	f.SetStatic(macN(3), ep)
	updated := func(n int) int64 {
		f.mu.RLock()
		defer f.mu.RUnlock()
		return f.get(macN(n)).updated
	}

	now.Add(int64(500 * time.Millisecond))
	f.Learn(macN(1), ep)
	if updated(1) != start {
		t.Error("the time was stored again after 500 ms")
	}
	now.Add(int64(500 * time.Millisecond))
	f.Learn(macN(1), ep)
	if updated(1) != now.Load() {
		t.Error("the time was not stored again after a second")
	}

	// mac 1 is seen every 100 ms for a minute; mac 2 never again.
	for i := 0; i < 600; i++ {
		now.Add(int64(100 * time.Millisecond))
		f.Learn(macN(1), ep)
		if f.Lookup(macN(1)) != ep {
			t.Fatalf("an entry in use has aged out after %d ms", i*100)
		}
	}
	if f.Lookup(macN(2)) != nil || f.Lookup(macN(3)) != ep {
		t.Error("the entry that was not seen still lives, or the static one does not")
	}
	if n := f.EvictExpired(); n != 1 || f.Len() != 2 {
		t.Errorf("evicted %d, %d entries left, want 1 and 2", n, f.Len())
	}

	// An entry that is not seen ages out a TTL after it was last seen, give or take the second.
	f.Learn(macN(4), ep)
	now.Add(int64(10 * time.Second))
	if f.Lookup(macN(4)) != ep {
		t.Error("expired exactly at the TTL")
	}
	now.Add(1)
	if f.Lookup(macN(4)) != nil {
		t.Error("alive after the TTL")
	}

	// A moved endpoint is seen at once, and a static entry stays static.
	f.Learn(macN(1), other)
	if f.Lookup(macN(1)) != other {
		t.Error("the new endpoint of a MAC is not seen at once")
	}
	f.Learn(macN(3), other)
	now.Add(int64(time.Hour))
	if f.Lookup(macN(3)) != other {
		t.Error("a static entry that was moved has aged out")
	}

	// With a TTL under four seconds an entry in use must not age either.
	short := NewFDB(40 * time.Millisecond)
	snow := fdbClockFor(short)
	short.Learn(macN(1), ep)
	for i := 0; i < 100; i++ {
		snow.Add(int64(10 * time.Millisecond))
		short.Learn(macN(1), ep)
		if short.Lookup(macN(1)) != ep {
			t.Fatalf("an entry seen every 10 ms has aged out under a TTL of 40 ms after %d ms", i*10)
		}
	}
}

// A table that is full evicts what has expired and then the entry seen longest ago.
func TestFDBEvictsTheOldestEntryOfAFullTable(t *testing.T) {
	f := NewFDB(time.Hour)
	now := fdbClockFor(f)
	f.SetMaxEntries(3)
	ep := epN(1)
	f.SetStatic(macN(100), ep)
	for i := 1; i <= 2; i++ {
		f.Learn(macN(i), ep)
		now.Add(int64(2 * time.Second))
	}
	now.Add(int64(2 * time.Second))
	f.Learn(macN(1), ep) // 1 is now the one seen last
	f.Learn(macN(3), ep)
	if f.Len() != 3 || f.Lookup(macN(2)) != nil || f.Lookup(macN(1)) == nil || f.Lookup(macN(3)) == nil || f.Lookup(macN(100)) == nil {
		t.Errorf("%d entries; 1:%v 2:%v 3:%v static:%v", f.Len(), f.Lookup(macN(1)), f.Lookup(macN(2)), f.Lookup(macN(3)), f.Lookup(macN(100)))
	}
}

// The FDB keeps its own copy of the address it is given: the caller's is a slice of a
// frame that is reused.
func TestFDBKeepsACopyOfTheAddress(t *testing.T) {
	f := NewFDB(time.Hour)
	ep := epN(1)
	frame := macN(5)
	f.Learn(frame, ep)
	copy(frame, macN(6)) // the frame's buffer is reused
	if f.Lookup(macN(5)) != ep || f.Lookup(macN(6)) != nil {
		t.Error("the entry follows the caller's slice")
	}
	f.DeleteByEndpoint(ep.ID())
	if f.Len() != 0 {
		t.Errorf("%d entries left after DeleteByEndpoint", f.Len())
	}
}

// Learn and Lookup from many goroutines, with the endpoints of the addresses moving, the
// clock running and entries being deleted and evicted: only the race detector and the
// invariants have a say.
func TestFDBConcurrentLearnAndLookup(t *testing.T) {
	const macs, eps, workers, rounds = 32, 4, 8, 20000
	f := NewFDB(20 * time.Millisecond)
	f.SetMaxEntries(24)
	var now atomic.Int64
	f.now = now.Load
	list := make([]Endpoint, eps)
	for i := range list {
		list[i] = epN(i)
	}
	f.SetStatic(macN(1000), list[0])

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // the clock, and the maintenance
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			now.Add(int64(time.Millisecond))
			switch i % 64 {
			case 0:
				f.EvictExpired()
			case 16:
				f.Delete(macN(i % macs))
			case 32:
				f.DeleteByEndpoint(list[i%eps].ID())
			case 48:
				f.SetStatic(macN(1000), list[i%eps])
			}
			time.Sleep(10 * time.Microsecond)
		}
	}()
	var bad atomic.Int64
	var work sync.WaitGroup
	for w := 0; w < workers; w++ {
		work.Add(1)
		go func() {
			defer work.Done()
			frame := make(net.HardwareAddr, 6) // reused, as a frame's is
			for i := 0; i < rounds; i++ {
				n := (w*7 + i) % macs
				copy(frame, macN(n))
				ep := list[(i/100+w)%eps] // moves every 100 frames
				f.Learn(frame, ep)
				got := f.Lookup(macN(n))
				ok := got == nil
				for _, e := range list {
					ok = ok || got == e
				}
				if !ok {
					bad.Add(1)
				}
			}
		}()
	}
	work.Wait()
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("%d lookups returned an endpoint that was never learned", bad.Load())
	}
	if f.Len() > 25 { // the bound, and the static entry that may be added beyond it
		t.Errorf("%d entries in a table bounded to 24", f.Len())
	}
	f.Learn(macN(5), list[2])
	f.Learn(macN(5), list[3])
	if f.Lookup(macN(5)) != list[3] {
		t.Error("the last endpoint learned is not the one looked up")
	}
}

// Learn of a known address, and Lookup, allocate nothing.
func TestFDBLearnAndLookupAllocateNothing(t *testing.T) {
	f := NewFDB(5 * time.Minute)
	now := fdbClockFor(f)
	ep, mac := epN(1), macN(1)
	f.Learn(mac, ep)
	if a := testing.AllocsPerRun(100, func() { f.Learn(mac, ep) }); a != 0 {
		t.Errorf("Learn of a known address: %.1f allocations", a)
	}
	if a := testing.AllocsPerRun(100, func() { f.Lookup(mac) }); a != 0 {
		t.Errorf("Lookup: %.1f allocations", a)
	}
	// Not even when it stores the time again, which is the write path without a new entry.
	if a := testing.AllocsPerRun(100, func() { now.Add(int64(2 * time.Second)); f.Learn(mac, ep) }); a != 0 {
		t.Errorf("Learn that refreshes the time: %.1f allocations", a)
	}
}

// BenchmarkFDBLearnAndLookup is what the bridge does for every frame it processes, from
// many goroutines at once: it learns the source address and looks up the destination's,
// all of them known.
func BenchmarkFDBLearnAndLookup(b *testing.B) {
	f := NewFDB(5 * time.Minute)
	const macs = 16
	eps := make([]Endpoint, macs)
	for i := range eps {
		eps[i] = epN(i)
		f.Learn(macN(i), eps[i])
	}
	var next atomic.Int64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := int(next.Add(1)) * 3
		src := make(net.HardwareAddr, 6)
		for pb.Next() {
			n := i % macs
			copy(src, macN(n))
			f.Learn(src, eps[n])
			if f.Lookup(macN((n+1)%macs)) == nil {
				b.Fatal("not found")
			}
			i++
		}
	})
}
