package provider

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/virtual-kubelet/virtual-kubelet/node"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	PodHandler "skifflet/internal/compute/podhandler"
)

const updateNode = "node-1"

func runningPod(name, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID("uid-" + name), Labels: map[string]string{"app": name}},
		Spec: corev1.PodSpec{
			NodeName:   updateNode,
			Containers: []corev1.Container{{Name: "app", Image: image}},
		},
	}
}

func TestUnappliedUpdates(t *testing.T) {
	deadline := int64(30)
	base := runningPod("p", "app:1")
	same := base.DeepCopy()
	same.Labels["extra"] = "yes"
	same.Annotations = map[string]string{"a": "b"}
	same.Spec.Tolerations = []corev1.Toleration{{Key: "x"}}
	if got := PodHandler.UnappliedUpdates(base, same); len(got) != 0 {
		t.Errorf("labels, annotations and tolerations are not unapplied changes: %v", got)
	}
	for name, tc := range map[string]struct {
		change func(*corev1.Pod)
		want   string
	}{
		"image": {func(p *corev1.Pod) { p.Spec.Containers[0].Image = "app:2" }, `spec.containers[app].image is "app:2" but the container runs "app:1"`},
		"resources": {func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
		}, "spec.containers[app].resources"},
		"deadline": {func(p *corev1.Pod) { p.Spec.ActiveDeadlineSeconds = &deadline }, "spec.activeDeadlineSeconds"},
		"ephemeral": {func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"}}}
		}, "ephemeral containers are not supported"},
	} {
		changed := base.DeepCopy()
		tc.change(changed)
		got := PodHandler.UnappliedUpdates(base, changed)
		if len(got) != 1 || !strings.Contains(got[0], tc.want) {
			t.Errorf("%s: %v, want one entry containing %q", name, got, tc.want)
		}
	}
}

// updateRig is Virtual Kubelet's real pod controller, over a fake API server, driving a
// real provider whose pod already runs: only the update path is exercised.
type updateRig struct {
	vk      *VirtualK8S
	owner   *PodHandler.PodLifecycle
	api     *fake.Clientset
	events  *record.FakeRecorder
	created *corev1.Pod
}

func newUpdateRig(t *testing.T, pod *corev1.Pod) *updateRig {
	t.Helper()
	// The runtime's own events (UpdateNotApplied) and the controller's go to one place.
	events := record.NewFakeRecorder(100)
	oldEvents := compute.Events
	compute.Events = events
	t.Cleanup(func() { compute.Events = oldEvents })

	vk := &VirtualK8S{Logger: logr.Discard()}
	vk.NodeName = updateNode
	owner := PodHandler.NewPodLifecycle(pod.DeepCopy(), endpoint.PodPath(filepath.Join(t.TempDir(), "pod")), nil)
	owner.InitializeStatuses()
	PodHandler.GlobalRegistry.Register(owner)
	t.Cleanup(func() { PodHandler.GlobalRegistry.Delete(pod.UID) })
	key := client.ObjectKeyFromObject(pod)
	vk.pods.Store(key, owner.GetPod())
	vk.activeUIDs.Store(key, pod.UID)

	api := fake.NewSimpleClientset(pod.DeepCopy())
	factory := informers.NewSharedInformerFactory(api, 0)
	pc, err := node.NewPodController(node.PodControllerConfig{
		PodClient:                 api.CoreV1(),
		PodInformer:               factory.Core().V1().Pods(),
		EventRecorder:             NewUpdateEventFilter(events, vk),
		Provider:                  vk,
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
	go func() { _ = pc.Run(ctx, 1) }()
	t.Cleanup(cancel)
	select {
	case <-pc.Ready():
	case <-time.After(20 * time.Second):
		t.Fatal("the pod controller did not start")
	}
	return &updateRig{vk: vk, owner: owner, api: api, events: events, created: pod}
}

// change patches the pod in the API, as a user (or a subresource) would.
func (r *updateRig) change(t *testing.T, mutate func(*corev1.Pod)) {
	t.Helper()
	pod, err := r.api.CoreV1().Pods("default").Get(context.Background(), r.created.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mutate(pod)
	if _, err := r.api.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// drain returns the events recorded so far, after giving the controller a moment.
func (r *updateRig) drain(settle time.Duration) []string {
	time.Sleep(settle)
	var out []string
	for {
		select {
		case e := <-r.events.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func (r *updateRig) waitEvent(t *testing.T, contains string, timeout time.Duration) []string {
	t.Helper()
	var seen []string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		seen = append(seen, r.drain(100*time.Millisecond)...)
		for _, e := range seen {
			if strings.Contains(e, contains) {
				return seen
			}
		}
	}
	t.Fatalf("no event containing %q; events so far: %v", contains, seen)
	return nil
}

// failedStatusUpdates counts the times the controller wrote a provider-failure status
// to the API server: what an error from UpdatePod would have made it do.
func (r *updateRig) failedStatusUpdates() int {
	count := 0
	for _, action := range r.api.Actions() {
		if u, ok := action.(clienttesting.UpdateAction); ok && u.GetSubresource() == "status" {
			if pod, ok := u.GetObject().(*corev1.Pod); ok && pod.Status.Reason == "ProviderFailed" {
				count++
			}
		}
	}
	return count
}

func has(events []string, contains string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e, contains) {
			n++
		}
	}
	return n
}

// The whole path a real update takes: the API changes the pod, Virtual Kubelet's
// informer and controller notice, and call the provider.
func TestControllerUpdates_MetadataIsAppliedAndTheRestIsReported(t *testing.T) {
	rig := newUpdateRig(t, runningPod("web", "app:1"))
	rig.drain(500 * time.Millisecond) // the initial sync

	// 1. Labels and annotations: applied, reported as a successful update.
	rig.change(t, func(p *corev1.Pod) {
		p.Labels["tier"] = "front"
		p.Annotations = map[string]string{"note": "hello"}
	})
	events := rig.waitEvent(t, providerUpdateSuccess, 10*time.Second)
	if has(events, "UpdateNotApplied") != 0 {
		t.Fatalf("a metadata change was reported as not applied: %v", events)
	}
	if got := rig.owner.GetPod(); got.Labels["tier"] != "front" || got.Annotations["note"] != "hello" {
		t.Fatalf("metadata not applied: %v %v", got.Labels, got.Annotations)
	}

	// 2. A new image: not applied, said so once, the success event dropped, the pod not
	// declared failed, and the running spec untouched.
	rig.change(t, func(p *corev1.Pod) { p.Spec.Containers[0].Image = "app:2" })
	events = rig.waitEvent(t, "UpdateNotApplied", 10*time.Second)
	if !strings.Contains(strings.Join(events, "\n"), `image is "app:2" but the container runs "app:1"`) {
		t.Fatalf("the event does not say what was asked and what runs: %v", events)
	}
	events = append(events, rig.drain(500*time.Millisecond)...)
	if has(events, providerUpdateSuccess) != 0 {
		t.Fatalf("an update that changed nothing was announced as successful: %v", events)
	}
	if got := rig.owner.GetPod().Spec.Containers[0].Image; got != "app:1" {
		t.Fatalf("the running pod's image is %q; an unapplied spec was recorded as the running one", got)
	}
	if rig.failedStatusUpdates() != 0 {
		t.Fatal("the controller declared a healthy pod failed")
	}
	if phase := rig.owner.GetPodStatus().Phase; phase == corev1.PodFailed {
		t.Fatalf("phase %s", phase)
	}

	// 3. The pod changes again for another reason: the same difference is not reported again,
	// but the loop of updates does not begin either.
	rig.change(t, func(p *corev1.Pod) { p.Annotations["second"] = "change" })
	events = rig.drain(1500 * time.Millisecond)
	if has(events, "UpdateNotApplied") != 0 {
		t.Fatalf("the same unapplied change was reported again: %v", events)
	}
	if rig.failedStatusUpdates() != 0 {
		t.Fatal("the controller declared a healthy pod failed")
	}

	// 4. Other changes are reported as they come: a deadline and an ephemeral container.
	deadline := int64(30)
	rig.change(t, func(p *corev1.Pod) { p.Spec.ActiveDeadlineSeconds = &deadline })
	rig.waitEvent(t, "activeDeadlineSeconds", 10*time.Second)
	rig.change(t, func(p *corev1.Pod) {
		p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "busybox"}}}
	})
	rig.waitEvent(t, "ephemeral containers are not supported", 10*time.Second)
	if rig.failedStatusUpdates() != 0 {
		t.Fatal("the controller declared a healthy pod failed")
	}
	if got := rig.owner.GetPod(); len(got.Spec.EphemeralContainers) != 0 || got.Spec.ActiveDeadlineSeconds != nil {
		t.Fatalf("unapplied spec recorded as running: %+v", got.Spec)
	}
}

// A change that is undone leaves nothing to report, and later changes are reported again.
func TestControllerUpdates_ReportsAgainAfterTheDifferenceWasGone(t *testing.T) {
	rig := newUpdateRig(t, runningPod("web2", "app:1"))
	rig.drain(500 * time.Millisecond)
	rig.change(t, func(p *corev1.Pod) { p.Spec.Containers[0].Image = "app:2" })
	rig.waitEvent(t, "UpdateNotApplied", 10*time.Second)
	rig.change(t, func(p *corev1.Pod) { p.Spec.Containers[0].Image = "app:1" }) // back to what runs
	rig.change(t, func(p *corev1.Pod) { p.Labels["x"] = "y" })                  // an applied change follows
	rig.waitEvent(t, providerUpdateSuccess, 10*time.Second)
	rig.change(t, func(p *corev1.Pod) { p.Spec.Containers[0].Image = "app:3" })
	events := rig.waitEvent(t, "UpdateNotApplied", 10*time.Second)
	if !strings.Contains(strings.Join(events, "\n"), `"app:3"`) {
		t.Fatalf("the new difference was not reported: %v", events)
	}
}
