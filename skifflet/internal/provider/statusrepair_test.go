package provider

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func runningReadyPod(name string, uid types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: name, UID: uid, ResourceVersion: "10"},
		Spec:       corev1.PodSpec{NodeName: "controller"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
			},
		},
	}
}

// markedNotReady is what the node lifecycle controller leaves when the node's heartbeat
// is late: Ready false, everything else as it was, a new version.
func markedNotReady(pod *corev1.Pod, version string) *corev1.Pod {
	api := pod.DeepCopy()
	api.ResourceVersion = version
	for i := range api.Status.Conditions {
		if api.Status.Conditions[i].Type == corev1.PodReady {
			api.Status.Conditions[i].Status = corev1.ConditionFalse
		}
	}
	return api
}

func readyInAPI(t *testing.T, api *fake.Clientset, name string) corev1.ConditionStatus {
	t.Helper()
	pod, err := api.CoreV1().Pods("kube-system").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status
		}
	}
	return ""
}

func TestStatusDifference(t *testing.T) {
	own := runningReadyPod("p", "u").Status
	same := *own.DeepCopy()
	for i := range same.Conditions {
		same.Conditions[i].LastTransitionTime = metav1.NewTime(same.Conditions[i].LastTransitionTime.Truncate(time.Second).Add(-time.Hour))
	}
	same.Conditions = append(same.Conditions, corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue})
	if d := statusDifference(same, own); d != "" {
		t.Fatalf("times and another controller's conditions are not differences, got %q", d)
	}
	if d := statusDifference(markedNotReady(&corev1.Pod{Status: own}, "").Status, own); d == "" {
		t.Fatal("Ready false in the API server and true in Skifflet is a difference")
	}
	pending := *own.DeepCopy()
	pending.Phase = corev1.PodPending
	if d := statusDifference(pending, own); d == "" {
		t.Fatal("another phase is a difference")
	}
	if d := statusDifference(corev1.PodStatus{Phase: corev1.PodRunning}, own); d == "" {
		t.Fatal("a condition missing in the API server is a difference")
	}
}

func TestRepairStatus_PutsBackWhatAnotherWriterChanged(t *testing.T) {
	own := runningReadyPod("coredns", "u1")
	terminalOwn := runningReadyPod("done", "u2")
	terminalOwn.Status.Phase = corev1.PodSucceeded
	replaced := runningReadyPod("replaced", "u3")

	apiPods := []*corev1.Pod{
		markedNotReady(own, "11"),
		markedNotReady(terminalOwn, "21"),
		markedNotReady(runningReadyPod("replaced", "u3-new"), "31"),
	}
	api := fake.NewSimpleClientset(apiPods[0], apiPods[1], apiPods[2])
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, p := range apiPods {
		if err := indexer.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	v := &VirtualK8S{Logger: logr.Discard()}
	v.NodeName = "controller"
	v.SetAssignedPodLister(corelisters.NewPodLister(indexer))
	for _, p := range []*corev1.Pod{own, terminalOwn, replaced} {
		v.pods.Store(client.ObjectKeyFromObject(p), p)
	}
	ctx := context.Background()

	// First look: the difference is noted, nothing written (Virtual Kubelet may have a
	// write queued).
	seen := v.repairStatus(ctx, api.CoreV1(), nil)
	if seen[own.UID] != "11" || len(seen) != 1 {
		t.Fatalf("noted %v, want only the running pod at version 11", seen)
	}
	if got := readyInAPI(t, api, "coredns"); got != corev1.ConditionFalse {
		t.Fatalf("written at the first look: Ready=%s", got)
	}

	// The pod changed in between: the difference is noted again, still not written.
	moved := markedNotReady(own, "12")
	if err := indexer.Update(moved); err != nil {
		t.Fatal(err)
	}
	seen = v.repairStatus(ctx, api.CoreV1(), seen)
	if seen[own.UID] != "12" || readyInAPI(t, api, "coredns") != corev1.ConditionFalse {
		t.Fatalf("a version not seen twice must not be written: noted %v", seen)
	}

	// Same version at the next look: Skifflet's status is written back; the terminal pod
	// and the replaced one (another UID in the API server) are left alone.
	seen = v.repairStatus(ctx, api.CoreV1(), seen)
	if len(seen) != 0 {
		t.Fatalf("still noted after the write: %v", seen)
	}
	if got := readyInAPI(t, api, "coredns"); got != corev1.ConditionTrue {
		t.Fatalf("Ready=%s after the repair, want True", got)
	}
	for _, name := range []string{"done", "replaced"} {
		if got := readyInAPI(t, api, name); got != corev1.ConditionFalse {
			t.Fatalf("%s was written (Ready=%s)", name, got)
		}
	}
}

func TestRepairStatus_NothingBeforeTheInformer(t *testing.T) {
	v := &VirtualK8S{Logger: logr.Discard()}
	v.pods.Store(client.ObjectKey{Namespace: "kube-system", Name: "p"}, runningReadyPod("p", "u"))
	if seen := v.repairStatus(context.Background(), fake.NewSimpleClientset().CoreV1(), nil); len(seen) != 0 {
		t.Fatalf("noted %v without a lister", seen)
	}
}
