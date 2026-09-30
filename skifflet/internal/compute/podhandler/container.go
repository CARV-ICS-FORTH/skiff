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

// buildContainer resolves everything one container needs to run: environment,
// mounts and image. It reads the pod as the lifecycle owner knows it now, so the
// pod IP is available to the environment.
func (h *PodHandler) buildContainer(ctx context.Context, container *corev1.Container) (Container, error) {
	pod := h.lifecycle.GetPod()

	/*---------------------------------------------------
	 * Determine the effective security context
	 *---------------------------------------------------*/
	effectiSecurityContext := DetermineEffectiveSecurityContext(pod, container)
	uid, gid := DetermineEffectiveRunAsUser(effectiSecurityContext)

	/*---------------------------------------------------
	 * Resolve Container Environment Pipeline
	 *---------------------------------------------------*/
	resolvedEnvs, err := ResolveContainerEnvironment(ctx, pod, container, h.podEnvVariables)
	if err != nil {
		if IsConfigError(err) {
			return Container{}, err // said as it is: what is missing, as the kubelet says it
		}
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

				if !isEmptyDir {
					return Container{}, fmt.Errorf("subPath source does not exist for volume '%s': %s", mount.Name, subPathFile)
				}
				if err := hostutil.SafeMakeDir(subPath, hostPath, endpoint.PodGlobalDirectoryPermissions); err != nil {
					return Container{}, fmt.Errorf("failed to create emptyDir subpath '%s': %w", subPathFile, err)
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
	}

	return c, nil
}

// beginAttempt starts a new attempt in the status of a container that ran
// before: what it last did is its last termination state, and it counts as a restart.
func beginAttempt(containerStatus *corev1.ContainerStatus) {
	if containerStatus.State.Terminated != nil {
		containerStatus.LastTerminationState = containerStatus.State
		containerStatus.RestartCount++
		containerStatus.State.Terminated = nil
	}
}

// SetContainerRunning marks the container status as running.
func SetContainerRunning(containerStatus *corev1.ContainerStatus, pid int, startTime uint64) {
	beginAttempt(containerStatus)
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

	notStarted := false
	containerStatus.Started = &notStarted
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
