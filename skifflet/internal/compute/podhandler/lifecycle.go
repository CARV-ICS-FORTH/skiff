package podhandler

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/runtime"
)

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

// PodLifecycle is the single execution owner of one pod UID: everything that
// starts, observes, signals or releases the pod's processes goes through it.
// Where each piece of state lives:
//
//   - The launched spec (spec) is the pod as it was created. Only its labels
//     and annotations follow later API updates; a changed spec is never
//     presented as the running configuration.
//   - The observed status (status) is what the runtime has seen: container
//     states, readiness, restarts, the pod IP. It has one writer, the methods
//     of this type, and every change is published to notifyFn in order.
//   - Process facts (sandbox, current attempts) are the runtime truth that only
//     Skifflet can know. pids.json in the pod directory is their on-disk copy,
//     kept only so that a later run can reap processes this one left behind.
//
// Everything else is a derived view: GetPod composes the spec and status, the
// provider's pod cache is refreshed only from published snapshots, and VK's own
// pod map belongs to the framework. None of them is an execution authority.
//
// A container's process is run as a sequence of attempts (see Attempt). An
// attempt identifies one launch: probes, hooks, exec and logs are bound to it,
// and anything holding an older attempt can neither publish status nor signal
// the process of a newer one.
type PodLifecycle struct {
	mu        sync.RWMutex
	publishMu sync.Mutex // orders snapshots given to notifyFn; taken before mu

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

	spec   *corev1.Pod
	status corev1.PodStatus

	terminating bool

	sandbox *SandboxRuntime
	current map[string]*Attempt   // latest attempt of each container
	counter map[string]int        // attempts started per container
	live    map[*Attempt]struct{} // attempts whose process has not been waited for
	ledger  *ResourceLedger

	notifyFn func(*corev1.Pod)
	logger   logr.Logger
}

// NewPodLifecycle creates a new lifecycle owner for a pod.
func NewPodLifecycle(pod *corev1.Pod, podDir endpoint.PodPath, notifyFn func(*corev1.Pod)) *PodLifecycle {
	return NewPodLifecycleWithContext(context.Background(), pod, podDir, notifyFn)
}

func NewPodLifecycleWithContext(parent context.Context, pod *corev1.Pod, podDir endpoint.PodPath, notifyFn func(*corev1.Pod)) *PodLifecycle {
	ctx, cancel := context.WithCancel(parent)
	key := client.ObjectKeyFromObject(pod)
	return &PodLifecycle{
		uid:        pod.GetUID(),
		key:        key,
		generation: pod.GetGeneration(),
		podDir:     podDir,
		ctx:        ctx,
		cancel:     cancel,
		doneCh:     make(chan struct{}),
		spec:       pod.DeepCopy(),
		status:     *pod.Status.DeepCopy(),
		current:    make(map[string]*Attempt),
		counter:    make(map[string]int),
		live:       make(map[*Attempt]struct{}),
		ledger:     NewResourceLedger(),
		notifyFn:   notifyFn,
		logger:     compute.DefaultLogger.WithValues("pod", key, "uid", pod.GetUID()),
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

// PodDir returns the filesystem path dedicated to this pod UID.
func (l *PodLifecycle) PodDir() endpoint.PodPath {
	return l.podDir
}

// Ledger returns the resource cleanup ledger for this pod.
func (l *PodLifecycle) Ledger() *ResourceLedger {
	return l.ledger
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

// GetPod returns a deep copy of the pod as this owner knows it: the launched
// spec with the current metadata labels and annotations, and the observed status.
func (l *PodLifecycle) GetPod() *corev1.Pod {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.composeLocked()
}

// GetPodStatus returns a deep copy of the observed pod status.
func (l *PodLifecycle) GetPodStatus() *corev1.PodStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.status.DeepCopy()
}

// RestartPolicy returns the restart policy of the launched spec.
func (l *PodLifecycle) RestartPolicy() corev1.RestartPolicy {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.spec.Spec.RestartPolicy
}

func (l *PodLifecycle) composeLocked() *corev1.Pod {
	pod := l.spec.DeepCopy()
	pod.Status = *l.status.DeepCopy()
	return pod
}

// UpdateFromAPI applies an API update. Labels and annotations follow the API,
// including removals; the spec does not change what is running.
func (l *PodLifecycle) UpdateFromAPI(newPod *corev1.Pod) {
	update := newPod.DeepCopy()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spec.Labels = update.Labels
	l.spec.Annotations = update.Annotations
}

// apply changes the observed status and publishes the result. fn runs under the
// lock on a composed copy of the pod, and reports whether it changed anything;
// with recompute the pod phase and conditions are then derived again from the
// container states. Snapshots reach notifyFn in the order the changes were made.
func (l *PodLifecycle) apply(recompute bool, fn func(pod *corev1.Pod) bool) bool {
	l.publishMu.Lock()
	defer l.publishMu.Unlock()

	l.mu.Lock()
	pod := l.composeLocked()
	if !fn(pod) {
		l.mu.Unlock()
		return false
	}
	if recompute {
		UpdateStatusFromRuntime(pod)
	}
	l.status = *pod.Status.DeepCopy()
	snapshot := l.composeLocked()
	l.mu.Unlock()

	if l.notifyFn != nil {
		l.notifyFn(snapshot)
	}
	return true
}

// Update changes the observed status through fn and publishes it.
func (l *PodLifecycle) Update(fn func(status *corev1.PodStatus)) {
	l.apply(true, func(pod *corev1.Pod) bool { fn(&pod.Status); return true })
}

// Fail publishes a terminal failure of the pod itself (not of a container),
// such as a rejected spec or a startup step that could not complete.
func (l *PodLifecycle) Fail(reason, format string, args ...any) {
	l.apply(false, func(pod *corev1.Pod) bool {
		compute.PodError(pod, reason, format, args...)
		return true
	})
}

// SetSandbox records the pod's network sandbox and publishes its IP.
func (l *PodLifecycle) SetSandbox(sb *SandboxRuntime) {
	l.mu.Lock()
	l.sandbox = sb
	l.mu.Unlock()
	l.Update(func(status *corev1.PodStatus) {
		status.PodIP = sb.IP
		status.PodIPs = []corev1.PodIP{{IP: sb.IP}}
	})
}

// SetContainerWaiting publishes why a container has not started.
func (l *PodLifecycle) SetContainerWaiting(name string, init bool, reason, message string) {
	l.apply(false, func(pod *corev1.Pod) bool {
		cs := containerStatus(&pod.Status, name, init)
		if cs == nil {
			return false
		}
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}
		return true
	})
}

// PrepareContainer records the identity of a container that is ready to run.
func (l *PodLifecycle) PrepareContainer(name string, init bool, image, imageID, containerID string) {
	l.apply(false, func(pod *corev1.Pod) bool {
		cs := containerStatus(&pod.Status, name, init)
		if cs == nil {
			return false
		}
		cs.Name, cs.Image, cs.ImageID, cs.ContainerID = name, image, imageID, containerID
		return true
	})
}

// InitializeStatuses gives every container a Waiting status before anything runs,
// so the pod is Pending with its containers creating rather than unknown.
func (l *PodLifecycle) InitializeStatuses() {
	l.apply(true, func(pod *corev1.Pod) bool {
		pod.Status.Phase = corev1.PodPending
		if pod.Status.InitContainerStatuses == nil {
			pod.Status.InitContainerStatuses = waitingStatuses(pod.Spec.InitContainers, "Init container is waiting to be created")
		}
		if pod.Status.ContainerStatuses == nil {
			pod.Status.ContainerStatuses = waitingStatuses(pod.Spec.Containers, "Container is waiting to be created")
		}
		return true
	})
}

func waitingStatuses(containers []corev1.Container, message string) []corev1.ContainerStatus {
	statuses := make([]corev1.ContainerStatus, len(containers))
	for i, c := range containers {
		statuses[i] = corev1.ContainerStatus{
			Name:  c.Name,
			Image: c.Image,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating", Message: message}},
		}
	}
	return statuses
}

func containerStatus(status *corev1.PodStatus, name string, init bool) *corev1.ContainerStatus {
	statuses := status.ContainerStatuses
	if init {
		statuses = status.InitContainerStatuses
	}
	for i := range statuses {
		if statuses[i].Name == name {
			return &statuses[i]
		}
	}
	return nil
}

// Attempt is one launch of one container. The launcher owns it from
// BeginAttempt until Wait returns; probes, hooks, exec and log readers hold it to
// act on that launch only. Once a newer attempt of the container exists, or the
// attempt has ended, everything it does is a no-op.
type Attempt struct {
	pl        *PodLifecycle
	Container string
	Init      bool
	ID        int // 1 for the first launch of the container, then increasing

	ctx    context.Context
	cancel context.CancelFunc
	logger logr.Logger

	// Set by Start under pl.mu.
	process   *os.Process
	pid       int
	startTime uint64
	done      chan struct{} // closed once the sole waiter has returned

	// The container's main process, remembered once it has been found and
	// verified (see Target); guarded by pl.mu.
	payload      int
	payloadStart uint64

	gates    probeGates  // set by Start
	stopping atomic.Bool // one termination sequence per attempt (see Stop)

	// spec is the container this attempt runs; its lifecycle hooks are what
	// PostStart and Stop run.
	spec *corev1.Container

	// log is the attempt's output, set before the process starts (guarded by pl.mu).
	log *AttemptLog

	// lifeCtx lasts as long as the process does, unlike ctx, which also ends when
	// the pod is revoked. The termination sequence (PreStop hook, signals) runs on
	// it: deleting the pod must not cancel the hook that is part of deleting it.
	lifeCtx    context.Context
	lifeCancel context.CancelFunc
}

// BeginAttempt creates the next attempt of a container. It fails once the pod is
// being revoked, so a revoked owner cannot launch anything new.
func (l *PodLifecycle) BeginAttempt(name string, init bool) (*Attempt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.terminating || l.ctx.Err() != nil {
		return nil, context.Canceled
	}
	if prev := l.current[name]; prev != nil {
		if _, running := l.live[prev]; running {
			return nil, fmt.Errorf("container %s: attempt %d is still running", name, prev.ID)
		}
	}
	l.counter[name]++
	ctx, cancel := context.WithCancel(l.ctx)
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	a := &Attempt{pl: l, Container: name, Init: init, ID: l.counter[name], ctx: ctx, cancel: cancel,
		lifeCtx: lifeCtx, lifeCancel: lifeCancel,
		logger: l.logger.WithValues("container", name, "attempt", l.counter[name])}
	l.current[name] = a
	return a, nil
}

// Context is cancelled when the attempt ends or the pod is revoked. Work bound
// to the attempt (probes, hooks, exec sessions) stops with it.
func (a *Attempt) Context() context.Context { return a.ctx }

// probeGates says what holds a container's statuses back once its process has
// started: a PostStart hook holds Running itself (the container stays Waiting
// until the hook has succeeded, as the kubelet reports it), a startup probe holds
// Started (and so Ready), a readiness probe holds Ready.
type probeGates struct {
	postStart, startup, readiness bool
}

// NoProbes is the gates of a container without hooks or probes.
var NoProbes = probeGates{}

func gatesOf(spec *corev1.Container) probeGates {
	return probeGates{
		postStart: spec.Lifecycle != nil && spec.Lifecycle.PostStart != nil,
		startup:   spec.StartupProbe != nil,
		readiness: spec.ReadinessProbe != nil,
	}
}

// Start makes the revocation check and the process start one transition, then
// publishes the container as running. The launcher remains the sole waiter.
func (a *Attempt) Start(cmd *exec.Cmd, gates probeGates) error {
	l := a.pl
	l.mu.Lock()
	if l.terminating || l.ctx.Err() != nil || a.ctx.Err() != nil {
		l.mu.Unlock()
		return context.Canceled
	}
	if l.current[a.Container] != a {
		l.mu.Unlock()
		return fmt.Errorf("container %s: attempt %d was superseded", a.Container, a.ID)
	}
	if err := cmd.Start(); err != nil {
		l.mu.Unlock()
		return err
	}
	a.process = cmd.Process
	a.pid = cmd.Process.Pid
	a.startTime, _ = runtime.GetProcessStartTime(a.pid)
	a.done = make(chan struct{})
	a.gates = gates
	l.live[a] = struct{}{}
	l.mu.Unlock()

	a.logger.Info("container attempt started", "action", "start", "pid", a.pid)
	if gates.postStart {
		a.update(func(cs *corev1.ContainerStatus) bool {
			beginAttempt(cs)
			cs.ContainerID = runtime.FormatProcessJobID(a.pid, a.startTime)
			cs.State.Running = nil
			cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "PostStartHookPending", Message: "Waiting for the postStart hook"}
			notStarted := false
			cs.Started, cs.Ready = &notStarted, false
			return true
		})
		return nil
	}
	a.SetRunning()
	return nil
}

// SetRunning publishes the container as running, with the Started and Ready
// statuses its probes leave open. Start does it for a container without a
// PostStart hook; with one, the hook's success does.
func (a *Attempt) SetRunning() {
	a.update(func(cs *corev1.ContainerStatus) bool {
		if cs.State.Running != nil {
			return false
		}
		SetContainerRunning(cs, a.pid, a.startTime)
		started := !a.gates.startup
		cs.Started = &started
		cs.Ready = started && !a.gates.readiness
		return true
	})
}

// Wait is the sole waiter for the process: it reaps it, ends the attempt's
// context, publishes the exit and returns the exit code.
func (a *Attempt) Wait(cmd *exec.Cmd) int {
	waitErr := cmd.Wait()
	l := a.pl
	l.mu.Lock()
	delete(l.live, a)
	close(a.done)
	l.mu.Unlock()
	a.cancel()
	a.lifeCancel()
	if a.log != nil {
		a.log.Finish() // the container's last output is in its log before its exit is published
	}

	exitCode := 0
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
		exitCode = cmd.ProcessState.ExitCode()
	} else if waitErr != nil {
		exitCode = 1
	}
	a.logger.Info("container attempt exited", "action", "exit", "exitCode", exitCode)
	a.update(func(cs *corev1.ContainerStatus) bool { SetContainerTerminated(cs, exitCode); return true })
	return exitCode
}

// Fail ends an attempt that never started a process, as a container that
// terminated with the given exit code.
func (a *Attempt) Fail(exitCode int) {
	a.cancel()
	a.lifeCancel()
	a.logger.Info("container attempt could not start", "action", "start", "reason", "launch failed", "exitCode", exitCode)
	a.update(func(cs *corev1.ContainerStatus) bool { SetContainerTerminated(cs, exitCode); return true })
}

// FailPod ends an attempt that never started a process and, in the same
// publication, fails the pod with the reason. One publication matters: the
// controller skips status updates for a pod the API server already shows as
// Failed, so a later reason would never arrive.
func (a *Attempt) FailPod(exitCode int, reason, format string, args ...any) {
	a.cancel()
	a.lifeCancel()
	a.logger.Info("container attempt could not start", "action", "start", "reason", reason, "exitCode", exitCode)
	a.pl.apply(true, func(pod *corev1.Pod) bool {
		if a.pl.current[a.Container] != a {
			return false
		}
		cs := containerStatus(&pod.Status, a.Container, a.Init)
		if cs == nil {
			return false
		}
		SetContainerTerminated(cs, exitCode)
		compute.PodError(pod, reason, format, args...)
		return true
	})
}

// SetStarted publishes that the container has passed its startup probe. Unless a
// readiness probe still has to say so, that also makes it ready.
func (a *Attempt) SetStarted() {
	changed := a.update(func(cs *corev1.ContainerStatus) bool {
		if cs.State.Running == nil || (cs.Started != nil && *cs.Started) {
			return false
		}
		started := true
		cs.Started = &started
		cs.Ready = !a.gates.readiness
		return true
	})
	if changed {
		a.logger.Info("container started", "action", "startup")
	}
}

// SetReady publishes the readiness of the attempt's container.
func (a *Attempt) SetReady(ready bool) {
	changed := a.update(func(cs *corev1.ContainerStatus) bool {
		if cs.State.Running == nil || cs.Ready == ready {
			return false
		}
		cs.Ready = ready
		return true
	})
	if changed {
		a.logger.Info("container readiness changed", "action", "readiness", "ready", ready)
	}
}

// update changes the container's status if this is still its current attempt.
func (a *Attempt) update(fn func(cs *corev1.ContainerStatus) bool) bool {
	l := a.pl
	return l.apply(true, func(pod *corev1.Pod) bool {
		// The attempt check shares the lock with the status change, so a late
		// callback of a superseded attempt cannot publish.
		if l.current[a.Container] != a {
			return false
		}
		cs := containerStatus(&pod.Status, a.Container, a.Init)
		if cs == nil {
			return false
		}
		return fn(cs)
	})
}

// Stop ends the attempt's process the way the kubelet ends a container: the
// PreStop hook first, if the container has one and there is time for it; then the
// termination signal; then, if the process is still there when the grace period
// has passed, a kill. The hook's time is part of the grace period, not added to
// it, and the process always gets at least minKillGrace after the signal. The
// container's own process is what a hook and the signal reach: the sandbox (and so
// the network) stays up until every container has ended.
//
// Only the first call of an attempt runs the hook and sends the signal, so
// competing paths (a failing probe, the pod's deletion) cannot run two termination
// sequences; a later call only enforces its own deadline, and may shorten the wait.
func (a *Attempt) Stop(grace time.Duration, reason string) {
	start := time.Now()
	deadline := start.Add(max(grace, minKillGrace))
	if a.stopping.CompareAndSwap(false, true) {
		a.logger.Info("stopping the container", "action", "stop", "reason", reason, "grace", grace.String())
		if hook := a.hook(hookPreStop); hook != nil && grace > 0 {
			ctx, cancel := context.WithDeadline(a.lifeCtx, start.Add(grace))
			if err := a.runHook(ctx, hookPreStop, hook, a.lifeCtx); err != nil {
				a.event(corev1.EventTypeWarning, "FailedPreStopHook", "PreStop hook failed: %v", err)
			}
			cancel()
			// What the hook used comes out of the grace period.
			deadline = time.Now().Add(max(grace-time.Since(start), minKillGrace))
		} else {
			deadline = time.Now().Add(max(grace, minKillGrace))
		}
		a.signalTerm()
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-a.done:
	case <-timer.C:
		a.logger.Info("the container did not stop in time; killing it", "action", "stop")
		a.kill()
	}
}

// signalTerm sends the termination signal to the launcher's process group, which
// carries it to the container's main process.
func (a *Attempt) signalTerm() {
	if !a.verified() {
		return
	}
	if syscall.Kill(-a.pid, syscall.SIGTERM) != nil {
		_ = a.process.Signal(syscall.SIGTERM)
	}
}

// killGrace is how long a killed container's launcher gets to exit on its own.
var killGrace = 3 * time.Second

// Signal delivers a signal to the attempt's process group, only while the
// process is still the one this attempt started and the pod is not revoked.
// SIGKILL is delivered as Kill does.
func (a *Attempt) Signal(sig syscall.Signal) bool {
	l := a.pl
	l.mu.RLock()
	_, running := l.live[a]
	ok := l.ctx.Err() == nil && !l.terminating && running && a.verified()
	l.mu.RUnlock()
	if !ok {
		return false
	}
	a.logger.Info("signalling container attempt", "action", "signal", "signal", sig.String())
	if sig == syscall.SIGKILL {
		a.kill()
		return true
	}
	if syscall.Kill(-a.pid, sig) == nil {
		return true
	}
	return a.process.Signal(sig) == nil
}

// kill stops the attempt's process for good. The SIGKILL goes to the container's
// main process first: the launcher then exits by itself and Apptainer removes the
// container's unpacked root filesystem. Killing the launcher outright skips that
// cleanup and leaves the filesystem behind. Only when the launcher has not exited
// shortly afterwards (or there is no main process to find) does its whole process
// group get the SIGKILL.
func (a *Attempt) kill() {
	if !a.verified() {
		return
	}
	if payload := a.mainProcess(); payload > 1 {
		_ = syscall.Kill(payload, syscall.SIGKILL)
		select {
		case <-a.done:
			return
		case <-time.After(killGrace):
		}
	}
	if a.verified() {
		_ = syscall.Kill(-a.pid, syscall.SIGKILL)
		_ = a.process.Kill()
	}
}

// mainProcess returns the PID of the container's main process, or 0 if it has
// not been found.
func (a *Attempt) mainProcess() int {
	a.pl.mu.RLock()
	payload := a.payload
	a.pl.mu.RUnlock()
	if payload > 1 {
		return payload
	}
	return findPayload(a.pid)
}

func (a *Attempt) verified() bool {
	return a.startTime == 0 || runtime.VerifyProcessIdentity(a.pid, a.startTime)
}

// Launcher returns the verified identity of the launcher process of a running attempt.
func (a *Attempt) Launcher() (pid int, startTime uint64, ok bool) {
	l := a.pl
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, running := l.live[a]; !running || !a.verified() {
		return 0, 0, false
	}
	return a.pid, a.startTime, true
}

// RunningAttempt returns the current attempt of a container if its process runs.
func (l *PodLifecycle) RunningAttempt(name string) (*Attempt, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	a := l.current[name]
	if a == nil {
		return nil, false
	}
	if _, running := l.live[a]; !running {
		return nil, false
	}
	return a, true
}

func (l *PodLifecycle) stopSandbox() {
	l.mu.RLock()
	sb := l.sandbox
	l.mu.RUnlock()

	if sb != nil {
		l.logger.Info("Stopping pod sandbox network and pause process", "launcherPID", sb.PausePID, "containerPID", sb.ContainerPID)
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
		attempts := make([]*Attempt, 0, len(l.live))
		for a := range l.live {
			attempts = append(attempts, a)
		}
		launchDone := l.launchDone
		l.mu.Unlock()

		// Every container is stopped by its own termination sequence, side by side,
		// within the same grace period.
		var stopped sync.WaitGroup
		for _, a := range attempts {
			stopped.Add(1)
			go func() {
				defer stopped.Done()
				a.Stop(gracePeriod, "the pod is being terminated")
				select {
				case <-a.done:
				case <-time.After(2 * time.Second):
				}
			}()
		}
		stopped.Wait()
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
		if err := SafeRemovePodDirectory(l.podDir.String()); err != nil {
			l.logger.Error(err, "failed to remove the pod directory", "directory", l.podDir)
		}
		close(l.doneCh)
	})
	<-l.doneCh
}

// RemovePlaidEndpoint requests plaidd to remove an endpoint by ID over the Plaid socket.
func RemovePlaidEndpoint(endpointID string) error {
	if endpointID == "" {
		return nil
	}
	conn, err := net.DialTimeout("unix", compute.Environment.PlaidSocketPath(), 2*time.Second)
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
