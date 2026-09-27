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

// FindContainerPID locates the live payload beneath an Apptainer launcher.
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
		time.Sleep(100 * time.Millisecond)
	}

	if errors.Is(removeErr, fs.ErrPermission) {
		// Walk with Lstat awareness: never chmod symlinks or external hostpath targets
		_ = filepath.Walk(podDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil {
				return nil
			}
			if info.Mode()&os.ModeSymlink != 0 {
				_ = os.Remove(path)
				return nil
			}
			_ = os.Chmod(path, 0700)
			return nil
		})
		return os.RemoveAll(podDir)
	}
	return removeErr
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

	// 1. Resolve pause PID
	var pausePIDStr string
	if localPod != nil && localPod.Annotations != nil {
		pausePIDStr = localPod.Annotations["skiff.io/pause-pid"]
	}
	if pausePIDStr == "" && localPod != nil {
		pausePIDStr = runtime.GetPodID(localPod)
	}

	// 2. Resolve secondary container PIDs from ContainerStatuses
	var secondaryPIDs []string
	seen := make(map[string]bool)
	if pausePIDStr != "" {
		seen[pausePIDStr] = true
	}
	collectPIDs := func(statuses []corev1.ContainerStatus) {
		for _, s := range statuses {
			cid := strings.TrimPrefix(s.ContainerID, "process://")
			cid = strings.TrimPrefix(cid, "pid://")
			if cid != "" && !seen[cid] {
				seen[cid] = true
				secondaryPIDs = append(secondaryPIDs, cid)
			}
		}
	}
	if localPod != nil {
		collectPIDs(localPod.Status.InitContainerStatuses)
		collectPIDs(localPod.Status.ContainerStatuses)
		if localPod.Annotations != nil {
			cpid := localPod.Annotations["skiff.io/container-pid"]
			if cpid != "" && !seen[cpid] {
				seen[cpid] = true
				secondaryPIDs = append(secondaryPIDs, cpid)
			}
		}
	}

	pidsPath := filepath.Join(podDir.String(), "job", "pids.json")
	if data, err := os.ReadFile(pidsPath); err == nil {
		var pids struct {
			PausePID      int   `json:"pause_pid"`
			ContainerPIDs []int `json:"container_pids"`
		}
		if err := json.Unmarshal(data, &pids); err == nil {
			if pids.PausePID > 0 {
				pidStr := strconv.Itoa(pids.PausePID)
				if pausePIDStr == "" {
					pausePIDStr = pidStr
					seen[pidStr] = true
				} else if !seen[pidStr] {
					seen[pidStr] = true
					secondaryPIDs = append(secondaryPIDs, pidStr)
				}
			}
			for _, pid := range pids.ContainerPIDs {
				pidStr := strconv.Itoa(pid)
				if !seen[pidStr] {
					seen[pidStr] = true
					secondaryPIDs = append(secondaryPIDs, pidStr)
				}
			}
		}
	}

	// 3. Terminate processes
	gracePeriod := 30 * time.Second
	if localPod != nil && localPod.Spec.TerminationGracePeriodSeconds != nil && *localPod.Spec.TerminationGracePeriodSeconds >= 0 {
		gracePeriod = time.Duration(*localPod.Spec.TerminationGracePeriodSeconds) * time.Second
	}

	var pl *PodLifecycle
	if localPod != nil && localPod.GetUID() != "" {
		pl = GlobalRegistry.Get(localPod.GetUID())
	}
	if pl == nil {
		pl = GlobalRegistry.GetByKey(podKey)
	}
	if pl != nil {
		pl.Terminate(gracePeriod)
		GlobalRegistry.Delete(pl.UID())
	} else if pausePIDStr != "" || len(secondaryPIDs) > 0 {
		out, err := runtime.KillPodProcessesWithTimeout(pausePIDStr, secondaryPIDs, gracePeriod)
		if err != nil && !errors.Is(err, runtime.ErrInvalidJob) {
			logger.Info("WARNING: Failed to kill process by PID, proceeding to remove pod directory", "pid", pausePIDStr, "pod", podKey, "err", err, "out", out)
		} else {
			logger.Info(" * Process is terminated", "pid", pausePIDStr, "pod", podKey, "out", out)
		}
		// Fallback force-kill any stubborn surviving processes
		for _, pidStr := range append(secondaryPIDs, pausePIDStr) {
			if pid, _, err := runtime.ParseProcessJobID(pidStr); err == nil && pid > 1 && !runtime.IsProcessDead(pid) {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}

	// 4. Remove Pod Directory (with retry for NFS file handle release)
	if err := SafeRemovePodDirectory(podDir.String()); err != nil {
		logger.Error(err, "failed to remove pod directory", "directory", podDir)
		return false
	}

	logger.Info(" * Pod directory is removed")

	// 5. Garbage collect empty namespace directory
	namespaceDir := filepath.Dir(podDir.String())
	if empty, _ := endpoint.IsEmpty(namespaceDir); empty {
		if err := os.Remove(namespaceDir); err == nil {
			logger.Info(" * Namespace directory is removed")
		}
	}

	return true
}

type PodHandler struct {
	mu sync.Mutex

	*corev1.Pod
	lifecycle *PodLifecycle

	podKey client.ObjectKey

	podEnvVariables []corev1.EnvVar
	podDirectory    endpoint.PodPath

	logger logr.Logger
}

type podPIDsRecord struct {
	PausePID            int            `json:"pause_pid"`
	PauseStartTime      uint64         `json:"pause_start_time,omitempty"`
	ContainerPIDs       []int          `json:"container_pids"`
	ContainerStartTimes map[int]uint64 `json:"container_start_times,omitempty"`
}

func (h *PodHandler) savePIDs(pausePID int, containerPID int) {
	pidsPath := filepath.Join(h.podDirectory.JobDir(), "pids.json")
	var pids podPIDsRecord
	if data, err := os.ReadFile(pidsPath); err == nil {
		_ = json.Unmarshal(data, &pids)
	}
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
		_ = os.WriteFile(pidsPath, data, 0644)
	}
}

func (h *PodHandler) updatePodState(fn func(p *corev1.Pod), notify func(*corev1.Pod)) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if fn != nil {
		fn(h.Pod)
	}
	UpdateStatusFromRuntime(h.Pod)
	snapshot := h.Pod.DeepCopy()
	if h.lifecycle != nil {
		h.lifecycle.Publish(snapshot)
	}
	if notify != nil {
		notify(snapshot)
	}
}

// CreatePod sets up the pod environment, launches the pause container, discovers the pod IP,
// configures DNS, and executes init and application containers in-process.
func CreatePod(ctx context.Context, pod *corev1.Pod, notify func(*corev1.Pod)) {
	podKey := client.ObjectKeyFromObject(pod)
	logger := compute.DefaultLogger.WithValues("pod", podKey)

	// Pre-flight capability validation (B8)
	if err := ValidatePodCapabilities(pod); err != nil {
		compute.PodError(pod, compute.ReasonUnsupportedFeatures, "capability validation failed: %v", err)
		if notify != nil {
			notify(pod.DeepCopy())
		}
		return
	}

	if ctx.Err() != nil {
		return
	}

	podEnvVars, err := FromServicesForPod(ctx, pod)
	if err != nil {
		compute.PodError(pod, "EnvVarError", "failed to list services when setting up env vars: %v", err)
		if notify != nil {
			notify(pod.DeepCopy())
		}
		return
	}

	podDir := compute.Skiff.PodWithUID(podKey, pod.GetUID())

	// Clean up any surviving processes from previous runs of this pod UID to prevent duplicate processes
	pidsPath := filepath.Join(podDir.JobDir(), "pids.json")
	if data, err := os.ReadFile(pidsPath); err == nil {
		var oldPids podPIDsRecord
		if err := json.Unmarshal(data, &oldPids); err == nil {
			for _, pid := range oldPids.ContainerPIDs {
				st := oldPids.ContainerStartTimes[pid]
				if pid > 1 && runtime.VerifyProcessIdentity(pid, st) && !runtime.IsProcessDead(pid) {
					_ = syscall.Kill(-pid, syscall.SIGKILL)
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
			if oldPids.PausePID > 1 && runtime.VerifyProcessIdentity(oldPids.PausePID, oldPids.PauseStartTime) && !runtime.IsProcessDead(oldPids.PausePID) {
				_ = syscall.Kill(-oldPids.PausePID, syscall.SIGKILL)
				_ = syscall.Kill(oldPids.PausePID, syscall.SIGKILL)
			}
		}
	}

	pl := NewPodLifecycle(pod, podDir, notify)
	GlobalRegistry.Register(pl)

	h := &PodHandler{
		Pod:             pod,
		lifecycle:       pl,
		podKey:          podKey,
		podDirectory:    podDir,
		logger:          logger,
		podEnvVariables: podEnvVars,
	}

	// Create directories
	if err := os.MkdirAll(h.podDirectory.JobDir(), endpoint.PodGlobalDirectoryPermissions); err != nil {
		compute.PodError(pod, "PodDirectoryError", "Cant create pod directory '%s': %v", h.podDirectory.JobDir(), err)
		if notify != nil {
			notify(pod.DeepCopy())
		}
		return
	}

	if err := os.MkdirAll(h.podDirectory.LogDir(), endpoint.PodGlobalDirectoryPermissions); err != nil {
		compute.PodError(pod, "PodDirectoryError", "cannot create log directory '%s': %v", h.podDirectory.LogDir(), err)
		if notify != nil {
			notify(pod.DeepCopy())
		}
		return
	}

	if err := os.MkdirAll(h.podDirectory.VolumeDir(), endpoint.PodGlobalDirectoryPermissions); err != nil {
		compute.PodError(pod, "PodDirectoryError", "cannot create volume directory '%s': %v", h.podDirectory.VolumeDir(), err)
		if notify != nil {
			notify(pod.DeepCopy())
		}
		return
	}

	// Initialize statuses early so the pod is properly recognized as Pending with Waiting containers
	h.updatePodState(func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		if p.Status.InitContainerStatuses == nil {
			p.Status.InitContainerStatuses = make([]corev1.ContainerStatus, len(p.Spec.InitContainers))
			for i, c := range p.Spec.InitContainers {
				p.Status.InitContainerStatuses[i] = corev1.ContainerStatus{
					Name:  c.Name,
					Image: c.Image,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "ContainerCreating",
							Message: "Init container is waiting to be created",
						},
					},
				}
			}
		}
		if p.Status.ContainerStatuses == nil {
			p.Status.ContainerStatuses = make([]corev1.ContainerStatus, len(p.Spec.Containers))
			for i, c := range p.Spec.Containers {
				p.Status.ContainerStatuses[i] = corev1.ContainerStatus{
					Name:  c.Name,
					Image: c.Image,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "ContainerCreating",
							Message: "Container is waiting to be created",
						},
					},
				}
			}
		}
	}, notify)

	logger.Info(" * Pod Environment has been created")

	// Mount Volumes
	for _, vol := range h.Pod.Spec.Volumes {
		if err := h.mountVolumeSource(ctx, vol); err != nil {
			compute.PodError(pod, "VolumeError", "%v", err)
			if notify != nil {
				notify(pod)
			}
			return
		}
	}
	h.logger.Info(" * All volumes have been mounted")

	// Pull Pause Image
	if err := waitForPodNetwork(ctx); err != nil {
		compute.PodError(pod, "NetworkNotReady", "%v", err)
		if notify != nil {
			notify(pod)
		}
		return
	}
	if err := PrepareDNS(pod, h.podDirectory, compute.Environment.KubeDNS, ""); err != nil {
		compute.PodError(pod, "DNSError", "failed to prepare pause DNS: %v", err)
		if notify != nil {
			notify(pod)
		}
		return
	}
	pauseImage, err := image.Pull(compute.Skiff.ImageDir(), image.Docker, compute.Environment.PauseImage)
	if err != nil {
		compute.PodError(pod, "ImagePullError", "ImagePull error. Image:%s: %v", compute.Environment.PauseImage, err)
		if notify != nil {
			notify(pod)
		}
		return
	}

	readinessFile := filepath.Join(h.podDirectory.JobDir(), "readiness.json")
	_ = os.Remove(readinessFile)

	// Launch Pause Container via compute.Environment.ApptainerBin (without --host-networking)
	pauseArgs := []string{
		"exec",
	}
	if strings.Contains(compute.Environment.ApptainerBin, "plaidtainer") || filepath.Base(compute.Environment.ApptainerBin) == "plaidtainer" {
		pauseArgs = append(pauseArgs, "--readiness-file", readinessFile)
	}
	pauseArgs = append(pauseArgs,
		"--dns", compute.Environment.KubeDNS,
		"--bind", filepath.Join(h.podDirectory.JobDir(), "resolv.conf")+":/etc/resolv.conf",
		"--nv",
		"--cleanenv",
		"--writable-tmpfs",
		"--no-mount", "home,bind-paths",
		pauseImage.Filepath,
		"/pause",
	)

	pauseCmd := exec.Command(compute.Environment.ApptainerBin, pauseArgs...)
	pauseCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pauseCmd.Env = append(os.Environ(), "PLAIDTAINER_READINESS_FILE="+readinessFile)

	if err := pauseCmd.Start(); err != nil {
		compute.PodError(pod, "PauseStartError", "failed to start pause container: %v", err)
		if notify != nil {
			notify(pod)
		}
		return
	}

	pausePID := pauseCmd.Process.Pid
	h.savePIDs(pausePID, 0)
	pauseStartTime, _ := runtime.GetProcessStartTime(pausePID)
	pauseJobID := runtime.FormatProcessJobID(pausePID, pauseStartTime)

	pauseDone := make(chan struct{})
	var pauseWaitOnce sync.Once

	go func() {
		_ = pauseCmd.Wait()
		close(pauseDone)
	}()

	killPauseAndWait := func() {
		pauseWaitOnce.Do(func() {
			if pauseCmd.Process != nil {
				_ = pauseCmd.Process.Kill()
			}
			select {
			case <-pauseDone:
			case <-time.After(5 * time.Second):
			}
		})
	}

	var containerPID int
	var podIP string
	var netnsPath string

	info, err := WaitForReadiness(ctx, readinessFile, 15*time.Second)
	if err == nil && info != nil && info.PausePID > 1 {
		containerPID = info.PausePID
		podIP = info.IP
		netnsPath = info.NetnsPath
		if netnsFile, err := VerifyNetnsDescriptor(netnsPath); err != nil {
			killPauseAndWait()
			compute.PodError(pod, "NetnsError", "failed to verify netns descriptor: %v", err)
			if notify != nil {
				notify(pod.DeepCopy())
			}
			return
		} else {
			_ = netnsFile.Close()
		}
	} else {
		containerPID = FindContainerPID(pausePID)
		if containerPID <= 0 {
			killPauseAndWait()
			compute.PodError(pod, "ContainerPIDError", "failed to discover container PID under pause PID %d", pausePID)
			if notify != nil {
				notify(pod.DeepCopy())
			}
			return
		}
		var ipErr error
		podIP, ipErr = GetPodIPFromNetns(containerPID)
		if ipErr != nil {
			killPauseAndWait()
			compute.PodError(pod, "NetnsIPError", "failed to discover pod IP from netns: %v", ipErr)
			if notify != nil {
				notify(pod.DeepCopy())
			}
			return
		}
		netnsPath = fmt.Sprintf("/proc/%d/ns/net", containerPID)
	}

	var expectedSubnet *net.IPNet
	if podCIDR := os.Getenv("POD_CIDR"); podCIDR != "" {
		_, expectedSubnet, _ = net.ParseCIDR(podCIDR)
	}
	if err := VerifyPodIP(podIP, expectedSubnet); err != nil {
		killPauseAndWait()
		compute.PodError(pod, "NetnsIPError", "invalid pod IP %q: %v", podIP, err)
		if notify != nil {
			notify(pod.DeepCopy())
		}
		return
	}

	h.updatePodState(func(p *corev1.Pod) {
		if p.Annotations == nil {
			p.Annotations = make(map[string]string)
		}
		p.Annotations["skiff.io/pause-pid"] = pauseJobID
		containerStartTime, _ := runtime.GetProcessStartTime(containerPID)
		p.Annotations["skiff.io/container-pid"] = runtime.FormatProcessJobID(containerPID, containerStartTime)
		p.Annotations["skiff.io/pod-ip"] = podIP
		p.Status.PodIP = podIP
		p.Status.PodIPs = []corev1.PodIP{{IP: podIP}}
		runtime.SetPodID(p, runtime.JobIDTypeProcess, pauseJobID)
	}, notify)

	if h.lifecycle != nil {
		h.lifecycle.SetSandbox(&SandboxRuntime{
			PausePID:  pausePID,
			StartTime: pauseStartTime,
			PauseCmd:  pauseCmd,
			IP:        podIP,
			NetnsPath: netnsPath,
		})
	}

	// Prepare DNS files in job directory
	if err := PrepareDNS(pod, h.podDirectory, compute.Environment.KubeDNS, podIP); err != nil {
		killPauseAndWait()
		compute.PodError(pod, "DNSError", "failed to prepare DNS: %v", err)
		if notify != nil {
			notify(pod.DeepCopy())
		}
		return
	}

	// Prepare Containers (now that PodIP is known, .status.podIP env vars will have the correct IP)
	var initContainers []Container
	pod.Status.InitContainerStatuses = make([]corev1.ContainerStatus, len(pod.Spec.InitContainers))
	for i := range pod.Spec.InitContainers {
		initContainer := &pod.Spec.InitContainers[i]
		initContainerStatus := &pod.Status.InitContainerStatuses[i]
		c, err := h.buildContainer(initContainer, initContainerStatus)
		if err != nil {
			killPauseAndWait()
			reason := "ErrImagePull"
			if !strings.Contains(err.Error(), "ImagePull") && !strings.Contains(err.Error(), "image") {
				reason = "InitContainerError"
			}
			initContainerStatus.State = corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{
					Reason:  reason,
					Message: err.Error(),
				},
			}
			compute.PodError(pod, reason, "failed to materialize pod.Spec.InitContainers[%d]: %v", i, err)
			if notify != nil {
				notify(pod.DeepCopy())
			}
			return
		}
		initContainers = append(initContainers, c)
	}

	var containers []Container
	pod.Status.ContainerStatuses = make([]corev1.ContainerStatus, len(pod.Spec.Containers))
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		containerStatus := &pod.Status.ContainerStatuses[i]
		c, err := h.buildContainer(container, containerStatus)
		if err != nil {
			killPauseAndWait()
			reason := "ErrImagePull"
			if !strings.Contains(err.Error(), "ImagePull") && !strings.Contains(err.Error(), "image") {
				reason = "MainContainerError"
			}
			containerStatus.State = corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{
					Reason:  reason,
					Message: err.Error(),
				},
			}
			compute.PodError(pod, reason, "failed to materialize pod.Spec.Containers[%d]: %v", i, err)
			if notify != nil {
				notify(pod.DeepCopy())
			}
			return
		}
		containers = append(containers, c)
	}

	// Update pod metadata with discovered IP and container initial states
	h.updatePodState(nil, notify)

	// Execute Init Containers sequentially with retry policy support
	for i, c := range initContainers {
		initIdx := i
		initAttempt := 0
		for {
			if ctx.Err() != nil {
				killPauseAndWait()
				return
			}

			args := c.BuildApptainerArgs(containerPID, h.podDirectory)

			cmd := exec.Command(compute.Environment.ApptainerBin, args...)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

			logFile, err := os.Create(c.LogsPath)
			if err != nil {
				killPauseAndWait()
				compute.PodError(pod, "LogFileError", "failed to create container log file %s: %v", c.LogsPath, err)
				if notify != nil {
					notify(pod.DeepCopy())
				}
				return
			}
			cmd.Stdout = logFile
			cmd.Stderr = logFile

			if err := cmd.Start(); err != nil {
				if logFile != nil {
					logFile.Close()
				}
				h.updatePodState(func(p *corev1.Pod) {
					SetContainerTerminated(&p.Status.InitContainerStatuses[initIdx], 128)
				}, notify)

				if pod.Spec.RestartPolicy == corev1.RestartPolicyNever {
					killPauseAndWait()
					return
				}
				initAttempt++
				backoff := time.Duration(1<<uint(min(initAttempt, 4))) * 250 * time.Millisecond
				if backoff > 10*time.Second {
					backoff = 10 * time.Second
				}
				select {
				case <-ctx.Done():
					killPauseAndWait()
					return
				case <-time.After(backoff):
				}
				continue
			}

			h.savePIDs(0, cmd.Process.Pid)
			startTime, _ := runtime.GetProcessStartTime(cmd.Process.Pid)
			h.updatePodState(func(p *corev1.Pod) {
				SetContainerRunning(&p.Status.InitContainerStatuses[initIdx], cmd.Process.Pid, startTime)
			}, notify)

			waitErr := cmd.Wait()
			if logFile != nil {
				logFile.Close()
			}

			exitCode := 0
			if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
				exitCode = cmd.ProcessState.ExitCode()
			} else if waitErr != nil {
				exitCode = 1
			}

			h.updatePodState(func(p *corev1.Pod) {
				SetContainerTerminated(&p.Status.InitContainerStatuses[initIdx], exitCode)
			}, notify)

			if exitCode != 0 {
				if pod.Spec.RestartPolicy == corev1.RestartPolicyNever {
					killPauseAndWait()
					logger.Error(fmt.Errorf("init container %s exited with %d", c.InstanceName, exitCode), "init container failed")
					return
				}
				initAttempt++
				backoff := time.Duration(1<<uint(min(initAttempt, 4))) * 250 * time.Millisecond
				if backoff > 10*time.Second {
					backoff = 10 * time.Second
				}
				select {
				case <-ctx.Done():
					killPauseAndWait()
					return
				case <-time.After(backoff):
				}
				continue
			}

			// Init container succeeded, proceed to next
			break
		}
	}

	// Execute Main Containers concurrently with restart policy support
	var appWg sync.WaitGroup
	for i, c := range containers {
		containerIdx := i
		containerDef := c
		appWg.Add(1)
		go func(idx int, c Container) {
			defer appWg.Done()
			restartAttempt := 0
			for {
				if ctx.Err() != nil {
					return
				}

				args := c.BuildApptainerArgs(containerPID, h.podDirectory)

				cmd := exec.Command(compute.Environment.ApptainerBin, args...)
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

				logFile, err := os.OpenFile(c.LogsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
				if err == nil {
					cmd.Stdout = logFile
					cmd.Stderr = logFile
				}

				if err := cmd.Start(); err != nil {
					if logFile != nil {
						logFile.Close()
					}
					h.updatePodState(func(p *corev1.Pod) {
						cs := &p.Status.ContainerStatuses[idx]
						SetContainerTerminated(cs, 128)
					}, notify)
				} else {
					h.savePIDs(0, cmd.Process.Pid)
					startTime, _ := runtime.GetProcessStartTime(cmd.Process.Pid)
					var containerID string
					h.updatePodState(func(p *corev1.Pod) {
						cs := &p.Status.ContainerStatuses[idx]
						SetContainerRunning(cs, cmd.Process.Pid, startTime)
						if p.Annotations == nil {
							p.Annotations = make(map[string]string)
						}
						p.Annotations["skiff.io/container-pid"] = cs.ContainerID
						containerID = cs.ContainerID
						probe := p.Spec.Containers[idx].ReadinessProbe
						if probe != nil {
							cs.Ready = false
						}
					}, notify)

					probe := pod.Spec.Containers[idx].ReadinessProbe
					probeCtx, probeCancel := context.WithCancel(ctx)
					if probe != nil {
						h.mu.Lock()
						currentIP := h.Pod.Status.PodIP
						h.mu.Unlock()
						watchReadinessProbe(probeCtx, currentIP, containerID, probe, func(ready bool) {
							h.updatePodState(func(p *corev1.Pod) {
								p.Status.ContainerStatuses[idx].Ready = ready
							}, notify)
						})
					}

					waitErr := cmd.Wait()
					probeCancel()
					if logFile != nil {
						logFile.Close()
					}

					exitCode := 0
					if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
						exitCode = cmd.ProcessState.ExitCode()
					} else if waitErr != nil {
						exitCode = 1
					}

					h.updatePodState(func(p *corev1.Pod) {
						cs := &p.Status.ContainerStatuses[idx]
						SetContainerTerminated(cs, exitCode)
					}, notify)
				}

				// Check restart policy
				if ctx.Err() != nil {
					return
				}
				shouldRestart := false
				lastExit := 0
				h.mu.Lock()
				if idx < len(h.Pod.Status.ContainerStatuses) && h.Pod.Status.ContainerStatuses[idx].State.Terminated != nil {
					lastExit = int(h.Pod.Status.ContainerStatuses[idx].State.Terminated.ExitCode)
				}
				h.mu.Unlock()

				switch pod.Spec.RestartPolicy {
				case corev1.RestartPolicyAlways:
					shouldRestart = true
				case corev1.RestartPolicyOnFailure:
					shouldRestart = (lastExit != 0)
				case corev1.RestartPolicyNever:
					shouldRestart = false
				}

				if !shouldRestart {
					return
				}

				restartAttempt++
				backoff := time.Duration(1<<uint(min(restartAttempt, 5))) * 250 * time.Millisecond
				if backoff > 10*time.Second {
					backoff = 10 * time.Second
				}
				if backoff < 500*time.Millisecond {
					backoff = 500 * time.Millisecond
				}

				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
			}
		}(containerIdx, containerDef)
	}

	// For non-restarting pods, wait for all application containers to terminate and finalize
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		go func() {
			appWg.Wait()
			if ctx.Err() == nil {
				killPauseAndWait()
				h.updatePodState(nil, notify)
			}
		}()
	}
}
