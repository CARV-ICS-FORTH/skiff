package podhandler

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/image"
)

// fieldPathRecorder is a fake recorder whose events begin with the container they refer
// to (the field path of the reference).
type fieldPathRecorder struct{ *record.FakeRecorder }

func (r fieldPathRecorder) Eventf(object runtime.Object, eventType, reason, format string, args ...any) {
	if ref, ok := object.(*corev1.ObjectReference); ok {
		format = ref.FieldPath + " " + format
	}
	r.FakeRecorder.Eventf(object, eventType, reason, format, args...)
}

func recordEvents(t *testing.T) *record.FakeRecorder {
	t.Helper()
	events := record.NewFakeRecorder(50)
	old := compute.Events
	compute.Events = fieldPathRecorder{events}
	t.Cleanup(func() { compute.Events = old })
	return events
}

// expectEvents takes the recorded events and matches them, in order, with the patterns.
func expectEvents(t *testing.T, events *record.FakeRecorder, patterns ...string) {
	t.Helper()
	for i, pattern := range patterns {
		select {
		case e := <-events.Events:
			if !regexp.MustCompile("^" + pattern + "$").MatchString(e) {
				t.Fatalf("event %d is %q, want %q", i+1, e, pattern)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("event %d (%q) was not recorded", i+1, pattern)
		}
	}
	select {
	case e := <-events.Events:
		t.Fatalf("unexpected event %q", e)
	default:
	}
}

// A pod's containers have the kubelet's events, in its order and with its wording: the init
// container's image is pulled (Pulling, Pulled with the time it took), the application
// container's is already there (Pulled), and each is Created, then Started as its process
// runs. Each event refers to its container as the kubelet does.
func TestContainerEvents_InitAndApplicationContainer(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "apptainer")
	// `apptainer pull --arch <arch> <file> <ref>` writes the image; a container exits at once.
	script := "#!/bin/sh\nif [ \"$1\" = pull ]; then echo sif > \"$4\"; fi\nexit 0\n"
	if err := os.WriteFile(launcher, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	old := compute.Environment.ApptainerBin
	compute.Environment.ApptainerBin = launcher
	t.Cleanup(func() { compute.Environment.ApptainerBin = old })
	saved := compute.Skiff
	compute.Skiff = endpoint.SkiffWithTmp(root, filepath.Join(root, ".skiff"))
	t.Cleanup(func() { compute.Skiff = saved })
	if err := os.MkdirAll(compute.Skiff.ImageDir(), 0755); err != nil {
		t.Fatal(err)
	}
	events := recordEvents(t)

	const img = "registry.example/team/tool:1.0"
	pod := &corev1.Pod{ObjectMeta: envPod().ObjectMeta, Spec: corev1.PodSpec{
		RestartPolicy:  corev1.RestartPolicyNever,
		InitContainers: []corev1.Container{{Name: "setup", Image: img, ImagePullPolicy: corev1.PullIfNotPresent}},
		Containers:     []corev1.Container{{Name: "app", Image: img, ImagePullPolicy: corev1.PullIfNotPresent}},
	}}
	podDir := endpoint.PodPath(filepath.Join(root, "pod"))
	for _, dir := range []string{podDir.JobDir(), podDir.LogDir()} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	h := newTestHandler(pod, podDir)
	h.lifecycle.InitializeStatuses()
	h.logger = h.lifecycle.logger

	run := func(spec *corev1.Container, init bool) {
		t.Helper()
		c, ok := h.prepareContainer(context.Background(), spec, init, 0)
		if !ok {
			t.Fatalf("container %s was not prepared", spec.Name)
		}
		if exit, result, err := h.runAttempt(&c, spec, init, 0); err != nil || result != attemptExited || exit != 0 {
			t.Fatalf("container %s: exit %d result %v err %v", spec.Name, exit, result, err)
		}
	}
	run(&h.Pod.Spec.InitContainers[0], true)
	run(&h.Pod.Spec.Containers[0], false)

	expectEvents(t, events,
		`Normal Pulling spec.initContainers\{setup\} Pulling image "registry.example/team/tool:1.0"`,
		`Normal Pulled spec.initContainers\{setup\} Successfully pulled image "registry.example/team/tool:1.0" in [0-9.]+m?s`,
		`Normal Created spec.initContainers\{setup\} Created container: setup`,
		`Normal Started spec.initContainers\{setup\} Started container setup`,
		`Normal Pulled spec.containers\{app\} Container image "registry.example/team/tool:1.0" already present on machine`,
		`Normal Created spec.containers\{app\} Created container: app`,
		`Normal Started spec.containers\{app\} Started container app`,
	)
	if _, err := image.ResolveLocal(compute.Skiff.ImageDir(), img); err != nil {
		t.Fatalf("the pulled image is not on the node: %v", err)
	}
}

// A container that cannot be launched has the kubelet's Failed event; a running container
// that is stopped with its pod has Killing.
func TestContainerEvents_FailedLaunchAndStop(t *testing.T) {
	events := recordEvents(t)

	pl := testLifecycle(t, nil, "app")
	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := sleeper("30")
	cmd.Path = "/no/such/apptainer"
	if a.Start(cmd, NoProbes) == nil {
		t.Fatal("a launcher that does not exist started")
	}
	a.Fail(128)
	expectEvents(t, events, `Warning Failed spec.containers\{app\} Error: .*/no/such/apptainer.*`)

	a, err = pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd = sleeper("30")
	if err := a.Start(cmd, NoProbes); err != nil {
		t.Fatal(err)
	}
	go a.Wait(cmd)
	pl.ReleaseLiveResources(time.Second)
	expectEvents(t, events,
		`Normal Created spec.containers\{app\} Created container: app`,
		`Normal Started spec.containers\{app\} Started container app`,
		`Normal Killing spec.containers\{app\} Stopping container app`,
	)
}
