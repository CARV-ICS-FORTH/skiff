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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/image"
	"skifflet/internal/compute/runtime"
	kubecontainer "skifflet/pkg/container"
	"skifflet/pkg/hostutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	mounter "k8s.io/utils/mount"
)

// ResolveContainerEnvironment resolves the final container environment by:
// 1. Starting with cluster/service-derived variables (lower precedence)
// 2. Applying container-specific environment variables from EnvFrom (ConfigMaps/Secrets)
// 3. Applying container-specific environment variables (higher precedence)
// 4. Resolving deferred runtime FieldRefs (status.podIP, status.hostIP, metadata.name, metadata.namespace, metadata.uid)
func ResolveContainerEnvironment(pod *corev1.Pod, container *corev1.Container, serviceEnvs []corev1.EnvVar) ([]corev1.EnvVar, error) {
	envMap := make(map[string]string)
	var order []string

	addOrUpdate := func(name, value string) {
		if _, exists := envMap[name]; !exists {
			order = append(order, name)
		}
		envMap[name] = value
	}

	// 1. Lower precedence: service-derived environment variables
	for _, envVar := range serviceEnvs {
		addOrUpdate(envVar.Name, envVar.Value)
	}

	// 2. Middle precedence: container.EnvFrom (ConfigMaps and Secrets)
	ctx := context.Background()
	for _, envFrom := range container.EnvFrom {
		if envFrom.ConfigMapRef != nil {
			optional := envFrom.ConfigMapRef.Optional != nil && *envFrom.ConfigMapRef.Optional
			if compute.K8SClientset != nil {
				cm, err := compute.K8SClientset.CoreV1().ConfigMaps(pod.Namespace).Get(ctx, envFrom.ConfigMapRef.Name, metav1.GetOptions{})
				if err != nil {
					if !optional {
						return nil, fmt.Errorf("configmap %q not found: %w", envFrom.ConfigMapRef.Name, err)
					}
				} else if cm != nil {
					for k, v := range cm.Data {
						key := envFrom.Prefix + k
						if len(validation.IsCIdentifier(key)) == 0 {
							addOrUpdate(key, v)
						}
					}
				}
			} else if !optional {
				return nil, fmt.Errorf("k8s clientset unavailable to resolve configmap %q", envFrom.ConfigMapRef.Name)
			}
		} else if envFrom.SecretRef != nil {
			optional := envFrom.SecretRef.Optional != nil && *envFrom.SecretRef.Optional
			if compute.K8SClientset != nil {
				sec, err := compute.K8SClientset.CoreV1().Secrets(pod.Namespace).Get(ctx, envFrom.SecretRef.Name, metav1.GetOptions{})
				if err != nil {
					if !optional {
						return nil, fmt.Errorf("secret %q not found: %w", envFrom.SecretRef.Name, err)
					}
				} else if sec != nil {
					for k, v := range sec.Data {
						key := envFrom.Prefix + k
						if len(validation.IsCIdentifier(key)) == 0 {
							addOrUpdate(key, string(v))
						}
					}
				}
			} else if !optional {
				return nil, fmt.Errorf("k8s clientset unavailable to resolve secret %q", envFrom.SecretRef.Name)
			}
		}
	}

	// 3. Higher precedence: container.Env (user-specified, explicit overrides, ValueFrom, etc.)
	for _, envVar := range container.Env {
		val := envVar.Value
		// Deferred FieldRef and ValueFrom resolution
		if envVar.ValueFrom != nil {
			if envVar.ValueFrom.FieldRef != nil {
				switch envVar.ValueFrom.FieldRef.FieldPath {
				case "status.podIP":
					val = pod.Status.PodIP
				case "status.hostIP":
					val = pod.Status.HostIP
				case "metadata.name":
					val = pod.Name
				case "metadata.namespace":
					val = pod.Namespace
				case "metadata.uid":
					val = string(pod.UID)
				}
			} else if envVar.ValueFrom.ConfigMapKeyRef != nil {
				ref := envVar.ValueFrom.ConfigMapKeyRef
				optional := ref.Optional != nil && *ref.Optional
				if compute.K8SClientset != nil {
					cm, err := compute.K8SClientset.CoreV1().ConfigMaps(pod.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
					if err != nil {
						if !optional {
							return nil, fmt.Errorf("configmap %q not found: %w", ref.Name, err)
						}
					} else if cm != nil {
						if v, ok := cm.Data[ref.Key]; ok {
							val = v
						} else if !optional {
							return nil, fmt.Errorf("key %q not found in configmap %q", ref.Key, ref.Name)
						}
					}
				} else if !optional {
					return nil, fmt.Errorf("k8s clientset unavailable to resolve configmap %q", ref.Name)
				}
			} else if envVar.ValueFrom.SecretKeyRef != nil {
				ref := envVar.ValueFrom.SecretKeyRef
				optional := ref.Optional != nil && *ref.Optional
				if compute.K8SClientset != nil {
					sec, err := compute.K8SClientset.CoreV1().Secrets(pod.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
					if err != nil {
						if !optional {
							return nil, fmt.Errorf("secret %q not found: %w", ref.Name, err)
						}
					} else if sec != nil {
						if v, ok := sec.Data[ref.Key]; ok {
							val = string(v)
						} else if !optional {
							return nil, fmt.Errorf("key %q not found in secret %q", ref.Key, ref.Name)
						}
					}
				} else if !optional {
					return nil, fmt.Errorf("k8s clientset unavailable to resolve secret %q", ref.Name)
				}
			}
		} else if val == ".status.podIP" { // legacy skifflet convention
			val = pod.Status.PodIP
		}

		addOrUpdate(envVar.Name, val)
	}

	resolved := make([]corev1.EnvVar, len(order))
	for i, name := range order {
		resolved[i] = corev1.EnvVar{
			Name:  name,
			Value: envMap[name],
		}
	}
	return resolved, nil
}

func findVolume(pod *corev1.Pod, name string) *corev1.Volume {
	if pod == nil {
		return nil
	}
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// buildContainer replicates the container preparation behavior.
func (h *PodHandler) buildContainer(container *corev1.Container, containerStatus *corev1.ContainerStatus) (Container, error) {
	/*---------------------------------------------------
	 * Determine the effective security context
	 *---------------------------------------------------*/
	effectiSecurityContext := DetermineEffectiveSecurityContext(h.Pod, container)
	uid, gid := DetermineEffectiveRunAsUser(effectiSecurityContext)

	/*---------------------------------------------------
	 * Resolve Container Environment Pipeline
	 *---------------------------------------------------*/
	resolvedEnvs, err := ResolveContainerEnvironment(h.Pod, container, h.podEnvVariables)
	if err != nil {
		return Container{}, fmt.Errorf("failed to resolve container environment: %w", err)
	}

	/*---------------------------------------------------
	 * Generate Environment Variables File
	 *---------------------------------------------------*/
	var b strings.Builder
	for _, envVar := range resolvedEnvs {
		if len(validation.IsCIdentifier(envVar.Name)) != 0 {
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", envVar.Name, EscapeSingleQuote(envVar.Value))
	}

	envfilePath := h.podDirectory.Container(container.Name).EnvFilePath()
	if err := os.WriteFile(envfilePath, []byte(b.String()), endpoint.PodGlobalDirectoryPermissions); err != nil {
		return Container{}, fmt.Errorf("cannot write env file for container '%s' of pod '%s': %w", container.Name, h.podKey, err)
	}

	/*---------------------------------------------------
	 * Prepare Mountpoints
	 *---------------------------------------------------*/
	binds := make([]string, len(container.VolumeMounts))

	for i, mount := range container.VolumeMounts {
		hostPath := filepath.Join(h.podDirectory.VolumeDir(), mount.Name)

		subPath := mount.SubPath
		var err error
		if mount.SubPathExpr != "" {
			subPath, err = kubecontainer.ExpandContainerVolumeMounts(mount, resolvedEnvs)
			if err != nil {
				return Container{}, fmt.Errorf("cannot expand env variables for container '%s' of pod '%s': %w", container.Name, h.podKey, err)
			}
		}

		if subPath != "" {
			if filepath.IsAbs(subPath) {
				return Container{}, fmt.Errorf("error SubPath '%s' must not be an absolute path", subPath)
			}

			subPathFile := filepath.Join(hostPath, subPath)

			subPathFileExists, err := mounter.PathExists(subPathFile)
			if err != nil {
				return Container{}, fmt.Errorf("could not determine if subPath exists. mount:'%v': %w", mount, err)
			}

			if !subPathFileExists {
				vol := findVolume(h.Pod, mount.Name)
				isEmptyDir := vol != nil && vol.VolumeSource.EmptyDir != nil

				if isEmptyDir || strings.HasSuffix(subPath, "/") {
					if err := hostutil.SafeMakeDir(subPath, hostPath, endpoint.PodGlobalDirectoryPermissions); err != nil {
						return Container{}, fmt.Errorf("failed to create dir placeholder. subpath:'%s': %w", subPathFile, err)
					}
				} else {
					if err := os.MkdirAll(filepath.Dir(subPathFile), endpoint.PodGlobalDirectoryPermissions); err != nil {
						return Container{}, fmt.Errorf("failed to create parent dir for subpath:'%s': %w", subPathFile, err)
					}
					if err = os.WriteFile(subPathFile, []byte{}, endpoint.PodGlobalDirectoryPermissions); err != nil {
						return Container{}, fmt.Errorf("failed to create placeholder. subpath:'%s': %w", subPathFile, err)
					}
				}
			}

			hostPath = subPathFile
		}

		accessMode := "rw"
		if mount.ReadOnly {
			accessMode = "ro"
		}

		binds[i] = hostPath + ":" + mount.MountPath + ":" + accessMode
	}

	containerID := fmt.Sprintf("%s_%s_%s", h.Pod.GetNamespace(), h.Pod.GetName(), container.Name)
	/*---------------------------------------------------
	 * Prepare Container Image
	 *---------------------------------------------------*/
	pullPolicy := container.ImagePullPolicy
	if pullPolicy == "" {
		if strings.HasSuffix(container.Image, ":latest") || !strings.Contains(container.Image, ":") {
			pullPolicy = corev1.PullAlways
		} else {
			pullPolicy = corev1.PullIfNotPresent
		}
	}

	var pullCtx context.Context = context.Background()
	if h.lifecycle != nil {
		pullCtx = h.lifecycle.Context()
	}

	var img *image.Image
	if pullPolicy == corev1.PullNever {
		img, err = image.ResolveLocal(compute.Skiff.ImageDir(), container.Image)
	} else {
		img, err = image.PullWithPolicy(pullCtx, compute.Skiff.ImageDir(), image.Docker, container.Image, pullPolicy)
	}

	if err != nil {
		return Container{}, fmt.Errorf("ImagePull error. Image:%s: %w", container.Image, err)
	}

	executionMode := "exec"
	if container.Command == nil {
		executionMode = "run"
	}

	/*---------------------------------------------------
	 * Prepare fields for Container Execution
	 *---------------------------------------------------*/
	containerPath := h.podDirectory.Container(container.Name)

	hostname := h.Pod.GetName()
	if h.Pod.Spec.Hostname != "" {
		hostname = h.Pod.Spec.Hostname
	}

	c := Container{
		InstanceName:  containerID,
		Hostname:      hostname,
		RunAsUser:     uid,
		RunAsGroup:    gid,
		ImageFilePath: img.Filepath,
		EnvFilePath:   containerPath.EnvFilePath(),
		Binds:         binds,
		Command:       kubecontainer.ExpandContainerCommandOnlyStatic(container.Command, resolvedEnvs),
		Args:          kubecontainer.ExpandContainerCommandOnlyStatic(container.Args, resolvedEnvs),
		ExecutionMode: executionMode,
		LogsPath:      containerPath.LogsPath(),
	}

	/*---------------------------------------------------
	 * Update Container Status Fields
	 *---------------------------------------------------*/
	containerStatus.Name = container.Name
	containerStatus.ContainerID = containerID
	containerStatus.Image = container.Image
	containerStatus.ImageID = img.Filepath

	return c, err
}

// SetContainerRunning marks the container status as running.
func SetContainerRunning(containerStatus *corev1.ContainerStatus, pid int, startTime uint64) {
	if containerStatus.State.Terminated != nil {
		containerStatus.LastTerminationState = containerStatus.State
		containerStatus.RestartCount++
	}
	containerStatus.ContainerID = runtime.FormatProcessJobID(pid, startTime)
	containerStatus.State.Waiting = nil
	containerStatus.State.Running = &corev1.ContainerStateRunning{
		StartedAt: metav1.Now(),
	}
	containerStatus.State.Terminated = nil
	started := true
	containerStatus.Started = &started
	containerStatus.Ready = true
}

// SetContainerTerminated marks the container status as terminated.
func SetContainerTerminated(containerStatus *corev1.ContainerStatus, exitCode int) {
	prevState := containerStatus.State

	var reason, message string
	if exitCode == 0 {
		reason = "Completed"
		message = "Container successfully terminated"
	} else {
		reason = "Error(" + containerStatus.Name + ")"
		message = HumanReadableCode(exitCode)
	}

	startedAt := metav1.Time{}
	if prevState.Running != nil {
		startedAt = prevState.Running.StartedAt
	} else if prevState.Terminated != nil {
		startedAt = prevState.Terminated.StartedAt
	}

	containerStatus.Ready = false
	containerStatus.State.Waiting = nil
	containerStatus.State.Running = nil
	containerStatus.State.Terminated = &corev1.ContainerStateTerminated{
		ExitCode:    int32(exitCode),
		Signal:      0,
		Reason:      reason,
		Message:     message,
		StartedAt:   startedAt,
		FinishedAt:  metav1.Now(),
		ContainerID: containerStatus.ContainerID,
	}
}

// SyncContainerStatuses updates in-memory container statuses and verifies alive processes in /proc.
func SyncContainerStatuses(pod *corev1.Pod) {
	checkStatus := func(containerStatus *corev1.ContainerStatus) {
		if containerStatus.State.Terminated != nil {
			return
		}

		if containerStatus.State.Running != nil {
			// Do not synthesize exit code 137; command waiters are authoritative for exit status.
			// Liveness polling only confirms process liveness without altering exit accounting.
			return
		}

		if containerStatus.State.Waiting == nil {
			containerStatus.State.Waiting = &corev1.ContainerStateWaiting{
				Reason:  "ContainerStarting",
				Message: "Container is starting",
			}
		}
	}

	for i := range pod.Status.InitContainerStatuses {
		checkStatus(&pod.Status.InitContainerStatuses[i])
	}
	for i := range pod.Status.ContainerStatuses {
		checkStatus(&pod.Status.ContainerStatuses[i])
	}
}
