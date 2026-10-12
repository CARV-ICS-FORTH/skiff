// Copyright © 2026 FORTH-ICS
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
	"time"

	corev1 "k8s.io/api/core/v1"
)

// The kubelet's back-off: a container that ends and is to be started again (a crash loop), and
// an image that cannot be pulled, wait before the next try. The first restart of a container
// is at once, then 10 s, doubling to 5 minutes; a container that ran for 10 minutes starts the
// count again. An image is tried again after 10 s, doubling to 5 minutes. While it waits, a
// container is Waiting with the reason CrashLoopBackOff or ImagePullBackOff.

// backOffUnit is the first pause (10 s); only tests change it, to run the rules without waiting.
var backOffUnit = 10 * time.Second

func maxBackOff() time.Duration        { return 30 * backOffUnit } // 5 minutes
func backOffResetAfter() time.Duration { return 60 * backOffUnit } // 10 minutes

// backOff is the pause before each next try.
type backOff struct{ next time.Duration }

// restartBackOff is the back-off of a container's restarts: the first is at once.
func restartBackOff() *backOff { return &backOff{} }

// pullBackOff is the back-off of an image's pulls: the first retry waits backOffUnit.
func pullBackOff() *backOff { return &backOff{next: backOffUnit} }

// delay is the pause before the next try, after one that lasted ran (a long one resets it).
func (b *backOff) delay(ran time.Duration) time.Duration {
	if ran >= backOffResetAfter() {
		b.next = 0
	}
	d := b.next
	switch {
	case b.next == 0:
		b.next = backOffUnit
	default:
		b.next = min(2*b.next, maxBackOff())
	}
	return d
}

// sleepCtx waits for d or for ctx to end; false when ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// restartsAfter says whether a container that ended with exitCode is started again: an init
// container until it succeeds, unless the pod's policy is Never; an application container as
// its pod's restart policy says.
func restartsAfter(policy corev1.RestartPolicy, init bool, exitCode int) bool {
	switch {
	case init:
		return exitCode != 0 && policy != corev1.RestartPolicyNever
	case policy == corev1.RestartPolicyAlways:
		return true
	case policy == corev1.RestartPolicyOnFailure:
		return exitCode != 0
	}
	return false
}

// runContainer runs a container until its restart policy says it is done: each attempt is
// prepared anew (its environment, its image as its pull policy says, as the kubelet does for
// every start of a container), run, and followed by the back-off. done is false when the
// container will not complete: the pod failed or is going away.
func (h *PodHandler) runContainer(ctx context.Context, spec *corev1.Container, init bool, index, sandboxPID int) (exitCode int, done bool) {
	pl := h.lifecycle
	restarts := restartBackOff()
	for {
		c, ok := h.prepareContainer(ctx, spec, init, index)
		if !ok {
			return 0, false
		}
		began := time.Now()
		exitCode, result, _ := h.runAttempt(&c, spec, init, sandboxPID)
		if result == attemptFilesError || ctx.Err() != nil {
			return exitCode, false // the pod failed with the reason, or is going away
		}
		if !restartsAfter(pl.RestartPolicy(), init, exitCode) {
			return exitCode, true
		}
		pause := restarts.delay(time.Since(began))
		waiting := corev1.ContainerStateWaiting{Reason: "ContainerCreating"}
		if pause > 0 {
			key := pl.Key()
			waiting = corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff",
				Message: fmt.Sprintf("back-off %s restarting failed container=%s pod=%s_%s(%s)", pause, spec.Name, key.Name, key.Namespace, pl.UID())}
			pl.ContainerEvent(spec.Name, corev1.EventTypeWarning, "BackOff", "Back-off restarting failed container %s in pod %s_%s(%s)", spec.Name, key.Name, key.Namespace, pl.UID())
		}
		pl.SetRestarting(spec.Name, init, waiting)
		if !sleepCtx(ctx, pause) {
			return exitCode, false
		}
	}
}
