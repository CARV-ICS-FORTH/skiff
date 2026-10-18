//go:build linux && (amd64 || arm64)

package direct

import (
	"net/netip"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// The tests run their "containers" as copies of the test binary: with DIRECT_TEST_HELPER
// set, TestMain runs the named scenario instead of the tests. A scenario prints what it
// saw, one line a thing, and exits.
var helpers = map[string]func(args []string) int{}

func TestMain(m *testing.M) {
	if name := os.Getenv("DIRECT_TEST_HELPER"); name != "" {
		time.AfterFunc(20*time.Second, func() { os.Exit(99) }) // a scenario that waits for what never comes
		os.Exit(helpers[name](os.Args[1:]))
	}
	os.Exit(m.Run())
}

// helperCmd is the command that runs scenario name; in a user and network namespace of its
// own if isolated (the namespaces of a pod: its addresses are not the host's, and
// whatever it does to the network stays in it). The tests that need them skip where the
// kernel does not give an unprivileged user the namespaces.
func helperCmd(name string, isolated bool, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "DIRECT_TEST_HELPER="+name)
	if isolated {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		}
	}
	return cmd
}

// requireNamespaces skips the test where an unprivileged user cannot make the namespaces.
func requireNamespaces(t *testing.T) {
	t.Helper()
	probe := exec.Command("true")
	probe.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	if err := probe.Run(); err != nil {
		t.Skipf("no user and network namespaces for an unprivileged user: %v", err)
	}
}

type fakeResolver map[string]Backend

func (f fakeResolver) Resolve(dst netip.AddrPort) (Backend, bool) {
	b, ok := f[dst.String()]
	return b, ok
}

// The pods of the tests are on the node whose address is the loopback, which is where their
// host sockets meet.
func (fakeResolver) HostOf(pod netip.Addr) (netip.Addr, bool) {
	if netip.MustParsePrefix("10.244.0.0/16").Contains(pod) {
		return netip.MustParseAddr("127.0.0.1"), true
	}
	return netip.Addr{}, false
}

// A pod's server is served at its own port on the host.
func (fakeResolver) HostPort(pod netip.AddrPort) (uint16, bool) { return pod.Port(), true }

var testKey = DeriveKey([]byte("the cluster's private key"))
