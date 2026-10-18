package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"plaid/pkg/netcfg"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func selectReadyContainerPID(pids []int, commOf map[int]string, descendant, ready func(int) bool) int {
	for _, pid := range pids {
		if commOf[pid] == "pause" && descendant(pid) && ready(pid) {
			return pid
		}
	}
	for _, pid := range pids {
		comm := commOf[pid]
		if comm == "" || comm == "ip" || comm == "nsenter" || strings.Contains(comm, "fuse") || strings.Contains(comm, "starter") || strings.Contains(comm, "apptainer") || strings.Contains(comm, "plaid") {
			continue
		}
		if descendant(pid) && ready(pid) {
			return pid
		}
	}
	return 0
}

// PlaidOptions captures Plaid-specific networking options extracted from CLI flags.
type PlaidOptions struct {
	HostNetworking bool
	IP             string
	Gateway        string
	DNS            string
	Socket         string
	Bind           string
	Binds          []string
	Binary         string
	MTU            int
	ReadinessFile  string

	// The direct path (see plaid/pkg/direct): the ports the container marks, the pod's
	// address, the socket factory and a file for the supervisor's log. With no ports a
	// container runs as it always did.
	DirectPorts  string
	PodIP        string
	DirectSocket string
	DirectLog    string
}

// EnsureDefaults sets default socket and binary paths if not explicitly configured.
func (opts *PlaidOptions) EnsureDefaults() {
	opts.Socket = netcfg.SocketPath(opts.Socket)
	if opts.Binary == "" {
		opts.Binary = "/usr/local/bin/plaid"
	}
	opts.DirectSocket = netcfg.DirectSocketPath(opts.DirectSocket)
	if opts.ReadinessFile == "" {
		if env := os.Getenv("PLAID_READINESS_FILE"); env != "" {
			opts.ReadinessFile = env
		}
	}
	if opts.MTU <= 0 {
		opts.MTU = netcfg.MTUFromEnv() // 0 when the environment says nothing: plaidd's MTU applies
	}
}

// mergePlaidOptions merges two PlaidOptions structs, giving precedence to override.
func mergePlaidOptions(base, override PlaidOptions) PlaidOptions {
	res := base
	if override.HostNetworking {
		res.HostNetworking = true
	}
	if override.IP != "" {
		res.IP = override.IP
	}
	if override.Gateway != "" {
		res.Gateway = override.Gateway
	}
	if override.DNS != "" {
		res.DNS = override.DNS
	}
	if override.Socket != "" {
		res.Socket = override.Socket
	}
	if override.Bind != "" {
		res.Bind = override.Bind
	}
	if len(override.Binds) > 0 {
		res.Binds = append(res.Binds, override.Binds...)
	}
	if override.Binary != "" {
		res.Binary = override.Binary
	}
	if override.MTU > 0 {
		res.MTU = override.MTU
	}
	if override.ReadinessFile != "" {
		res.ReadinessFile = override.ReadinessFile
	}
	if override.DirectPorts != "" {
		res.DirectPorts = override.DirectPorts
	}
	if override.PodIP != "" {
		res.PodIP = override.PodIP
	}
	if override.DirectSocket != "" {
		res.DirectSocket = override.DirectSocket
	}
	if override.DirectLog != "" {
		res.DirectLog = override.DirectLog
	}
	return res
}

// parseWrapperArgs parses options strictly up to the container image/target argument.
// Any arguments following the image (or following '--') are preserved byte-for-byte as workload arguments.
func parseWrapperArgs(args []string) (opts PlaidOptions, apptainerFlags []string, image string, workloadArgs []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Explicit delimiter: everything after '--' belongs to the image / workload
		if arg == "--" {
			remaining := args[i+1:]
			if image == "" && len(remaining) > 0 {
				image = remaining[0]
				workloadArgs = remaining[1:]
			} else {
				workloadArgs = remaining
			}
			return opts, apptainerFlags, image, workloadArgs
		}

		// Plaid-specific options
		if arg == "--host-networking" {
			opts.HostNetworking = true
			continue
		}
		if strings.HasPrefix(arg, "--ip=") {
			opts.IP = strings.TrimPrefix(arg, "--ip=")
			continue
		}
		if arg == "--ip" && i+1 < len(args) {
			opts.IP = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--gateway=") {
			opts.Gateway = strings.TrimPrefix(arg, "--gateway=")
			continue
		}
		if arg == "--gateway" && i+1 < len(args) {
			opts.Gateway = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--dns=") {
			opts.DNS = strings.TrimPrefix(arg, "--dns=")
			continue
		}
		if arg == "--dns" && i+1 < len(args) {
			opts.DNS = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--socket=") {
			opts.Socket = strings.TrimPrefix(arg, "--socket=")
			continue
		}
		if arg == "--socket" && i+1 < len(args) {
			opts.Socket = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--bind=") {
			val := strings.TrimPrefix(arg, "--bind=")
			opts.Bind = val
			opts.Binds = append(opts.Binds, val)
			continue
		}
		if arg == "--bind" && i+1 < len(args) {
			val := args[i+1]
			opts.Bind = val
			opts.Binds = append(opts.Binds, val)
			i++
			continue
		}
		if strings.HasPrefix(arg, "--binary=") {
			opts.Binary = strings.TrimPrefix(arg, "--binary=")
			continue
		}
		if arg == "--binary" && i+1 < len(args) {
			opts.Binary = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--mtu=") {
			if val, err := strconv.Atoi(strings.TrimPrefix(arg, "--mtu=")); err == nil {
				opts.MTU = val
			}
			continue
		}
		if arg == "--mtu" && i+1 < len(args) {
			if val, err := strconv.Atoi(args[i+1]); err == nil {
				opts.MTU = val
			}
			i++
			continue
		}
		if strings.HasPrefix(arg, "--readiness-file=") {
			opts.ReadinessFile = strings.TrimPrefix(arg, "--readiness-file=")
			continue
		}
		if arg == "--readiness-file" && i+1 < len(args) {
			opts.ReadinessFile = args[i+1]
			i++
			continue
		}

		// The direct path's options
		if v, ok := optionValue(args, &i, "--direct-ports"); ok {
			opts.DirectPorts = v
			continue
		}
		if v, ok := optionValue(args, &i, "--pod-ip"); ok {
			opts.PodIP = v
			continue
		}
		if v, ok := optionValue(args, &i, "--direct-socket"); ok {
			opts.DirectSocket = v
			continue
		}
		if v, ok := optionValue(args, &i, "--direct-log"); ok {
			opts.DirectLog = v
			continue
		}

		// Apptainer flags
		if strings.HasPrefix(arg, "-") {
			apptainerFlags = append(apptainerFlags, arg)
			if !strings.Contains(arg, "=") && takesArg(arg) && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				apptainerFlags = append(apptainerFlags, args[i+1])
				i++
			}
			continue
		}

		// First non-flag argument is the image! Stop parsing wrapper options.
		image = arg
		workloadArgs = args[i+1:]
		return opts, apptainerFlags, image, workloadArgs
	}

	return opts, apptainerFlags, image, workloadArgs
}

// optionValue reads the value of the option name at args[*i], given as name=value or as
// name value, and moves *i past it.
func optionValue(args []string, i *int, name string) (string, bool) {
	if v, ok := strings.CutPrefix(args[*i], name+"="); ok {
		return v, true
	}
	if args[*i] == name && *i+1 < len(args) {
		*i++
		return args[*i], true
	}
	return "", false
}

// parsePlaidOptions scans args and extracts Plaid-specific flags, returning the parsed options
// and remaining arguments intended for Apptainer.
func parsePlaidOptions(args []string) (PlaidOptions, []string) {
	opts, flags, image, workload := parseWrapperArgs(args)
	var remaining []string
	remaining = append(remaining, flags...)
	if image != "" {
		remaining = append(remaining, image)
	}
	remaining = append(remaining, workload...)
	return opts, remaining
}

// findApptainerBinary locates the underlying apptainer binary.
func findApptainerBinary() (string, error) {
	if bin := os.Getenv("APPTAINER_BIN"); bin != "" {
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}
	bin, err := exec.LookPath("apptainer")
	if err != nil {
		return "", fmt.Errorf("apptainer binary not found in PATH: %w", err)
	}
	return bin, nil
}

// findHostPlaidBinary locates the host plaid binary to bind-mount into containers.
func findHostPlaidBinary() string {
	var candidates []string

	// 1. Explicit environment variable override
	if envBin := os.Getenv("PLAID_BIN"); envBin != "" {
		candidates = append(candidates, envBin)
	}

	// 2. Sibling binary in the same directory as this executable
	execPath, err := os.Executable()
	if err == nil {
		execDir := filepath.Dir(execPath)
		candidates = append(candidates,
			filepath.Join(execDir, "plaid"),
			filepath.Join(execDir, "plaid-linux"),
		)
	}

	// 3. Standard installation paths
	candidates = append(candidates,
		"/usr/local/bin/plaid",
		"/opt/cni/bin/plaid",
		"/usr/libexec/cni/plaid",
	)

	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Mode()&0111 != 0 {
			return c
		}
	}

	if p, err := exec.LookPath("plaid"); err == nil {
		return p
	}

	return ""
}

// runCommand runs a command, forwards stdio and returns exit code.
func runCommand(bin string, args []string) int {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "Error executing %s: %v\n", bin, err)
		return 1
	}
	return 0
}

// A passthrough workload needs no wrapper cleanup. Replace the wrapper so the
// execution owner's waiter observes Apptainer's lifetime through termination.
func execPassthroughCommand(bin string, args []string) int {
	if err := syscall.Exec(bin, append([]string{bin}, args...), os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "Error executing %s: %v\n", bin, err)
		return 1
	}
	return 0
}

// findContainerPID locates the live payload process beneath an Apptainer launcher.
func findContainerPID(parentPID int, targetIP string) int {
	cleanTargetIP := ""
	if targetIP != "" {
		cleanTargetIP = strings.Split(targetIP, "/")[0]
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := os.ReadDir("/proc")
		if err == nil {
			parentOf := make(map[int]int)
			pgrpOf := make(map[int]int)
			commOf := make(map[int]string)
			var pids []int
			for _, entry := range entries {
				pid, err := strconv.Atoi(entry.Name())
				if err != nil || pid <= 0 {
					continue
				}
				pids = append(pids, pid)
				if commBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
					commOf[pid] = strings.TrimSpace(string(commBytes))
				}
				if statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
					idx := strings.LastIndex(string(statData), ")")
					if idx != -1 && idx+2 < len(statData) {
						statFields := strings.Fields(string(statData[idx+2:]))
						if len(statFields) >= 2 {
							if ppid, _ := strconv.Atoi(statFields[1]); ppid > 0 {
								parentOf[pid] = ppid
							}
						}
						if len(statFields) >= 3 {
							if pgrp, _ := strconv.Atoi(statFields[2]); pgrp > 0 {
								pgrpOf[pid] = pgrp
							}
						}
					}
				}
			}

			isDescendant := func(pid int) bool {
				if pid == parentPID {
					return false
				}
				if pgrp, ok := pgrpOf[pid]; ok && pgrp == parentPID {
					return true
				}
				curr := pid
				for {
					ppid, ok := parentOf[curr]
					if !ok || ppid <= 1 {
						return false
					}
					if ppid == parentPID {
						return true
					}
					curr = ppid
				}
			}

			selfNetns, _ := os.Readlink("/proc/self/ns/net")
			hostNetns, _ := os.Readlink("/proc/1/ns/net")
			isReady := func(pid int) bool {
				ns, _ := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid))
				if cleanTargetIP == "" || selfNetns == "" || ns == "" || ns == selfNetns || ns == hostNetns {
					return false
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				out, err := exec.CommandContext(ctx, "nsenter", "-t", strconv.Itoa(pid), "-n", "ip", "-o", "-4", "addr", "show", "dev", "eth0").Output()
				return err == nil && strings.Contains(string(out), " "+cleanTargetIP+"/")
			}
			if pid := selectReadyContainerPID(pids, commOf, isDescendant, isReady); pid > 1 {
				return pid
			}
		}

		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	return 0
}

// runCommandWithCleanup runs a command with bounded signal trapping and cleanup callback.
func runCommandWithCleanup(bin string, args []string, cleanup func()) int {
	return runCommandWithCleanupAndReadiness(bin, args, cleanup, "", "", "", "")
}

// runCommandWithCleanupAndReadiness runs a command with bounded signal trapping, cleanup callback,
// and optional post-launch readiness handshake file generation recording actual container netns and PID.
func runCommandWithCleanupAndReadiness(bin string, args []string, cleanup func(), readinessFile string, containerID, ipStr, gwStr string) int {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	if err := cmd.Start(); err != nil {
		cleanup()
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "Error starting %s: %v\n", bin, err)
		return 1
	}

	var discoveredPID int32
	if readinessFile != "" {
		go func() {
			cpid := findContainerPID(cmd.Process.Pid, ipStr)
			if cpid <= 1 {
				return
			}
			atomic.StoreInt32(&discoveredPID, int32(cpid))
			cleanIP := strings.Split(ipStr, "/")[0]
			cleanGW := strings.Split(gwStr, "/")[0]
			netnsPath := fmt.Sprintf("/proc/%d/ns/net", cpid)
			var netnsInode uint64
			if stat, err := os.Stat(netnsPath); err == nil {
				if sys, ok := stat.Sys().(*syscall.Stat_t); ok {
					netnsInode = sys.Ino
				}
			}
			if netnsInode == 0 {
				return
			}
			readinessData := fmt.Sprintf(`{"pause_pid":%d,"netns_path":"%s","netns_inode":%d,"endpoint_id":"%s","ip":"%s","gateway":"%s"}`+"\n",
				cpid, netnsPath, netnsInode, containerID, cleanIP, cleanGW)
			_ = os.WriteFile(readinessFile, []byte(readinessData), 0644)
		}()
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var waitErr error
	select {
	case sig := <-sigChan:
		if cmd.Process != nil {
			_ = cmd.Process.Signal(sig)
		}
		// Bounded escalation: wait up to 5 seconds for graceful shutdown, then SIGKILL
		select {
		case waitErr = <-done:
		case <-time.After(5 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			waitErr = <-done
		case <-sigChan:
			// Second signal immediately escalates to SIGKILL
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			waitErr = <-done
		}
	case waitErr = <-done:
	}

	cpidToKill := int(atomic.LoadInt32(&discoveredPID))
	if cpidToKill > 1 {
		_ = syscall.Kill(-cpidToKill, syscall.SIGKILL)
		_ = syscall.Kill(cpidToKill, syscall.SIGKILL)
	}

	cleanup()

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}
