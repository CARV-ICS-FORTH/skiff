package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"plaid/pkg/api"
	"plaid/pkg/direct"
	"plaid/pkg/netcfg"
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
		"--contain",
		"--cleanenv",
		"--writable-tmpfs",
		"--device", "nvidia.com/gpu=0",
		"--device", "nvidia.com/gpu=1",
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
	// (a CDI device name after --device is its value, not the image)
	if !strings.Contains(joinedFlags, "--device nvidia.com/gpu=0 --device nvidia.com/gpu=1") {
		t.Errorf("expected both --device flags with their values in flags, got %s", joinedFlags)
	}
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

// The wrapper's own settings follow the shared rules: an explicit value, then the
// environment, then (for the socket) the default; an MTU nobody states stays unset
// here, so that plaidd's applies.
func TestEnsureDefaults_SocketAndMTUFollowTheSharedRules(t *testing.T) {
	t.Setenv("PLAID_SOCKET_PATH", "")
	t.Setenv("PLAID_MTU", "")
	var opts PlaidOptions
	opts.EnsureDefaults()
	if opts.Socket != "/run/plaid/plaidd.sock" || opts.MTU != 0 {
		t.Fatalf("defaults: socket %q, MTU %d", opts.Socket, opts.MTU)
	}

	t.Setenv("PLAID_SOCKET_PATH", "/run/lab/plaidd.sock")
	t.Setenv("PLAID_MTU", "1400")
	opts = PlaidOptions{}
	opts.EnsureDefaults()
	if opts.Socket != "/run/lab/plaidd.sock" || opts.MTU != 1400 {
		t.Fatalf("environment: socket %q, MTU %d", opts.Socket, opts.MTU)
	}

	opts = PlaidOptions{Socket: "/explicit.sock", MTU: 1300}
	opts.EnsureDefaults()
	if opts.Socket != "/explicit.sock" || opts.MTU != 1300 {
		t.Fatalf("explicit over environment: socket %q, MTU %d", opts.Socket, opts.MTU)
	}
}

// Standalone slirp: without a stated resolver the container gets slirp's, the third
// address of the gateway's network; an explicit one is taken as it is; and inside a
// cluster (tap) a missing resolver is never made up.
func TestResolveNetworkParams_StandaloneResolverAndOverrides(t *testing.T) {
	slirp := &api.Response{GatewayMode: "slirp", GatewayIP: "10.0.2.2"}
	if _, _, dns := resolveNetworkParams(slirp, "10.0.2.15/24", "", ""); dns != "10.0.2.3" {
		t.Errorf("standalone default resolver %q, want 10.0.2.3", dns)
	}
	if _, _, dns := resolveNetworkParams(slirp, "10.0.2.15/24", "", "9.9.9.9"); dns != "9.9.9.9" {
		t.Errorf("explicit resolver %q", dns)
	}
	slirp.Resolver = "192.0.2.53"
	if _, _, dns := resolveNetworkParams(slirp, "10.0.2.15/24", "", ""); dns != "192.0.2.53" {
		t.Errorf("resolver published by plaidd %q", dns)
	}
	tap := &api.Response{GatewayMode: "tap", GatewayIP: "10.200.3.1"}
	if _, _, dns := resolveNetworkParams(tap, "10.200.3.4/24", "", ""); dns != "" {
		t.Errorf("a cluster must state its resolver; got %q", dns)
	}
}

func TestParseDirectPathOptions(t *testing.T) {
	opts, flags, image, workload := parseWrapperArgs([]string{
		"--direct-ports", "5201,5203", "--pod-ip=10.244.1.9", "--direct-socket", "/tmp/f.sock", "--direct-log=/tmp/d.log",
		"--pid", "--no-init", "--netns-path", "/proc/12/ns/net", "img.sif", "iperf3", "-s", "--pod-ip", "x",
	})
	if opts.DirectPorts != "5201,5203" || opts.PodIP != "10.244.1.9" || opts.DirectSocket != "/tmp/f.sock" || opts.DirectLog != "/tmp/d.log" {
		t.Errorf("options: %+v", opts)
	}
	if strings.Join(flags, " ") != "--pid --no-init --netns-path /proc/12/ns/net" {
		t.Errorf("Apptainer flags: %q", flags)
	}
	// What follows the image is the container's, whatever it looks like.
	if image != "img.sif" || strings.Join(workload, " ") != "iperf3 -s --pod-ip x" {
		t.Errorf("image %q, workload %q", image, workload)
	}
	merged := mergePlaidOptions(PlaidOptions{DirectSocket: "/g.sock"}, opts)
	if merged.DirectSocket != "/tmp/f.sock" || merged.DirectPorts != "5201,5203" {
		t.Errorf("merged: %+v", merged)
	}
	t.Setenv("PLAID_DIRECT_SOCKET", "/env.sock")
	d := PlaidOptions{}
	d.EnsureDefaults()
	if d.DirectSocket != "/env.sock" {
		t.Errorf("the environment's factory socket was not used: %q", d.DirectSocket)
	}
}

// writeKubeconfig makes a cluster kubeconfig whose user has a private key, and points the
// bubble's environment at it, as the bubble does.
func writeKubeconfig(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	yaml := "apiVersion: v1\nkind: Config\ncurrent-context: c\n" +
		"clusters: [{name: k, cluster: {server: 'https://127.0.0.1:6443'}}]\n" +
		"users: [{name: u, user: {client-key-data: " + base64.StdEncoding.EncodeToString([]byte(key)) + "}}]\n" +
		"contexts: [{name: c, context: {cluster: k, user: u}}]\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(netcfg.EnvDirectKeyFile, path)
	return path
}

func TestDirectConfigRefusals(t *testing.T) {
	writeKubeconfig(t, "a private key")
	good := PlaidOptions{DirectPorts: "5201", PodIP: "10.244.1.9", DirectSocket: "/tmp/f.sock", Socket: "/tmp/plaidd.sock"}
	if _, err := directConfig(good); err != nil {
		t.Fatalf("a complete configuration was refused: %v", err)
	}
	for name, mutate := range map[string]func(*PlaidOptions){
		"bad port":   func(o *PlaidOptions) { o.DirectPorts = "http" },
		"no pod IP":  func(o *PlaidOptions) { o.PodIP = "" },
		"IPv6 pod":   func(o *PlaidOptions) { o.PodIP = "fd00::1" },
		"no factory": func(o *PlaidOptions) { o.DirectSocket = "" },
	} {
		o := good
		mutate(&o)
		if _, err := directConfig(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The key is the cluster's: no key file, or a kubeconfig without a key, is no direct path.
	t.Setenv(netcfg.EnvDirectKeyFile, "")
	if _, err := directConfig(good); err == nil {
		t.Error("accepted without a key file")
	}
	t.Setenv(netcfg.EnvDirectKeyFile, filepath.Join(t.TempDir(), "absent"))
	if _, err := directConfig(good); err == nil {
		t.Error("accepted with a key file that is not there")
	}
	writeKubeconfig(t, "a private key")
	t.Setenv(netcfg.EnvDirectAckTimeout, "soon")
	if _, err := directConfig(good); err == nil {
		t.Error("accepted an ack timeout that is no duration")
	}
	t.Setenv(netcfg.EnvDirectPortBase, "80")
	if _, err := directConfig(good); err == nil {
		t.Error("accepted a port base in the system's ports")
	}
	t.Setenv(netcfg.EnvDirectPortBase, "")
	t.Setenv(netcfg.EnvDirectAckTimeout, "250ms")
	if cfg, err := directConfig(good); err != nil || cfg.AckTimeout != 250*time.Millisecond {
		t.Errorf("ack timeout: %v, %v", cfg.AckTimeout, err)
	}
}

func TestMain(m *testing.M) {
	direct.RunWatchdogIfRequested() // the supervisors of the tests below start the test binary as their watchdog
	os.Exit(m.Run())
}

// launchDirect runs script as the container of a launcher with direct ports, in a process
// of its own (it replaces itself or ends in a signal), and returns how that ended.
func launchDirect(t *testing.T, script string, signalGroup syscall.Signal) (*exec.Cmd, error, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLaunchDirectHelper$")
	cmd.Env = append(os.Environ(), "PLAID_TEST_DIRECT=1", "PLAID_TEST_SCRIPT="+script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if signalGroup != 0 {
		time.Sleep(500 * time.Millisecond)
		syscall.Kill(-cmd.Process.Pid, signalGroup)
	}
	timer := time.AfterFunc(10*time.Second, func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	defer timer.Stop()
	err := cmd.Wait()
	if strings.Contains(stderr.String(), "the direct path is off") {
		t.Skip(strings.TrimSpace(stderr.String()))
	}
	return cmd, err, stderr.String()
}

func TestLaunchDirectHelper(t *testing.T) {
	if os.Getenv("PLAID_TEST_DIRECT") != "1" {
		t.Skip("a helper of the tests below")
	}
	opts := PlaidOptions{DirectPorts: "5201", PodIP: "10.244.1.9", DirectSocket: "/nonexistent/factory.sock", Socket: "/nonexistent/plaidd.sock"}
	os.Exit(launchHostNetworking("/bin/sh", []string{os.Getenv("PLAID_TEST_SCRIPT")}, opts))
}

func writeScript(t *testing.T, body string) string {
	path := filepath.Join(t.TempDir(), "container.sh")
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The launcher outlives nothing it supervises and ends as Apptainer ends: with its exit
// status, and of the signal that killed it.
func TestLaunchDirectEndsAsTheContainerDoes(t *testing.T) {
	writeKubeconfig(t, "a private key")
	cmd, err, _ := launchDirect(t, writeScript(t, "exit 7\n"), 0)
	if cmd.ProcessState.ExitCode() != 7 {
		t.Errorf("exit status %d (%v), want 7", cmd.ProcessState.ExitCode(), err)
	}
	cmd, _, _ = launchDirect(t, writeScript(t, "kill -KILL $$\n"), 0)
	if ws := cmd.ProcessState.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("ended %v, want killed by SIGKILL as the container's launcher was", ws)
	}
	cmd, _, _ = launchDirect(t, writeScript(t, "kill -TERM $$\n"), 0)
	if ws := cmd.ProcessState.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("ended %v, want killed by SIGTERM", ws)
	}
}

// A signal to the process group reaches the container; the launcher stays until it has
// finished its graceful work, as the one that started the launcher expects.
func TestLaunchDirectWaitsForTheContainerToTerminate(t *testing.T) {
	writeKubeconfig(t, "a private key")
	root := t.TempDir()
	complete := filepath.Join(root, "complete")
	script := writeScript(t, "trap 'sleep 0.3; touch "+complete+"; exit 0' TERM\nsleep 30 &\nwait $!\n")
	cmd, err, stderr := launchDirect(t, script, syscall.SIGTERM)
	if err != nil {
		t.Fatalf("the launcher ended badly: %v (%s)", err, stderr)
	}
	if _, err := os.Stat(complete); err != nil {
		t.Errorf("the launcher ended before the container finished: %v (%v)", err, cmd.ProcessState)
	}
}

func TestClusterKey(t *testing.T) {
	write := func(user string) string {
		path := filepath.Join(t.TempDir(), "kubeconfig")
		yaml := "apiVersion: v1\nkind: Config\ncurrent-context: c\n" +
			"clusters: [{name: k, cluster: {server: 'https://127.0.0.1:6443'}}]\n" +
			"users: [{name: other, user: {client-key-data: b3RoZXI=}}, {name: u, user: " + user + "}]\n" +
			"contexts: [{name: c, context: {cluster: k, user: u}}]\n"
		if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// The key follows the current context's user and the private key, and is not the private key itself.
	a, b := write("{client-key-data: Y2x1c3RlciBBIGtleQ==}"), write("{client-key-data: Y2x1c3RlciBCIGtleQ==}") // "cluster A key", "cluster B key"
	k1, err := clusterKey(a)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := clusterKey(a)
	k3, _ := clusterKey(b)
	if string(k1) != string(k2) || string(k1) == string(k3) || len(k1) != 32 || string(k1) != string(direct.DeriveKey([]byte("cluster A key"))) {
		t.Errorf("keys %x %x %x", k1, k2, k3)
	}
	if _, err := clusterKey(write("{token: abc}")); err == nil {
		t.Error("a user with no private key gave a key")
	}
	if _, err := clusterKey(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a kubeconfig that is not there gave a key")
	}
}

func TestDirectNameFollowsTheLogPath(t *testing.T) {
	for log, want := range map[string]string{
		"/tmp/.skiff.20002/.pods/default/dp-server-a_99ef93ff-4a28-42e5-83e0-042d4ebe3656/job/i5201.direct.log": "default/dp-server-a/i5201",
		"/s/pods/kube-system/coredns_1/job/coredns.direct.log":                                                  "kube-system/coredns/coredns",
	} {
		if got := directName(log); got != want {
			t.Errorf("directName(%q) = %q, want %q", log, got, want)
		}
	}
	if got := directName(""); got != "pid "+strconv.Itoa(os.Getpid()) {
		t.Errorf("without a log: %q", got)
	}
}

type reportRecorder struct{ got chan api.DirectReport }

func (r reportRecorder) HandleReportDirect(report *api.DirectReport) error {
	r.got <- *report
	return nil
}

// The counters reach plaidd named by the container, with the supervisor's process; a plaidd that is
// not there is told of once in the log and does not stop the supervisor.
func TestCounterReporterTellsPlaidd(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "p.sock")
	srv := api.NewServer(sock, nil, nil, nil)
	rec := reportRecorder{make(chan api.DirectReport, 4)}
	srv.SetDirectHandler(rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.MarkReady()
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	report := counterReporter(api.NewClient(sock), "default/p/c", logf)
	report(api.DirectCounters{Connects: 2}, true)
	r := <-rec.got
	if r.Supervisor != "default/p/c" || r.PID != os.Getpid() || !r.Ended || r.Counters.Connects != 2 {
		t.Errorf("the report is %+v", r)
	}
	srv.Close()
	report(api.DirectCounters{}, false)
	report(api.DirectCounters{}, false)
	if len(logged) != 1 {
		t.Errorf("a plaidd that is not there was logged %d times: %q", len(logged), logged)
	}
}

// containerHelper is the "container" of the test below: it records, every 50 ms, what a call
// that the supervisor holds (getsockname on a new socket) says.
func TestContainerHelper(t *testing.T) {
	if os.Getenv("PLAID_TEST_CONTAINER") != "1" {
		t.Skip("a helper of the test below")
	}
	log, err := os.OpenFile(os.Getenv("PLAID_TEST_LOG"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		os.Exit(2)
	}
	for {
		fd, _ := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		_, err := syscall.Getsockname(fd)
		syscall.Close(fd)
		fmt.Fprintf(log, "getsockname: %v\n", err)
		time.Sleep(50 * time.Millisecond)
	}
}

// A supervisor that is killed (SIGKILL: nothing in it runs) leaves a container whose
// getsockname, connect, bind and accept all fail with ENOSYS, which no program takes as a
// network that is down. The container must not outlive its supervisor.
func TestContainerDoesNotOutliveItsSupervisor(t *testing.T) {
	writeKubeconfig(t, "a private key")
	dir := t.TempDir()
	logFile, pidFile := filepath.Join(dir, "calls"), filepath.Join(dir, "pid")
	script := writeScript(t, "echo $$ > "+pidFile+"\nPLAID_TEST_CONTAINER=1 PLAID_TEST_LOG="+logFile+" exec "+os.Args[0]+" -test.run=^TestContainerHelper$\n")
	sup := exec.Command(os.Args[0], "-test.run=^TestLaunchDirectHelper$")
	sup.Env = append(os.Environ(), "PLAID_TEST_DIRECT=1", "PLAID_TEST_SCRIPT="+script)
	sup.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderrFile, err := os.Create(filepath.Join(dir, "stderr")) // a file: a pipe would be held open by the container
	if err != nil {
		t.Fatal(err)
	}
	sup.Stderr = stderrFile
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	stderr := func() string { b, _ := os.ReadFile(stderrFile.Name()); return string(b) }
	reaped := make(chan struct{})
	go func() { sup.Wait(); close(reaped) }()
	defer syscall.Kill(-sup.Process.Pid, syscall.SIGKILL)
	var container int
	for i := 0; i < 100 && container == 0; i++ {
		if b, err := os.ReadFile(pidFile); err == nil {
			container, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if strings.Contains(stderr(), "the direct path is off") {
		t.Skip(strings.TrimSpace(stderr()))
	}
	if container == 0 {
		t.Fatalf("the container did not start: %s", stderr())
	}
	time.Sleep(300 * time.Millisecond)
	if b, _ := os.ReadFile(logFile); !strings.Contains(string(b), "getsockname: <nil>") {
		t.Fatalf("the container's calls did not work under the supervisor: %q", b)
	}
	syscall.Kill(sup.Process.Pid, syscall.SIGKILL) // the supervisor alone
	<-reaped
	for i := 0; i < 60; i++ { // up to 3 s
		if syscall.Kill(container, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(logFile)
	t.Errorf("the container is still running after its supervisor was killed; its calls now say: %s", lastLine(string(b)))
	syscall.Kill(container, syscall.SIGKILL)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
