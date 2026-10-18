//go:build linux && (amd64 || arm64)

package direct

import (
	"fmt"
	"net/netip"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Backend is where a Service address leads: the pod that will serve the connection, and the
// host socket address on its node that the connection goes to.
type Backend struct {
	Pod  netip.AddrPort // the pod's address and port
	Host netip.AddrPort // the node's address and the port the pod's server listens on there
}

// Resolver finds the backend of a Service address. It says false for an address that is no
// Service's (or that has no backend that can take a connection), and the connect goes the
// way it would have without the supervisor.
type Resolver interface {
	Resolve(dst netip.AddrPort) (Backend, bool)
}

// Config is what a supervisor needs to know.
type Config struct {
	// Ports are the ports the container marks: connections to a Service port in the set,
	// and binds of a port in it, take the direct path. (One list serves both roles; they are
	// different things in general, the Service's port and the container's.)
	Ports Ports
	// PodIP is the pod's address, which the container is told for its own sockets.
	PodIP netip.Addr
	// Factory makes the host sockets.
	Factory *FactoryClient
	// Resolver finds the backends of Service addresses.
	Resolver Resolver
	// Logf, when set, receives a line for every decision (the container's own output is
	// not the place for them).
	Logf func(format string, args ...any)
}

// Supervisor holds the container's connect, bind, accept, getsockname and getpeername
// calls and decides them.
type Supervisor struct {
	cfg      Config
	cmd      *exec.Cmd
	listener int
	targets  *targets
	table    *Table
	stop     chan struct{}
	loopDone chan struct{}
	handlers sync.WaitGroup
	stats    Stats
}

// Stats counts what the supervisor has done.
type Stats struct {
	Notifications atomic.Uint64 // calls held
	Swapped       atomic.Uint64 // sockets put in place of the container's (connect, bind)
	Accepted      atomic.Uint64 // connections handed to a swapped listener's accept
	Names         atomic.Uint64 // getsockname and getpeername answered
	Errors        atomic.Uint64 // calls that went wrong in the supervisor (and were let through, or failed)
}

// Start starts cmd, with the filter on it and on everything it starts, and serves the
// held calls until cmd and everything it started have ended. The supervisor must stay the
// ancestor of the container: it is how it is allowed to look into it.
//
// The filter is installed on a thread of its own, which starts cmd and ends: a filter
// cannot be removed, and the supervisor's other threads must not be held. Run the call
// only once cmd is configured, and use cmd's Wait through the supervisor's.
func Start(cfg Config, cmd *exec.Cmd) (*Supervisor, error) {
	if cfg.Factory == nil || cfg.Resolver == nil || !cfg.PodIP.Is4() {
		return nil, fmt.Errorf("the direct path needs a socket factory, a resolver and the pod's IPv4 address")
	}
	type started struct {
		listener int
		err      error
	}
	result := make(chan started, 1)
	go func() {
		runtime.LockOSThread() // and never unlock: the thread ends with this goroutine, filter and all
		listener, err := installFilter(filteredCalls)
		if err == nil {
			if err = cmd.Start(); err != nil {
				unix.Close(listener)
			}
		}
		result <- started{listener, err}
	}()
	r := <-result
	if r.err != nil {
		return nil, r.err
	}
	s := &Supervisor{
		cfg:      cfg,
		cmd:      cmd,
		listener: r.listener,
		targets:  newTargets(),
		table:    NewTable(DefaultTableSize),
		stop:     make(chan struct{}),
		loopDone: make(chan struct{}),
	}
	go s.serve()
	return s, nil
}

// Stats is the supervisor's counters.
func (s *Supervisor) Stats() *Stats { return &s.stats }

// Wait waits for the command to end, then for the held calls of what is left of it (nothing,
// normally: a container's processes end with it), and returns the command's error.
func (s *Supervisor) Wait() error {
	err := s.cmd.Wait()
	select {
	case <-s.loopDone:
	case <-time.After(drainGrace):
		close(s.stop)
		select {
		case <-s.loopDone:
		case <-time.After(drainGrace): // a receive that never returns: leave it
		}
	}
	s.handlers.Wait()
	s.targets.closeAll()
	unix.Close(s.listener)
	return err
}

// drainGrace is how long the supervisor keeps serving after the command has ended, for
// processes the container left behind.
const drainGrace = time.Second

func (s *Supervisor) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}

// serve receives the held calls. It waits with poll, so that it learns that no task is
// left that the filter holds (POLLHUP) or that it should stop.
func (s *Supervisor) serve() {
	defer close(s.loopDone)
	fds := []unix.PollFd{{Fd: int32(s.listener), Events: unix.POLLIN}}
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		_, err := unix.Poll(fds, 100)
		if err != nil && err != unix.EINTR {
			return
		}
		if fds[0].Revents&unix.POLLIN != 0 {
			var n notif
			if err := ioctlRecv(s.listener, &n); err != nil {
				if err == unix.EINTR || err == unix.ENOENT {
					continue
				}
				return
			}
			s.stats.Notifications.Add(1)
			s.hold(&n)
			continue
		}
		if fds[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0 {
			return
		}
	}
}

// hold decides a held call. A call that can wait (a connect makes a connection, an accept
// waits for one) is decided on a goroutine of its own, so that the others are not held up.
func (s *Supervisor) hold(n *notif) {
	switch int(n.Nr) {
	case unix.SYS_CONNECT, unix.SYS_ACCEPT, unix.SYS_ACCEPT4:
		s.handlers.Add(1)
		go func(n notif) {
			defer s.handlers.Done()
			s.answer(&n)
		}(*n)
	default:
		s.answer(n)
	}
}

// answer decides one call and tells the kernel. Whatever goes wrong in the supervisor lets
// the call run as it would have without one: the container loses the direct path for that
// call, not its network.
func (s *Supervisor) answer(n *notif) {
	var r reply
	func() {
		defer func() {
			if p := recover(); p != nil {
				s.stats.Errors.Add(1)
				s.logf("call %d of thread %d: panic: %v", n.Nr, n.Pid, p)
				r = letThrough
			}
		}()
		r = s.decide(n)
	}()
	if r.answered {
		return
	}
	resp := notifResp{ID: n.ID, Val: r.val, Errno: r.errno}
	if r.through {
		resp.Flags = flagContinue
	}
	if err := ioctlSend(s.listener, &resp); err != nil && err != unix.ENOENT {
		s.logf("answering call %d of thread %d: %v", n.Nr, n.Pid, err)
	}
}

// reply is a decision: let the call run, give it a result, or fail it; or, answered, that
// the kernel has been told already (an accept returns the new descriptor in the same step
// that puts it in place).
type reply struct {
	through  bool
	val      int64
	errno    int32
	answered bool
}

var letThrough = reply{through: true}

func succeed() reply { return reply{} }

func failWith(errno syscall.Errno) reply { return reply{errno: -int32(errno)} }

func errnoOf(err error) syscall.Errno {
	var errno syscall.Errno
	if e, ok := err.(syscall.Errno); ok {
		errno = e
	} else {
		errno = unix.EIO
	}
	return errno
}

// decide is the supervisor's judgement of one held call.
func (s *Supervisor) decide(n *notif) reply {
	return letThrough
}
