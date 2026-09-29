package podhandler

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// startProbes runs the probes of a container for one attempt. Both stop when the
// attempt ends (its context is cancelled), and their results reach the pod only
// through the attempt, which drops them once it has been superseded. Network
// probes run from here against the pod IP; exec probes run inside the container
// through the attempt's verified target.
func startProbes(a *Attempt, podIP string, spec *corev1.Container) {
	check := func(ctx context.Context, probe *corev1.Probe, timeout time.Duration) bool {
		return evaluateProbe(ctx, probe, podIP, timeout, a.execProbe)
	}
	if probe := spec.ReadinessProbe; probe != nil {
		watchReadinessProbe(a.Context(), probe, check, a.SetReady)
	}
	if probe := spec.LivenessProbe; probe != nil {
		watchLivenessProbe(a.Context(), probe, check, func() {
			a.logger.Info("liveness probe failed; stopping the container", "action", "probe", "reason", "liveness failure threshold reached")
			a.Signal(syscall.SIGKILL)
		})
	}
}

// probeCheck runs one probe once.
type probeCheck func(ctx context.Context, probe *corev1.Probe, timeout time.Duration) bool

// execProbe runs a command probe inside the container. A container that is not
// (yet) a usable target simply fails the probe.
func (a *Attempt) execProbe(ctx context.Context, argv []string, timeout time.Duration) bool {
	target, err := a.Target(ctx, 0)
	if err != nil {
		return false
	}
	code, err := target.Run(ctx, argv, timeout)
	return err == nil && code == 0
}

// watchReadinessProbe checks the live PodIP from the bubble and reports readiness changes via onReadyChange.
func watchReadinessProbe(ctx context.Context, probe *corev1.Probe, check probeCheck, onReadyChange func(ready bool)) {
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
			if check(ctx, probe, timeout) {
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
func watchLivenessProbe(ctx context.Context, probe *corev1.Probe, check probeCheck, onFailure func()) {
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
			if check(ctx, probe, timeout) {
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

// evaluateProbe runs one probe once. Network probes are made from here against
// the pod IP, bound to ctx; exec probes are handed to runExec.
func evaluateProbe(ctx context.Context, probe *corev1.Probe, podIP string, timeout time.Duration,
	runExec func(ctx context.Context, argv []string, timeout time.Duration) bool) bool {
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
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
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
		conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", host, port))
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}

	if probe.Exec != nil {
		if len(probe.Exec.Command) == 0 || runExec == nil {
			return false
		}
		return runExec(ctx, probe.Exec.Command, timeout)
	}

	return true
}
