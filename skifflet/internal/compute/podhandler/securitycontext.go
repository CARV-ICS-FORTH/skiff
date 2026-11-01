// Copyright © 2023 FORTH-ICS
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package podhandler

import (
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"skifflet/internal/compute/image"
)

const (
	RootUID = 0
	RootGID = 0
)

// defaultCapabilities are the capabilities of a container that runs as root: those the container
// runtimes of Kubernetes give (containerd's default), so that an image runs here as it does
// there.
var defaultCapabilities = []string{
	"CAP_AUDIT_WRITE", "CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_KILL", "CAP_MKNOD",
	"CAP_NET_BIND_SERVICE", "CAP_NET_RAW", "CAP_SETFCAP", "CAP_SETGID", "CAP_SETPCAP", "CAP_SETUID", "CAP_SYS_CHROOT",
}

// capabilityArgs are Apptainer's options for the capabilities of a container that runs as root,
// worked out as containerd's CRI does it (WithCapabilities): all with privileged or an added ALL,
// none with a dropped ALL, which wins over an added one, else the default ones, then what is added
// by name and what is dropped by name, which wins. All of them is
// what root has in Apptainer without options. They are all within the bubble's user namespace:
// the pod's author asks for them, not its image.
func capabilityArgs(privileged bool, caps *corev1.Capabilities) []string {
	if privileged {
		return nil
	}
	var add, drop []string
	if caps != nil {
		add, drop = capabilityNames(caps.Add), capabilityNames(caps.Drop)
	}
	if slices.Contains(add, "CAP_ALL") && !slices.Contains(drop, "CAP_ALL") {
		if len(drop) == 0 {
			return nil
		}
		return []string{"--drop-caps", strings.Join(drop, ",")}
	}
	var kept []string
	if !slices.Contains(drop, "CAP_ALL") {
		kept = slices.Clone(defaultCapabilities)
	}
	for _, name := range add {
		if name != "CAP_ALL" && !slices.Contains(kept, name) {
			kept = append(kept, name)
		}
	}
	kept = slices.DeleteFunc(kept, func(name string) bool { return slices.Contains(drop, name) })
	if len(kept) == 0 {
		return []string{"--no-privs"}
	}
	return []string{"--no-privs", "--add-caps", strings.Join(kept, ",")}
}

// capabilityNames are names of Kubernetes (with or without the prefix) as Apptainer's.
func capabilityNames(caps []corev1.Capability) []string {
	names := make([]string, 0, len(caps))
	for _, c := range caps {
		name := strings.ToUpper(string(c))
		if !strings.HasPrefix(name, "CAP_") {
			name = "CAP_" + name
		}
		names = append(names, name)
	}
	return names
}

// DetermineEffectiveSecurityContext returns a synthesized SecurityContext for reading effective configurations
// from the provided pod's and container's security context. Container's fields take precedence in cases where both
// are set
func DetermineEffectiveSecurityContext(pod *corev1.Pod, container *corev1.Container) *corev1.SecurityContext {
	effectiveSc := securityContextFromPodSecurityContext(pod)
	containerSc := container.SecurityContext

	if effectiveSc == nil && containerSc == nil {
		return &corev1.SecurityContext{}
	}
	if effectiveSc != nil && containerSc == nil {
		return effectiveSc
	}
	if effectiveSc == nil && containerSc != nil {
		return containerSc
	}

	if containerSc.SELinuxOptions != nil {
		effectiveSc.SELinuxOptions = new(corev1.SELinuxOptions)
		*effectiveSc.SELinuxOptions = *containerSc.SELinuxOptions
	}

	if containerSc.WindowsOptions != nil {
		// only override fields that are set at the container level, not the whole thing
		if effectiveSc.WindowsOptions == nil {
			effectiveSc.WindowsOptions = &corev1.WindowsSecurityContextOptions{}
		}
		if containerSc.WindowsOptions.GMSACredentialSpecName != nil || containerSc.WindowsOptions.GMSACredentialSpec != nil {
			// both GMSA fields go hand in hand
			effectiveSc.WindowsOptions.GMSACredentialSpecName = containerSc.WindowsOptions.GMSACredentialSpecName
			effectiveSc.WindowsOptions.GMSACredentialSpec = containerSc.WindowsOptions.GMSACredentialSpec
		}
		if containerSc.WindowsOptions.RunAsUserName != nil {
			effectiveSc.WindowsOptions.RunAsUserName = containerSc.WindowsOptions.RunAsUserName
		}
		if containerSc.WindowsOptions.HostProcess != nil {
			effectiveSc.WindowsOptions.HostProcess = containerSc.WindowsOptions.HostProcess
		}
	}

	if containerSc.Capabilities != nil {
		effectiveSc.Capabilities = new(corev1.Capabilities)
		*effectiveSc.Capabilities = *containerSc.Capabilities
	}

	if containerSc.Privileged != nil {
		effectiveSc.Privileged = new(bool)
		*effectiveSc.Privileged = *containerSc.Privileged
	}

	if containerSc.RunAsNonRoot != nil {
		effectiveSc.RunAsNonRoot = new(bool)
		*effectiveSc.RunAsNonRoot = *containerSc.RunAsNonRoot
	}

	if containerSc.RunAsUser != nil {
		effectiveSc.RunAsUser = new(int64)
		*effectiveSc.RunAsUser = *containerSc.RunAsUser
	}

	if containerSc.RunAsGroup != nil {
		effectiveSc.RunAsGroup = new(int64)
		*effectiveSc.RunAsGroup = *containerSc.RunAsGroup
	}

	if containerSc.ReadOnlyRootFilesystem != nil {
		effectiveSc.ReadOnlyRootFilesystem = new(bool)
		*effectiveSc.ReadOnlyRootFilesystem = *containerSc.ReadOnlyRootFilesystem
	}

	if containerSc.AllowPrivilegeEscalation != nil {
		effectiveSc.AllowPrivilegeEscalation = new(bool)
		*effectiveSc.AllowPrivilegeEscalation = *containerSc.AllowPrivilegeEscalation
	}

	if containerSc.ProcMount != nil {
		effectiveSc.ProcMount = new(corev1.ProcMountType)
		*effectiveSc.ProcMount = *containerSc.ProcMount
	}

	return effectiveSc
}

func securityContextFromPodSecurityContext(pod *corev1.Pod) *corev1.SecurityContext {
	if pod.Spec.SecurityContext == nil {
		return nil
	}

	synthesized := &corev1.SecurityContext{}

	if pod.Spec.SecurityContext.SELinuxOptions != nil {
		synthesized.SELinuxOptions = &corev1.SELinuxOptions{}
		*synthesized.SELinuxOptions = *pod.Spec.SecurityContext.SELinuxOptions
	}

	if pod.Spec.SecurityContext.WindowsOptions != nil {
		synthesized.WindowsOptions = &corev1.WindowsSecurityContextOptions{}
		*synthesized.WindowsOptions = *pod.Spec.SecurityContext.WindowsOptions
	}

	if pod.Spec.SecurityContext.RunAsUser != nil {
		synthesized.RunAsUser = new(int64)
		*synthesized.RunAsUser = *pod.Spec.SecurityContext.RunAsUser
	}

	if pod.Spec.SecurityContext.RunAsGroup != nil {
		synthesized.RunAsGroup = new(int64)
		*synthesized.RunAsGroup = *pod.Spec.SecurityContext.RunAsGroup
	}

	if pod.Spec.SecurityContext.RunAsNonRoot != nil {
		synthesized.RunAsNonRoot = new(bool)
		*synthesized.RunAsNonRoot = *pod.Spec.SecurityContext.RunAsNonRoot
	}

	return synthesized
}

// DetermineEffectiveRunAsUser is the user and group a container runs as, in the order the
// kubelet and the container runtime use: the pod's runAsUser and runAsGroup, then the user
// the image says (its USER, with the group of its passwd entry), then root. runAsNonRoot
// refuses a container that would run as root, as the kubelet does: with a runAsUser of 0 the
// pod contradicts itself; without a user, the image "will run as root". The error is a
// ConfigError, which keeps the container waiting (CreateContainerConfigError).
func DetermineEffectiveRunAsUser(sc *corev1.SecurityContext, img image.User) (uid int64, gid int64, err error) {
	uid = RootUID
	gid = RootGID

	if img.Set {
		uid, gid = img.UID, img.GID
	}

	if sc.RunAsUser != nil {
		uid = *sc.RunAsUser
		if sc.RunAsGroup == nil {
			gid = RootGID
		}
	}

	if sc.RunAsGroup != nil {
		gid = *sc.RunAsGroup
	}

	if sc.RunAsNonRoot != nil && *sc.RunAsNonRoot && uid == RootUID {
		if sc.RunAsUser != nil {
			return 0, 0, configErrorf("container's runAsUser breaks non-root policy")
		}
		return 0, 0, configErrorf("container has runAsNonRoot and image will run as root")
	}

	return uid, gid, nil
}

// UnappliedSecurityContext lists the securityContext fields of a pod that Skifflet accepts and
// does not apply, as the text of an event. What it applies is the user and group a
// container runs as (runAsUser, runAsGroup, the image's user) and runAsNonRoot (a container
// that would run as root is refused), and privileged and capabilities (see capabilityArgs: a
// container that is not root has none, as with a runtime). The rest has no equivalent here: a
// container runs in a user namespace of the job's user, with a writable root filesystem of its
// own, its volumes are writable by any user (so a group for them, fsGroup, is not needed to
// write), and the filters and labels of a Linux security module are not Skiff's to set. A
// pod that sets them is not refused (charts set them routinely and run unchanged), and it is
// told which were not applied. Empty when there are none.
func UnappliedSecurityContext(pod *corev1.Pod) string {
	var parts []string
	if sc := pod.Spec.SecurityContext; sc != nil {
		var fields []string
		if sc.FSGroup != nil {
			fields = append(fields, "fsGroup")
		}
		if sc.FSGroupChangePolicy != nil {
			fields = append(fields, "fsGroupChangePolicy")
		}
		if len(sc.SupplementalGroups) > 0 {
			fields = append(fields, "supplementalGroups")
		}
		if sc.SupplementalGroupsPolicy != nil {
			fields = append(fields, "supplementalGroupsPolicy")
		}
		if sc.SELinuxOptions != nil {
			fields = append(fields, "seLinuxOptions")
		}
		if sc.SeccompProfile != nil && sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
			fields = append(fields, "seccompProfile")
		}
		if sc.AppArmorProfile != nil && sc.AppArmorProfile.Type != corev1.AppArmorProfileTypeUnconfined {
			fields = append(fields, "appArmorProfile")
		}
		if len(sc.Sysctls) > 0 {
			fields = append(fields, "sysctls")
		}
		if sc.SELinuxChangePolicy != nil {
			fields = append(fields, "seLinuxChangePolicy")
		}
		if len(fields) > 0 {
			parts = append(parts, "pod: "+strings.Join(fields, ", "))
		}
	}
	containers := make([]corev1.Container, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	containers = append(containers, pod.Spec.InitContainers...)
	containers = append(containers, pod.Spec.Containers...)
	for i := range containers {
		sc := containers[i].SecurityContext
		if sc == nil {
			continue
		}
		var fields []string
		if sc.ReadOnlyRootFilesystem != nil && *sc.ReadOnlyRootFilesystem {
			fields = append(fields, "readOnlyRootFilesystem")
		}
		if sc.AllowPrivilegeEscalation != nil && *sc.AllowPrivilegeEscalation {
			fields = append(fields, "allowPrivilegeEscalation")
		}
		if sc.ProcMount != nil && *sc.ProcMount != corev1.DefaultProcMount {
			fields = append(fields, "procMount")
		}
		if sc.SELinuxOptions != nil {
			fields = append(fields, "seLinuxOptions")
		}
		if sc.SeccompProfile != nil && sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
			fields = append(fields, "seccompProfile")
		}
		if sc.AppArmorProfile != nil && sc.AppArmorProfile.Type != corev1.AppArmorProfileTypeUnconfined {
			fields = append(fields, "appArmorProfile")
		}
		if len(fields) > 0 {
			parts = append(parts, "container "+containers[i].Name+": "+strings.Join(fields, ", "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ")
}
