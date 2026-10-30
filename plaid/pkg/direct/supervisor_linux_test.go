//go:build linux && (amd64 || arm64)

package direct

import (
	"fmt"
	"net/netip"
	"strings"
	"syscall"
	"testing"
)

func init() {
	// names: getsockname on a fresh socket, ten times.
	helpers["names"] = func([]string) int {
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		if err != nil {
			return 2
		}
		for i := 0; i < 10; i++ {
			if _, err := syscall.Getsockname(fd); err != nil {
				return 3
			}
		}
		return 0
	}
}

func testConfig(t testing.TB) Config {
	t.Helper()
	return Config{
		ConnectPorts: Ports{5201: {}},
		ListenPorts:  Ports{5201: {}},
		PodIP:        netip.MustParseAddr("10.244.1.9"),
		Key:          testKey,
		Factory:      startFactory(t),
		Resolver:     fakeResolver{},
		Logf:         t.Logf,
	}
}

// The supervisor holds the calls of its command and of nothing else, and lets through
// what it does not decide: the command behaves as without it.
func TestSupervisorHoldsAndLetsThrough(t *testing.T) {
	s, err := Start(testConfig(t), helperCmd("names", false))
	if err != nil {
		t.Skipf("no seccomp user notification here: %v", err)
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("the command failed under the supervisor: %v", err)
	}
	if got := s.Stats().Notifications.Load(); got < 10 {
		t.Errorf("%d calls held, want at least the ten getsockname", got)
	}
	// The filter is on the command and on the thread that started it, not on this one.
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	before := s.Stats().Notifications.Load()
	if _, err := syscall.Getsockname(fd); err != nil {
		t.Fatal(err)
	}
	if s.Stats().Notifications.Load() != before {
		t.Error("the supervisor's own process was held")
	}
}

func TestSupervisorRefusesAnIncompleteConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Resolver = nil
	if _, err := Start(cfg, helperCmd("names", false)); err == nil {
		t.Error("started without a resolver")
	}
}

// The log has a line for the first connections and says that it will not have the rest.
func TestConnectionsAreLoggedOnlyAtFirst(t *testing.T) {
	var lines []string
	s := &Supervisor{cfg: Config{Logf: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }}}
	for i := 0; i < 3*connLogFirst; i++ {
		s.logConn("connection %d", i)
	}
	if len(lines) != connLogFirst+1 || lines[0] != "connection 0" || lines[connLogFirst-1] != fmt.Sprintf("connection %d", connLogFirst-1) ||
		!strings.Contains(lines[connLogFirst], "counted, not logged") {
		t.Errorf("%d lines: %q", len(lines), lines)
	}
}
