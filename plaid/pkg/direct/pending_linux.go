//go:build linux && (amd64 || arm64)

package direct

import (
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// kept holds host sockets that a given-up call made, by what identifies the call that is
// made again, for at most maxAge and at most perKey of each key. A socket that nobody takes
// is closed (and expired, if set, is told what went with it). A negative descriptor is a note
// that has no socket to close.
type kept[K comparable, V any] struct {
	maxAge  time.Duration
	perKey  int
	expired func(V)

	mu      sync.Mutex
	entries map[K][]*keptEntry[V]
}

type keptEntry[V any] struct {
	fd    int
	value V
	timer *time.Timer
}

// put keeps the socket, and says whether it did: it does not if perKey are kept already, and
// then the socket is the caller's to close.
func (k *kept[K, V]) put(key K, fd int, value V) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.entries[key]) >= k.perKey {
		return false
	}
	e := &keptEntry[V]{fd: fd, value: value}
	e.timer = time.AfterFunc(k.maxAge, func() { k.expire(key, e) })
	if k.entries == nil {
		k.entries = make(map[K][]*keptEntry[V])
	}
	k.entries[key] = append(k.entries[key], e)
	return true
}

// take gives up the socket that was kept first for the key.
func (k *kept[K, V]) take(key K) (fd int, value V, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	es := k.entries[key]
	if len(es) == 0 {
		return -1, value, false
	}
	e := es[0]
	e.timer.Stop()
	if len(es) == 1 {
		delete(k.entries, key)
	} else {
		k.entries[key] = es[1:]
	}
	return e.fd, e.value, true
}

// has says whether a socket is kept for the key.
func (k *kept[K, V]) has(key K) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.entries[key]) > 0
}

func (k *kept[K, V]) expire(key K, e *keptEntry[V]) {
	k.mu.Lock()
	es := k.entries[key]
	for i, other := range es {
		if other != e {
			continue
		}
		if len(es) == 1 {
			delete(k.entries, key)
		} else {
			k.entries[key] = append(es[:i:i], es[i+1:]...)
		}
		k.mu.Unlock()
		k.discard(e)
		return
	}
	k.mu.Unlock() // taken meanwhile
}

func (k *kept[K, V]) discard(e *keptEntry[V]) {
	if e.fd >= 0 {
		unix.Close(e.fd)
	}
	if k.expired != nil {
		k.expired(e.value)
	}
}

// drop closes what is kept for the key.
func (k *kept[K, V]) drop(key K) {
	k.mu.Lock()
	es := k.entries[key]
	delete(k.entries, key)
	k.mu.Unlock()
	for _, e := range es {
		e.timer.Stop()
		k.discard(e)
	}
}

// closeAll closes everything that is kept.
func (k *kept[K, V]) closeAll() {
	k.mu.Lock()
	all := k.entries
	k.entries = nil
	k.mu.Unlock()
	for _, es := range all {
		for _, e := range es {
			e.timer.Stop()
			k.discard(e)
		}
	}
}
