//go:build linux

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"plaid/pkg/api"
	"plaid/pkg/tap"
)

// queueHandler records the queues of the TAP that each AddEndpoint is given.
type queueHandler struct {
	testAPIHandler
	mu2    sync.Mutex
	queues map[string]int // pod ID -> descriptors received
	multi  map[string]bool
}

func (h *queueHandler) HandleAddEndpoint(req *api.Request, tapFDs []int) error {
	h.mu2.Lock()
	defer h.mu2.Unlock()
	h.queues[req.PodID], h.multi[req.PodID] = len(tapFDs), true
	for _, fd := range tapFDs {
		var ifr struct {
			name  [16]byte
			flags uint16
			pad   [22]byte
		}
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tap.TUNGETIFF, uintptr(unsafe.Pointer(&ifr))); errno != 0 || ifr.flags&tap.IFF_MULTI_QUEUE == 0 {
			h.multi[req.PodID] = false
		}
		_ = os.NewFile(uintptr(fd), "tap").Close()
	}
	return nil
}

// inNamespace builds the plugin and runs the test again in a user and network namespace of its
// own, in which it makes TAPs as it would in a pod's. In the namespace it returns the
// plugin's path and true; outside it runs the child and returns false after the child
// reported.
func inNamespace(t *testing.T, test string) (bin string, inside bool) {
	t.Helper()
	if bin = os.Getenv("PLAID_QUEUES_BIN"); bin != "" {
		return bin, true
	}
	if err := exec.Command("unshare", "-Urn", "true").Run(); err != nil {
		t.Skipf("cannot create a user and network namespace here: %v", err)
	}
	bin = filepath.Join(t.TempDir(), "plaid")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("Failed to build plaid binary: %v, output: %s", err, out)
	}
	cmd := exec.Command("unshare", "-Urn", os.Args[0], "-test.run=^"+test+"$", "-test.v")
	cmd.Env = append(os.Environ(), "PLAID_QUEUES_BIN="+bin)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if strings.Contains(string(out), "--- SKIP") {
		t.Skip("skipped in the namespace")
	}
	if err != nil {
		t.Fatalf("the namespace child failed: %v", err)
	}
	return bin, false
}

func newQueueServer(t *testing.T) (string, *queueHandler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sock := filepath.Join(t.TempDir(), "plaidd.sock")
	h := &queueHandler{testAPIHandler: testAPIHandler{endpoints: map[string]*api.Request{}}, queues: map[string]int{}, multi: map[string]bool{}}
	srv := api.NewServer(sock, h, h, h)
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	srv.MarkReady()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, h
}

// The plugin makes the TAP with the number of queues that its config asks for (four if it
// says nothing, at most tap.MaxQueues) and passes a descriptor for each. It runs in a user
// and network namespace of its own, in which it makes the TAPs, as it would in a pod's.
func TestCNIPluginPassesTheQueuesOfTheTAP(t *testing.T) {
	bin, inside := inNamespace(t, "TestCNIPluginPassesTheQueuesOfTheTAP")
	if !inside {
		return
	}
	sock, h := newQueueServer(t)
	for i, c := range []struct {
		conf   string
		queues int
	}{
		{``, defaultTapQueues},
		{`"tapQueues": 1,`, 1},
		{`"tapQueues": 2,`, 2},
		{`"tapQueues": 99,`, tap.MaxQueues},
	} {
		ifName := fmt.Sprintf("eth%d", i)
		id := canonicalEndpointID("pod", ifName)
		conf := fmt.Sprintf(`{"cniVersion": "0.4.0", "name": "cbr0", "type": "plaid", %s "socketPath": %q, "mtu": 1450, "ip": "10.244.%d.7/24", "gateway": "10.244.%d.1"}`, c.conf, sock, i+1, i+1)
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(),
			"CNI_COMMAND=ADD", "CNI_CONTAINERID=pod", "CNI_IFNAME="+ifName,
			fmt.Sprintf("CNI_NETNS=/proc/%d/ns/net", os.Getpid()))
		cmd.Stdin = bytes.NewReader([]byte(conf))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ADD with %q: %v: %s", c.conf, err, out)
		}
		h.check(t, id, c.queues, c.conf)
	}
}

// check reports whether the endpoint id was given want descriptors, of a multi-queue TAP if
// there are several.
func (h *queueHandler) check(t *testing.T, id string, want int, what string) {
	t.Helper()
	h.mu2.Lock()
	got, multi := h.queues[id], h.multi[id]
	h.mu2.Unlock()
	if got != want {
		t.Errorf("%s: %d descriptors, want %d", what, got, want)
	}
	if multi != (want > 1) {
		t.Errorf("%s: the descriptors are of a multi-queue TAP: %v", what, multi)
	}
}

// plaid init and plaid exec make the same TAP: --queues, else $PLAID_TAP_QUEUES, else the default,
// clamped to tap.MaxQueues, one for a single-queue TAP, and a negative or invalid number is
// refused without a TAP made.
func TestInitPassesTheQueuesOfTheTAP(t *testing.T) {
	bin, inside := inNamespace(t, "TestInitPassesTheQueuesOfTheTAP")
	if !inside {
		return
	}
	sock, h := newQueueServer(t)
	for i, c := range []struct {
		sub    string
		flags  []string
		env    string
		queues int // 0: refused
	}{
		{"init", nil, "", defaultTapQueues},
		{"init", []string{"--queues", "1"}, "", 1},
		{"init", []string{"--queues", "99"}, "", tap.MaxQueues},
		{"init", nil, "2", 2},
		{"init", []string{"--queues", "3"}, "2", 3}, // the flag wins
		{"exec", []string{"--queues", "2"}, "", 2},
		{"init", []string{"--queues", "-1"}, "", 0},
		{"init", nil, "many", 0},
	} {
		id := fmt.Sprintf("init%d", i)
		args := []string{c.sub, "--ip", fmt.Sprintf("10.244.%d.7/24", i+1), "--gateway", fmt.Sprintf("10.244.%d.1", i+1),
			"--socket", sock, "--container-id", id, "--ifname", fmt.Sprintf("eth%d", i), "--mtu", "1450"}
		args = append(args, c.flags...)
		if c.sub == "exec" {
			args = append(args, "--", "/bin/true")
		}
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "PLAID_TAP_QUEUES="+c.env)
		out, err := cmd.CombinedOutput()
		what := fmt.Sprintf("plaid %s %v with PLAID_TAP_QUEUES=%q", c.sub, c.flags, c.env)
		if c.queues == 0 {
			if err == nil {
				t.Errorf("%s was accepted", what)
			}
			if h.queues[id] != 0 {
				t.Errorf("%s: a TAP was passed", what)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v: %s", what, err, out)
		}
		h.check(t, id, c.queues, what)
	}
}
