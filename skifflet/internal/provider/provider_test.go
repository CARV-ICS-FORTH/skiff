package provider

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	PodHandler "skifflet/internal/compute/podhandler"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconcileNonTerminalPodsNotifiesOnlyOnChange(t *testing.T) {
	tmpDir := t.TempDir()
	compute.Skiff = endpoint.SkiffWithPods(tmpDir, filepath.Join(tmpDir, ".skiff", ".pods"))

	podKey := client.ObjectKey{Namespace: "default", Name: "test-pod"}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: podKey.Namespace,
			Name:      podKey.Name,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "c1", Image: "alpine"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "c1",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
					},
				},
			},
		},
	}

	podDir := compute.Skiff.Pod(podKey)
	_ = os.MkdirAll(podDir.JobDir(), 0755)

	var notifyCount int
	vk := &VirtualK8S{
		updatedPod: func(p *corev1.Pod) {
			notifyCount++
		},
	}
	vk.pods.Store(podKey, pod)

	// First call - PodHandler.UpdateStatusFromRuntime will calculate status from runtime
	vk.reconcileNonTerminalPods()

	// Reconcile automatically persisted the updated status, so next reconcile has matching status.
	notifyCount = 0

	// Second reconcile without any runtime changes should NOT trigger updatedPod
	vk.reconcileNonTerminalPods()

	if notifyCount != 0 {
		t.Fatalf("expected 0 notifications when pod status did not change, got %d", notifyCount)
	}

	// Now simulate a change in runtime (e.g. container terminated)
	podCopy := pod.DeepCopy()
	PodHandler.SetContainerTerminated(&podCopy.Status.ContainerStatuses[0], 0)
	vk.pods.Store(podKey, podCopy)

	vk.reconcileNonTerminalPods()

	if notifyCount != 1 {
		t.Fatalf("expected 1 notification when pod status changed, got %d", notifyCount)
	}
}

func TestNodeNameFiltering(t *testing.T) {
	tmpDir := t.TempDir()
	compute.Skiff = endpoint.SkiffWithPods(tmpDir, filepath.Join(tmpDir, ".skiff", ".pods"))

	podKeyA := client.ObjectKey{Namespace: "default", Name: "pod-a"}
	podA := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: podKeyA.Namespace,
			Name:      podKeyA.Name,
		},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{
				{Name: "c1", Image: "alpine"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	podKeyB := client.ObjectKey{Namespace: "default", Name: "pod-b"}
	podB := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: podKeyB.Namespace,
			Name:      podKeyB.Name,
		},
		Spec: corev1.PodSpec{
			NodeName: "node-b",
			Containers: []corev1.Container{
				{Name: "c1", Image: "alpine"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	vk := &VirtualK8S{
		InitConfig: InitConfig{
			NodeName: "node-a",
		},
	}

	for _, pod := range []*corev1.Pod{podA, podB} {
		podKey := client.ObjectKeyFromObject(pod)
		podDir := compute.Skiff.Pod(podKey)
		_ = os.MkdirAll(podDir.JobDir(), 0755)
		vk.pods.Store(podKey, pod)
	}

	// 1. GetPods should return only podA
	pods, err := vk.GetPods(context.Background())
	if err != nil {
		t.Fatalf("GetPods failed: %v", err)
	}
	if len(pods) != 1 || pods[0].Name != "pod-a" {
		t.Fatalf("expected only pod-a in GetPods, got %d pods", len(pods))
	}

	// 2. DeletePod on podB (owned by node-b) should skip deletion without error or removing directory
	err = vk.DeletePod(context.Background(), podB)
	if err != nil {
		t.Fatalf("DeletePod failed: %v", err)
	}
	podBDir := compute.Skiff.Pod(podKeyB)
	if _, statErr := os.Stat(string(podBDir)); os.IsNotExist(statErr) {
		t.Fatalf("pod-b directory was deleted by node-a's DeletePod pass")
	}

	// 3. Reconcile should not trigger notifications for pod-b
	var notifiedPods []string
	vk.updatedPod = func(p *corev1.Pod) {
		notifiedPods = append(notifiedPods, p.Name)
	}
	vk.reconcileNonTerminalPods()

	for _, name := range notifiedPods {
		if name == "pod-b" {
			t.Fatalf("reconcileNonTerminalPods processed pod-b which belongs to node-b")
		}
	}
}

func TestGetContainerLogs_OptionsAndErrors(t *testing.T) {
	tmpDir := t.TempDir()
	compute.Skiff = endpoint.SkiffWithPods(tmpDir, filepath.Join(tmpDir, ".skiff", ".pods"))

	podKey := client.ObjectKey{Namespace: "default", Name: "log-pod"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: podKey.Namespace,
			Name:      podKey.Name,
			UID:       "log-generation",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "alpine"},
			},
		},
	}

	podDir := compute.Skiff.PodWithUID(podKey, pod.GetUID())
	_ = os.MkdirAll(podDir.LogDir(), 0755)
	logFile := podDir.Container("app").LogsPath()
	_ = os.WriteFile(logFile, []byte("line1\nline2\nline3\nline4\n"), 0644)

	vk := &VirtualK8S{}
	vk.pods.Store(podKey, pod)

	// 1. Missing pod returns NotFound
	_, err := vk.GetContainerLogs(context.Background(), "default", "nonexistent-pod", "app", vkapi.ContainerLogOpts{})
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("expected NotFound for missing pod, got: %v", err)
	}

	// 2. Missing container returns NotFound
	_, err = vk.GetContainerLogs(context.Background(), "default", "log-pod", "nonexistent-container", vkapi.ContainerLogOpts{})
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("expected NotFound for missing container, got: %v", err)
	}

	// 3. Tail 0 returns all lines
	rc, err := vk.GetContainerLogs(context.Background(), "default", "log-pod", "app", vkapi.ContainerLogOpts{Tail: 0})
	if err != nil {
		t.Fatalf("unexpected error with Tail 0: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	expectedAll := "line1\nline2\nline3\nline4\n"
	if string(data) != expectedAll {
		t.Fatalf("expected %q for Tail 0, got %q", expectedAll, string(data))
	}

	// 4. Tail 2 returns last 2 lines
	rc, err = vk.GetContainerLogs(context.Background(), "default", "log-pod", "app", vkapi.ContainerLogOpts{Tail: 2})
	if err != nil {
		t.Fatalf("unexpected error with Tail 2: %v", err)
	}
	defer rc.Close()
	data, _ = io.ReadAll(rc)
	expectedTail := "line3\nline4\n"
	if string(data) != expectedTail {
		t.Fatalf("expected %q for Tail 2, got %q", expectedTail, string(data))
	}

	// 5. Follow streams new data until context cancellation
	ctx, cancel := context.WithCancel(context.Background())
	rc, err = vk.GetContainerLogs(ctx, "default", "log-pod", "app", vkapi.ContainerLogOpts{Follow: true, Tail: 1})
	if err != nil {
		t.Fatalf("unexpected error with Follow: %v", err)
	}
	defer rc.Close()

	// Initial tail 1 line should be available
	buf := make([]byte, 64)
	n, err := rc.Read(buf)
	if err != nil {
		t.Fatalf("failed to read initial follow byte: %v", err)
	}
	if !bytes.Contains(buf[:n], []byte("line4")) {
		t.Fatalf("expected initial line4, got %q", string(buf[:n]))
	}

	// Cancel context to stop follow
	cancel()
	time.Sleep(50 * time.Millisecond)
	_, _ = rc.Read(buf) // Must unblock without hang
}

func TestProvider_UnsupportedOperations(t *testing.T) {
	vk := &VirtualK8S{}

	// Port-forward must return an error rather than empty success
	err := vk.PortForward(context.Background(), "default", "p1", 8080, nil)
	if err == nil {
		t.Fatalf("expected error from unsupported PortForward, got nil")
	}

	// StatsSummary must return an error
	_, err = vk.GetStatsSummary(context.Background())
	if err == nil {
		t.Fatalf("expected error from unsupported GetStatsSummary, got nil")
	}

	// RunInContainer on missing pod must return NotFound
	err = vk.RunInContainer(context.Background(), "default", "p1", "c1", []string{"ls"}, nil)
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("expected NotFound for RunInContainer on missing pod, got: %v", err)
	}
}
