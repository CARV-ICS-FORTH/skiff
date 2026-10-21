package direct

import (
	"container/list"
	"net/netip"
	"sync"
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
// is not intercepted, so the table cannot know that a socket is gone; it keeps the most
// recently used ones, and an entry that stays is harmless because no other socket has its
// cookie. A listener is looked up only when a connection arrives on it, and a server with
// two of them, one busy and one not, would see the quiet one pushed out by the connections
// of the busy one (and then have it accept connections as the pod's network's, with the
// preamble in them), so listeners have a table of their own.
type Table struct {
	mu               sync.Mutex
	conns, listeners lru
}

type lru struct {
	max     int
	entries map[uint64]*list.Element
	order   *list.List // most recently used first
}

type tableEntry struct {
	cookie uint64
	socket Socket
}

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
		conns:     lru{max: max, entries: make(map[uint64]*list.Element), order: list.New()},
		listeners: lru{max: maxListeners, entries: make(map[uint64]*list.Element), order: list.New()},
	}
}

func (l *lru) add(cookie uint64, s Socket) {
	if el, ok := l.entries[cookie]; ok {
		el.Value.(*tableEntry).socket = s
		l.order.MoveToFront(el)
		return
	}
	l.entries[cookie] = l.order.PushFront(&tableEntry{cookie: cookie, socket: s})
	for l.order.Len() > l.max {
		oldest := l.order.Back()
		delete(l.entries, oldest.Value.(*tableEntry).cookie)
		l.order.Remove(oldest)
	}
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
	} else {
		t.conns.add(cookie, s)
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
