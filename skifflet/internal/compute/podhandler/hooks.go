package podhandler

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"skifflet/internal/compute"
)

// Lifecycle hooks follow the kubelet's semantics.
//
//   - PostStart runs when the container's process has been started, concurrently
//     with its entrypoint: there is no ordering between them. The container is not
//     reported Running (and its probes do not start) until the hook has succeeded. A
//     hook that fails stops the container, which then follows the restart policy.
//   - PreStop runs before the termination signal, whatever the reason for
//     terminating: the pod's deletion, or a failing liveness or startup probe. It
//     uses up the container's grace period (there is no second one), it is not
//     run for a container that already ended or when the grace period is zero, and
//     a failing PreStop does not prevent the termination.
//   - Handlers: exec, HTTP GET and sleep. TCP handlers are rejected with the pod.
//
// Hooks run through the attempt that was current when they started, so they can
// never reach the next attempt of a restarted container; they may be repeated
// (after a crash of Skifflet, for example) and applications should tolerate that.

const (
	hookPostStart = "PostStart"
	hookPreStop   = "PreStop"

	// hookTargetWait is how long an exec hook waits for the container's process to
	// be in place (a large image takes a while to unpack).
	hookTargetWait = 30 * time.Second

	// hookHTTPTimeout bounds an HTTP hook that has no deadline of its own.
	hookHTTPTimeout = 2 * time.Minute
)

// hook returns the handler of the named hook of the attempt's container, if any.
func (a *Attempt) hook(name string) *corev1.LifecycleHandler {
	if a.spec == nil || a.spec.Lifecycle == nil {
		return nil
	}
	if name == hookPostStart {
		return a.spec.Lifecycle.PostStart
	}
	return a.spec.Lifecycle.PreStop
}

// runHook runs a handler and reports why it failed. It ends with ctx, and when
// ended does: the context whose end means there is nothing left to run it in.
func (a *Attempt) runHook(ctx context.Context, name string, h *corev1.LifecycleHandler, ended context.Context) error {
	a.logger.Info("running a lifecycle hook", "action", "hook", "hook", name)
	err := a.runHandler(ctx, h, ended)
	if err != nil {
		a.logger.Info("a lifecycle hook failed", "action", "hook", "hook", name, "reason", err.Error())
	}
	return err
}

func (a *Attempt) runHandler(ctx context.Context, h *corev1.LifecycleHandler, ended context.Context) error {
	switch {
	case h.Exec != nil:
		if len(h.Exec.Command) == 0 {
			return errors.New("the exec hook has no command")
		}
		target, err := a.target(ctx, hookTargetWait, ended)
		if err != nil {
			return err
		}
		code, err := target.run(ctx, h.Exec.Command)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("the command %v exited with code %d", h.Exec.Command, code)
		}
		return nil

	case h.HTTPGet != nil:
		port, err := resolvePort(h.HTTPGet.Port, a.spec.Ports)
		if err != nil {
			return err
		}
		host := hostOrPod(h.HTTPGet.Host, a.pl.GetPodStatus().PodIP)
		if host == "" {
			return errors.New(errNoHost)
		}
		timeout := hookHTTPTimeout
		if deadline, ok := ctx.Deadline(); ok {
			timeout = time.Until(deadline)
		}
		if ok, detail := httpProbe(ctx, h.HTTPGet, host, port, timeout); !ok {
			return errors.New(detail)
		}
		return nil

	case h.Sleep != nil:
		timer := time.NewTimer(time.Duration(h.Sleep.Seconds) * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-ended.Done():
			return fmt.Errorf("%w: attempt %d ended", ErrContainerNotRunning, a.ID)
		}
	}
	return errors.New("the hook has no supported handler")
}

// runPostStart runs the PostStart hook of a container whose process has started
// and, when it succeeds, publishes the container as running and starts its probes.
// A failing hook stops the container. Nothing happens for an attempt that ended in
// the meantime.
func (a *Attempt) runPostStart(podIP string, runExec execRunner) {
	// The launcher has started, but the application starts only once the image is
	// unpacked: the kubelet's "started" is the application's, so wait for it. (Any
	// handler may expect the application to exist, not only exec.)
	_, err := a.target(a.ctx, hookTargetWait, a.ctx)
	if err == nil {
		err = a.runHook(a.ctx, hookPostStart, a.hook(hookPostStart), a.ctx)
	}
	if err != nil {
		if a.ctx.Err() != nil {
			return // the attempt or the pod ended while the hook ran
		}
		a.event(corev1.EventTypeWarning, "FailedPostStartHook", "PostStart hook failed: %v", err)
		a.Stop(TerminationGrace(nil, a.pl.GetPod()), "the PostStart hook failed")
		return
	}
	a.SetRunning()
	startProbes(a, podIP, a.spec, runExec)
}

// event records an event about the attempt's container, when events are recorded.
func (a *Attempt) event(eventType, reason, format string, args ...any) {
	if compute.Events == nil {
		return
	}
	pod := a.pl.GetPod()
	ref := &corev1.ObjectReference{
		Kind: "Pod", APIVersion: "v1", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID,
		FieldPath: fmt.Sprintf("spec.containers{%s}", a.Container),
	}
	compute.Events.Eventf(ref, eventType, reason, format, args...)
}
