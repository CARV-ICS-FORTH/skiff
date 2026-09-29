package podhandler

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"skifflet/internal/compute/runtime"

	corev1 "k8s.io/api/core/v1"
)

// startProbes runs the probes of a container for one attempt. Both stop when the
// attempt ends (its context is cancelled), and their results reach the pod only
// through the attempt, which drops them once it has been superseded.
func startProbes(a *Attempt, podIP string, spec *corev1.Container) {
	launcherID := a.LauncherID()
	if probe := spec.ReadinessProbe; probe != nil {
		watchReadinessProbe(a.Context(), podIP, launcherID, probe, a.SetReady)
	}
	if probe := spec.LivenessProbe; probe != nil {
		watchLivenessProbe(a.Context(), podIP, launcherID, probe, func() { a.Signal(syscall.SIGKILL) })
	}
}

// watchReadinessProbe checks the live PodIP from the bubble and reports readiness changes via onReadyChange.
func watchReadinessProbe(ctx context.Context, podIP string, containerID string, probe *corev1.Probe, onReadyChange func(ready bool)) {
	go func() {
		if delay := time.Duration(probe.InitialDelaySeconds) * time.Second; delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		period := time.Duration(probe.PeriodSeconds) * time.Second
		if period <= 0 {
			period = 10 * time.Second
		}
		timeout := time.Duration(probe.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = time.Second
		}
		successThreshold := int(probe.SuccessThreshold)
		if successThreshold <= 0 {
			successThreshold = 1
		}
		failureThreshold := int(probe.FailureThreshold)
		if failureThreshold <= 0 {
			failureThreshold = 3
		}
		successes, failures := 0, 0
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		isReady := false
		for {
			if ctx.Err() != nil {
				return
			}
			if evaluateReadinessProbe(probe, podIP, containerID, timeout) {
				successes++
				failures = 0
				if successes >= successThreshold && !isReady {
					isReady = true
					if onReadyChange != nil && ctx.Err() == nil {
						onReadyChange(true)
					}
				}
			} else {
				failures++
				successes = 0
				if failures >= failureThreshold && isReady {
					isReady = false
					if onReadyChange != nil && ctx.Err() == nil {
						onReadyChange(false)
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// watchLivenessProbe checks the live PodIP and invokes onFailure when failure threshold is reached.
func watchLivenessProbe(ctx context.Context, podIP string, containerID string, probe *corev1.Probe, onFailure func()) {
	go func() {
		if delay := time.Duration(probe.InitialDelaySeconds) * time.Second; delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		period := time.Duration(probe.PeriodSeconds) * time.Second
		if period <= 0 {
			period = 10 * time.Second
		}
		timeout := time.Duration(probe.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = time.Second
		}
		failureThreshold := int(probe.FailureThreshold)
		if failureThreshold <= 0 {
			failureThreshold = 3
		}
		failures := 0
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			if evaluateReadinessProbe(probe, podIP, containerID, timeout) {
				failures = 0
			} else {
				failures++
				if failures >= failureThreshold {
					if onFailure != nil && ctx.Err() == nil {
						onFailure()
					}
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func evaluateReadinessProbe(probe *corev1.Probe, podIP, containerID string, timeout time.Duration) bool {
	if probe.Exec == nil {
		return evaluateProbe(probe, podIP, timeout)
	}
	if len(probe.Exec.Command) == 0 {
		return false
	}
	launcherPID, _, err := runtime.ParseProcessJobID(containerID)
	if err != nil || runtime.IsProcessDead(launcherPID) {
		return false
	}
	containerPID := FindContainerPID(launcherPID)
	if containerPID == launcherPID || runtime.IsProcessDead(containerPID) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{"-t", strconv.Itoa(containerPID), "-m", "-n", "-p", "-r", "--"}
	args = append(args, probe.Exec.Command...)
	return exec.CommandContext(ctx, "nsenter", args...).Run() == nil
}

func evaluateProbe(probe *corev1.Probe, podIP string, timeout time.Duration) bool {
	if probe.HTTPGet != nil {
		port := probe.HTTPGet.Port.IntValue()
		if port <= 0 {
			port = 80
		}
		host := probe.HTTPGet.Host
		if host == "" {
			host = podIP
		}
		if host == "" {
			return false
		}
		scheme := strings.ToLower(string(probe.HTTPGet.Scheme))
		if scheme == "" {
			scheme = "http"
		}
		url := fmt.Sprintf("%s://%s:%d%s", scheme, host, port, probe.HTTPGet.Path)
		client := &http.Client{Timeout: timeout}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return false
		}
		for _, h := range probe.HTTPGet.HTTPHeaders {
			req.Header.Set(h.Name, h.Value)
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 400
	}

	if probe.TCPSocket != nil {
		port := probe.TCPSocket.Port.IntValue()
		if port <= 0 {
			return false
		}
		host := probe.TCPSocket.Host
		if host == "" {
			host = podIP
		}
		if host == "" {
			return false
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), timeout)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}

	if probe.Exec != nil && len(probe.Exec.Command) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, probe.Exec.Command[0], probe.Exec.Command[1:]...)
		return cmd.Run() == nil
	}

	return true
}
