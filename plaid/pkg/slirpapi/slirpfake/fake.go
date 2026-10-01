// Package slirpfake is a stand-in for slirp4netns's API socket, for tests: it keeps a list
// of forwards, answers like slirp4netns does (including its error for a port that is
// taken), and can be told to misbehave.
package slirpfake

import (
	"encoding/json"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"plaid/pkg/slirpapi"
)

// Fake is a fake API server.
type Fake struct {
	mu       sync.Mutex
	listener net.Listener
	nextID   int
	entries  []slirpapi.Entry
	requests []string
	taken    map[string]bool // "proto:port" of host ports something else holds
	delay    time.Duration
	mode     string
}

// SetMode makes the server misbehave: "hang" (never answer), "garbage" (a broken answer),
// "close" (no answer), "top-level" (listings as {"entries": ...}); "" is normal.
func (f *Fake) SetMode(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = mode
}

// SetDelay makes every answer wait.
func (f *Fake) SetDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// Start listens on path.
func Start(path string) (*Fake, error) {
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	f := &Fake{listener: l, nextID: 1, taken: map[string]bool{}}
	go f.serve()
	return f, nil
}

// Close stops the server.
func (f *Fake) Close() { f.listener.Close() }

// Take marks a host port as held by something else: adding a forward for it fails.
func (f *Fake) Take(proto string, port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taken[key(proto, port)] = true
}

// Release undoes Take.
func (f *Fake) Release(proto string, port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.taken, key(proto, port))
}

// Preload adds a forward as if it had been made before (a fixed one, or a stale one).
func (f *Fake) Preload(e slirpapi.Entry) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.ID = f.nextID
	f.nextID++
	f.entries = append(f.entries, e)
	return e.ID
}

// Entries returns the forwards.
func (f *Fake) Entries() []slirpapi.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]slirpapi.Entry(nil), f.entries...)
}

// Requests returns the commands received, in order.
func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func key(proto string, port int) string { return proto + ":" + strconv.Itoa(port) }

func (f *Fake) serve() {
	for {
		c, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *Fake) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 65536)
	n, err := c.Read(buf)
	if err != nil || n == 0 {
		return
	}
	var req struct {
		Execute   string `json:"execute"`
		Arguments struct {
			Proto     string `json:"proto"`
			HostAddr  string `json:"host_addr"`
			HostPort  int    `json:"host_port"`
			GuestAddr string `json:"guest_addr"`
			GuestPort int    `json:"guest_port"`
			ID        int    `json:"id"`
		} `json:"arguments"`
	}
	f.mu.Lock()
	f.requests = append(f.requests, string(buf[:n]))
	mode, delay := f.mode, f.delay
	f.mu.Unlock()
	if mode == "hang" {
		time.Sleep(30 * time.Second)
		return
	}
	if mode == "close" {
		return
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if mode == "garbage" {
		c.Write([]byte(`{"retu`))
		return
	}
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		c.Write([]byte(`{"error":{"desc":"bad request"}}`))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a := req.Arguments
	var answer any
	switch req.Execute {
	case "add_hostfwd":
		switch {
		case f.taken[key(a.Proto, a.HostPort)] || f.has(a.Proto, a.HostAddr, a.HostPort):
			answer = map[string]any{"error": map[string]string{"desc": "bad request: add_hostfwd: slirp_add_hostfwd failed"}}
		default:
			id := f.nextID
			f.nextID++
			f.entries = append(f.entries, slirpapi.Entry{ID: id, Proto: a.Proto, HostAddr: a.HostAddr, HostPort: a.HostPort, GuestAddr: a.GuestAddr, GuestPort: a.GuestPort})
			answer = map[string]any{"return": map[string]int{"id": id}}
		}
	case "remove_hostfwd":
		found := false
		for i, e := range f.entries {
			if e.ID == a.ID {
				f.entries = append(f.entries[:i], f.entries[i+1:]...)
				found = true
				break
			}
		}
		if found {
			answer = map[string]any{"return": 0}
		} else {
			answer = map[string]any{"error": map[string]string{"desc": "remove_hostfwd failed: no such id"}}
		}
	case "list_hostfwd":
		entries := append([]slirpapi.Entry{}, f.entries...)
		if mode == "top-level" {
			answer = map[string]any{"entries": entries}
		} else {
			answer = map[string]any{"return": map[string]any{"entries": entries}}
		}
	default:
		answer = map[string]any{"error": map[string]string{"desc": "unknown command"}}
	}
	out, _ := json.Marshal(answer)
	c.Write(out)
}

func (f *Fake) has(proto, addr string, port int) bool {
	for _, e := range f.entries {
		if e.Proto == proto && e.HostPort == port && (e.HostAddr == addr || e.HostAddr == "0.0.0.0" || e.HostAddr == "") {
			return true
		}
	}
	return false
}
