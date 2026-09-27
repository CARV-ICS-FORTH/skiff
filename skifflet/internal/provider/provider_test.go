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

func TestProvider_UIDIsolation_RaceFree(t *testing.T) {
	podKey := client.ObjectKey{Namespace: "default", Name: "race-uid-pod"}

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: podKey.Namespace,
			Name:      podKey.Name,
			UID:       "uid-generation-1",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c1", Image: "alpine"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: podKey.Namespace,
			Name:      podKey.Name,
			UID:       "uid-generation-2",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c1", Image: "alpine"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	var notifiedUIDs []string
	vk := &VirtualK8S{
		updatedPod: func(p *corev1.Pod) {
			notifiedUIDs = append(notifiedUIDs, string(p.UID))
		},
	}

	// Register pod2 as the current active UID
	vk.activeUIDs.Store(podKey, pod2.UID)
	vk.pods.Store(podKey, pod2.DeepCopy())

	// Stale callbacks from superseded pod1 must be rejected
	vk.saveAndNotifyPod("old_pod_callback", pod1)

	current, err := vk.GetPod(context.Background(), podKey.Namespace, podKey.Name)
	if err != nil {
		t.Fatalf("GetPod failed: %v", err)
	}
	if current.UID != pod2.UID {
		t.Fatalf("stale callback superseded active pod UID: got %s, expected %s", current.UID, pod2.UID)
	}

	// Stale deletion targeting superseded pod1 must be ignored
	err = vk.DeletePod(context.Background(), pod1)
	if err != nil {
		t.Fatalf("DeletePod with old UID returned error: %v", err)
	}

	// Active pod2 must still exist
	current, err = vk.GetPod(context.Background(), podKey.Namespace, podKey.Name)
	if err != nil {
		t.Fatalf("active pod was deleted by stale DeletePod: %v", err)
	}
	if current.UID != pod2.UID {
		t.Fatalf("active pod changed after stale DeletePod: got %s, expected %s", current.UID, pod2.UID)
	}
}

func TestFollowLogReader_CloseUnblocksRead(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")
	if err := os.WriteFile(logPath, []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	reader, err := newFollowLogReader(context.Background(), logPath, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	// Read initial data
	buf := make([]byte, 64)
	n, err := reader.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("expected to read initial bytes, got n=%d, err=%v", n, err)
	}

	// Now reader is at EOF and will wait. Call Close() in background and verify it unblocks Read().
	readDone := make(chan struct{})
	var readErr error
	go func() {
		_, readErr = reader.Read(buf)
		close(readDone)
	}()

	time.Sleep(50 * time.Millisecond)
	if err := reader.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case <-readDone:
		if readErr != io.EOF {
			t.Fatalf("expected io.EOF after Close(), got %v", readErr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("reader.Read() did not unblock after Close()")
	}
}

func TestFollowLogReader_DelayedFileReopen(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "delayed.log")

	// Create reader BEFORE file exists
	reader, err := newFollowLogReader(context.Background(), logPath, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	readDone := make(chan string)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				readDone <- string(buf[:n])
				return
			}
			if err != nil {
				return
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(logPath, []byte("delayed line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	select {
	case data := <-readDone:
		if data != "delayed line\n" {
			t.Fatalf("expected 'delayed line\\n', got %q", data)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("reader did not read delayed file after creation")
	}
}

func TestFollowLogReader_PreservesHistoryWithoutTail(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "history.log")
	if err := os.WriteFile(logPath, []byte("line 1\nline 2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	reader, err := newFollowLogReader(context.Background(), logPath, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	buf := make([]byte, 128)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("failed to read initial history: %v", err)
	}
	if string(buf[:n]) != "line 1\nline 2\n" {
		t.Fatalf("expected 'line 1\\nline 2\\n', got %q", string(buf[:n]))
	}

	// Now append a new line
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("line 3\n")
	_ = f.Close()

	n2, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("failed to read appended line: %v", err)
	}
	if string(buf[:n2]) != "line 3\n" {
		t.Fatalf("expected 'line 3\\n', got %q", string(buf[:n2]))
	}
}

func TestGetContainerLogs_RejectsPrevious(t *testing.T) {
	vk := &VirtualK8S{}
	_, err := vk.GetContainerLogs(context.Background(), "default", "pod-x", "container-y", vkapi.ContainerLogOpts{Previous: true})
	if err == nil || !errdefs.IsInvalidInput(err) {
		t.Fatalf("expected InvalidInput error when Previous=true, got %v", err)
	}
}
