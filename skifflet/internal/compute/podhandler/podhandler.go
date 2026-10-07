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

	goruntime "runtime"
	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/image"
	"skifflet/internal/compute/runtime"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FindContainerPID locates the live process beneath the Apptainer launcher of the
// pause container, by heuristics. It is the fallback for a sandbox launched without
// the Plaid handshake, which reports the process itself. Application containers do
// not use it: they are resolved through Attempt.Target, which verifies what it finds.
func FindContainerPID(parentPID int) int {
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := os.ReadDir("/proc")
		if err == nil {
			parentOf := make(map[int]int)
			pgrpOf := make(map[int]int)
			var pids []int
			for _, entry := range entries {
				pid, err := strconv.Atoi(entry.Name())
				if err != nil || pid <= 0 {
					continue
				}
				pids = append(pids, pid)
				if statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
					idx := strings.LastIndex(string(statData), ")")
					if idx != -1 && idx+2 < len(statData) {
						statFields := strings.Fields(string(statData[idx+2:]))
						if len(statFields) >= 2 {
							if ppid, _ := strconv.Atoi(statFields[1]); ppid > 0 {
								parentOf[pid] = ppid
							}
						}
						if len(statFields) >= 3 {
							if pgrp, _ := strconv.Atoi(statFields[2]); pgrp > 0 {
								pgrpOf[pid] = pgrp
							}
						}
					}
				}
			}

			isDescendant := func(pid int) bool {
				if pid == parentPID {
					return false
				}
				if pgrp, ok := pgrpOf[pid]; ok && pgrp == parentPID {
					return true
				}
				curr := pid
				for {
					ppid, ok := parentOf[curr]
					if !ok || ppid <= 1 {
						return false
					}
					if ppid == parentPID {
						return true
					}
					curr = ppid
				}
			}

			// Priority 1: A descendant whose comm is pause
			for _, pid := range pids {
				if isDescendant(pid) {
					comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
					cStr := strings.TrimSpace(string(comm))
					if cStr == "pause" {
						return pid
					}
				}
			}

			// Priority 2: Any descendant whose network namespace differs from host /proc/1/ns/net
			hostNetns, _ := os.Readlink("/proc/1/ns/net")
			if hostNetns != "" {
				for _, pid := range pids {
					if isDescendant(pid) {
						ns, _ := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid))
						if ns != "" && ns != hostNetns {
							comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
							cStr := strings.TrimSpace(string(comm))
							if !strings.Contains(cStr, "fuse") && !strings.Contains(cStr, "starter") && !strings.Contains(cStr, "plaidtainer") {
								return pid
							}
						}
					}
				}
			}

			// Priority 3: Any non-fuse descendant whose comm is not starter/apptainer/plaidtainer
			for _, pid := range pids {
				if isDescendant(pid) {
					comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
					cStr := strings.TrimSpace(string(comm))
					if !strings.Contains(cStr, "fuse") && !strings.Contains(cStr, "starter") && !strings.Contains(cStr, "apptainer") && !strings.Contains(cStr, "plaidtainer") {
						return pid
					}
				}
			}
		}

		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if goruntime.GOOS != "linux" {
		return parentPID
	}
	return 0
}

// GetPodIPFromNetns queries the network namespace of pausePID to find its assigned non-loopback IPv4 address on eth0.
func GetPodIPFromNetns(pausePID int) (string, error) {
	if testIP := os.Getenv("SKIFF_TEST_POD_IP"); testIP != "" {
		return testIP, nil
	}

	netnsPath := fmt.Sprintf("/proc/%d/ns/net", pausePID)
	if _, err := os.Stat(netnsPath); os.IsNotExist(err) {
		if goruntime.GOOS != "linux" {
			return "10.244.0.100", nil
		}
		return "", fmt.Errorf("netns path %s does not exist", netnsPath)
	}

	var expectedSubnet *net.IPNet
	if podCIDR := os.Getenv("POD_CIDR"); podCIDR != "" {
		_, expectedSubnet, _ = net.ParseCIDR(podCIDR)
	}

	hostNetns, _ := os.Readlink("/proc/1/ns/net")

	deadline := time.Now().Add(15 * time.Second)
	for {
		// Ensure the process has actually unshared its network namespace
		currNetns, _ := os.Readlink(netnsPath)
		if currNetns != "" && hostNetns != "" && currNetns == hostNetns {
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}

		cmd := exec.Command("nsenter", "-t", strconv.Itoa(pausePID), "-n", "ip", "-o", "-4", "addr", "show", "dev", "eth0")
		output, err := cmd.Output()
		if err == nil {
			lines := strings.Split(string(output), "\n")
			for _, line := range lines {
				fields := strings.Fields(line)
				for i, field := range fields {
					if field == "inet" && i+1 < len(fields) {
						ipCIDR := fields[i+1]
						ipStr := strings.Split(ipCIDR, "/")[0]
						if err := VerifyPodIP(ipStr, expectedSubnet); err == nil {
							if expectedSubnet != nil || !strings.HasPrefix(ipStr, "10.0.") {
								return ipStr, nil
							}
						}
					}
				}
			}
		}

		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	return "", fmt.Errorf("timeout waiting for valid pod IP on eth0 in netns of PID %d", pausePID)
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
func prepareAttemptScratch(container endpoint.ContainerPath, attempt int) error {
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

	if err := prepareAttemptScratch(h.podDirectory.Container(spec.Name), a.ID); err != nil {
		logs.Abort()
		a.FailPod(128, "TmpDirError", "failed to create the container's /tmp: %v", err)
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
	if !init {
		gates = gatesOf(spec)
		a.spec = spec
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
// lets it start; its siblings are not held back. Anything else (its image, its
// mounts) fails the pod. ok is false when the container will not be started.
func (h *PodHandler) prepareContainer(ctx context.Context, spec *corev1.Container, init bool, index int) (c Container, ok bool) {
	pl := h.lifecycle
	failReason, field := "MainContainerError", "Containers"
	if init {
		failReason, field = "InitContainerError", "InitContainers"
	}
	lastMessage := ""
	for attempt := 0; ; attempt++ {
		c, err := h.buildContainer(ctx, spec)
		if err == nil {
			pl.PrepareContainer(spec.Name, init, spec.Image, c.ImageFilePath, c.InstanceName)
			return c, true
		}
		if ctx.Err() != nil {
			return Container{}, false
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
		reason := "ErrImagePull"
		if !strings.Contains(err.Error(), "ImagePull") && !strings.Contains(err.Error(), "image") {
			reason = failReason
		}
		pl.SetContainerWaiting(spec.Name, init, reason, err.Error())
		pl.Fail(reason, "failed to materialize pod.Spec.%s[%d]: %v", field, index, err)
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

	// Initialize statuses early so the pod is properly recognized as Pending with Waiting containers
	pl.InitializeStatuses()

	logger.Info(" * Pod Environment has been created")

	// Mount Volumes
	if !h.mountVolumes(ctx) {
		return
	}
	h.logger.Info(" * All volumes have been mounted")

	// Pull Pause Image
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

	// Launch Pause Container via compute.Environment.ApptainerBin (without --host-networking)
	pauseArgs := []string{
		"exec",
	}
	usesPlaidtainer := strings.Contains(compute.Environment.ApptainerBin, "plaidtainer") || filepath.Base(compute.Environment.ApptainerBin) == "plaidtainer"
	if usesPlaidtainer {
		pauseArgs = append(pauseArgs, "--readiness-file", readinessFile)
	}
	pauseArgs = append(pauseArgs,
		"--dns", compute.Environment.KubeDNS,
		"--bind", h.podDirectory.ResolvConf()+":/etc/resolv.conf",
		"--nv",
		"--cleanenv",
		"--writable-tmpfs",
		"--no-mount", NoMounts,
		pauseImage.Filepath,
		"/pause",
	)

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

	info, err := WaitForReadiness(ctx, readinessFile, 15*time.Second)
	if usesPlaidtainer && (err != nil || info == nil || info.PausePID <= 1 || info.NetnsPath == "" || info.NetnsInode == 0 || info.EndpointID == "") {
		killPauseAndWait()
		pl.Fail("SandboxReadinessError", "plaidtainer readiness handshake failed: %v", err)
		return
	}
	if err == nil && info != nil && info.PausePID > 1 {
		containerPID = info.PausePID
		podIP = strings.Split(info.IP, "/")[0]
		netnsPath = info.NetnsPath
		if netnsFile, err = VerifyNetnsDescriptor(netnsPath); err != nil {
			killPauseAndWait()
			pl.Fail("NetnsError", "failed to verify netns descriptor: %v", err)
			return
		}
		if usesPlaidtainer {
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
		}
	} else {
		containerPID = FindContainerPID(pausePID)
		if containerPID <= 0 {
			killPauseAndWait()
			pl.Fail("ContainerPIDError", "failed to discover container PID under pause PID %d", pausePID)
			return
		}
		var ipErr error
		podIP, ipErr = GetPodIPFromNetns(containerPID)
		if ipErr != nil {
			killPauseAndWait()
			pl.Fail("NetnsIPError", "failed to discover pod IP from netns: %v", ipErr)
			return
		}
		netnsPath = fmt.Sprintf("/proc/%d/ns/net", containerPID)
	}

	if err := VerifyPodIP(podIP, h.expectedSubnet); err != nil {
		killPauseAndWait()
		pl.Fail("NetnsIPError", "invalid pod IP %q: %v", podIP, err)
		return
	}
	if usesPlaidtainer {
		observedIP, observeErr := GetPodIPFromNetns(containerPID)
		if observeErr != nil || observedIP != podIP {
			killPauseAndWait()
			pl.Fail("NetnsIPError", "sandbox eth0 address does not match handshake: assigned=%s observed=%s error=%v", podIP, observedIP, observeErr)
			return
		}
	}

	if err := allowUnprivilegedPorts(containerPID); err != nil {
		h.logger.Info("could not let non-root containers bind low ports", "pod", h.podKey, "error", err.Error())
	}

	var endpointID string
	if info != nil && info.EndpointID != "" {
		endpointID = info.EndpointID
	}

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

	// Execute Init Containers sequentially with retry policy support
	for i := range pod.Spec.InitContainers {
		spec := &pod.Spec.InitContainers[i]
		c, ok := h.prepareContainer(ctx, spec, true, i)
		if !ok {
			killPauseAndWait() // the pod is failed with the reason, or is going away
			return
		}
		initAttempt := 0
		for {
			if ctx.Err() != nil {
				killPauseAndWait()
				return
			}
			exitCode, result, _ := h.runAttempt(&c, spec, true, containerPID)
			if result == attemptFilesError {
				killPauseAndWait() // the pod is already failed with the reason
				return
			}
			if result == attemptExited && exitCode == 0 {
				break // Init container succeeded, proceed to next
			}
			if pl.RestartPolicy() == corev1.RestartPolicyNever {
				killPauseAndWait()
				if result == attemptExited {
					logger.Error(fmt.Errorf("init container %s exited with %d", c.InstanceName, exitCode), "init container failed")
				}
				return
			}
			initAttempt++
			select {
			case <-ctx.Done():
				killPauseAndWait()
				return
			case <-time.After(backoffFor(initAttempt, 4, 0)):
			}
		}
	}

	// Execute Main Containers concurrently with restart policy support
	var appWg sync.WaitGroup
	for i := range pod.Spec.Containers {
		spec := &pod.Spec.Containers[i]
		index := i
		appWg.Add(1)
		go func() {
			defer appWg.Done()
			c, ok := h.prepareContainer(ctx, spec, false, index)
			if !ok {
				if ctx.Err() == nil {
					// A container that cannot be made ready for a reason other than its
					// configuration (its image) fails the pod, with everything in it.
					pl.ReleaseLiveResources(0)
				}
				return
			}
			restartAttempt := 0
			for {
				if ctx.Err() != nil {
					return
				}
				exitCode, result, _ := h.runAttempt(&c, spec, false, containerPID)
				if result == attemptFilesError {
					return // the pod is already failed with the reason
				}
				if ctx.Err() != nil {
					return
				}
				switch pl.RestartPolicy() {
				case corev1.RestartPolicyAlways:
				case corev1.RestartPolicyOnFailure:
					if exitCode == 0 {
						return
					}
				default:
					return
				}
				restartAttempt++
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoffFor(restartAttempt, 5, 500*time.Millisecond)):
				}
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
