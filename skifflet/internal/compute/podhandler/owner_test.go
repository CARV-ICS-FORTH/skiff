package podhandler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
)

// installOwner does what the provider does for a new pod generation: create the
// lifecycle owner, open its launch gate and register it. Preparation is then
// started with prepare, which runs the production PreparePod.
func installOwner(t *testing.T, pod *corev1.Pod, notify func(*corev1.Pod)) *PodLifecycle {
	t.Helper()
	owner := NewPodLifecycleWithContext(context.Background(), pod.DeepCopy(),
		compute.Skiff.PodWithUID(client.ObjectKeyFromObject(pod), pod.UID), notify)
	owner.BeginLaunch()
	if old := GlobalRegistry.Register(owner); old != nil {
		old.Terminate(0)
		GlobalRegistry.DeleteOwner(old)
	}
	return owner
}

// prepare runs the production preparation of an installed owner and returns a
// channel closed when it has quiesced.
func prepare(owner *PodLifecycle) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		PreparePod(owner)
	}()
	return done
}

// newTestHandler builds a handler the way PreparePod does: over a lifecycle owner,
// with the spec as a read-only snapshot.
func newTestHandler(pod *corev1.Pod, podDir endpoint.PodPath) *PodHandler {
	owner := NewPodLifecycle(pod, podDir, nil)
	return &PodHandler{Pod: owner.GetPod(), lifecycle: owner, podDirectory: podDir}
}
