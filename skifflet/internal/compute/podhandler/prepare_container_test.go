package podhandler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
)

// handlerFor builds a handler over a lifecycle whose pod has these containers, with
// an image file each container can be built from.
func handlerFor(t *testing.T, containers ...corev1.Container) (*PodHandler, string) {
	t.Helper()
	podDir := endpoint.PodPath(t.TempDir())
	if err := os.MkdirAll(podDir.JobDir(), 0755); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(t.TempDir(), "fixture.sif")
	if err := os.WriteFile(image, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: envPod().ObjectMeta, Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever}}
	for _, c := range containers {
		c.Image, c.ImagePullPolicy = image, corev1.PullNever
		pod.Spec.Containers = append(pod.Spec.Containers, c)
	}
	h := newTestHandler(pod, podDir)
	h.lifecycle.InitializeStatuses()
	h.logger = h.lifecycle.logger
	return h, image
}

func configMapEnv(name string) corev1.Container {
	return corev1.Container{Name: "app", Env: []corev1.EnvVar{cmKey(name, "mode", false)}}
}

// A container that refers to a ConfigMap that does not exist waits with the
// kubelet's reason and message, with an event, and starts once the ConfigMap is
// created, without anybody restarting anything.
func TestPrepareContainer_WaitsForMissingConfiguration(t *testing.T) {
	config := &fakeConfig{}
	useConfig(t, config)
	events := record.NewFakeRecorder(10)
	old := compute.Events
	compute.Events = events
	t.Cleanup(func() { compute.Events = old })

	h, _ := handlerFor(t, configMapEnv("settings"))
	spec := &h.Pod.Spec.Containers[0]
	type result struct {
		c  Container
		ok bool
	}
	done := make(chan result, 1)
	go func() {
		c, ok := h.prepareContainer(context.Background(), spec, false, 0)
		done <- result{c, ok}
	}()

	eventually(t, "the container to wait for its configuration", func() bool {
		w := containerState(h.lifecycle, "app").State.Waiting
		return w != nil && w.Reason == "CreateContainerConfigError" && strings.Contains(w.Message, `configmap "settings" not found`)
	})
	select {
	case e := <-events.Events:
		if !strings.Contains(e, "Warning Failed Error: configmap \"settings\" not found") {
			t.Fatalf("event %q", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event says why the container waits")
	}
	select {
	case r := <-done:
		t.Fatalf("the container was prepared without its configuration: %+v", r)
	default:
	}
	if h.lifecycle.GetPodStatus().Phase == corev1.PodFailed {
		t.Fatal("a missing ConfigMap failed the pod")
	}

	// The ConfigMap is created: the next try succeeds.
	config.setConfigMaps(map[string]*corev1.ConfigMap{"team/settings": {Data: map[string]string{"mode": "fast"}}})
	select {
	case r := <-done:
		if !r.ok {
			t.Fatal("the container was not prepared once its ConfigMap existed")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the container never retried")
	}
}

// One container waiting for its configuration does not hold another back.
func TestPrepareContainer_SiblingsAreNotHeldBack(t *testing.T) {
	useConfig(t, &fakeConfig{})
	h, _ := handlerFor(t, configMapEnv("missing"), corev1.Container{Name: "plain"})
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	go func() {
		defer close(waiting)
		h.prepareContainer(ctx, &h.Pod.Spec.Containers[0], false, 0)
	}()
	defer func() {
		cancel()
		<-waiting // it reads the configuration until it ends
	}()
	c, ok := h.prepareContainer(context.Background(), &h.Pod.Spec.Containers[1], false, 1)
	if !ok || c.InstanceName == "" {
		t.Fatalf("the sibling was not prepared: %+v, %v", c, ok)
	}
}

// Nothing is retried once the pod is going away.
func TestPrepareContainer_StopsWhenThePodDoes(t *testing.T) {
	useConfig(t, &fakeConfig{})
	h, _ := handlerFor(t, configMapEnv("missing"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { _, ok := h.prepareContainer(ctx, &h.Pod.Spec.Containers[0], false, 0); done <- ok }()
	eventually(t, "the container to wait", func() bool { return containerState(h.lifecycle, "app").State.Waiting != nil })
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("prepared")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("still retrying after the pod's context ended")
	}
}

// An image that cannot be had does not fail the pod, as with the kubelet: the container waits
// with ErrImageNeverPull (ErrImagePull for a pull), then ImagePullBackOff, and is tried again,
// and it starts once the image is there.
func TestPrepareContainer_ImageFailureBacksOff(t *testing.T) {
	useConfig(t, &fakeConfig{})
	old := backOffUnit
	backOffUnit = 50 * time.Millisecond
	t.Cleanup(func() { backOffUnit = old })
	h, _ := handlerFor(t, corev1.Container{Name: "app"})
	spec := &h.Pod.Spec.Containers[0]
	spec.Image = filepath.Join(t.TempDir(), "late.sif")
	done := make(chan bool, 1)
	go func() { _, ok := h.prepareContainer(context.Background(), spec, false, 0); done <- ok }()
	eventually(t, "the container to wait for its image", func() bool {
		w := containerState(h.lifecycle, "app").State.Waiting
		return w != nil && w.Reason == "ErrImageNeverPull"
	})
	eventually(t, "the container to back off", func() bool {
		w := containerState(h.lifecycle, "app").State.Waiting
		return w != nil && w.Reason == "ImagePullBackOff"
	})
	if phase := h.lifecycle.GetPodStatus().Phase; phase == corev1.PodFailed {
		t.Fatal("an image that is not there failed the pod")
	}
	if err := os.WriteFile(spec.Image, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the container was not prepared once its image was there")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still backing off after the image appeared")
	}
}

// The back-off of restarts: the first at once, then the unit doubling to its maximum, and
// again from the start after a long run.
func TestBackOff(t *testing.T) {
	old := backOffUnit
	backOffUnit = time.Second
	t.Cleanup(func() { backOffUnit = old })
	b := restartBackOff()
	var got []time.Duration
	for i := 0; i < 8; i++ {
		got = append(got, b.delay(0))
	}
	want := []time.Duration{0, 1, 2, 4, 8, 16, 30, 30}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("delays %v, want %v seconds", got, want)
		}
	}
	if d := b.delay(backOffResetAfter()); d != 0 {
		t.Fatalf("after a long run the pause is %v, want 0", d)
	}
	if d := pullBackOff().delay(0); d != time.Second {
		t.Fatalf("the first pull retry waits %v, want the unit", d)
	}
}
