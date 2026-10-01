package provider

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

// slowDeletion is the real provider in front of Virtual Kubelet's real pod controller,
// except that deleting some pods takes as long as a PreStop hook that uses up its
// grace period: DeletePod does not return until the test lets it. Creation is only
// recorded; what is under test is whether the controller keeps serving other pods
// while a deletion is in progress.
type slowDeletion struct {
	*VirtualK8S

	mu       sync.Mutex
	deleting map[string]chan struct{} // pod name -> released by the test
	started  chan string              // a deletion began
	created  chan string              // a creation reached the provider
}

func (s *slowDeletion) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	s.mu.Lock()
	gate := s.deleting[pod.Name]
	s.mu.Unlock()
	s.started <- pod.Name
	select {
	case <-gate:
	case <-ctx.Done():
	}
	return s.VirtualK8S.DeletePod(ctx, pod)
}

func (s *slowDeletion) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	s.created <- pod.Name
	return nil
}

func terminatingPod(name string) *corev1.Pod {
	pod := runningPod(name, "app:1")
	now := metav1.Now()
	grace := int64(30)
	pod.DeletionTimestamp = &now
	pod.DeletionGracePeriodSeconds = &grace
	pod.Status.Phase = corev1.PodRunning
	// Virtual Kubelet only asks the provider to delete a pod that still has a container running.
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: now}}}}
	return pod
}

// While pods are being deleted, a pod that arrives is created at once. The number of
// deletions that may be in progress at the same time is the number of pod sync workers
// minus the one that serves everything else: the supported concurrency of deletions.
func TestControllerCreatesPodsWhileOthersAreBeingDeleted(t *testing.T) {
	// At least a handful, and at most what leaves one worker for everything else.
	deletions := max(DefaultPodSyncWorkers-1, 3)
	api := fake.NewSimpleClientset()
	slow := &slowDeletion{
		VirtualK8S: &VirtualK8S{Logger: logr.Discard()},
		deleting:   map[string]chan struct{}{},
		started:    make(chan string, 64),
		created:    make(chan string, 64),
	}
	slow.NodeName = updateNode

	var terminating []*corev1.Pod
	for i := 0; i < deletions; i++ {
		pod := terminatingPod("old-" + string(rune('a'+i)))
		slow.deleting[pod.Name] = make(chan struct{})
		terminating = append(terminating, pod)
		if _, err := api.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	factory := informers.NewSharedInformerFactory(api, 0)
	pc, err := node.NewPodController(node.PodControllerConfig{
		PodClient:                 api.CoreV1(),
		PodInformer:               factory.Core().V1().Pods(),
		EventRecorder:             record.NewFakeRecorder(1000),
		Provider:                  slow,
		ConfigMapInformer:         factory.Core().V1().ConfigMaps(),
		SecretInformer:            factory.Core().V1().Secrets(),
		ServiceInformer:           factory.Core().V1().Services(),
		SkipDownwardAPIResolution: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	go func() { _ = pc.Run(ctx, DefaultPodSyncWorkers) }()
	t.Cleanup(func() {
		for _, gate := range slow.deleting {
			select {
			case <-gate:
			default:
				close(gate)
			}
		}
		cancel()
	})
	select {
	case <-pc.Ready():
	case <-time.After(20 * time.Second):
		t.Fatal("the pod controller did not start")
	}

	// Every deletion is in progress and none has finished.
	for range terminating {
		select {
		case <-slow.started:
		case <-time.After(10 * time.Second):
			t.Fatal("a deletion did not start")
		}
	}

	// An unrelated pod is created and its creation reaches the provider while they run.
	fresh := runningPod("fresh", "app:2")
	if _, err := api.CoreV1().Pods("default").Create(context.Background(), fresh, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case name := <-slow.created:
		if name != "fresh" {
			t.Fatalf("the provider was asked to create %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("a new pod was not created while %d pods were being deleted: the controller is held up by the deletions", deletions)
	}

	// Releasing the deletions lets each one finish through the real provider.
	for _, pod := range terminating {
		close(slow.deleting[pod.Name])
	}
}
