package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

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
// started it watches Apptainer's own life. A container that marks direct ports is the
// exception: plaidtainer stays, as the supervisor that decides the container's connect,
// bind, accept and address calls (see plaid/pkg/direct), and ends as Apptainer ends.
func launchHostNetworking(apptainerBin string, args []string, opts PlaidOptions) int {
	if opts.DirectPorts == "" {
		return execPassthroughCommand(apptainerBin, args)
	}
	cfg, err := directConfig(opts)
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

// directConfig is what the supervisor of a container needs, from its options.
func directConfig(opts PlaidOptions) (direct.Config, error) {
	ports, err := direct.ParsePorts(opts.DirectPorts)
	if err != nil {
		return direct.Config{}, fmt.Errorf("--direct-ports: %w", err)
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
	resolver := direct.NewAPIResolver(api.NewClient(opts.Socket), hostIP, portBase)
	cfg := direct.Config{
		Ports:      ports,
		PodIP:      podIP,
		Key:        key,
		AckTimeout: ackTimeout,
		Factory:    direct.NewFactoryClient(opts.DirectSocket),
		Resolver:   resolver,
	}
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
