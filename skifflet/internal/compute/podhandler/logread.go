package podhandler

import (
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/cri-client/pkg/logs"

	"skifflet/internal/compute/endpoint"
)

var (
	// ErrLogNotStarted means the container has not started, so there is nothing to read.
	ErrLogNotStarted = errors.New("container is waiting to start")
	// ErrNoPreviousLog means there is no earlier attempt of the container.
	ErrNoPreviousLog = errors.New("previous terminated container not found")
	// ErrNoSuchContainer means the pod has no container of that name.
	ErrNoSuchContainer = errors.New("container not found")
)

// LogSelection is the one attempt whose log a request reads. A request is bound to
// it: it never continues into the next attempt of the container.
type LogSelection struct {
	Attempt int
	Path    string
	live    *Attempt // the attempt if its process still runs, else nil
}

// Live says whether the attempt is still writing its log.
func (s LogSelection) Live() bool { return s.live != nil }

// SelectLog picks the attempt of a container whose log is read: the current one,
// or with previous the one the current one replaced.
//
// The current attempt is the latest one, running or finished. The previous one is
// what the kubelet calls the last terminated container: the attempt before the
// latest, unless the latest has ended and the container is waiting to be started
// again (restart policy, back-off), in which case the ended one is the last
// terminated container itself.
func (l *PodLifecycle) SelectLog(container string, previous bool) (LogSelection, error) {
	pod := l.GetPod()
	var init bool
	found := false
	for _, c := range pod.Spec.InitContainers {
		if c.Name == container {
			init, found = true, true
		}
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == container {
			found = true
		}
	}
	if !found {
		return LogSelection{}, fmt.Errorf("%w: %s", ErrNoSuchContainer, container)
	}

	l.mu.RLock()
	latest := l.counter[container]
	current := l.current[container]
	_, running := l.live[current]
	l.mu.RUnlock()
	if latest == 0 {
		return LogSelection{}, fmt.Errorf("%w: %s", ErrLogNotStarted, container)
	}

	attempt := latest
	if previous {
		if running || !l.restartPending(pod, container, init) {
			attempt = latest - 1
		}
		if attempt < 1 {
			return LogSelection{}, fmt.Errorf("%w: %s", ErrNoPreviousLog, container)
		}
	}
	path := l.podDir.Container(container).AttemptLogPath(attempt)
	sel := LogSelection{Attempt: attempt, Path: path}
	if attempt == latest && running {
		sel.live = current
	}
	return sel, nil
}

// restartPending says whether a container that has ended will be started again.
func (l *PodLifecycle) restartPending(pod *corev1.Pod, container string, init bool) bool {
	l.mu.RLock()
	terminating := l.terminating
	l.mu.RUnlock()
	if terminating {
		return false
	}
	cs := containerStatus(&pod.Status, container, init)
	if cs == nil || cs.State.Terminated == nil {
		return false
	}
	failed := cs.State.Terminated.ExitCode != 0
	switch pod.Spec.RestartPolicy {
	case corev1.RestartPolicyAlways:
		return !init || failed
	case corev1.RestartPolicyOnFailure:
		return failed
	}
	return false
}

// AttemptLogPath is used by tests and diagnostics: where an attempt's log is.
func (l *PodLifecycle) AttemptLogPath(container string, attempt int) string {
	return endpoint.PodPath(l.podDir).Container(container).AttemptLogPath(attempt)
}

// ReadLog copies the selected attempt's log to w as the options ask (see the CRI
// log options: tail, limit, since, timestamps, follow). Following ends when the
// attempt has ended and its remaining output has been copied, or when ctx ends.
// Output and error are written to the same stream, in the order they were logged.
func ReadLog(ctx context.Context, sel LogSelection, opts *logs.LogOptions, w io.Writer) error {
	return logs.ReadLogs(ctx, sel.Path, "", opts, attemptRuntime{live: sel.live}, w, w)
}

// attemptRuntime answers the one question the log reader asks of a container
// runtime: is the container still running, so that more output may come. The
// answer is about the attempt that was selected, never about a later one.
type attemptRuntime struct {
	internalapi.RuntimeService
	live *Attempt
}

func (r attemptRuntime) ContainerStatus(_ context.Context, _ string, _ bool) (*runtimeapi.ContainerStatusResponse, error) {
	state := runtimeapi.ContainerState_CONTAINER_EXITED
	if r.live != nil {
		select {
		case <-r.live.LogDone():
		default:
			state = runtimeapi.ContainerState_CONTAINER_RUNNING
		}
	}
	return &runtimeapi.ContainerStatusResponse{Status: &runtimeapi.ContainerStatus{State: state}}, nil
}

// LogDone is closed when the attempt's log is complete (nil, and so never ready,
// for an attempt without a log).
func (a *Attempt) LogDone() <-chan struct{} {
	a.pl.mu.RLock()
	defer a.pl.mu.RUnlock()
	if a.log == nil {
		return nil
	}
	return a.log.Done()
}
