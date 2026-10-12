// Copyright © 2022 FORTH-ICS
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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/gpu"
	"skifflet/internal/compute/image"
	"skifflet/internal/compute/runtime"
	"skifflet/internal/compute/volume/projected"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// observePodIP reads the IPv4 address of eth0 in the network namespace of process pid, as the
// node sees it, and returns the first that is a pod address of the node's subnet (Plaid's,
// see waitForPodNetwork). The sandbox handshake says what the address is; this checks that
// the namespace really has it.
func observePodIP(pid int, subnet *net.IPNet) (string, error) {
	netnsPath := fmt.Sprintf("/proc/%d/ns/net", pid)
	if _, err := os.Stat(netnsPath); err != nil {
		return "", fmt.Errorf("netns path %s: %w", netnsPath, err)
	}
	hostNetns, _ := os.Readlink("/proc/1/ns/net")
	deadline := time.Now().Add(15 * time.Second)
	for {
		// The process must have a network namespace of its own.
		if currNetns, _ := os.Readlink(netnsPath); currNetns != "" && currNetns != hostNetns {
			output, err := exec.Command("nsenter", "-t", strconv.Itoa(pid), "-n", "ip", "-o", "-4", "addr", "show", "dev", "eth0").Output()
			if err == nil {
				for _, line := range strings.Split(string(output), "\n") {
					fields := strings.Fields(line)
					for i, field := range fields {
						if field == "inet" && i+1 < len(fields) {
							ip := strings.Split(fields[i+1], "/")[0]
							if VerifyPodIP(ip, subnet) == nil {
								return ip, nil
							}
						}
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for an address of %v on eth0 in the netns of PID %d", subnet, pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// SafeRemovePodDirectory removes a pod directory, retrying for NFS file handle release
// and safely handling permissions without modifying symlink targets on the host.
func SafeRemovePodDirectory(podDir string) error {
	var removeErr error
	for attempt := 0; attempt < 10; attempt++ {
		removeErr = os.RemoveAll(podDir)
		if removeErr == nil || errors.Is(removeErr, fs.ErrNotExist) {
			return nil
		}
		if errors.Is(removeErr, fs.ErrPermission) {
			break // retrying does not help; the modes are fixed below
		}
		time.Sleep(100 * time.Millisecond)
	}

	if errors.Is(removeErr, fs.ErrPermission) {
		makeRemovable(podDir)
		return os.RemoveAll(podDir)
	}
	return removeErr
}

// prepareAttemptScratch gives attempt N of a container its own /tmp and /var/tmp, empty
// (a container's temporary files are gone when it is restarted, as in Kubernetes, where
// they are part of the writable layer of the container that ran), its writable layer (an
// empty overlay directory, see ContainerPath.AttemptOverlayDir) and the directory Apptainer
// unpacks the image in (see ContainerPath.AttemptUnpackDir). What the attempts before it left is removed
// first, so a pod keeps one set per container, and a restart can never see its
// predecessor's files. The temporary directories are world-writable with the sticky bit,
// like a /tmp, whatever user the container runs as.
//
// The working directory, when it is not /, is made in the writable layer (its upper
// directory, which Apptainer lays over the image), so that a directory the image does not
// have exists when the container starts there, as the kubelet's runtime makes it.
func prepareAttemptScratch(container endpoint.ContainerPath, attempt int, workingDir string) error {
	attempts, err := container.ScratchAttempts()
	if err != nil {
		return err
	}
	for _, old := range attempts {
		if err := SafeRemovePodDirectory(container.AttemptScratchDir(old)); err != nil {
			return fmt.Errorf("cannot remove the temporary files of attempt %d: %w", old, err)
		}
	}
	for _, dir := range []string{container.AttemptTmp(attempt), container.AttemptVarTmp(attempt)} {
		if err := os.MkdirAll(dir, 0o1777); err != nil {
			return err
		}
		if err := os.Chmod(dir, os.ModeSticky|0o777); err != nil { // the umask would have trimmed the mode
			return err
		}
	}
	for _, dir := range []string{container.AttemptOverlayDir(attempt), container.AttemptUnpackDir(attempt)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if workingDir != "" && workingDir != "/" {
		if err := os.MkdirAll(filepath.Join(container.AttemptOverlayDir(attempt), "upper", workingDir), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// podPIDsRecord is the on-disk record of a pod's processes (pids.json in the job
// directory). The lifecycle owner holds the same facts in memory; this copy exists
// only so that a later run of Skifflet can reap what this one left behind.
type podPIDsRecord struct {
	PausePID            int            `json:"pause_pid"`
	PauseStartTime      uint64         `json:"pause_start_time,omitempty"`
	ContainerPIDs       []int          `json:"container_pids"`
	ContainerStartTimes map[int]uint64 `json:"container_start_times,omitempty"`
}

func readProcessRecord(podDir endpoint.PodPath) (podPIDsRecord, bool) {
	var rec podPIDsRecord
	data, err := os.ReadFile(podDir.PidsFile())
	if err != nil || json.Unmarshal(data, &rec) != nil {
		return podPIDsRecord{}, false
	}
	return rec, true
}

// jobIDs lists the recorded processes as verifiable job IDs: the pause launcher
// first, then the container launchers.
func (r podPIDsRecord) jobIDs() (pause string, containers []string) {
	if r.PausePID > 1 {
		pause = runtime.FormatProcessJobID(r.PausePID, r.PauseStartTime)
	}
	for _, pid := range r.ContainerPIDs {
		if pid > 1 {
			containers = append(containers, runtime.FormatProcessJobID(pid, r.ContainerStartTimes[pid]))
		}
	}
	return pause, containers
}

// ReapRecordedProcesses kills the processes a pod directory's record names, but
// only those that are still the recorded process (same start time). It is the
// single crash-cleanup path: used when Skifflet starts, before a pod is prepared,
// and when a pod without a live owner is deleted.
func ReapRecordedProcesses(podDir endpoint.PodPath) {
	rec, ok := readProcessRecord(podDir)
	if !ok {
		return
	}
	kill := func(pid int, start uint64) {
		if pid > 1 && runtime.VerifyProcessIdentity(pid, start) && !runtime.IsProcessDead(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	for _, pid := range rec.ContainerPIDs {
		kill(pid, rec.ContainerStartTimes[pid])
	}
	kill(rec.PausePID, rec.PauseStartTime)
}

// podEndpointID returns the Plaid endpoint recorded by the sandbox handshake.
func podEndpointID(podDir endpoint.PodPath) string {
	data, err := os.ReadFile(podDir.ReadinessFile())
	if err != nil {
		return ""
	}
	var info struct {
		EndpointID string `json:"endpoint_id"`
	}
	if json.Unmarshal(data, &info) != nil {
		return ""
	}
	return info.EndpointID
}

// TerminationGrace is how long a pod's processes get to stop: the grace period
// of the deletion request (kubectl delete --grace-period) when it has one, then
// the value the pod was launched with, then the Kubernetes default of 30 seconds.
func TerminationGrace(request, launched *corev1.Pod) time.Duration {
	if request != nil && request.DeletionGracePeriodSeconds != nil && *request.DeletionGracePeriodSeconds >= 0 {
		return time.Duration(*request.DeletionGracePeriodSeconds) * time.Second
	}
	if launched != nil && launched.Spec.TerminationGracePeriodSeconds != nil && *launched.Spec.TerminationGracePeriodSeconds >= 0 {
		return time.Duration(*launched.Spec.TerminationGracePeriodSeconds) * time.Second
	}
	return 30 * time.Second
}

// makeRemovable restores owner access to every directory below path so that it
// can be deleted: an unpacked container root filesystem contains directories
// without any permissions. It never follows symlinks, so it cannot change
// anything outside the tree (a HostPath volume is a symlink into the host).
func makeRemovable(path string) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return
	}
	_ = os.Chmod(path, 0700)
	entries, _ := os.ReadDir(path)
	for _, entry := range entries {
		makeRemovable(filepath.Join(path, entry.Name()))
	}
}

// DeletePod terminates all processes associated with a Pod and removes its directory structure.
func DeletePod(podKey client.ObjectKey, localPod *corev1.Pod) bool {
	logger := compute.DefaultLogger.WithValues("pod", podKey)

	podDir := compute.Skiff.Pod(podKey)
	if localPod != nil {
		podDir = compute.Skiff.PodWithUID(podKey, localPod.GetUID())
	}
	_, statErr := os.Stat(podDir.String())
	logger.Info(" * DeletePod invoked", "podDir", podDir.String(), "dirExists", statErr == nil)

	gracePeriod := TerminationGrace(localPod, localPod)

	var pl *PodLifecycle
	if localPod != nil && localPod.GetUID() != "" {
		pl = GlobalRegistry.Get(localPod.GetUID())
	}
	if pl == nil && (localPod == nil || localPod.GetUID() == "") {
		pl = GlobalRegistry.GetByKey(podKey)
	}
	endpointID := podEndpointID(podDir)
	if pl != nil {
		pl.Terminate(gracePeriod)
		GlobalRegistry.DeleteOwner(pl)
	} else if rec, ok := readProcessRecord(podDir); ok {
		// No live owner: what remains is what the on-disk record names.
		pause, containers := rec.jobIDs()
		out, err := runtime.KillPodProcessesWithTimeout(pause, containers, gracePeriod)
		if err != nil && !errors.Is(err, runtime.ErrInvalidJob) {
			logger.Info("WARNING: Failed to kill recorded processes, proceeding to remove pod directory", "pause", pause, "err", err, "out", out)
		} else {
			logger.Info(" * Recorded processes are terminated", "pause", pause, "out", out)
		}
		ReapRecordedProcesses(podDir)
	}

	if endpointID != "" {
		_ = RemovePlaidEndpoint(endpointID)
	}

	// Remove Pod Directory (with retry for NFS file handle release)
	if err := SafeRemovePodDirectory(podDir.String()); err != nil {
		logger.Error(err, "failed to remove pod directory", "directory", podDir)
		return false
	}

	logger.Info(" * Pod directory is removed")

	// Garbage collect empty namespace directory
	namespaceDir := filepath.Dir(podDir.String())
	if empty, _ := endpoint.IsEmpty(namespaceDir); empty {
		if err := os.Remove(namespaceDir); err == nil {
			logger.Info(" * Namespace directory is removed")
		}
	}

	return true
}

// PodHandler carries what preparing one pod needs. The embedded Pod is the spec
// the pod was launched with: it is read (volumes, DNS) and never written. Status
// is owned by the lifecycle, not by this type.
type PodHandler struct {
	pidsMu sync.Mutex

	*corev1.Pod
	lifecycle *PodLifecycle

	podKey client.ObjectKey

	podEnvVariables []corev1.EnvVar
	podDirectory    endpoint.PodPath
	expectedSubnet  *net.IPNet

	// gpus are the GPUs of each container that asks for some, by name (see reserveGPUs).
	gpus map[string][]gpu.Device

	// projected are the writers of the pod's projected volumes, by name, which syncVolumes
	// keeps (their service account tokens are renewed through them).
	projected map[string]*projected.VolumeMounter

	logger logr.Logger
	pids   podPIDsRecord
}

func (h *PodHandler) savePIDs(pausePID int, containerPID int) {
	h.pidsMu.Lock()
	defer h.pidsMu.Unlock()
	pidsPath := h.podDirectory.PidsFile()
	pids := &h.pids
	if pids.ContainerStartTimes == nil {
		pids.ContainerStartTimes = make(map[int]uint64)
	}
	if pausePID > 0 {
		pids.PausePID = pausePID
		if st, err := runtime.GetProcessStartTime(pausePID); err == nil {
			pids.PauseStartTime = st
		}
	}
	if containerPID > 0 {
		exists := false
		for _, p := range pids.ContainerPIDs {
			if p == containerPID {
				exists = true
				break
			}
		}
		if !exists {
			pids.ContainerPIDs = append(pids.ContainerPIDs, containerPID)
		}
		if st, err := runtime.GetProcessStartTime(containerPID); err == nil {
			pids.ContainerStartTimes[containerPID] = st
		}
	}
	if data, err := json.Marshal(pids); err == nil {
		f, err := os.CreateTemp(h.podDirectory.JobDir(), ".pids-*")
		if err != nil {
			h.logger.Error(err, "failed to create process record")
			return
		}
		defer os.Remove(f.Name())
		if _, err = f.Write(data); err == nil {
			err = f.Chmod(0644)
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(f.Name(), pidsPath)
		}
		if err != nil {
			h.logger.Error(err, "failed to save process record")
		}
	}
}

// bindVariables are the environment variables with which Apptainer takes bind mounts
// from its caller. The bubble was started with binds, and Apptainer puts them in the
// environment of what runs in it, so a container launched with them would mount the
// bubble's directories (the user's ~/.skiff with the cluster's credentials among them).
var bindVariables = []string{"APPTAINER_BIND", "SINGULARITY_BIND", "APPTAINER_BINDPATH", "SINGULARITY_BINDPATH"}

// launchEnv is the environment of an Apptainer process of this pod: the
// process's own, without the bubble's bind mounts (a container gets the volumes of
// its pod and nothing else), with Apptainer's temporary directory tmp (inside the pod
// directory: the pod's own for the sandbox, the attempt's for a container, where its
// image is unpacked), and Apptainer quiet. Its informational messages ("Cleaning up image...")
// would otherwise be taken for the container's own output and end up in its log; its
// warnings and errors still appear.
func (h *PodHandler) launchEnv(tmp string) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(bindVariables, name) {
			env = append(env, entry)
		}
	}
	return append(env, "APPTAINER_TMPDIR="+tmp, "SINGULARITY_TMPDIR="+tmp, "APPTAINER_QUIET=true", "SINGULARITY_QUIET=true")
}

// attemptResult says how one run of a container ended.
type attemptResult int

const (
	attemptExited     attemptResult = iota // the process ran and exited
	attemptNotStarted                      // it could not start (or the pod was revoked); reported as exit code 128
	attemptFilesError                      // its log file or its /tmp could not be created; fatal for the pod
)

// runAttempt launches one attempt of a container in the pod's sandbox and waits
// for its process. It is the only launch path for both init and application
// containers; status, probes and process tracking all go through the attempt.
func (h *PodHandler) runAttempt(c *Container, spec *corev1.Container, init bool, sandboxPID int) (int, attemptResult, error) {
	a, err := h.lifecycle.BeginAttempt(spec.Name, init)
	if err != nil {
		return 128, attemptNotStarted, err
	}

	logs, err := a.OpenLog(h.podDirectory.Container(spec.Name))
	if err != nil {
		a.FailPod(128, "LogFileError", "failed to open the container log: %v", err)
		return 128, attemptFilesError, err
	}

	if err := prepareAttemptScratch(h.podDirectory.Container(spec.Name), a.ID, c.WorkingDir); err != nil {
		logs.Abort()
		a.FailPod(128, "TmpDirError", "failed to create the container's /tmp: %v", err)
		return 128, attemptFilesError, err
	}
	if err := prepareTerminationLog(c, h.podDirectory.Container(spec.Name).AttemptTerminationLog(a.ID)); err != nil {
		logs.Abort()
		a.FailPod(128, "TerminationLogError", "failed to create the container's termination message file: %v", err)
		return 128, attemptFilesError, err
	}

	bin, args := c.LaunchCommand(compute.Environment.ApptainerBin, sandboxPID, h.podDirectory, a.ID)
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = "/" // the container starts in /, as in Kubernetes without a working directory
	cmd.Env = h.launchEnv(h.podDirectory.Container(spec.Name).AttemptUnpackDir(a.ID))
	cmd.Stdout = logs.Stdout()
	cmd.Stderr = logs.Stderr()

	var gates probeGates
	a.spec = spec // (an init container has no hooks or probes: admission refuses them)
	a.stopSignal = c.StopSignal
	if !init {
		gates = gatesOf(spec)
	}
	if err := a.Start(cmd, gates); err != nil {
		logs.Abort()
		a.Fail(128)
		return 128, attemptNotStarted, err
	}
	logs.Begin()
	h.savePIDs(0, a.pid)
	if !init {
		podIP := h.lifecycle.GetPodStatus().PodIP
		if gates.postStart {
			go a.runPostStart(podIP, a.execProbe)
		} else {
			startProbes(a, podIP, spec, a.execProbe)
		}
	}
	return a.Wait(cmd), attemptExited, nil
}

func backoffFor(attempt, maxShift int, floor time.Duration) time.Duration {
	backoff := time.Duration(1<<uint(min(attempt, maxShift))) * 250 * time.Millisecond
	if backoff > 10*time.Second {
		backoff = 10 * time.Second
	}
	if backoff < floor {
		backoff = floor
	}
	return backoff
}

// prepareContainer makes a container ready to run: its environment, mounts and
// image. The pod's IP is known by now, so it can be in the environment.
//
// A container whose configuration cannot be resolved (a ConfigMap or Secret it
// refers to does not exist, a key is missing, a Downward API reference cannot be
// honoured) is not the pod's failure. As the kubelet reports it, the container is
// Waiting with the reason CreateContainerConfigError and the message, an event says
// so, and it is tried again with a growing pause, so that creating what was missing
// lets it start; its siblings are not held back. An image that cannot be pulled is
// tried again the same way, with the kubelet's back-off: the container is Waiting with
// ErrImagePull, then ImagePullBackOff while it waits. Anything else (its mounts) fails
// the pod. ok is false when the container will not be started.
func (h *PodHandler) prepareContainer(ctx context.Context, spec *corev1.Container, init bool, index int) (c Container, ok bool) {
	pl := h.lifecycle
	failReason, field := "MainContainerError", "Containers"
	if init {
		failReason, field = "InitContainerError", "InitContainers"
	}
	lastMessage := ""
	pulls := pullBackOff()
	for attempt := 0; ; attempt++ {
		c, err := h.buildContainer(ctx, spec)
		if err == nil {
			pl.PrepareContainer(spec.Name, init, spec.Image, c.ImageFilePath, c.InstanceName)
			return c, true
		}
		if ctx.Err() != nil {
			return Container{}, false
		}
		var pullErr *ImagePullError
		if errors.As(err, &pullErr) {
			h.logger.Info("image cannot be pulled; backing off", "container", spec.Name, "image", spec.Image, "reason", pullErr.Err.Error())
			pl.SetContainerWaiting(spec.Name, init, pullErr.Reason(), err.Error())
			pl.ContainerEvent(spec.Name, corev1.EventTypeWarning, "Failed", "Failed to pull image %q: %v", spec.Image, pullErr.Err)
			// The failure itself is shown for a fifth of the pause (the kubelet shows it until
			// its next look at the pod), then the back-off for the rest.
			pause := pulls.delay(0)
			if !sleepCtx(ctx, pause/5) {
				return Container{}, false
			}
			pl.SetContainerWaiting(spec.Name, init, "ImagePullBackOff", fmt.Sprintf("Back-off pulling image %q", spec.Image))
			pl.ContainerEvent(spec.Name, corev1.EventTypeNormal, "BackOff", "Back-off pulling image %q", spec.Image)
			if !sleepCtx(ctx, pause-pause/5) {
				return Container{}, false
			}
			continue
		}
		if IsConfigError(err) {
			if message := err.Error(); message != lastMessage {
				lastMessage = message
				h.logger.Info("container configuration cannot be resolved; waiting", "container", spec.Name, "reason", message)
				pl.SetContainerWaiting(spec.Name, init, configReason(err), message)
				pl.ContainerEvent(spec.Name, corev1.EventTypeWarning, "Failed", "Error: %s", message)
			}
			select {
			case <-ctx.Done():
				return Container{}, false
			case <-time.After(backoffFor(attempt, 5, time.Second)):
			}
			continue
		}
		pl.SetContainerWaiting(spec.Name, init, failReason, err.Error())
		pl.Fail(failReason, "failed to materialize pod.Spec.%s[%d]: %v", field, index, err)
		return Container{}, false
	}
}

// mountVolumes prepares the pod's volumes, one after the other. A volume that cannot be
// mounted yet (its claim is not bound, its volume is not there: VolumePendingError) is
// waited for, as the kubelet does: the pod stays Pending with its containers in
// ContainerCreating, a FailedMount event says what it waits for (once per distinct
// reason), and it is looked at again with a growing pause, until the volume is there or
// the pod is deleted or replaced, which ends the wait at once. Any other error fails the
// pod. false means the pod is not to be launched.
func (h *PodHandler) mountVolumes(ctx context.Context) bool {
	pl := h.lifecycle
	for _, vol := range h.Pod.Spec.Volumes {
		last := ""
		for attempt := 0; ; attempt++ {
			err := h.mountVolumeSource(ctx, vol)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return false
			}
			var pending *VolumePendingError
			if !errors.As(err, &pending) {
				pl.Fail("VolumeError", "%v", err)
				return false
			}
			message := fmt.Sprintf("MountVolume: volume %q: %s", vol.Name, pending.Message)
			if message != last {
				last = message
				h.logger.Info("volume is not ready; waiting", "volume", vol.Name, "reason", pending.Message)
				h.waitingForVolume(message)
				pl.PodEvent(corev1.EventTypeWarning, "FailedMount", "%s", message)
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(backoffFor(attempt, 5, time.Second)):
			}
		}
	}
	return true
}

// waitingForVolume says in the status of every container why none has started.
func (h *PodHandler) waitingForVolume(message string) {
	for _, spec := range h.Pod.Spec.InitContainers {
		h.lifecycle.SetContainerWaiting(spec.Name, true, "ContainerCreating", message)
	}
	for _, spec := range h.Pod.Spec.Containers {
		h.lifecycle.SetContainerWaiting(spec.Name, false, "ContainerCreating", message)
	}
}

// PreparePod launches the pod for its lifecycle owner, which the caller has
// already registered. It never registers a generation, and closes startup
// tracking on every return, including a panic.
func PreparePod(pl *PodLifecycle) {
	ctx := pl.Context()
	launchedApps := false
	defer func() {
		pl.EndLaunch()
		if !launchedApps {
			pl.ReleaseLiveResources(0)
		}
	}()

	pod := pl.GetPod() // the spec to run; read-only from here on
	podKey := pl.Key()
	logger := compute.DefaultLogger.WithValues("pod", podKey)

	// Pre-flight capability validation (B8)
	if err := ValidatePodCapabilities(pod); err != nil {
		pl.Fail(compute.ReasonUnsupportedFeatures, "capability validation failed: %v", err)
		return
	}

	// The pod's RuntimeClass: one Skifflet does not run refuses the pod; a RuntimeClass that
	// cannot be read now (the API server is away) is asked about again.
	for attempt := 0; ; attempt++ {
		err := CheckRuntimeClass(ctx, pod)
		if err == nil {
			break
		}
		var refused *RuntimeClassRefusal
		if errors.As(err, &refused) {
			pl.Fail(compute.ReasonUnsupportedFeatures, "capability validation failed: %v", err)
			return
		}
		logger.Info("the pod's RuntimeClass cannot be read yet; asking again", "reason", err.Error())
		if !sleepCtx(ctx, backoffFor(attempt, 5, time.Second)) {
			return
		}
	}

	// The GPUs the pod asks for, taken from the node's before anything of the pod exists and
	// given back with the rest of its live resources.
	gpus, err := reserveGPUs(pl, pod)
	if err != nil {
		pl.Fail("UnexpectedAdmissionError", "%v", err)
		return
	}

	if unapplied := UnappliedSecurityContext(pod); unapplied != "" {
		pl.PodEvent(corev1.EventTypeWarning, "SecurityContextNotApplied",
			"securityContext fields accepted and not applied (containers run without privileges, with a writable root filesystem of their own, and their volumes are writable by any user; only runAsUser, runAsGroup and runAsNonRoot are applied): %s", unapplied)
	}

	if ctx.Err() != nil {
		return
	}

	podEnvVars, err := FromServicesForPod(ctx, pod)
	if err != nil {
		pl.Fail("EnvVarError", "failed to list services when setting up env vars: %v", err)
		return
	}

	if ctx.Err() != nil {
		return
	}

	podDir := pl.PodDir()

	// Clean up any surviving processes from previous runs of this pod UID to prevent duplicate processes
	ReapRecordedProcesses(podDir)

	h := &PodHandler{
		Pod:             pod,
		lifecycle:       pl,
		podKey:          podKey,
		podDirectory:    podDir,
		logger:          logger,
		podEnvVariables: podEnvVars,
		gpus:            gpus,
	}

	// Create directories
	for _, dir := range []struct{ path, reason, what string }{
		{h.podDirectory.JobDir(), "PodDirectoryError", "pod directory"},
		{h.podDirectory.LogDir(), "PodDirectoryError", "log directory"},
		{h.podDirectory.VolumeDir(), "PodDirectoryError", "volume directory"},
		{h.podDirectory.TmpDir(), "PodDirectoryError", "temporary directory"},
	} {
		if err := os.MkdirAll(dir.path, endpoint.PodGlobalDirectoryPermissions); err != nil {
			pl.Fail(dir.reason, "Cant create %s '%s': %v", dir.what, dir.path, err)
			return
		}
	}

	// The pod's /dev/shm, shared by its containers as in Kubernetes
	if err := h.mountShm(); err != nil {
		pl.Fail("ShmError", "failed to create the pod's /dev/shm: %v", err)
		return
	}

	// Initialize statuses early so the pod is properly recognized as Pending with Waiting containers
	pl.InitializeStatuses()

	logger.Info(" * Pod Environment has been created")

	// Mount Volumes
	if !h.mountVolumes(ctx) {
		return
	}
	h.logger.Info(" * All volumes have been mounted")
	go h.syncVolumes(ctx)

	// The node's pod network (Plaid's TAP datapath and the node's subnet), then the pause image
	expectedSubnet, err := waitForPodNetwork(ctx)
	if err != nil {
		pl.Fail("NetworkNotReady", "%v", err)
		return
	}
	h.expectedSubnet = expectedSubnet
	if err := PrepareDNS(pod, h.podDirectory, compute.Environment.KubeDNS, compute.Environment.ClusterDomainOrDefault(), ""); err != nil {
		pl.Fail("DNSError", "failed to prepare pause DNS: %v", err)
		return
	}
	pauseImage, err := image.PullWithPolicy(ctx, compute.Skiff.ImageDir(), image.Docker, compute.Environment.PauseImage, corev1.PullIfNotPresent)
	if err != nil {
		pl.Fail("ImagePullError", "ImagePull error. Image:%s: %v", compute.Environment.PauseImage, err)
		return
	}

	readinessFile := h.podDirectory.ReadinessFile()
	_ = os.Remove(readinessFile)

	// The pause container, through plaidtainer (without --host-networking: it gives the pod its
	// network namespace and address on the overlay, and says so in the readiness handshake).
	pauseArgs := []string{
		"exec",
		"--readiness-file", readinessFile,
		"--dns", compute.Environment.KubeDNS,
		"--bind", h.podDirectory.ResolvConf() + ":/etc/resolv.conf",
		"--cleanenv",
		"--writable-tmpfs",
		"--no-mount", NoMounts,
		pauseImage.Filepath,
		"/pause",
	}

	pauseCmd := exec.Command(compute.Environment.ApptainerBin, pauseArgs...)
	pauseCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pauseCmd.Dir = "/" // the launcher's own directory is not the container's (see NoMounts)
	pauseCmd.Env = append(h.launchEnv(h.podDirectory.TmpDir()), "PLAIDTAINER_READINESS_FILE="+readinessFile)

	if err := pauseCmd.Start(); err != nil {
		pl.Fail("PauseStartError", "failed to start pause container: %v", err)
		return
	}

	pausePID := pauseCmd.Process.Pid
	h.savePIDs(pausePID, 0)
	pauseStartTime, _ := runtime.GetProcessStartTime(pausePID)

	pauseDone := make(chan struct{})
	var pauseWaitOnce sync.Once

	go func() {
		_ = pauseCmd.Wait()
		close(pauseDone)
	}()

	killPauseAndWait := func() {
		pauseWaitOnce.Do(func() {
			if pauseCmd.Process != nil {
				_ = pauseCmd.Process.Signal(syscall.SIGTERM)
			}
			select {
			case <-pauseDone:
			case <-time.After(5 * time.Second):
				if pauseCmd.Process != nil {
					_ = pauseCmd.Process.Kill()
				}
				select {
				case <-pauseDone:
				case <-time.After(2 * time.Second):
				}
			}
		})
	}

	var containerPID int
	var podIP string
	var netnsPath string
	var netnsFile *os.File
	defer func() {
		if netnsFile != nil {
			_ = netnsFile.Close()
		}
	}()

	// The handshake is the only way the sandbox is known: its process, its namespace (opened
	// and checked against the inode it reports) and its address, which the namespace must have.
	info, err := WaitForReadiness(ctx, readinessFile, 15*time.Second)
	if err != nil || info == nil || info.PausePID <= 1 || info.NetnsPath == "" || info.NetnsInode == 0 || info.EndpointID == "" {
		killPauseAndWait()
		pl.Fail("SandboxReadinessError", "plaidtainer readiness handshake failed: %v", err)
		return
	}
	containerPID = info.PausePID
	podIP = strings.Split(info.IP, "/")[0]
	netnsPath = info.NetnsPath
	if netnsFile, err = VerifyNetnsDescriptor(netnsPath); err != nil {
		killPauseAndWait()
		pl.Fail("NetnsError", "failed to verify netns descriptor: %v", err)
		return
	}
	stat, statErr := netnsFile.Stat()
	if statErr != nil {
		killPauseAndWait()
		pl.Fail("NetnsError", "failed to stat sandbox namespace: %v", statErr)
		return
	}
	if sys, ok := stat.Sys().(*syscall.Stat_t); !ok || sys.Ino != info.NetnsInode {
		killPauseAndWait()
		pl.Fail("NetnsError", "sandbox namespace changed during handshake")
		return
	}
	if err := VerifyPodIP(podIP, h.expectedSubnet); err != nil {
		killPauseAndWait()
		pl.Fail("NetnsIPError", "invalid pod IP %q: %v", podIP, err)
		return
	}
	if observedIP, observeErr := observePodIP(containerPID, h.expectedSubnet); observeErr != nil || observedIP != podIP {
		killPauseAndWait()
		pl.Fail("NetnsIPError", "sandbox eth0 address does not match handshake: assigned=%s observed=%s error=%v", podIP, observedIP, observeErr)
		return
	}

	// A container that is not root may bind any port, as the support table says; a sandbox
	// where that cannot be set is not one the pod can run in.
	if err := allowUnprivilegedPorts(containerPID); err != nil {
		killPauseAndWait()
		pl.Fail("SandboxError", "could not let the pod's containers bind ports below 1024: %v", err)
		return
	}

	endpointID := info.EndpointID

	pl.SetSandbox(&SandboxRuntime{
		PausePID:     pausePID,
		ContainerPID: containerPID,
		StartTime:    pauseStartTime,
		PauseCmd:     pauseCmd,
		IP:           podIP,
		NetnsPath:    netnsPath,
		NetnsFile:    netnsFile,
		PauseDone:    pauseDone,
		StopPause:    killPauseAndWait,
		EndpointID:   endpointID,
	})
	netnsFile = nil

	// Prepare DNS files in job directory
	if err := PrepareDNS(pod, h.podDirectory, compute.Environment.KubeDNS, compute.Environment.ClusterDomainOrDefault(), podIP); err != nil {
		killPauseAndWait()
		pl.Fail("DNSError", "failed to prepare DNS: %v", err)
		return
	}

	// Publish the discovered IP and the containers' initial states
	pl.Update(func(*corev1.PodStatus) {})

	// Each container is prepared (its environment resolved, its mounts and image
	// made ready) just before it first runs, as the kubelet does: an init container
	// before it, the application containers after the init containers have
	// finished. A container whose configuration cannot be resolved waits, and only
	// it does (see prepareContainer).

	// The init containers one after the other, each until it succeeds (or the pod's policy is
	// Never and it failed); then the application containers together, each as the restart
	// policy says (runContainer).
	for i := range pod.Spec.InitContainers {
		exitCode, done := h.runContainer(ctx, &pod.Spec.InitContainers[i], true, i, containerPID)
		if !done || exitCode != 0 {
			if done {
				logger.Error(fmt.Errorf("init container %s exited with %d", pod.Spec.InitContainers[i].Name, exitCode), "init container failed")
			}
			killPauseAndWait() // the pod is failed with the reason, or is going away
			return
		}
	}

	var appWg sync.WaitGroup
	for i := range pod.Spec.Containers {
		spec := &pod.Spec.Containers[i]
		index := i
		appWg.Add(1)
		go func() {
			defer appWg.Done()
			if _, done := h.runContainer(ctx, spec, false, index, containerPID); !done && ctx.Err() == nil {
				// The pod failed as a whole (an image that cannot be had, a container's log or
				// scratch that cannot be made): what still runs in it stops.
				pl.ReleaseLiveResources(0)
			}
		}()
	}

	launchedApps = true
	// A normal terminal pod keeps status and logs while releasing its live resources.
	go func() {
		appWg.Wait()
		if ctx.Err() == nil {
			pl.Update(func(*corev1.PodStatus) {})
			pl.ReleaseLiveResources(0)
		}
	}()
}
