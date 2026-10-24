package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"plaid/pkg/api"
	"plaid/pkg/direct"
	"plaid/pkg/netcfg"
	"sigs.k8s.io/yaml"
)

func init() {
	// The supervisor's filter is installed on a thread that ends when it has started the
	// container. The main thread cannot end (the runtime keeps it, idle, to the end of the
	// process), so a filter put on it would stay, and the supervisor would not learn that
	// no task with the filter is left. The main goroutine keeps the main thread to itself.
	runtime.LockOSThread()
}

// launchHostNetworking runs a container that shares the network namespace it was given
// (the pod's, by --netns-path): plaidtainer is replaced by Apptainer, so that the one who
// started it watches Apptainer's own life. A container that marks direct ports, to connect
// to or to listen on, is the exception: plaidtainer stays, as the supervisor that decides
// the container's connect, bind, accept and address calls (see plaid/pkg/direct), and ends
// as Apptainer ends.
func launchHostNetworking(apptainerBin string, args []string, opts PlaidOptions) int {
	if opts.DirectConnectPorts == "" && opts.DirectListenPorts == "" {
		return execPassthroughCommand(apptainerBin, args)
	}
	var cfg direct.Config
	err := checkPtraceScope(yamaPtraceScopeFile)
	if err == nil {
		cfg, err = directConfig(opts)
	}
	if err != nil {
		// The container is worth more running than not running: without the direct path
		// its connections are on the pod network, which is where they were.
		fmt.Fprintf(os.Stderr, "[plaidtainer] the direct path is off: %v\n", err)
		return execPassthroughCommand(apptainerBin, args)
	}
	cmd := exec.Command(apptainerBin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	sup, err := direct.Start(cfg, cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[plaidtainer] the direct path is off: %v\n", err)
		return execPassthroughCommand(apptainerBin, args)
	}

	// Whoever signals a container signals its process group, of which plaidtainer is
	// a member (Skifflet: syscall.Kill(-pid, sig)), so Apptainer gets every signal itself;
	// plaidtainer must not end before it, and must not pass the signals on a second time.
	signals := make(chan os.Signal, 16)
	signal.Notify(signals)
	go func() {
		for range signals {
		}
	}()
	err = sup.Wait()
	code, sig := directExit(cmd, err)
	if sig != 0 {
		// Apptainer ended of a signal (it raises on itself the one that killed the
		// container's process): so does plaidtainer, which is how the one who started it reads it.
		signal.Reset()
		// The Go runtime keeps handlers of its own for the signals of a crash (SIGSEGV,
		// SIGILL, SIGABRT...) that answer with a dump of plaidtainer's goroutines in the
		// container's log: the default action is put back.
		var dfl [4]uintptr // the kernel's struct sigaction, all zero: SIG_DFL
		_, _, _ = syscall.RawSyscall6(syscall.SYS_RT_SIGACTION, uintptr(sig), uintptr(unsafe.Pointer(&dfl)), 0, 8, 0, 0)
		_ = syscall.Kill(os.Getpid(), sig)
		time.Sleep(time.Second) // a signal that does nothing, or is not delivered at once
	}
	return code
}

// directExit says how the command ended: its exit status, or the signal that killed it.
func directExit(cmd *exec.Cmd, waitErr error) (code int, sig syscall.Signal) {
	if st, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && st.Signaled() {
		return 128 + int(st.Signal()), st.Signal()
	}
	if code := cmd.ProcessState.ExitCode(); code >= 0 {
		return code, 0
	}
	if waitErr != nil {
		return 1, 0
	}
	return 0, 0
}

// clusterKey is the key of the direct path from the private key of the current context's user
// in the cluster's kubeconfig at path (see direct.DeriveKey).
func clusterKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		CurrentContext string `json:"current-context"`
		Contexts       []struct {
			Name    string `json:"name"`
			Context struct {
				User string `json:"user"`
			} `json:"context"`
		} `json:"contexts"`
		Users []struct {
			Name string `json:"name"`
			User struct {
				ClientKeyData []byte `json:"client-key-data"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	for _, c := range cfg.Contexts {
		if c.Name != cfg.CurrentContext {
			continue
		}
		for _, u := range cfg.Users {
			if u.Name == c.Context.User && len(u.User.ClientKeyData) > 0 {
				return direct.DeriveKey(u.User.ClientKeyData), nil
			}
		}
	}
	return nil, errors.New("no client-key-data for the current context's user")
}

// yamaPtraceScopeFile is where the kernel says how far Yama restricts ptrace; the file is
// not there where Yama is not in the kernel.
const yamaPtraceScopeFile = "/proc/sys/kernel/yama/ptrace_scope"

// checkPtraceScope says whether the supervisor will be allowed to look into the container: it
// reads the container's descriptors (pidfd_getfd) and memory (process_vm_readv and writev),
// which is ptrace access, and has it as the container's ancestor under Yama's scope 1 (the
// nodes' setting) or none. Scope 2 gives it to a process with CAP_SYS_PTRACE only, which
// plaidtainer does not have, and scope 3 to none. Without the check every call would fail
// to be looked into, and the container would be left with a supervisor that cannot decide
// anything. A file that is not there, cannot be read or says something else is no Yama that
// is known to refuse.
func checkPtraceScope(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if scope, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && scope >= 2 {
		return fmt.Errorf("Yama's ptrace_scope is %d (%s): the supervisor must be allowed to trace the container as its parent, which takes 1 or 0", scope, path)
	}
	return nil
}

// directConfig is what the supervisor of a container needs, from its options.
func directConfig(opts PlaidOptions) (direct.Config, error) {
	connectPorts, err := direct.ParsePorts(opts.DirectConnectPorts)
	if err != nil {
		return direct.Config{}, fmt.Errorf("--direct-connect-ports: %w", err)
	}
	listenPorts, err := direct.ParsePorts(opts.DirectListenPorts)
	if err != nil {
		return direct.Config{}, fmt.Errorf("--direct-listen-ports: %w", err)
	}
	podIP, err := netip.ParseAddr(opts.PodIP)
	if err != nil || !podIP.Is4() {
		return direct.Config{}, fmt.Errorf("--pod-ip %q is not an IPv4 address", opts.PodIP)
	}
	if opts.DirectSocket == "" {
		return direct.Config{}, fmt.Errorf("no socket factory (--direct-socket, or $PLAID_DIRECT_SOCKET)")
	}
	// What the deployment says, in the bubble's environment (the container does not get it):
	// the cluster's kubeconfig, which the key is derived from, and how long to wait for an ack.
	keyFile := os.Getenv(netcfg.EnvDirectKeyFile)
	if keyFile == "" {
		return direct.Config{}, fmt.Errorf("no key (%s): the cluster's kubeconfig", netcfg.EnvDirectKeyFile)
	}
	key, err := clusterKey(keyFile)
	if err != nil {
		return direct.Config{}, fmt.Errorf("the key from %s: %w", keyFile, err)
	}
	var ackTimeout time.Duration
	if v := os.Getenv(netcfg.EnvDirectAckTimeout); v != "" {
		if ackTimeout, err = time.ParseDuration(v); err != nil || ackTimeout <= 0 {
			return direct.Config{}, fmt.Errorf("%s=%q is not a duration", netcfg.EnvDirectAckTimeout, v)
		}
	}
	portBase := direct.DefaultPortBase
	if v := os.Getenv(netcfg.EnvDirectPortBase); v != "" {
		if portBase, err = strconv.Atoi(v); err != nil {
			return direct.Config{}, fmt.Errorf("%s=%q is not a port", netcfg.EnvDirectPortBase, v)
		}
	}
	if err := direct.CheckPortBase(portBase); err != nil {
		return direct.Config{}, fmt.Errorf("%s: %w", netcfg.EnvDirectPortBase, err)
	}
	// This node's address, if the deployment says it ($HOST_IP in a bubble); else plaidd's
	// overlay address is it.
	hostIP, _ := netip.ParseAddr(os.Getenv("HOST_IP"))
	client := api.NewClient(opts.Socket)
	resolver := direct.NewAPIResolver(client, hostIP, portBase)
	cfg := direct.Config{
		ConnectPorts: connectPorts,
		ListenPorts:  listenPorts,
		PodIP:        podIP,
		Key:          key,
		AckTimeout:   ackTimeout,
		Factory:      direct.NewFactoryClient(opts.DirectSocket),
		Resolver:     resolver,
	}
	cfg.Report = counterReporter(client, directName(opts.DirectLog), func(format string, args ...any) {
		if cfg.Logf != nil {
			cfg.Logf(format, args...)
		}
	})
	if opts.DirectLog != "" {
		f, err := os.OpenFile(opts.DirectLog, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			return direct.Config{}, err
		}
		cfg.Logf = func(format string, args ...any) {
			fmt.Fprintf(f, time.Now().Format("15:04:05.000 ")+format+"\n", args...)
		}
		resolver.Logf = cfg.Logf
	}
	return cfg, nil
}

// directName is the container a supervisor works for, as "namespace/pod/container", from the
// path of its log, which Skifflet puts at <pods>/<namespace>/<pod>_<uid>/job/<container>.direct.log
// (a pod's name has no underscore). Without a log it is the supervisor's process.
func directName(logPath string) string {
	if logPath == "" {
		return "pid " + strconv.Itoa(os.Getpid())
	}
	container := strings.TrimSuffix(filepath.Base(logPath), ".direct.log")
	podDir := filepath.Dir(filepath.Dir(logPath))
	pod, _, _ := strings.Cut(filepath.Base(podDir), "_")
	return filepath.Base(filepath.Dir(podDir)) + "/" + pod + "/" + container
}

// counterReporter gives the supervisor's counters to plaidd. A report that fails is logged
// the first time (plaidd may not be there, or may be an older one, and the container does
// not need it).
func counterReporter(client *api.Client, name string, logf func(format string, args ...any)) func(api.DirectCounters, bool) {
	var failed atomic.Bool
	return func(c api.DirectCounters, ended bool) {
		err := client.ReportDirect(api.DirectReport{Supervisor: name, PID: os.Getpid(), Ended: ended, Counters: c})
		if err != nil && !failed.Swap(true) {
			logf("reporting the counters to plaidd: %v", err)
		}
	}
}
