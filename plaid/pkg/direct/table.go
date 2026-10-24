package direct

import (
	"container/list"
	"net/netip"
	"sync"
	"time"
)

// Kind says what a socket that was put in place of the container's is.
type Kind int

const (
	KindClient   Kind = iota + 1 // connected to a backend; the peer is the address dialled
	KindListener                 // bound on the host; the address is what the container bound
	KindAccepted                 // taken from a listener; the peer is the client's pod address
)

// Socket is what the container is told about a socket that is not what it seems: the
// addresses it must see, whatever the host socket's are.
type Socket struct {
	Kind   Kind
	Family int // the socket's address family, which the addresses are written in
	Local  netip.AddrPort
	Peer   netip.AddrPort
}

// Sizes of the table: the connections a supervisor remembers, and its listeners, which are
// kept apart so that connections coming and going do not push them out (see Table).
const (
	DefaultTableSize         = 1 << 16
	DefaultListenerTableSize = 1 << 12
)

// Table remembers the sockets that were put in place, by socket cookie (SO_COOKIE: unique
// for the life of the machine, unlike an inode number, which is reused, and the same for
// a socket however many descriptors, processes and SCM_RIGHTS copies refer to it). close
// is not intercepted, so the table cannot see that a socket is gone; an entry that stays is
// harmless because no other socket has its cookie, but the entries of closed connections
// would fill the table, and a pooled connection that lives for hours would be the least
// recently used of them. So when the connection table nears its size, a sweep asks the
// kernel which of the cookies are those of sockets that still exist and drops the others
// (see SetLiveness); the least recently used are pushed out only when the table is full and
// the sweep has not made room. A listener is looked up only when a connection arrives on it, and a server with
// two of them, one busy and one not, would see the quiet one pushed out by the connections
// of the busy one (and then have it accept connections as the pod's network's, with the
// preamble in them), so listeners have a table of their own.
type Table struct {
	mu               sync.Mutex
	conns, listeners lru

	live     func() (map[uint64]struct{}, error) // the cookies of the sockets that exist; nil: no sweeps
	logf     func(format string, args ...any)
	sweeping bool          // a sweep is running
	sweptAt  time.Time     // when the last one ended
	sweepGap time.Duration // the least time between the end of one and the start of the next
	failed   bool          // a sweep has failed, and it was logged
}

type lru struct {
	max     int
	entries map[uint64]*list.Element
	order   *list.List // most recently used first
	added   uint64     // the entries added so far, which numbers them (tableEntry.seq)
}

type tableEntry struct {
	cookie uint64
	socket Socket
	seq    uint64
}

// A sweep starts when the connection table is this many percent full, and not less than
// sweepGap after the last one ended: what it asks of the kernel is a list of every TCP socket
// of the host, which is not worth making for a table that has just been swept and is as full
// as before because its sockets are alive.
const (
	sweepHighWater = 90
	sweepGap       = 3 * time.Second
)

// NewTable makes a table that keeps at most max connections and maxListeners listeners
// (DefaultTableSize and DefaultListenerTableSize if not positive).
func NewTable(max, maxListeners int) *Table {
	if max <= 0 {
		max = DefaultTableSize
	}
	if maxListeners <= 0 {
		maxListeners = DefaultListenerTableSize
	}
	return &Table{
		sweepGap:  sweepGap,
		conns:     lru{max: max, entries: make(map[uint64]*list.Element), order: list.New()},
		listeners: lru{max: maxListeners, entries: make(map[uint64]*list.Element), order: list.New()},
	}
}

// add remembers a socket, and says whether it is a new one.
func (l *lru) add(cookie uint64, s Socket) bool {
	if el, ok := l.entries[cookie]; ok {
		el.Value.(*tableEntry).socket = s
		l.order.MoveToFront(el)
		return false
	}
	l.added++
	l.entries[cookie] = l.order.PushFront(&tableEntry{cookie: cookie, socket: s, seq: l.added})
	for l.order.Len() > l.max {
		l.remove(l.order.Back())
	}
	return true
}

func (l *lru) remove(el *list.Element) {
	delete(l.entries, el.Value.(*tableEntry).cookie)
	l.order.Remove(el)
}

func (l *lru) get(cookie uint64) (Socket, bool) {
	el, ok := l.entries[cookie]
	if !ok {
		return Socket{}, false
	}
	l.order.MoveToFront(el)
	return el.Value.(*tableEntry).socket, true
}

// Add remembers a socket.
func (t *Table) Add(cookie uint64, s Socket) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.Kind == KindListener {
		t.listeners.add(cookie, s)
	} else if t.conns.add(cookie, s) {
		t.sweepIfNearlyFull()
	}
}

// SetLiveness gives the table the means to tell which of its sockets still exist: live
// returns the cookies of all the sockets that do (a socket in any state counts, down to
// one that is closing), and logf, if not nil, receives the line for a failure of it. Without
// it the table only pushes out the least recently used.
func (t *Table) SetLiveness(live func() (map[uint64]struct{}, error), logf func(format string, args ...any)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.live, t.logf = live, logf
}

// sweepIfNearlyFull starts a sweep of the connections in the background, with the lock
// held: the call that adds an entry, which a held system call of the container waits for,
// never waits for the kernel.
func (t *Table) sweepIfNearlyFull() {
	if t.live == nil || t.sweeping || time.Since(t.sweptAt) < t.sweepGap ||
		len(t.conns.entries) < t.conns.max-t.conns.max*(100-sweepHighWater)/100 {
		return
	}
	t.sweeping = true
	go t.sweep(t.live, t.conns.added)
}

// sweep drops the connections whose sockets no longer exist. An entry added after the
// list was asked for (the entries numbered above mark) is kept: its socket may not have
// been in the list. A list that cannot be had leaves the table as it is, and is logged once.
func (t *Table) sweep(live func() (map[uint64]struct{}, error), mark uint64) {
	alive, err := live()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweeping = false
	t.sweptAt = time.Now()
	if err != nil {
		if !t.failed && t.logf != nil {
			t.logf("the table of swapped sockets cannot tell which of them are closed, so it keeps the most recently used ones: %v", err)
		}
		t.failed = true
		return
	}
	for el := t.conns.order.Back(); el != nil; {
		prev := el.Prev()
		e := el.Value.(*tableEntry)
		if _, ok := alive[e.cookie]; !ok && e.seq <= mark {
			t.conns.remove(el)
		}
		el = prev
	}
}

// Get returns the socket with the cookie, if it is remembered.
func (t *Table) Get(cookie uint64) (Socket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.listeners.get(cookie); ok {
		return s, true
	}
	return t.conns.get(cookie)
}

// Len is the number of sockets remembered.
func (t *Table) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns.entries) + len(t.listeners.entries)
}
