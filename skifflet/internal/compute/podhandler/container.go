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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/gpu"
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

// An ImagePullError says that a container's image cannot be had: it could not be pulled, or,
// with imagePullPolicy Never, it is not on the node. The kubelet does not fail the pod for it:
// it tries again with a back-off (see prepareContainer).
type ImagePullError struct {
	Image string
	Never bool
	Err   error
}

func (e *ImagePullError) Error() string {
	if e.Never {
		return fmt.Sprintf("container image %q is not present with pull policy of Never: %v", e.Image, e.Err)
	}
	return fmt.Sprintf("failed to pull image %q: %v", e.Image, e.Err)
}

func (e *ImagePullError) Unwrap() error { return e.Err }

// Reason is the waiting reason the kubelet gives it.
func (e *ImagePullError) Reason() string {
	if e.Never {
		return "ErrImageNeverPull"
	}
	return "ErrImagePull"
}

// imageConfigOf says who an image runs as (its USER) and where it starts (its WORKDIR); tests
// replace it, since their images are files that are not SIFs.
var imageConfigOf = func(ctx context.Context, img *image.Image) (image.Config, error) { return img.Config(ctx) }

// workingDirOf is the directory a container starts in, as with the kubelet's runtime: its
// workingDir, else its image's WORKDIR, else /. It must be an absolute path.
func workingDirOf(container *corev1.Container, imageDir string) (string, error) {
	dir := container.WorkingDir
	if dir == "" {
		dir = imageDir
	}
	if dir == "" {
		return "/", nil
	}
	if !filepath.IsAbs(dir) {
		return "", &ConfigError{Message: fmt.Sprintf("the working directory %q is not an absolute path", dir), Reason: "CreateContainerConfigError"}
	}
	return filepath.Clean(dir), nil
}

// buildContainer resolves everything one container needs to run: environment,
// mounts and image. It reads the pod as the lifecycle owner knows it now, so the
// pod IP is available to the environment.
func (h *PodHandler) buildContainer(ctx context.Context, container *corev1.Container) (Container, error) {
	pod := h.lifecycle.GetPod()

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

	// A container with GPUs has them named as the device plugin names them. The variable
	// is set after the pod's own, so that an image's "all" (the NVIDIA images set it) or a
	// pod's does not name GPUs the container does not have.
	devices := h.gpus[container.Name]
	if len(devices) > 0 {
		resolvedEnvs = append(resolvedEnvs, corev1.EnvVar{Name: "NVIDIA_VISIBLE_DEVICES", Value: visibleDevices(devices)})
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
			// As the kubelet: an absolute subPath, or one that steps up (..), is refused after
			// expansion, and so is one that leaves its volume through a symlink (below).
			if filepath.IsAbs(subPath) {
				return Container{}, &ConfigError{Message: fmt.Sprintf("subPath %q of volume %q must not be an absolute path", subPath, mount.Name), Reason: "CreateContainerConfigError"}
			}
			for _, part := range strings.Split(filepath.ToSlash(subPath), "/") {
				if part == ".." {
					return Container{}, &ConfigError{Message: fmt.Sprintf("subPath %q of volume %q must not contain '..'", subPath, mount.Name), Reason: "CreateContainerConfigError"}
				}
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

			// What the subPath is, its symlinks followed, must be inside the volume (whose own
			// path may be a symlink: a hostPath or a claim's volume): a symlink made in the volume
			// (by an init container, say) cannot take the mount out of it. The resolved path is
			// what is bound.
			resolved, err := subPathInside(hostPath, subPathFile)
			if err != nil {
				return Container{}, &ConfigError{Message: fmt.Sprintf("subPath %q of volume %q: %v", subPath, mount.Name, err), Reason: "CreateContainerConfigError"}
			}
			hostPath = resolved
		}

		accessMode := "rw"
		if mount.ReadOnly || claimReadOnly(h.Pod, mount.Name) {
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
		return Container{}, &ImagePullError{Image: container.Image, Never: pullPolicy == corev1.PullNever, Err: err}
	}

	/*---------------------------------------------------
	 * Determine the user: the pod's, else the image's, else root
	 *---------------------------------------------------*/
	imageConfig, err := imageConfigOf(pullCtx, img)
	if err != nil {
		var notFound *image.NotFoundError
		if errors.As(err, &notFound) {
			return Container{}, &ConfigError{Message: notFound.Message, Reason: "CreateContainerError"}
		}
		return Container{}, fmt.Errorf("image %s: %w", container.Image, err)
	}
	uid, gid, err := DetermineEffectiveRunAsUser(DetermineEffectiveSecurityContext(pod, container), imageConfig.User)
	if err != nil {
		var cfg *ConfigError
		if errors.As(err, &cfg) {
			return Container{}, &ConfigError{Message: fmt.Sprintf("%s (pod: \"%s_%s(%s)\", container: %s)", cfg.Message, pod.Name, pod.Namespace, pod.UID, container.Name), Reason: cfg.Reason}
		}
		return Container{}, err
	}

	workingDir, err := workingDirOf(container, imageConfig.WorkingDir)
	if err != nil {
		return Container{}, err
	}
	stopSignal, err := stopSignalOf(container, imageConfig.StopSignal)
	if err != nil {
		return Container{}, err
	}

	executionMode := "exec"
	if container.Command == nil {
		executionMode = "run"
	}

	/*---------------------------------------------------
	 * Prepare fields for Container Execution
	 *---------------------------------------------------*/
	containerPath := h.podDirectory.Container(container.Name)

	hostname, err := containerHostname(h.Pod, compute.Environment.ClusterDomainOrDefault())
	if err != nil {
		return Container{}, err
	}

	c := Container{
		Name:          container.Name,
		InstanceName:  containerID,
		Hostname:      hostname,
		RunAsUser:     uid,
		RunAsGroup:    gid,
		ImageFilePath: img.Filepath,
		WorkingDir:    workingDir,
		StopSignal:    stopSignal,
		TerminationMessagePath: func() string {
			if terminationMessageBound(container.TerminationMessagePath) {
				return container.TerminationMessagePath
			}
			return ""
		}(),
		EnvFilePath:   containerPath.EnvFilePath(),
		Binds:         binds,
		GPUs:          minors(devices),
		Command:       kubecontainer.ExpandContainerCommand(container.Command, resolvedEnvs),
		Args:          kubecontainer.ExpandContainerCommand(container.Args, resolvedEnvs),
		ExecutionMode: executionMode,
	}

	return c, nil
}

// subPathInside resolves path (below the volume at root) with its symlinks, and says whether it
// stays inside the volume, itself resolved.
func subPathInside(root, path string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("it resolves to %s, outside its volume", real)
	}
	return real, nil
}

// beginAttempt starts attempt N in the status of a container: what it last did is its last
// termination state, and every attempt after the first counts as a restart.
func beginAttempt(containerStatus *corev1.ContainerStatus, attempt int) {
	if containerStatus.State.Terminated != nil {
		containerStatus.LastTerminationState = corev1.ContainerState{Terminated: containerStatus.State.Terminated}
		containerStatus.State.Terminated = nil
	}
	if attempt > 1 {
		containerStatus.RestartCount = int32(attempt - 1)
	}
}

// SetContainerRunning marks the container status as running, as attempt N.
func SetContainerRunning(containerStatus *corev1.ContainerStatus, pid int, startTime uint64, attempt int) {
	beginAttempt(containerStatus, attempt)
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
		reason = "Error"
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

// claimReadOnly says whether the pod asks for a volume to be read-only at the claim
// (persistentVolumeClaim.readOnly), which applies to every mount of it.
func claimReadOnly(pod *corev1.Pod, volume string) bool {
	vol := findVolume(pod, volume)
	return vol != nil && vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ReadOnly
}

// minors lists the minor numbers of GPUs.
func minors(devices []gpu.Device) []int {
	out := make([]int, len(devices))
	for i, d := range devices {
		out[i] = d.Minor
	}
	return out
}
