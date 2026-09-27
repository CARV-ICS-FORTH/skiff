package podhandler

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	validateContainer := func(prefix string, c *corev1.Container) {
		if c.Lifecycle != nil {
			if c.Lifecycle.PostStart != nil {
				unsupported = append(unsupported, fmt.Sprintf("%s.Lifecycle.PostStart", prefix))
			}
			if c.Lifecycle.PreStop != nil {
				unsupported = append(unsupported, fmt.Sprintf("%s.Lifecycle.PreStop", prefix))
			}
		}
		validateProbe := func(probeName string, p *corev1.Probe) {
			if p == nil {
				return
			}
			if p.GRPC != nil {
				unsupported = append(unsupported, fmt.Sprintf("%s.%s.GRPC", prefix, probeName))
			}
			if p.HTTPGet != nil && p.HTTPGet.Port.Type == intstr.String {
				unsupported = append(unsupported, fmt.Sprintf("%s.%s.HTTPGet.NamedPort", prefix, probeName))
			}
			if p.TCPSocket != nil && p.TCPSocket.Port.Type == intstr.String {
				unsupported = append(unsupported, fmt.Sprintf("%s.%s.TCPSocket.NamedPort", prefix, probeName))
			}
		}
		validateProbe("ReadinessProbe", c.ReadinessProbe)
		validateProbe("LivenessProbe", c.LivenessProbe)
		validateProbe("StartupProbe", c.StartupProbe)
	}

	for i := range pod.Spec.InitContainers {
		validateContainer(fmt.Sprintf("Spec.InitContainers[%d]", i), &pod.Spec.InitContainers[i])
	}
	for i := range pod.Spec.Containers {
		validateContainer(fmt.Sprintf("Spec.Containers[%d]", i), &pod.Spec.Containers[i])
	}

	if len(unsupported) > 0 {
		return fmt.Errorf("unsupported features: %s", strings.Join(unsupported, ", "))
	}
	return nil
}
