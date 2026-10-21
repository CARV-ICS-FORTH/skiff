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

// DefaultTableSize bounds the sockets a supervisor remembers.
const DefaultTableSize = 1 << 16

// Table remembers the sockets that were put in place, by socket cookie (SO_COOKIE: unique
// for the life of the machine, unlike an inode number, which is reused, and the same for
// a socket however many descriptors, processes and SCM_RIGHTS copies refer to it). close
// is not intercepted, so the table cannot know that a socket is gone; it keeps the most
// recently used ones, and an entry that stays is harmless because no other socket has its
// cookie.
type Table struct {
	mu      sync.Mutex
	max     int
	entries map[uint64]*list.Element
	order   *list.List // most recently used first
}

type tableEntry struct {
	cookie uint64
	socket Socket
}

// NewTable makes a table that keeps at most max sockets (DefaultTableSize if not positive).
func NewTable(max int) *Table {
	if max <= 0 {
		max = DefaultTableSize
	}
	return &Table{max: max, entries: make(map[uint64]*list.Element), order: list.New()}
}

// Add remembers a socket.
func (t *Table) Add(cookie uint64, s Socket) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.entries[cookie]; ok {
		el.Value.(*tableEntry).socket = s
		t.order.MoveToFront(el)
		return
	}
	t.entries[cookie] = t.order.PushFront(&tableEntry{cookie: cookie, socket: s})
	for t.order.Len() > t.max {
		oldest := t.order.Back()
		delete(t.entries, oldest.Value.(*tableEntry).cookie)
		t.order.Remove(oldest)
	}
}

// Get returns the socket with the cookie, if it is remembered.
func (t *Table) Get(cookie uint64) (Socket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.entries[cookie]
	if !ok {
		return Socket{}, false
	}
	t.order.MoveToFront(el)
	return el.Value.(*tableEntry).socket, true
}

// Len is the number of sockets remembered.
func (t *Table) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}
