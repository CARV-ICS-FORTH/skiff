package podhandler

import (
	"context"
	"os/exec"
	"strconv"
	"time"

	"skifflet/internal/compute/runtime"

	corev1 "k8s.io/api/core/v1"
)

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
			if evaluateReadinessProbe(probe, podIP, containerID, timeout) {
				successes++
				failures = 0
				if successes >= successThreshold && !isReady {
					isReady = true
					if onReadyChange != nil {
						onReadyChange(true)
					}
				}
			} else {
				failures++
				successes = 0
				if failures >= failureThreshold && isReady {
					isReady = false
					if onReadyChange != nil {
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
