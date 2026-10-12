package podhandler

import (
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"skifflet/internal/compute"
	"skifflet/pkg/fieldpath"
)

// ValidatePodCapabilities verifies that the requested pod specification is supported by skifflet
// before any local side effects or process launches occur.
func ValidatePodCapabilities(pod *corev1.Pod) error {
	var unsupported []string

	if len(pod.Spec.EphemeralContainers) > 0 {
		unsupported = append(unsupported, "Spec.EphemeralContainers")
	}

	// The pod-level deadline is not enforced, so it is not accepted (a Job's own
	// deadline is the Job controller's business and does not reach the pod).
	if pod.Spec.ActiveDeadlineSeconds != nil {
		unsupported = append(unsupported, "Spec.ActiveDeadlineSeconds (the pod deadline is not enforced)")
	}

	if pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace {
		unsupported = append(unsupported, "Spec.ShareProcessNamespace (not implemented: every container has a PID namespace of its own)")
	}
	if len(pod.Spec.ReadinessGates) > 0 {
		unsupported = append(unsupported, "Spec.ReadinessGates (not implemented)")
	}
	if len(pod.Spec.ResourceClaims) > 0 {
		unsupported = append(unsupported, "Spec.ResourceClaims (dynamic resource allocation is not implemented)")
	}
	if len(pod.Spec.ImagePullSecrets) > 0 {
		unsupported = append(unsupported, "Spec.ImagePullSecrets (private registries are not implemented: remove the field for a public image)")
	}
	if pod.Spec.OS != nil && pod.Spec.OS.Name != corev1.Linux {
		unsupported = append(unsupported, fmt.Sprintf("Spec.OS (the pod is for %s and the node runs Linux)", pod.Spec.OS.Name))
	}
	if pod.Spec.HostnameOverride != nil {
		unsupported = append(unsupported, "Spec.HostnameOverride (not implemented)")
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

	// Volumes: the sources Skifflet can provide. A claim is accepted here and its
	// volume checked when it is bound (hostPath and local volumes only, no block mode).
	for i := range pod.Spec.Volumes {
		unsupported = append(unsupported, validateVolume(pod, fmt.Sprintf("Spec.Volumes[%d]", i), &pod.Spec.Volumes[i])...)
	}

	// Validate container-level capabilities
	validateContainer := func(prefix string, c *corev1.Container, init bool) {
		if init && c.RestartPolicy != nil {
			unsupported = append(unsupported, prefix+".RestartPolicy (restartable init containers)")
		}
		if !init && c.RestartPolicy != nil {
			unsupported = append(unsupported, prefix+".RestartPolicy (restart rules of a container are not implemented)")
		}
		if len(c.RestartPolicyRules) > 0 {
			unsupported = append(unsupported, prefix+".RestartPolicyRules (not implemented)")
		}
		if c.Stdin || c.StdinOnce || c.TTY {
			unsupported = append(unsupported, prefix+".Stdin/TTY (nothing attaches to a container: use kubectl exec -it)")
		}
		if len(c.Resources.Claims) > 0 {
			unsupported = append(unsupported, prefix+".Resources.Claims (dynamic resource allocation is not implemented)")
		}
		if len(c.VolumeDevices) > 0 {
			unsupported = append(unsupported, prefix+".VolumeDevices (block devices)")
		}
		unsupported = append(unsupported, validateEnvironment(pod, prefix, c)...)

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

// supportedVolumeSources are the volume sources a pod may use.
var supportedVolumeSources = map[string]bool{
	"EmptyDir": true, "ConfigMap": true, "Secret": true, "HostPath": true,
	"DownwardAPI": true, "Projected": true, "PersistentVolumeClaim": true,
}

// volumeSourceName names the source of a volume (the field of VolumeSource that is set).
func volumeSourceName(source *corev1.VolumeSource) string {
	v := reflect.ValueOf(*source)
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.Ptr && !f.IsNil() {
			return v.Type().Field(i).Name
		}
	}
	return ""
}

func validateVolume(pod *corev1.Pod, prefix string, vol *corev1.Volume) []string {
	name := volumeSourceName(&vol.VolumeSource)
	if !supportedVolumeSources[name] {
		return []string{fmt.Sprintf("%s (volume %q: %s volumes are not supported)", prefix, vol.Name, name)}
	}
	var out []string
	switch {
	case vol.HostPath != nil:
		// The bubble sees the host only through Skiff's two roots (see endpoint.CheckVolumePath).
		if err := compute.Skiff.CheckVolumePath(vol.HostPath.Path); err != nil {
			out = append(out, fmt.Sprintf("%s.HostPath.Path (volume %q: %v)", prefix, vol.Name, err))
		}
	case vol.DownwardAPI != nil:
		for j, item := range vol.DownwardAPI.Items {
			out = append(out, validateDownwardItem(pod, fmt.Sprintf("%s.DownwardAPI.Items[%d]", prefix, j), item)...)
		}
	case vol.Projected != nil:
		for j, source := range vol.Projected.Sources {
			at := fmt.Sprintf("%s.Projected.Sources[%d]", prefix, j)
			switch {
			case source.ClusterTrustBundle != nil:
				out = append(out, at+" (ClusterTrustBundle projections are not supported)")
			case source.PodCertificate != nil:
				out = append(out, at+" (PodCertificate projections are not supported)")
			case source.DownwardAPI != nil:
				for k, item := range source.DownwardAPI.Items {
					out = append(out, validateDownwardItem(pod, fmt.Sprintf("%s.DownwardAPI.Items[%d]", at, k), item)...)
				}
			}
		}
	}
	return out
}

// validateDownwardItem checks a Downward API file: its reference is one Skifflet
// can resolve, so the pod is not launched with a file that would be empty or wrong.
func validateDownwardItem(pod *corev1.Pod, prefix string, item corev1.DownwardAPIVolumeFile) []string {
	switch {
	case item.FieldRef != nil:
		if err := fieldpath.ValidateFieldPath(item.FieldRef.FieldPath); err != nil {
			return []string{fmt.Sprintf("%s.FieldRef (%v)", prefix, err)}
		}
	case item.ResourceFieldRef != nil:
		if err := fieldpath.ValidateResourceFieldRef(pod, item.ResourceFieldRef, true); err != nil {
			return []string{fmt.Sprintf("%s.ResourceFieldRef (%v)", prefix, err)}
		}
	default:
		return []string{prefix + " (neither fieldRef nor resourceFieldRef)"}
	}
	return nil
}

// validateEnvironment checks the Downward API references of a container's
// environment, which are then evaluated at start by the same functions.
func validateEnvironment(pod *corev1.Pod, prefix string, c *corev1.Container) []string {
	var out []string
	for _, env := range c.Env {
		from := env.ValueFrom
		if from == nil {
			continue
		}
		at := fmt.Sprintf("%s.Env[%s].ValueFrom", prefix, env.Name)
		switch {
		case from.FieldRef != nil:
			if err := fieldpath.ValidateFieldPath(from.FieldRef.FieldPath); err != nil {
				out = append(out, fmt.Sprintf("%s.FieldRef (%v)", at, err))
			}
		case from.ResourceFieldRef != nil:
			if err := fieldpath.ValidateResourceFieldRef(pod, from.ResourceFieldRef, false); err != nil {
				out = append(out, fmt.Sprintf("%s.ResourceFieldRef (%v)", at, err))
			}
		case from.FileKeyRef != nil:
			out = append(out, at+".FileKeyRef (file-based environment variables are not supported)")
		}
	}
	return out
}

// What Skifflet does with each field of a pod, so that none passes unnoticed. Every field of
// the structures below is in the list (a test makes sure of it, so a field that comes with a
// new Kubernetes is classified before the build passes):
//
//   - fieldImplemented: Skifflet does what Kubernetes does with it (the support table of
//     skifflet/README.md says where it falls short);
//   - fieldAccepted: accepted with the reason given and the support table saying so; the
//     scheduler's fields, for one, are done with before a pod reaches the node;
//   - fieldRefused: a pod that sets it is refused at creation (ValidatePodCapabilities), with
//     the field named.
type fieldSupport int

const (
	fieldImplemented fieldSupport = iota
	fieldAccepted
	fieldRefused
)

const scheduled = fieldAccepted // the scheduler's business, done with before the pod reaches the node

var podFields = map[string]map[string]fieldSupport{
	"PodSpec": {
		"Volumes": fieldImplemented, "InitContainers": fieldImplemented, "Containers": fieldImplemented,
		"EphemeralContainers": fieldRefused, "RestartPolicy": fieldImplemented,
		"TerminationGracePeriodSeconds": fieldImplemented, "ActiveDeadlineSeconds": fieldRefused,
		"DNSPolicy": fieldImplemented, "NodeSelector": scheduled, "ServiceAccountName": fieldImplemented,
		"DeprecatedServiceAccount": fieldImplemented, "AutomountServiceAccountToken": fieldImplemented,
		"NodeName": fieldImplemented, "HostNetwork": fieldRefused, "HostPID": fieldRefused, "HostIPC": fieldRefused,
		"ShareProcessNamespace": fieldRefused, "SecurityContext": fieldImplemented, "ImagePullSecrets": fieldRefused,
		"Hostname": fieldImplemented, "Subdomain": fieldImplemented, "Affinity": scheduled, "SchedulerName": scheduled,
		"Tolerations": scheduled, "HostAliases": fieldImplemented, "PriorityClassName": scheduled, "Priority": scheduled,
		"DNSConfig": fieldImplemented, "ReadinessGates": fieldRefused, "RuntimeClassName": fieldImplemented,
		"EnableServiceLinks": fieldImplemented, "PreemptionPolicy": scheduled,
		"Overhead":                  fieldAccepted, // counted by the scheduler, not enforced (as limits are not)
		"TopologySpreadConstraints": scheduled, "SetHostnameAsFQDN": fieldImplemented, "OS": fieldImplemented,
		"HostUsers":       fieldAccepted, // the containers always run in a user namespace of the job's user
		"SchedulingGates": scheduled, "ResourceClaims": fieldRefused,
		"Resources":        fieldAccepted, // the pod's requests are the scheduler's; limits are not enforced
		"HostnameOverride": fieldRefused, "SchedulingGroup": scheduled,
	},
	"Container": {
		"Name": fieldImplemented, "Image": fieldImplemented, "Command": fieldImplemented, "Args": fieldImplemented,
		"WorkingDir": fieldImplemented, "Ports": fieldImplemented, "EnvFrom": fieldImplemented, "Env": fieldImplemented,
		"Resources":     fieldImplemented, // requests and GPUs; limits accepted and not enforced (support table)
		"ResizePolicy":  fieldAccepted,    // an in-place resize is not applied, and is reported (UpdateNotApplied)
		"RestartPolicy": fieldRefused, "RestartPolicyRules": fieldRefused, "VolumeMounts": fieldImplemented,
		"VolumeDevices": fieldRefused, "LivenessProbe": fieldImplemented, "ReadinessProbe": fieldImplemented,
		"StartupProbe": fieldImplemented, "Lifecycle": fieldImplemented, "TerminationMessagePath": fieldImplemented,
		"TerminationMessagePolicy": fieldImplemented, "ImagePullPolicy": fieldImplemented,
		"SecurityContext": fieldImplemented, "Stdin": fieldRefused, "StdinOnce": fieldRefused, "TTY": fieldRefused,
	},
	// The fields of a security context that are not applied are named in the pod's Warning event
	// SecurityContextNotApplied (UnappliedSecurityContext); WindowsOptions apply to Windows only.
	"PodSecurityContext": {
		"SELinuxOptions": fieldAccepted, "WindowsOptions": fieldAccepted, "RunAsUser": fieldImplemented,
		"RunAsGroup": fieldImplemented, "RunAsNonRoot": fieldImplemented, "SupplementalGroups": fieldAccepted,
		"SupplementalGroupsPolicy": fieldAccepted, "FSGroup": fieldAccepted, "Sysctls": fieldAccepted,
		"FSGroupChangePolicy": fieldAccepted, "SeccompProfile": fieldAccepted, "AppArmorProfile": fieldAccepted,
		"SELinuxChangePolicy": fieldAccepted,
	},
	"SecurityContext": {
		"Capabilities": fieldAccepted, "Privileged": fieldAccepted, "SELinuxOptions": fieldAccepted,
		"WindowsOptions": fieldAccepted, "RunAsUser": fieldImplemented, "RunAsGroup": fieldImplemented,
		"RunAsNonRoot": fieldImplemented, "ReadOnlyRootFilesystem": fieldAccepted,
		"AllowPrivilegeEscalation": fieldAccepted, "ProcMount": fieldAccepted, "SeccompProfile": fieldAccepted,
		"AppArmorProfile": fieldAccepted,
	},
	"Lifecycle": {"PostStart": fieldImplemented, "PreStop": fieldImplemented, "StopSignal": fieldImplemented},
}
