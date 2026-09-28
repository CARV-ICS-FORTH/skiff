package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"plaid/pkg/api"
)

func TestPassthroughWaitsForTermCompletion(t *testing.T) {
	if os.Getenv("PLAID_TEST_PASSTHROUGH") == "1" {
		os.Exit(execPassthroughCommand("/bin/sh", []string{os.Getenv("PLAID_TEST_SCRIPT")}))
	}
	root := t.TempDir()
	script := filepath.Join(root, "handler.sh")
	ready, complete := filepath.Join(root, "ready"), filepath.Join(root, "complete")
	if err := os.WriteFile(script, []byte("trap 'sleep 0.2; touch \"$PLAID_TEST_COMPLETE\"; exit 0' TERM\ntouch \"$PLAID_TEST_READY\"\nsleep 30 &\nwait $!\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPassthroughWaitsForTermCompletion$")
	cmd.Env = append(os.Environ(), "PLAID_TEST_PASSTHROUGH=1", "PLAID_TEST_SCRIPT="+script, "PLAID_TEST_READY="+ready, "PLAID_TEST_COMPLETE="+complete)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	completed := false
	t.Cleanup(func() {
		if !completed {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatal("passthrough workload did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	completed = err == nil
	if err != nil {
		t.Fatalf("wrapper ended before graceful completion: %v", err)
	}
	if _, err := os.Stat(complete); err != nil {
		t.Fatalf("waiter completed before TERM work: %v", err)
	}
}

func TestContainerSelectionWaitsForVerifiedNetwork(t *testing.T) {
	pids := []int{10, 11, 12, 13}
	comm := map[int]string{10: "ip", 11: "pause", 12: "pause", 13: "sh"}
	descendant := func(pid int) bool { return pid != 12 }
	ready := map[int]bool{10: true, 12: true}
	check := func(want int) {
		t.Helper()
		if got := selectReadyContainerPID(pids, comm, descendant, func(pid int) bool { return ready[pid] }); got != want {
			t.Fatalf("selected PID %d, want %d", got, want)
		}
	}
	// A setup command, an unconfigured pause, and another launcher's pause
	// cannot publish this launcher's readiness handshake.
	check(0)
	ready[11] = true
	check(11)
	ready[11] = false
	ready[13] = true
	check(13)
}

func TestTapResolverRequiresConfiguration(t *testing.T) {
	status := &api.Response{GatewayMode: "tap", GatewayIP: "10.244.1.1"}
	_, _, dns := resolveNetworkParams(status, "10.244.1.4/24", "", "")
	if dns != "" {
		t.Fatalf("unexpected inner slirp resolver %q", dns)
	}
	status.Resolver = "10.43.0.10"
	_, _, dns = resolveNetworkParams(status, "10.244.1.4/24", "", "")
	if dns != status.Resolver {
		t.Fatalf("expected published resolver, got %q", dns)
	}
}

func TestParsePlaidOptions(t *testing.T) {
	args := []string{
		"--host-networking",
		"--ip=10.244.1.20/24",
		"--gateway", "10.244.1.1",
		"--dns=10.244.1.3",
		"--socket=/run/test/plaidd.sock",
		"--bind=/data:/data",
		"--cleanenv",
		"-B", "/tmp:/tmp",
		"alpine.sif",
		"ping", "-c", "3", "10.244.1.1",
	}

	opts, remaining := parsePlaidOptions(args)

	if !opts.HostNetworking {
		t.Errorf("expected HostNetworking to be true")
	}
	if opts.IP != "10.244.1.20/24" {
		t.Errorf("expected IP '10.244.1.20/24', got %q", opts.IP)
	}
	if opts.Gateway != "10.244.1.1" {
		t.Errorf("expected Gateway '10.244.1.1', got %q", opts.Gateway)
	}
	if opts.DNS != "10.244.1.3" {
		t.Errorf("expected DNS '10.244.1.3', got %q", opts.DNS)
	}
	if opts.Socket != "/run/test/plaidd.sock" {
		t.Errorf("expected Socket '/run/test/plaidd.sock', got %q", opts.Socket)
	}
	if opts.Bind != "/data:/data" {
		t.Errorf("expected Bind '/data:/data', got %q", opts.Bind)
	}

	expectedRemaining := []string{
		"--cleanenv",
		"-B", "/tmp:/tmp",
		"alpine.sif",
		"ping", "-c", "3", "10.244.1.1",
	}
	if len(remaining) != len(expectedRemaining) {
		t.Fatalf("expected %d remaining args, got %d: %v", len(expectedRemaining), len(remaining), remaining)
	}
	for i, r := range remaining {
		if r != expectedRemaining[i] {
			t.Errorf("at index %d: expected %q, got %q", i, expectedRemaining[i], r)
		}
	}
}

func TestSplitFlagsAndPositional(t *testing.T) {
	args := []string{
		"--cleanenv",
		"-B", "/data:/data",
		"--contain",
		"alpine.sif",
		"echo", "hello", "world",
	}

	flags, posArgs := splitFlagsAndPositional(args)

	expectedFlags := []string{"--cleanenv", "-B", "/data:/data", "--contain"}
	expectedPosArgs := []string{"alpine.sif", "echo", "hello", "world"}

	if strings.Join(flags, " ") != strings.Join(expectedFlags, " ") {
		t.Errorf("expected flags %v, got %v", expectedFlags, flags)
	}
	if strings.Join(posArgs, " ") != strings.Join(expectedPosArgs, " ") {
		t.Errorf("expected posArgs %v, got %v", expectedPosArgs, posArgs)
	}
}

func TestSplitImageAndCommand(t *testing.T) {
	args := []string{
		"-B", "/data:/data",
		"docker://alpine:latest",
		"sh", "-c", "echo hello",
	}

	flags, image, cmdArgs := splitImageAndCommand(args)

	if len(flags) != 2 || flags[0] != "-B" || flags[1] != "/data:/data" {
		t.Errorf("unexpected flags: %v", flags)
	}
	if image != "docker://alpine:latest" {
		t.Errorf("expected image 'docker://alpine:latest', got %q", image)
	}
	if len(cmdArgs) != 3 || cmdArgs[0] != "sh" || cmdArgs[1] != "-c" || cmdArgs[2] != "echo hello" {
		t.Errorf("unexpected cmdArgs: %v", cmdArgs)
	}
}

func TestResolveNetworkParams(t *testing.T) {
	status := &api.Response{
		GatewayIP: "10.244.2.1",
	}

	ipStr, gwStr, dnsStr := resolveNetworkParams(status, "10.244.2.15/24", "", "")
	if ipStr != "10.244.2.15/24" {
		t.Errorf("expected IP '10.244.2.15/24', got %q", ipStr)
	}
	if gwStr != "10.244.2.1" {
		t.Errorf("expected GW '10.244.2.1', got %q", gwStr)
	}
	if dnsStr != "10.244.2.3" {
		t.Errorf("expected DNS '10.244.2.3', got %q", dnsStr)
	}

	// Case with nil status and explicit DNS
	ipStr2, gwStr2, dnsStr2 := resolveNetworkParams(nil, "10.244.5.10/24", "", "8.8.8.8")
	if ipStr2 != "10.244.5.10/24" {
		t.Errorf("expected IP '10.244.5.10/24', got %q", ipStr2)
	}
	if gwStr2 != "10.244.5.1" {
		t.Errorf("expected derived GW '10.244.5.1', got %q", gwStr2)
	}
	if dnsStr2 != "8.8.8.8" {
		t.Errorf("expected DNS '8.8.8.8', got %q", dnsStr2)
	}
}

func TestSplitFlagsWithNoMountAndNetnsPath(t *testing.T) {
	args := []string{
		"--nv",
		"--cleanenv",
		"--writable-tmpfs",
		"--no-mount", "home,bind-paths",
		"--unsquash",
		"--netns-path", "/proc/12345/ns/net",
		"/images/pause.sif",
		"/pause",
	}

	flags, image, cmdArgs := splitImageAndCommand(args)
	if image != "/images/pause.sif" {
		t.Errorf("expected image '/images/pause.sif', got %q", image)
	}
	if len(cmdArgs) != 1 || cmdArgs[0] != "/pause" {
		t.Errorf("expected cmdArgs ['/pause'], got %v", cmdArgs)
	}
	joinedFlags := strings.Join(flags, " ")
	if !strings.Contains(joinedFlags, "--no-mount home,bind-paths") {
		t.Errorf("expected --no-mount home,bind-paths in flags, got %s", joinedFlags)
	}
	if !strings.Contains(joinedFlags, "--netns-path /proc/12345/ns/net") {
		t.Errorf("expected --netns-path /proc/12345/ns/net in flags, got %s", joinedFlags)
	}
}

func TestMergePlaidOptions(t *testing.T) {
	base := PlaidOptions{
		HostNetworking: true,
		DNS:            "1.1.1.1",
		Socket:         "/run/base/plaidd.sock",
		Binds:          []string{"/a:/a"},
	}
	override := PlaidOptions{
		IP:      "10.244.1.5/24",
		Gateway: "10.244.1.1",
		Binds:   []string{"/b:/b"},
	}

	merged := mergePlaidOptions(base, override)
	if !merged.HostNetworking {
		t.Errorf("expected HostNetworking true")
	}
	if merged.DNS != "1.1.1.1" {
		t.Errorf("expected DNS '1.1.1.1', got %q", merged.DNS)
	}
	if merged.Socket != "/run/base/plaidd.sock" {
		t.Errorf("expected Socket '/run/base/plaidd.sock', got %q", merged.Socket)
	}
	if merged.IP != "10.244.1.5/24" {
		t.Errorf("expected IP '10.244.1.5/24', got %q", merged.IP)
	}
	if merged.Gateway != "10.244.1.1" {
		t.Errorf("expected Gateway '10.244.1.1', got %q", merged.Gateway)
	}
	if len(merged.Binds) != 2 || merged.Binds[0] != "/a:/a" || merged.Binds[1] != "/b:/b" {
		t.Errorf("expected 2 binds [/a:/a, /b:/b], got %v", merged.Binds)
	}
}

func TestWorkloadArgPreservation(t *testing.T) {
	// Diagnostic from expert review: exec alpine.sif program --bind app-value --ip app-address
	args := []string{
		"--bind", "/host:/cont",
		"alpine.sif",
		"program",
		"--bind", "app-value",
		"--ip", "app-address",
		"-c", "config.yaml",
	}

	opts, flags, image, cmdArgs := parseWrapperArgs(args)

	if image != "alpine.sif" {
		t.Fatalf("expected image 'alpine.sif', got %q", image)
	}
	if opts.Bind != "/host:/cont" {
		t.Errorf("expected wrapper bind '/host:/cont', got %q", opts.Bind)
	}
	if opts.IP != "" {
		t.Errorf("expected wrapper IP to be empty, got stolen value %q", opts.IP)
	}

	expectedCmdArgs := []string{"program", "--bind", "app-value", "--ip", "app-address", "-c", "config.yaml"}
	if strings.Join(cmdArgs, " ") != strings.Join(expectedCmdArgs, " ") {
		t.Fatalf("expected workload args %v, got %v", expectedCmdArgs, cmdArgs)
	}
	_ = flags
}

func TestShortFlagContainDoesNotEatImage(t *testing.T) {
	// In Apptainer, -c is --contain (boolean), not taking an argument!
	args := []string{
		"-c",
		"alpine.sif",
		"sh",
	}

	_, flags, image, cmdArgs := parseWrapperArgs(args)
	if len(flags) != 1 || flags[0] != "-c" {
		t.Errorf("expected flags ['-c'], got %v", flags)
	}
	if image != "alpine.sif" {
		t.Errorf("expected image 'alpine.sif', got %q (did -c eat the image?)", image)
	}
	if len(cmdArgs) != 1 || cmdArgs[0] != "sh" {
		t.Errorf("expected cmdArgs ['sh'], got %v", cmdArgs)
	}
}

func TestExplicitDoubleDashBoundary(t *testing.T) {
	args := []string{
		"--ip", "10.244.1.50",
		"--",
		"alpine.sif",
		"--ip", "workload-ip",
	}

	opts, _, image, cmdArgs := parseWrapperArgs(args)
	if opts.IP != "10.244.1.50" {
		t.Errorf("expected wrapper IP '10.244.1.50', got %q", opts.IP)
	}
	if image != "alpine.sif" {
		t.Errorf("expected image 'alpine.sif', got %q", image)
	}
	if len(cmdArgs) != 2 || cmdArgs[0] != "--ip" || cmdArgs[1] != "workload-ip" {
		t.Errorf("expected cmdArgs ['--ip', 'workload-ip'], got %v", cmdArgs)
	}
}

func TestRandomID(t *testing.T) {
	id1 := randomID("test")
	id2 := randomID("test")
	if id1 == id2 {
		t.Errorf("expected random IDs to be distinct, got %q and %q", id1, id2)
	}
	if !strings.HasPrefix(id1, "test-") || len(id1) < 10 {
		t.Errorf("unexpected ID format: %q", id1)
	}
}
