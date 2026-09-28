package podhandler

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"skifflet/internal/compute/endpoint"
)

func TestPodLifecycle_RaceFree(t *testing.T) {
	tmpDir := t.TempDir()
	podDir := endpoint.PodPath(filepath.Join(tmpDir, "pod"))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-race-pod",
			Namespace: "default",
			UID:       types.UID("uid-12345"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "c1", Image: "alpine"},
				{Name: "c2", Image: "nginx"},
				{Name: "c3", Image: "redis"},
			},
		},
	}

	var notifyCount int
	var notifyMu sync.Mutex
	notifyFn := func(p *corev1.Pod) {
		notifyMu.Lock()
		notifyCount++
		notifyMu.Unlock()
	}

	pl := NewPodLifecycle(pod, podDir, notifyFn)
	pl.RegisterContainer(pod.Spec.Containers[0], false)
	pl.RegisterContainer(pod.Spec.Containers[1], false)
	pl.RegisterContainer(pod.Spec.Containers[2], false)

	var wg sync.WaitGroup

	// Reader goroutines (simulating GetPod and GetPodStatus calls from K8s API)
	for i := 0; i < 5; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = pl.GetPod()
				time.Sleep(1 * time.Millisecond)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = pl.GetPodStatus()
				time.Sleep(1 * time.Millisecond)
			}
		}()
	}

	// Writer goroutines (simulating container starts and exits)
	for i := 0; i < 3; i++ {
		idx := i
		cName := fmt.Sprintf("c%d", idx+1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(5 * time.Millisecond)
			pl.SetContainerRunning(cName, nil, 1000+idx, 50000+uint64(idx))
			time.Sleep(10 * time.Millisecond)
			pl.HandleContainerExit(cName, idx, nil)
		}()
	}

	// API update goroutine (simulating informer updates)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			updated := pod.DeepCopy()
			updated.Labels = map[string]string{"iter": fmt.Sprintf("%d", j)}
			pl.UpdateFromAPI(updated)
			time.Sleep(2 * time.Millisecond)
		}
	}()

	wg.Wait()

	finalPod := pl.GetPod()
	if finalPod.Status.Phase == "" {
		t.Errorf("expected non-empty final phase, got empty")
	}
}

func TestPodLifecycle_DeleteCancelsWork(t *testing.T) {
	tmpDir := t.TempDir()
	podDir := endpoint.PodPath(filepath.Join(tmpDir, "pod"))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cancel-pod",
			Namespace: "default",
			UID:       types.UID("uid-cancel-123"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c1"}},
		},
	}

	pl := NewPodLifecycle(pod, podDir, nil)

	select {
	case <-pl.Context().Done():
		t.Fatal("context should not be done initially")
	default:
	}

	// Terminate should cancel context
	go pl.Terminate(50 * time.Millisecond)

	select {
	case <-pl.Context().Done():
		// Success: context was cancelled
	case <-time.After(1 * time.Second):
		t.Fatal("context was not cancelled upon Terminate")
	}
}

func TestPodLifecycle_SnapshotIsolation(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "snapshots", Namespace: "default", UID: "snapshot-uid"}}
	pl := NewPodLifecycle(pod, endpoint.PodPath(t.TempDir()), nil)
	updated := pod.DeepCopy()
	updated.Labels = map[string]string{"version": "current"}
	pl.UpdateFromAPI(updated)
	updated.Labels["version"] = "external"
	if got := pl.GetPod().Labels["version"]; got != "current" {
		t.Fatalf("API update retained caller's labels: %s", got)
	}
	spec := corev1.Container{Name: "app", Command: []string{"original"}}
	pl.RegisterContainer(spec, false)
	spec.Command[0] = "external"
	first, _ := pl.GetContainer("app")
	first.Spec.Command[0] = "snapshot"
	first.Status.State.Waiting.Reason = "snapshot"
	second, _ := pl.GetContainer("app")
	if second.Spec.Command[0] != "original" || second.Status.State.Waiting.Reason != "ContainerCreating" {
		t.Fatal("container snapshot mutated lifecycle state")
	}
}

func TestPodLifecycle_CancelDuringLaunchPreventsProcessStart(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "launch", Namespace: "default", UID: "launch-uid"}}
	pl := NewPodLifecycle(pod, endpoint.PodPath(filepath.Join(t.TempDir(), "pod")), nil)
	pl.BeginLaunch()
	terminated := make(chan struct{})
	go func() {
		pl.Terminate(0)
		close(terminated)
	}()
	select {
	case <-pl.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("termination did not cancel the launcher")
	}
	if err := pl.StartTrackedProcess(exec.Command("sleep", "30")); err != context.Canceled {
		t.Fatalf("process started after cancellation: %v", err)
	}
	pl.EndLaunch()
	select {
	case <-terminated:
	case <-time.After(2 * time.Second):
		t.Fatal("termination did not wait for launcher quiescence")
	}
}

func TestPodLifecycle_TerminateReapsTrackedProcessOnce(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "default", UID: "running-uid"}}
	pl := NewPodLifecycle(pod, endpoint.PodPath(filepath.Join(t.TempDir(), "pod")), nil)
	pl.BeginLaunch()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := pl.StartTrackedProcess(cmd); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pl.FinishTrackedProcess(cmd.Process.Pid)
		waited <- err
	}()
	pl.EndLaunch()
	pl.Terminate(300 * time.Millisecond)
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("sole process waiter did not complete")
	}
	if pl.SignalTrackedProcess(cmd.Process.Pid, syscall.SIGKILL) {
		t.Fatal("completed attempt remained signalable")
	}
}

func TestPodLifecycle_StartFailureLeavesNoProcessOwnership(t *testing.T) {
	pl := NewPodLifecycle(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "failed-launch"}}, endpoint.PodPath(t.TempDir()), nil)
	pl.BeginLaunch()
	if err := pl.StartTrackedProcess(exec.Command(filepath.Join(t.TempDir(), "missing-launcher"))); err == nil {
		t.Fatal("missing launcher unexpectedly started")
	}
	pl.EndLaunch()
	pl.ReleaseLiveResources(0)
	pl.Terminate(0)
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	if len(pl.attempts) != 0 {
		t.Fatal("failed start retained process ownership")
	}
}

func TestPodRegistry_AntiResurrection(t *testing.T) {
	reg := NewPodRegistry()
	tmpDir := t.TempDir()

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-a",
			Namespace: "default",
			UID:       types.UID("uid-gen-1"),
		},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-a",
			Namespace: "default",
			UID:       types.UID("uid-gen-2"),
		},
	}

	l1 := NewPodLifecycle(pod1, endpoint.PodPath(tmpDir), nil)
	reg.Register(l1)

	if reg.GetByKey(l1.Key()).UID() != types.UID("uid-gen-1") {
		t.Fatalf("expected uid-gen-1 in registry")
	}

	// Registering pod2 with same name but new UID must cancel l1
	l2 := NewPodLifecycle(pod2, endpoint.PodPath(tmpDir), nil)
	reg.Register(l2)

	select {
	case <-l1.Context().Done():
		// Success: l1 was cancelled
	default:
		t.Fatalf("l1 was not cancelled when l2 registered under the same key")
	}

	if reg.GetByKey(l2.Key()).UID() != types.UID("uid-gen-2") {
		t.Fatalf("expected uid-gen-2 in registry after replacement")
	}
}
