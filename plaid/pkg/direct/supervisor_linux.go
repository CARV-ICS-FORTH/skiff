//go:build linux && (amd64 || arm64)

package direct

import (
	"fmt"
	"net/netip"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"plaid/pkg/api"
)

// Supervisor holds the container's connect, bind, accept, getsockname and getpeername
// calls and decides them.
type Supervisor struct {
	cfg        Config
	cmd        *exec.Cmd
	listener   int
	targets    *targets
	table      *Table
	stop       chan struct{}
	loopDone   chan struct{}
	watchdog   *watchdog // ends the container if the supervisor does not (see watchdog_linux.go)
	loopOnce   sync.Once
	handlers   sync.WaitGroup
	stats      Stats
	timings    Timings
	connLogged atomic.Uint64 // connections logged so far (logConn)

	connecting atomic.Int32 // connects being decided
	epollFirst atomic.Bool  // a socket of the container was in an epoll set before its connect
	lastAnswer atomic.Int64 // when a call was last answered, in unix nanoseconds

	pushMu     sync.Mutex
	pushedLast bool // the last report has been made

	proxyMu sync.Mutex
	proxies map[netip.AddrPort]*localProxy // by the address in the pod's network
}

// Stats counts what the supervisor has done.
type Stats struct {
	Notifications atomic.Uint64 // calls held
	Connects      atomic.Uint64 // connects made on the host network for the container
	Binds         atomic.Uint64 // listeners put on the host network
	Accepted      atomic.Uint64 // connections handed to a swapped listener's accept
	Names         atomic.Uint64 // getsockname and getpeername answered
	Proxied       atomic.Uint64 // connections to a swapped listener's pod-side socket that were carried to it
	ProxyFailed   atomic.Uint64 // those that could not be
	Errors        atomic.Uint64 // calls that went wrong in the supervisor (and were let through, or failed)

	// Fallbacks counts, by reason, the connects and binds that were let through to the pod
	// network after the direct path was tried or ruled out; Rejected the connections to a
	// swapped listener that were closed without the container seeing them.
	Fallbacks Reasons
	Rejected  Reasons
}

// Start starts cmd, with the filter on it and on everything it starts, and serves the
// held calls until cmd and everything it started have ended. The supervisor must stay the
// ancestor of the container: it is how it is allowed to look into it.
//
// The filter is installed on a thread of its own, which starts cmd and ends: a filter
// cannot be removed, and the supervisor's other threads must not be held. The runtime
// does not end its main thread, so a program that calls Start should keep its main
// goroutine on the main thread (runtime.LockOSThread in init); otherwise the supervisor
// may not learn that the command has gone, and Wait takes a second more. Use the
// supervisor's Wait, not cmd's. A watchdog process ends the container if the supervisor is
// killed (watchdog_linux.go); the program must call RunWatchdogIfRequested first thing in
// main.
func Start(cfg Config, cmd *exec.Cmd) (*Supervisor, error) {
	if cfg.Factory == nil || cfg.Resolver == nil || !cfg.PodIP.Is4() || len(cfg.Key) == 0 {
		return nil, fmt.Errorf("the direct path needs a socket factory, a resolver, a key and the pod's IPv4 address")
	}
	if cfg.AckTimeout <= 0 {
		cfg.AckTimeout = DefaultAckTimeout
	}
	// The watchdog is started here, on a thread that is not filtered, before the filter's.
	wd, err := startWatchdog()
	if err != nil && cfg.Logf != nil {
		cfg.Logf("no watchdog, the container will not end with its supervisor: %v", err)
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
		wd.finished()
		return nil, r.err
	}
	wd.launched(cmd.Process.Pid)
	s := &Supervisor{
		cfg:      cfg,
		cmd:      cmd,
		listener: r.listener,
		targets:  newTargets(),
		table:    NewTable(DefaultTableSize),
		stop:     make(chan struct{}),
		loopDone: make(chan struct{}),
		watchdog: wd,
	}
	go s.serve()
	go s.report()
	return s, nil
}

// Stats is the supervisor's counters.
func (s *Supervisor) Stats() *Stats { return &s.stats }

// Swapped is the number of sockets put in place of the container's.
func (st *Stats) Swapped() uint64 { return st.Connects.Load() + st.Binds.Load() }

// Counters is a copy of the counters, as plaidd takes them.
func (st *Stats) Counters() api.DirectCounters {
	return api.DirectCounters{
		Connects: st.Connects.Load(), Binds: st.Binds.Load(), Accepts: st.Accepted.Load(),
		Proxied: st.Proxied.Load(), ProxyFailed: st.ProxyFailed.Load(), Errors: st.Errors.Load(),
		Fallbacks: st.Fallbacks.Snapshot(), Rejected: st.Rejected.Snapshot(),
	}
}

// summary is the counters in a line of the log.
func (st *Stats) summary() string {
	return fmt.Sprintf("held %d, connects %d, binds %d, accepts %d, proxied %d (failed %d); fallbacks: %s; rejected: %s",
		st.Notifications.Load(), st.Connects.Load(), st.Binds.Load(), st.Accepted.Load(), st.Proxied.Load(), st.ProxyFailed.Load(), st.Fallbacks.String(), st.Rejected.String())
}

// reportEvery is how often the counters are logged and reported (Config.Report), when they
// have changed: the log of a container that runs for days says what its supervisor has done,
// not only when it ends, and plaidd's status has it.
const reportEvery = 10 * time.Second

func (s *Supervisor) report() {
	tick := time.NewTicker(reportEvery)
	defer tick.Stop()
	s.push(false) // so that the supervisor is known as soon as it runs
	var last string
	for {
		select {
		case <-s.loopDone:
			return
		case <-tick.C:
		}
		if now := s.stats.summary(); now != last {
			last = now
			s.logf("stats, %s", now)
			s.logTimings()
			s.push(false)
		}
	}
}

// push gives the counters to Config.Report, ended if the supervisor has. Nothing is
// reported after the last report: it would come to plaidd as a supervisor that began again.
func (s *Supervisor) push(ended bool) {
	if s.cfg.Report == nil {
		return
	}
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	if s.pushedLast {
		return
	}
	s.pushedLast = ended
	s.cfg.Report(s.stats.Counters(), ended)
}

// Wait waits for the command to end, then for the held calls of what is left of it (nothing,
// normally: a container's processes end with it), and returns the command's error.
func (s *Supervisor) Wait() error {
	err := s.cmd.Wait()
	s.watchdog.finished()
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
	s.closeProxies()
	s.targets.closeAll()
	unix.Close(s.listener)
	s.logf("ended, %s", s.stats.summary())
	s.logTimings()
	// The last counters: plaidd is waited for a moment, not for as long as it might take.
	done := make(chan struct{})
	go func() {
		s.push(true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(finalReportWait):
	}
	return err
}

// logTimings logs where the time of the connects and accepts went (see Timings).
func (s *Supervisor) logTimings() {
	for _, line := range strings.Split(s.timings.String(), "\n") {
		if line != "" {
			s.logf("timing, %s", line)
		}
	}
}

// drainGrace is how long the supervisor keeps serving after the command has ended, for
// processes the container left behind.
const drainGrace = time.Second

// finalReportWait is how long the end of a supervisor waits for plaidd to take its last counters.
const finalReportWait = 3 * time.Second

func (s *Supervisor) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}

// serve receives the held calls, and decides the quick ones. It waits with poll, so that it
// learns that no task is left that the filter holds (POLLHUP) or that it should stop.
//
// A call that can wait (a connect makes a connection, an accept waits for one) is decided
// by the goroutine that received it, and another goroutine takes over the receiving first,
// so that the others are not held up. Handing the call to a new goroutine instead made the
// connect wait for a thread to be woken for it (36 microseconds on the cluster's nodes);
// the thread that is woken now is the next receiver's, while this one works.
func (s *Supervisor) serve() {
	fds := []unix.PollFd{{Fd: int32(s.listener), Events: unix.POLLIN}}
	for {
		select {
		case <-s.stop:
			s.loopEnded()
			return
		default:
		}
		_, err := s.waitForCall(fds)
		if err != nil && err != unix.EINTR {
			s.loopEnded()
			return
		}
		if fds[0].Revents&unix.POLLIN != 0 {
			var n notif
			if err := ioctlRecv(s.listener, &n); err != nil {
				if err == unix.EINTR || err == unix.ENOENT {
					continue
				}
				s.loopEnded()
				return
			}
			s.stats.Notifications.Add(1)
			tl := s.timings.begin()
			switch int(n.Nr) {
			case unix.SYS_CONNECT, unix.SYS_ACCEPT, unix.SYS_ACCEPT4:
				s.handlers.Add(1)
				go s.serve()
				s.answer(&n, tl)
				s.handlers.Done()
				return
			}
			s.answer(&n, tl)
			continue
		}
		if fds[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0 {
			s.loopEnded()
			return
		}
	}
}

// A receiver does not go to sleep in poll while a connect is being decided and for a moment
// after one has been: the container's next calls (Go asks for the new socket's two addresses
// at once) come within microseconds of the answer, and waking a sleeping thread for each
// takes a good part of what the call costs (about 30 microseconds on the cluster's nodes).
// It costs a core for as long as a connect takes, at most spinMax for each wait.
const (
	spinMax   = 2 * time.Millisecond
	spinAfter = 150 * time.Microsecond
)

// waitForCall waits for the listener to have something, spinning first where it is likely to
// come soon.
func (s *Supervisor) waitForCall(fds []unix.PollFd) (int, error) {
	start := time.Now()
	for s.connecting.Load() > 0 || time.Since(time.Unix(0, s.lastAnswer.Load())) < spinAfter {
		n, err := unix.Poll(fds, 0)
		if n > 0 || (err != nil && err != unix.EINTR) {
			return n, err
		}
		if time.Since(start) > spinMax {
			break
		}
	}
	return unix.Poll(fds, 100)
}

// loopEnded says that the receiving has ended (there is one receiver at a time).
func (s *Supervisor) loopEnded() { s.loopOnce.Do(func() { close(s.loopDone) }) }

// answer decides one call and tells the kernel. Whatever goes wrong in the supervisor lets
// the call run as it would have without one: the container loses the direct path for that
// call, not its network.
func (s *Supervisor) answer(n *notif, tl *timeline) {
	if int(n.Nr) == unix.SYS_CONNECT {
		s.connecting.Add(1)
		defer s.connecting.Add(-1)
	}
	defer func() { s.lastAnswer.Store(time.Now().UnixNano()) }()
	var r reply
	func() {
		defer func() {
			if p := recover(); p != nil {
				s.stats.Errors.Add(1)
				s.logf("call %d of thread %d: panic: %v", n.Nr, n.Pid, p)
				r = letThrough
			}
		}()
		r = s.decide(n, tl)
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
		return
	}
	if !r.through && r.errno == 0 { // a connect that was swapped, a name that was given
		switch int(n.Nr) {
		case unix.SYS_CONNECT:
			tl.mark(phReply)
			tl.finish(phConnect)
		case unix.SYS_GETSOCKNAME, unix.SYS_GETPEERNAME:
			tl.finish(phName)
		}
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
func (s *Supervisor) decide(n *notif, tl *timeline) reply {
	switch int(n.Nr) {
	case unix.SYS_CONNECT:
		return s.connect(n, tl)
	case unix.SYS_BIND:
		return s.bind(n)
	case unix.SYS_ACCEPT, unix.SYS_ACCEPT4:
		return s.accept(n, tl)
	case unix.SYS_GETSOCKNAME, unix.SYS_GETPEERNAME:
		return s.name(n, tl)
	}
	return letThrough
}
