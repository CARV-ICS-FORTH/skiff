package podhandler

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/runtime"
)

// ContainerRuntime holds active process and execution metadata for a container.
type ContainerRuntime struct {
	Name      string
	Cmd       *exec.Cmd
	PID       int
	StartTime uint64
	Spec      corev1.Container
	Status    corev1.ContainerStatus

	RestartCount         int32
	LastTerminationState corev1.ContainerState
	IsInit               bool
	Finished             bool
	ExitCode             int
}

// SandboxRuntime holds handles for the pod's network sandbox and pause container.
type SandboxRuntime struct {
	PausePID     int
	ContainerPID int
	StartTime    uint64
	PauseCmd     *exec.Cmd
	PauseDone    <-chan struct{}
	StopPause    func()
	NetnsPath    string
	NetnsFile    *os.File
	EndpointID   string
	IP           string
	Gateway      string
}

type trackedProcess struct {
	done    chan struct{}
	process *os.Process
	start   uint64
}

// PodLifecycle coordinates state transitions, concurrency control, and resource cleanup for a single pod UID.
type PodLifecycle struct {
	mu sync.RWMutex

	uid        types.UID
	key        client.ObjectKey
	generation int64
	podDir     endpoint.PodPath

	ctx        context.Context
	cancel     context.CancelFunc
	doneCh     chan struct{}
	liveOnce   sync.Once
	deleteOnce sync.Once
	launchDone chan struct{}
	attempts   map[int]*trackedProcess

	desiredPod   *corev1.Pod
	publishedPod *corev1.Pod

	terminating bool
	terminal    bool

	sandbox    *SandboxRuntime
	containers map[string]*ContainerRuntime
	ledger     *ResourceLedger

	notifyFn func(*corev1.Pod)
}

// NewPodLifecycle creates a new lifecycle owner for a pod.
func NewPodLifecycle(pod *corev1.Pod, podDir endpoint.PodPath, notifyFn func(*corev1.Pod)) *PodLifecycle {
	return NewPodLifecycleWithContext(context.Background(), pod, podDir, notifyFn)
}

func NewPodLifecycleWithContext(parent context.Context, pod *corev1.Pod, podDir endpoint.PodPath, notifyFn func(*corev1.Pod)) *PodLifecycle {
	ctx, cancel := context.WithCancel(parent)
	key := client.ObjectKeyFromObject(pod)

	pl := &PodLifecycle{
		uid:          pod.GetUID(),
		key:          key,
		generation:   pod.GetGeneration(),
		podDir:       podDir,
		ctx:          ctx,
		cancel:       cancel,
		doneCh:       make(chan struct{}),
		desiredPod:   pod.DeepCopy(),
		publishedPod: pod.DeepCopy(),
		containers:   make(map[string]*ContainerRuntime),
		attempts:     make(map[int]*trackedProcess),
		ledger:       NewResourceLedger(),
		notifyFn:     notifyFn,
	}

	// Initialize container status structures
	pl.initStatus()
	return pl
}

func (l *PodLifecycle) initStatus() {
	if l.publishedPod.Status.Phase == "" {
		l.publishedPod.Status.Phase = corev1.PodPending
	}
	if l.publishedPod.Status.Conditions == nil {
		l.publishedPod.Status.Conditions = []corev1.PodCondition{
			{
				Type:               corev1.PodScheduled,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.Now(),
			},
			{
				Type:               corev1.PodInitialized,
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
			},
			{
				Type:               corev1.PodReady,
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
			},
			{
				Type:               corev1.ContainersReady,
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
			},
		}
	}
}

// UID returns the pod's unique identifier.
func (l *PodLifecycle) UID() types.UID {
	return l.uid
}

// Key returns the namespaced key of the pod.
func (l *PodLifecycle) Key() client.ObjectKey {
	return l.key
}

// Context returns the cancellation context for this pod's active execution.
func (l *PodLifecycle) Context() context.Context {
	return l.ctx
}

// Revoke closes the launch gate without waiting for startup or resource cleanup.
func (l *PodLifecycle) Revoke() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.terminating = true
	l.cancel()
}

// BeginLaunch records that the production launcher can still start processes.
func (l *PodLifecycle) BeginLaunch() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.launchDone = make(chan struct{})
}

func (l *PodLifecycle) EndLaunch() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.launchDone != nil {
		close(l.launchDone)
		l.launchDone = nil
	}
}

// StartTrackedProcess makes the cancellation check and command start one
// transition. The launcher remains the sole waiter for the process.
func (l *PodLifecycle) StartTrackedProcess(cmd *exec.Cmd) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.terminating || l.ctx.Err() != nil {
		return context.Canceled
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	start, _ := runtime.GetProcessStartTime(cmd.Process.Pid)
	l.attempts[cmd.Process.Pid] = &trackedProcess{done: make(chan struct{}), process: cmd.Process, start: start}
	return nil
}

func (l *PodLifecycle) FinishTrackedProcess(pid int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if done, ok := l.attempts[pid]; ok {
		delete(l.attempts, pid)
		close(done.done)
	}
}

// SignalTrackedProcess only signals a PID while it belongs to this active
// launch attempt. FinishTrackedProcess closes this gate after the sole waiter.
func (l *PodLifecycle) SignalTrackedProcess(pid int, signal syscall.Signal) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx.Err() != nil || l.terminating {
		return false
	}
	attempt, ok := l.attempts[pid]
	if !ok || (attempt.start != 0 && !runtime.VerifyProcessIdentity(pid, attempt.start)) {
		return false
	}
	if syscall.Kill(-pid, signal) == nil {
		return true
	}
	return attempt.process.Signal(signal) == nil
}

// Ledger returns the resource cleanup ledger for this pod.
func (l *PodLifecycle) Ledger() *ResourceLedger {
	return l.ledger
}

// GetPod returns an immutable deep copy of the current pod snapshot.
func (l *PodLifecycle) GetPod() *corev1.Pod {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.publishedPod.DeepCopy()
}

// GetPodStatus returns an immutable deep copy of the current pod status.
func (l *PodLifecycle) GetPodStatus() *corev1.PodStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.publishedPod.Status.DeepCopy()
}

// Publish updates the published snapshot of the pod from an authoritative source.
func (l *PodLifecycle) Publish(snapshot *corev1.Pod) {
	if snapshot == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.publishedPod = snapshot.DeepCopy()
}

// UpdateFromAPI updates the desired spec without overwriting local runtime state.
func (l *PodLifecycle) UpdateFromAPI(newPod *corev1.Pod) {
	newPod = newPod.DeepCopy()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.desiredPod = newPod
	// Update metadata and labels while preserving status and runtime annotations
	l.publishedPod.Labels = newPod.Labels
	if l.publishedPod.Annotations == nil {
		l.publishedPod.Annotations = make(map[string]string)
	}
	for k, v := range newPod.Annotations {
		// Do not let API overwrite internal runtime metadata
		if k != "skiff.io/pause-pid" && k != "skiff.io/container-pid" && k != "pod.skiff/id" && k != "skiff.io/pod-ip" && k != "skiff.io/endpoint-id" {
			l.publishedPod.Annotations[k] = v
		}
	}
}

// SetSandbox records the active sandbox runtime handle.
func (l *PodLifecycle) SetSandbox(sb *SandboxRuntime) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sandbox = sb
	l.publishedPod.Status.PodIP = sb.IP
	l.publishedPod.Status.PodIPs = []corev1.PodIP{{IP: sb.IP}}
	if l.publishedPod.Annotations == nil {
		l.publishedPod.Annotations = make(map[string]string)
	}
	l.publishedPod.Annotations["skiff.io/pod-ip"] = sb.IP
	pauseJobID := runtime.FormatProcessJobID(sb.PausePID, sb.StartTime)
	l.publishedPod.Annotations["skiff.io/pause-pid"] = pauseJobID
	if sb.EndpointID != "" {
		l.publishedPod.Annotations["skiff.io/endpoint-id"] = sb.EndpointID
	}
	runtime.SetPodID(l.publishedPod, runtime.JobIDTypeProcess, pauseJobID)

}

// RegisterContainer registers an initialized container before launch.
func (l *PodLifecycle) RegisterContainer(spec corev1.Container, isInit bool) *ContainerRuntime {
	l.mu.Lock()
	defer l.mu.Unlock()

	cr := &ContainerRuntime{
		Name:   spec.Name,
		Spec:   *spec.DeepCopy(),
		IsInit: isInit,
		Status: corev1.ContainerStatus{
			Name:  spec.Name,
			Image: spec.Image,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ContainerCreating",
					Message: "Container is being started",
				},
			},
		},
	}
	l.containers[spec.Name] = cr
	l.syncStatusesLocked()
	return cr
}

// SetContainerRunning marks a container as running with its verified PID and start time.
func (l *PodLifecycle) SetContainerRunning(name string, cmd *exec.Cmd, pid int, startTime uint64) {
	l.mu.Lock()
	cr, ok := l.containers[name]
	if !ok {
		l.mu.Unlock()
		return
	}

	cr.Cmd = cmd
	cr.PID = pid
	cr.StartTime = startTime
	jobID := runtime.FormatProcessJobID(pid, startTime)
	cr.Status.ContainerID = jobID
	if l.publishedPod != nil {
		if l.publishedPod.Annotations == nil {
			l.publishedPod.Annotations = make(map[string]string)
		}
		l.publishedPod.Annotations["skiff.io/container-pid"] = jobID
	}
	cr.Status.State.Waiting = nil
	cr.Status.State.Running = &corev1.ContainerStateRunning{
		StartedAt: metav1.Now(),
	}

	// Readiness probes: if no probe configured, mark ready; otherwise await probe
	if cr.Spec.ReadinessProbe == nil {
		cr.Status.Ready = true
	} else {
		cr.Status.Ready = false
		l.StartProbes(name, cr.Spec.ReadinessProbe, true)
	}

	l.recomputePhaseLocked()
	l.syncStatusesLocked()
	snapshot := l.publishedPod.DeepCopy()
	l.mu.Unlock()

	if l.notifyFn != nil {
		l.notifyFn(snapshot)
	}
}

// PodDir returns the filesystem path dedicated to this pod UID.
func (l *PodLifecycle) PodDir() endpoint.PodPath {
	return l.podDir
}

// GetContainer returns a shallow copy of the container runtime handle if found.
func (l *PodLifecycle) GetContainer(name string) (*ContainerRuntime, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cr, ok := l.containers[name]
	if !ok {
		return nil, false
	}
	copyCr := *cr
	copyCr.Spec = *cr.Spec.DeepCopy()
	copyCr.Status = *cr.Status.DeepCopy()
	return &copyCr, true
}

// SetContainerReady updates the readiness status of a container based on probe evaluation.
func (l *PodLifecycle) SetContainerReady(name string, ready bool) {
	l.mu.Lock()
	cr, ok := l.containers[name]
	if !ok || cr.Finished {
		l.mu.Unlock()
		return
	}
	if cr.Status.Ready == ready {
		l.mu.Unlock()
		return
	}
	cr.Status.Ready = ready
	l.recomputePhaseLocked()
	l.syncStatusesLocked()
	snapshot := l.publishedPod.DeepCopy()
	l.mu.Unlock()

	if l.notifyFn != nil {
		l.notifyFn(snapshot)
	}
}

// StartProbes launches periodic probe evaluation for containers with probes.
func (l *PodLifecycle) StartProbes(name string, probe *corev1.Probe, isReadiness bool) {
	if probe == nil {
		return
	}

	go func() {
		// Initial delay
		if probe.InitialDelaySeconds > 0 {
			select {
			case <-l.ctx.Done():
				return
			case <-time.After(time.Duration(probe.InitialDelaySeconds) * time.Second):
			}
		}

		period := time.Duration(probe.PeriodSeconds) * time.Second
		if period <= 0 {
			period = 10 * time.Second
		}
		timeout := time.Duration(probe.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 1 * time.Second
		}

		successThreshold := int(probe.SuccessThreshold)
		if successThreshold <= 0 {
			successThreshold = 1
		}
		failureThreshold := int(probe.FailureThreshold)
		if failureThreshold <= 0 {
			failureThreshold = 3
		}

		consecutiveSuccesses := 0
		consecutiveFailures := 0

		ticker := time.NewTicker(period)
		defer ticker.Stop()

		for {
			select {
			case <-l.ctx.Done():
				return
			case <-ticker.C:
				l.mu.RLock()
				cr, ok := l.containers[name]
				sb := l.sandbox
				finished := !ok || cr.Finished
				l.mu.RUnlock()

				if finished || sb == nil {
					return
				}

				success := evaluateProbe(probe, sb.IP, timeout)
				if success {
					consecutiveSuccesses++
					consecutiveFailures = 0
					if consecutiveSuccesses >= successThreshold {
						if isReadiness {
							l.SetContainerReady(name, true)
						}
					}
				} else {
					consecutiveFailures++
					consecutiveSuccesses = 0
					if consecutiveFailures >= failureThreshold {
						if isReadiness {
							l.SetContainerReady(name, false)
						}
					}
				}
			}
		}
	}()
}

func evaluateProbe(probe *corev1.Probe, podIP string, timeout time.Duration) bool {
	if probe.HTTPGet != nil {
		port := probe.HTTPGet.Port.IntValue()
		if port <= 0 {
			port = 80
		}
		host := probe.HTTPGet.Host
		if host == "" {
			host = podIP
		}
		if host == "" {
			return false
		}
		scheme := strings.ToLower(string(probe.HTTPGet.Scheme))
		if scheme == "" {
			scheme = "http"
		}
		url := fmt.Sprintf("%s://%s:%d%s", scheme, host, port, probe.HTTPGet.Path)
		client := &http.Client{Timeout: timeout}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return false
		}
		for _, h := range probe.HTTPGet.HTTPHeaders {
			req.Header.Set(h.Name, h.Value)
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 400
	}

	if probe.TCPSocket != nil {
		port := probe.TCPSocket.Port.IntValue()
		if port <= 0 {
			return false
		}
		host := probe.TCPSocket.Host
		if host == "" {
			host = podIP
		}
		if host == "" {
			return false
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), timeout)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}

	if probe.Exec != nil && len(probe.Exec.Command) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, probe.Exec.Command[0], probe.Exec.Command[1:]...)
		return cmd.Run() == nil
	}

	return true
}

// HandleContainerExit records the authoritative exit code from cmd.Wait().
func (l *PodLifecycle) HandleContainerExit(name string, exitCode int, waitErr error) {
	l.mu.Lock()
	cr, ok := l.containers[name]
	if !ok {
		l.mu.Unlock()
		return
	}

	cr.Finished = true
	cr.ExitCode = exitCode
	cr.Status.Ready = false

	var reason, msg string
	if exitCode == 0 {
		reason = "Completed"
		msg = "Container completed successfully"
	} else {
		reason = fmt.Sprintf("Error(%s)", name)
		if waitErr != nil {
			msg = waitErr.Error()
		} else {
			msg = HumanReadableCode(exitCode)
		}
	}

	prevState := cr.Status.State
	if prevState.Running != nil || prevState.Waiting != nil {
		cr.LastTerminationState = prevState
	}

	cr.Status.State.Running = nil
	cr.Status.State.Waiting = nil
	cr.Status.State.Terminated = &corev1.ContainerStateTerminated{
		ExitCode:    int32(exitCode),
		Reason:      reason,
		Message:     msg,
		FinishedAt:  metav1.Now(),
		ContainerID: cr.Status.ContainerID,
	}

	l.recomputePhaseLocked()
	l.syncStatusesLocked()

	// Check if all containers have completed and no restart is due
	allFinished := true
	for _, c := range l.containers {
		if !c.IsInit && !c.Finished {
			allFinished = false
			break
		}
	}

	shouldStopSandbox := false
	if allFinished && !l.terminal {
		restartPolicy := l.desiredPod.Spec.RestartPolicy
		if restartPolicy == corev1.RestartPolicyNever ||
			(restartPolicy == corev1.RestartPolicyOnFailure && l.publishedPod.Status.Phase == corev1.PodSucceeded) {
			shouldStopSandbox = true
			l.terminal = true
		}
	}

	snapshot := l.publishedPod.DeepCopy()
	l.mu.Unlock()

	if shouldStopSandbox {
		l.stopSandbox()
	}

	if l.notifyFn != nil {
		l.notifyFn(snapshot)
	}
}

func (l *PodLifecycle) syncStatusesLocked() {
	var initStatuses []corev1.ContainerStatus
	for _, ic := range l.desiredPod.Spec.InitContainers {
		if cr, ok := l.containers[ic.Name]; ok {
			initStatuses = append(initStatuses, cr.Status)
		}
	}
	l.publishedPod.Status.InitContainerStatuses = initStatuses

	var appStatuses []corev1.ContainerStatus
	allReady := true
	for _, c := range l.desiredPod.Spec.Containers {
		if cr, ok := l.containers[c.Name]; ok {
			appStatuses = append(appStatuses, cr.Status)
			if !cr.Status.Ready {
				allReady = false
			}
		} else {
			allReady = false
		}
	}
	l.publishedPod.Status.ContainerStatuses = appStatuses

	// Update Ready and ContainersReady conditions
	for i := range l.publishedPod.Status.Conditions {
		cond := &l.publishedPod.Status.Conditions[i]
		switch cond.Type {
		case corev1.ContainersReady:
			if allReady && len(appStatuses) > 0 {
				cond.Status = corev1.ConditionTrue
			} else {
				cond.Status = corev1.ConditionFalse
			}
			cond.LastTransitionTime = metav1.Now()
		case corev1.PodReady:
			if allReady && (l.publishedPod.Status.Phase == corev1.PodRunning || l.publishedPod.Status.Phase == corev1.PodSucceeded) {
				cond.Status = corev1.ConditionTrue
				cond.Reason = "ContainersReady"
				cond.Message = "all containers are ready"
			} else {
				cond.Status = corev1.ConditionFalse
				cond.Reason = "ContainersUnready"
				cond.Message = "at least one container is not ready"
			}
			cond.LastTransitionTime = metav1.Now()
		}
	}
}

func (l *PodLifecycle) recomputePhaseLocked() {
	if l.terminating {
		return
	}

	// 1. Check init containers
	for _, ic := range l.desiredPod.Spec.InitContainers {
		cr, ok := l.containers[ic.Name]
		if !ok || !cr.Finished {
			l.publishedPod.Status.Phase = corev1.PodPending
			return
		}
		if cr.ExitCode != 0 {
			l.publishedPod.Status.Phase = corev1.PodFailed
			l.publishedPod.Status.Reason = "InitContainerFailed"
			l.publishedPod.Status.Message = fmt.Sprintf("Init container %s failed with exit code %d", cr.Name, cr.ExitCode)
			return
		}
	}

	// 2. Init containers succeeded
	for i := range l.publishedPod.Status.Conditions {
		if l.publishedPod.Status.Conditions[i].Type == corev1.PodInitialized {
			l.publishedPod.Status.Conditions[i].Status = corev1.ConditionTrue
			l.publishedPod.Status.Conditions[i].LastTransitionTime = metav1.Now()
		}
	}

	// 3. Evaluate main containers
	totalAppContainers := len(l.desiredPod.Spec.Containers)
	if totalAppContainers == 0 {
		l.publishedPod.Status.Phase = corev1.PodSucceeded
		return
	}

	runningCount := 0
	succeededCount := 0
	failedCount := 0

	for _, c := range l.desiredPod.Spec.Containers {
		cr, ok := l.containers[c.Name]
		if !ok {
			continue
		}
		if cr.Status.State.Running != nil {
			runningCount++
		} else if cr.Finished {
			if cr.ExitCode == 0 {
				succeededCount++
			} else {
				failedCount++
			}
		}
	}

	if runningCount > 0 {
		l.publishedPod.Status.Phase = corev1.PodRunning
		return
	}

	if succeededCount == totalAppContainers {
		l.publishedPod.Status.Phase = corev1.PodSucceeded
		l.publishedPod.Status.Reason = "Completed"
		return
	}

	if failedCount > 0 && (failedCount+succeededCount == totalAppContainers) {
		restartPolicy := l.desiredPod.Spec.RestartPolicy
		if restartPolicy == corev1.RestartPolicyNever {
			l.publishedPod.Status.Phase = corev1.PodFailed
			l.publishedPod.Status.Reason = "ContainerFailed"
			return
		}
		if restartPolicy == corev1.RestartPolicyOnFailure {
			l.publishedPod.Status.Phase = corev1.PodRunning
			l.publishedPod.Status.Reason = "CrashLoopBackOff"
			return
		}
	}
}

func (l *PodLifecycle) stopSandbox() {
	l.mu.RLock()
	sb := l.sandbox
	l.mu.RUnlock()

	if sb != nil {
		compute.DefaultLogger.Info("Stopping pod sandbox network and pause process", "pod", l.key, "launcherPID", sb.PausePID, "containerPID", sb.ContainerPID)
		if sb.StopPause != nil {
			sb.StopPause()
		} else if sb.PauseCmd != nil && sb.PauseCmd.Process != nil && sb.PausePID > 1 {
			if !runtime.IsProcessDead(sb.PausePID) {
				_ = syscall.Kill(-sb.PausePID, syscall.SIGTERM)
				_ = syscall.Kill(sb.PausePID, syscall.SIGTERM)
				deadline := time.Now().Add(3 * time.Second)
				for !runtime.IsProcessDead(sb.PausePID) && time.Now().Before(deadline) {
					time.Sleep(50 * time.Millisecond)
				}
				if !runtime.IsProcessDead(sb.PausePID) {
					_ = syscall.Kill(-sb.PausePID, syscall.SIGKILL)
					_ = syscall.Kill(sb.PausePID, syscall.SIGKILL)
				}
			}
			if sb.PauseDone != nil {
				select {
				case <-sb.PauseDone:
				case <-time.After(5 * time.Second):
				}
			}
		}
		if sb.ContainerPID > 1 && !runtime.IsProcessDead(sb.ContainerPID) {
			_ = syscall.Kill(-sb.ContainerPID, syscall.SIGKILL)
			_ = syscall.Kill(sb.ContainerPID, syscall.SIGKILL)
		}
		if sb.NetnsFile != nil {
			_ = sb.NetnsFile.Close()
		}
		if sb.EndpointID != "" {
			_ = RemovePlaidEndpoint(sb.EndpointID)
		}
	}
}

// ReleaseLiveResources stops process and network allocations while retaining
// the pod's final status, logs, and metadata until API deletion.
func (l *PodLifecycle) ReleaseLiveResources(gracePeriod time.Duration) {
	l.liveOnce.Do(func() {
		l.mu.Lock()
		l.terminating = true
		l.cancel()
		attempts := make(map[int]*trackedProcess, len(l.attempts))
		for pid, attempt := range l.attempts {
			attempts[pid] = attempt
		}
		launchDone := l.launchDone
		l.mu.Unlock()

		for pid, attempt := range attempts {
			if attempt.start == 0 || runtime.VerifyProcessIdentity(pid, attempt.start) {
				if syscall.Kill(-pid, syscall.SIGTERM) != nil {
					_ = attempt.process.Signal(syscall.SIGTERM)
				}
			}
		}
		deadline := time.Now().Add(gracePeriod)
		for pid, attempt := range attempts {
			remaining := time.Until(deadline)
			if remaining > 0 {
				select {
				case <-attempt.done:
					continue
				case <-time.After(remaining):
				}
			}
			if attempt.start == 0 || runtime.VerifyProcessIdentity(pid, attempt.start) {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				_ = attempt.process.Kill()
			}
			select {
			case <-attempt.done:
			case <-time.After(2 * time.Second):
			}
		}
		if launchDone != nil {
			select {
			case <-launchDone:
			case <-time.After(10 * time.Second):
			}
		}
		l.stopSandbox()
		_ = l.ledger.CleanupAll()
	})
}

// Terminate also removes the UID-owned files after live resources are released.
func (l *PodLifecycle) Terminate(gracePeriod time.Duration) {
	l.deleteOnce.Do(func() {
		l.ReleaseLiveResources(gracePeriod)
		_ = os.RemoveAll(l.podDir.String())
		close(l.doneCh)
	})
	<-l.doneCh
}

// RemovePlaidEndpoint requests plaidd to remove an endpoint by ID over /run/plaid/plaidd.sock.
func RemovePlaidEndpoint(endpointID string) error {
	if endpointID == "" {
		return nil
	}
	conn, err := net.DialTimeout("unix", "/run/plaid/plaidd.sock", 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	req := fmt.Sprintf(`{"action":"remove_endpoint","pod_id":%q,"container_id":%q}`, endpointID, endpointID)
	data := []byte(req)
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)
	if _, err := conn.Write(frame); err != nil {
		return err
	}

	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return err
	}
	respLen := binary.BigEndian.Uint32(hdr)
	respData := make([]byte, respLen)
	if _, err := io.ReadFull(conn, respData); err != nil {
		return err
	}
	var resp struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(respData, &resp); err == nil && !resp.Success {
		return fmt.Errorf("remove_endpoint error: %s", resp.Error)
	}
	return nil
}
