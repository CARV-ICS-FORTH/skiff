package provider

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	PodHandler "skifflet/internal/compute/podhandler"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
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

func TestProvider_UnsupportedOperations(t *testing.T) {
	vk := &VirtualK8S{}

	// Port-forward must return an error rather than empty success
	err := vk.PortForward(context.Background(), "default", "p1", 8080, nil)
	if err == nil {
		t.Fatalf("expected error from unsupported PortForward, got nil")
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

func TestProvider_ReconcileConcurrentReadersAndStaleSnapshot(t *testing.T) {
	tmpDir := t.TempDir()
	compute.Skiff = endpoint.SkiffWithPods(tmpDir, filepath.Join(tmpDir, ".skiff", ".pods"))
	key := client.ObjectKey{Namespace: "default", Name: "snapshot-pod"}
	old := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, UID: "old"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "alpine"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	vk := &VirtualK8S{}
	vk.activeUIDs.Store(key, old.UID)
	vk.pods.Store(key, old.DeepCopy())
	stale, _ := vk.pods.Load(key)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			vk.reconcileNonTerminalPods()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = vk.GetPod(context.Background(), key.Namespace, key.Name)
			_, _ = vk.GetPodStatus(context.Background(), key.Namespace, key.Name)
		}
	}()
	wg.Wait()

	replacement := old.DeepCopy()
	replacement.UID = "new"
	vk.stateMu.Lock()
	vk.activeUIDs.Store(key, replacement.UID)
	vk.pods.Store(key, replacement.DeepCopy())
	vk.stateMu.Unlock()
	vk.saveAndNotifyPodExpected("delayed-reconcile", old, stale.(*corev1.Pod))
	current, err := vk.GetPod(context.Background(), key.Namespace, key.Name)
	if err != nil || current.UID != replacement.UID {
		t.Fatalf("stale snapshot affected replacement: pod=%v error=%v", current, err)
	}
}

func TestProvider_ReplacementRejectsDelayedCallbackAndDelete(t *testing.T) {
	root := t.TempDir()
	previousSkiff := compute.Skiff
	compute.Skiff = endpoint.SkiffWithPods(root, filepath.Join(root, ".pods"))
	defer func() { compute.Skiff = previousSkiff }()
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "generation", Name: "replacement", UID: "generation-old"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "unused"}}}}
	key := client.ObjectKeyFromObject(old)
	oldOwner := PodHandler.NewPodLifecycle(old.DeepCopy(), endpoint.PodPath(filepath.Join(root, "old")), nil)
	PodHandler.GlobalRegistry.Register(oldOwner)
	defer PodHandler.GlobalRegistry.Delete(old.UID)
	v := &VirtualK8S{}
	preparationDone := make(chan struct{})
	v.dispatchPreparation = func(run func()) {
		go func() { defer close(preparationDone); run() }()
	}
	v.activeUIDs.Store(key, old.UID)
	v.pods.Store(key, old.DeepCopy())
	lookup, _ := v.pods.Load(key)
	callbackReady, release := make(chan struct{}), make(chan struct{})
	callbackDone := make(chan struct{})
	go func() {
		close(callbackReady)
		<-release
		v.saveAndNotifyPodExpected("delayed-old", old.DeepCopy(), lookup.(*corev1.Pod))
		close(callbackDone)
	}()
	<-callbackReady

	replacement := old.DeepCopy()
	replacement.UID = "generation-new"
	// This actual UpdatePod replacement must reject the unsupported spec before
	// allocation, and must not apply its UID to the registered old execution.
	replacement.Spec.Containers[0].StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
	updated := make(chan struct{}, 1)
	v.updatedPod = func(p *corev1.Pod) {
		if p.UID == replacement.UID {
			updated <- struct{}{}
		}
	}
	if err := v.UpdatePod(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	select {
	case <-updated:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not publish its rejection")
	}
	if oldOwner.GetPod().UID != old.UID || oldOwner.Context().Err() == nil {
		t.Fatal("UpdatePod mutated the old owner or failed to cancel it")
	}
	awaitPreparation(t, preparationDone, "unsupported replacement preparation")
	newOwner := PodHandler.GlobalRegistry.Get(replacement.UID)
	if newOwner == nil {
		t.Fatal("replacement has no lifecycle owner")
	}
	defer newOwner.Terminate(0)
	defer PodHandler.GlobalRegistry.Delete(replacement.UID)
	newCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v.stateMu.Lock()
	v.cancelFuncs.Store(key, context.CancelFunc(cancel))
	v.stateMu.Unlock()
	close(release)
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- v.DeletePod(context.Background(), old.DeepCopy()) }()
	<-callbackDone
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	current, err := v.GetPod(context.Background(), key.Namespace, key.Name)
	if err != nil || current.UID != replacement.UID {
		t.Fatalf("replacement snapshot lost: %v, %v", current, err)
	}
	if _, ok := v.cancelFuncs.Load(key); !ok || newCtx.Err() != nil || PodHandler.GlobalRegistry.GetByKey(key) != newOwner {
		t.Fatal("delayed old work affected replacement cancellation or registry ownership")
	}
}

func TestGetAssignedPods(t *testing.T) {
	vk := &VirtualK8S{}
	if _, err := vk.GetAssignedPods(context.Background()); err == nil {
		t.Fatal("assigned pods must not be reported (as empty) before the informer exists")
	}

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, name := range []string{"a", "b"} {
		if err := indexer.Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	vk.SetAssignedPodLister(corelisters.NewPodLister(indexer))
	// Something the provider runs that the API server did not assign must not leak into the view.
	vk.pods.Store(client.ObjectKey{Namespace: "default", Name: "local-only"}, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "local-only"}})

	pods, err := vk.GetAssignedPods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range pods {
		got[p.Name] = true
	}
	if len(got) != 2 || !got["a"] || !got["b"] {
		t.Fatalf("assigned pods = %v, want a and b from the informer", got)
	}
}
