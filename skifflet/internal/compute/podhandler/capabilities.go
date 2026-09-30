package podhandler

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// ValidatePodCapabilities verifies that the requested pod specification is supported by skifflet
// before any local side effects or process launches occur.
func ValidatePodCapabilities(pod *corev1.Pod) error {
	var unsupported []string

	if len(pod.Spec.EphemeralContainers) > 0 {
		unsupported = append(unsupported, "Spec.EphemeralContainers")
	}

	if pod.Spec.HostNetwork {
		unsupported = append(unsupported, "Spec.HostNetwork")
	}
	if pod.Spec.HostPID {
		unsupported = append(unsupported, "Spec.HostPID")
	}
	if pod.Spec.HostIPC {
		unsupported = append(unsupported, "Spec.HostIPC")
	}

	// Validate container-level capabilities
	validateContainer := func(prefix string, c *corev1.Container, init bool) {
		// PostStart and PreStop hooks run for application containers, through exec,
		// HTTP GET or sleep handlers; TCP handlers, hooks of init containers, and
		// handlers with nothing to run are unsupported.
		if c.Lifecycle != nil {
			for _, hook := range []struct {
				name string
				h    *corev1.LifecycleHandler
			}{{"PostStart", c.Lifecycle.PostStart}, {"PreStop", c.Lifecycle.PreStop}} {
				field := fmt.Sprintf("%s.Lifecycle.%s", prefix, hook.name)
				switch h := hook.h; {
				case h == nil:
				case init:
					unsupported = append(unsupported, field+" (hooks of init containers)")
				case h.TCPSocket != nil:
					unsupported = append(unsupported, field+".TCPSocket")
				case h.Exec == nil && h.HTTPGet == nil && h.Sleep == nil:
					unsupported = append(unsupported, field+" (no handler)")
				}
			}
		}
		// Startup, readiness and liveness probes over HTTP, TCP and exec (with named
		// ports) are supported; gRPC probes are not.
		for _, probe := range []struct {
			name string
			p    *corev1.Probe
		}{{"ReadinessProbe", c.ReadinessProbe}, {"LivenessProbe", c.LivenessProbe}, {"StartupProbe", c.StartupProbe}} {
			if probe.p != nil && probe.p.GRPC != nil {
				unsupported = append(unsupported, fmt.Sprintf("%s.%s.GRPC", prefix, probe.name))
			}
		}
	}

	for i := range pod.Spec.InitContainers {
		validateContainer(fmt.Sprintf("Spec.InitContainers[%d]", i), &pod.Spec.InitContainers[i], true)
	}
	for i := range pod.Spec.Containers {
		validateContainer(fmt.Sprintf("Spec.Containers[%d]", i), &pod.Spec.Containers[i], false)
	}

	if len(unsupported) > 0 {
		return fmt.Errorf("unsupported features: %s", strings.Join(unsupported, ", "))
	}
	return nil
}
