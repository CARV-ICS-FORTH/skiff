package podhandler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
)

type claimFixture struct {
	pl     *PodLifecycle
	h      *PodHandler
	events *eventLog
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (e *eventLog) add(reason, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, reason+": "+message)
}

func (e *eventLog) count(reason string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.events {
		if strings.HasPrefix(ev, reason+":") {
			n++
		}
	}
	return n
}

func claimScheme(t *testing.T) *kruntime.Scheme {
	scheme := kruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func boundClaim(volume string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "data"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: volume},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func hostPathVolume(name, path string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PersistentVolumeSpec{
		PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path}}}}
}

// newClaimFixture is a pod with one claim volume, prepared through the real
// preparation code, with the API client given by the test and events captured.
func newClaimFixture(t *testing.T, c client.Client) *claimFixture {
	t.Helper()
	oldClient, oldEvents := compute.K8SClient, podEvents
	compute.K8SClient = c
	events := &eventLog{}
	podEvents = func(_ *corev1.Pod, _, reason, message string) { events.add(reason, message) }
	t.Cleanup(func() { compute.K8SClient, podEvents = oldClient, oldEvents })

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "claims", Namespace: "default", UID: types.UID("uid-" + t.Name())},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			InitContainers: []corev1.Container{{Name: "setup", Image: "unused"}},
			Containers:     []corev1.Container{{Name: "app", Image: "unused"}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}},
		},
	}
	pl := NewPodLifecycle(pod, endpoint.PodPath(filepath.Join(t.TempDir(), "pod")), nil)
	pl.InitializeStatuses()
	if err := os.MkdirAll(pl.PodDir().VolumeDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &PodHandler{Pod: pod, lifecycle: pl, podKey: client.ObjectKeyFromObject(pod), logger: logr.Discard(), podDirectory: pl.PodDir()}
	return &claimFixture{pl: pl, h: h, events: events}
}

func waitingReason(pl *PodLifecycle) string {
	for _, cs := range pl.GetPodStatus().ContainerStatuses {
		if cs.State.Waiting != nil {
			return cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message
		}
	}
	return ""
}

func awaitCondition(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// What a claim lookup says for each state a claim can be in: wait for what may come, fail
// what will not.
func TestPersistentVolumeClaimSource_PendingOrFailed(t *testing.T) {
	scheme := claimScheme(t)
	inside := filepath.Join(compute.Skiff.String(), "volumes", "v")
	block := corev1.PersistentVolumeBlock
	for _, tc := range []struct {
		name    string
		objects []client.Object
		wrap    func(client.WithWatch) client.Client
		pending string // "" means not a pending error
		failure string // for a terminal error
	}{
		{name: "no claim", pending: `persistentvolumeclaim "data" not found`},
		{name: "claim not bound", objects: []client.Object{func() client.Object {
			c := boundClaim("")
			c.Status.Phase = corev1.ClaimPending
			return c
		}()}, pending: `not bound to a volume yet (phase "Pending")`},
		{name: "volume not there", objects: []client.Object{boundClaim("pv-data")}, pending: `persistentvolume "pv-data" of claim "data" not found`},
		{name: "API server unreachable", wrap: func(c client.WithWatch) client.Client {
			return interceptor.NewClient(c, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("connection refused")
			}})
		}, pending: "connection refused"},
		{name: "claim lost its volume", objects: []client.Object{func() client.Object {
			c := boundClaim("pv-data")
			c.Status.Phase = corev1.ClaimLost
			return c
		}()}, failure: "lost its volume"},
		{name: "block volume", objects: []client.Object{func() client.Object {
			c := boundClaim("pv-data")
			c.Spec.VolumeMode = &block
			return c
		}()}, failure: "block volume"},
		{name: "unsupported volume type", objects: []client.Object{boundClaim("pv-data"), &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-data"},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: "nfs", Path: "/"}}}}}, failure: "unsupported PersistentVolume type"},
		{name: "volume outside the locations", objects: []client.Object{boundClaim("pv-data"), hostPathVolume("pv-data", "/mnt/x")}, failure: "outside the locations"},
		{name: "ready", objects: []client.Object{boundClaim("pv-data"), hostPathVolume("pv-data", inside)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()
			var c client.Client = base
			if tc.wrap != nil {
				c = tc.wrap(base)
			}
			fx := newClaimFixture(t, c)
			err := fx.h.PersistentVolumeClaimSource(context.Background(), fx.h.Pod.Spec.Volumes[0])
			var pending *VolumePendingError
			switch {
			case tc.pending != "":
				if !errors.As(err, &pending) || !strings.Contains(err.Error(), tc.pending) {
					t.Fatalf("got %v, want a pending error containing %q", err, tc.pending)
				}
			case tc.failure != "":
				if err == nil || errors.As(err, &pending) || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("got %v, want a failure containing %q", err, tc.failure)
				}
			default:
				if err != nil {
					t.Fatalf("got %v", err)
				}
			}
		})
	}
}

// Through the preparation code: a claim that is bound late leaves the pod waiting, with
// the reason in the containers' status and one FailedMount event (not one per look), and
// the pod goes on as soon as the volume is there. Nothing fails it.
func TestMountVolumes_WaitsForAClaimThatIsBoundLater(t *testing.T) {
	scheme := claimScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	fx := newClaimFixture(t, c)

	done := make(chan bool, 1)
	go func() { done <- fx.h.mountVolumes(context.Background()) }()

	awaitCondition(t, "the wait to be reported", 5*time.Second, func() bool { return fx.events.count("FailedMount") == 1 })
	if got := waitingReason(fx.pl); !strings.HasPrefix(got, "ContainerCreating: MountVolume: volume \"data\": persistentvolumeclaim \"data\" not found") {
		t.Errorf("the containers wait with %q", got)
	}
	if init := fx.pl.GetPodStatus().InitContainerStatuses[0].State.Waiting; init == nil || init.Reason != "ContainerCreating" {
		t.Errorf("the init container's state: %+v", init)
	}
	select {
	case <-done:
		t.Fatal("the pod went on without its volume")
	case <-time.After(2500 * time.Millisecond): // several looks
	}
	if phase := fx.pl.GetPodStatus().Phase; phase != corev1.PodPending {
		t.Fatalf("the pod is %s while it waits", phase)
	}
	if n := fx.events.count("FailedMount"); n != 1 {
		t.Errorf("%d FailedMount events for one reason", n)
	}

	// The claim appears, bound to a volume that appears a moment later: the reason changes, then it works.
	inside := filepath.Join(compute.Skiff.String(), "volumes", "late")
	if err := c.Create(context.Background(), boundClaim("pv-late")); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, "the next reason", 5*time.Second, func() bool { return fx.events.count("FailedMount") == 2 })
	if err := c.Create(context.Background(), hostPathVolume("pv-late", inside)); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatalf("the pod was failed: %s", fx.pl.GetPodStatus().Message)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the pod did not go on after the volume appeared")
	}
	if _, err := os.Lstat(filepath.Join(fx.pl.PodDir().VolumeDir(), "data")); err != nil {
		t.Errorf("the volume is not linked into the pod: %v", err)
	}
	if phase := fx.pl.GetPodStatus().Phase; phase == corev1.PodFailed {
		t.Error("the pod failed")
	}
}

// Deleting or replacing the pod ends the wait at once, without failing anything.
func TestMountVolumes_CancellationEndsTheWait(t *testing.T) {
	fx := newClaimFixture(t, fake.NewClientBuilder().WithScheme(claimScheme(t)).Build())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- fx.h.mountVolumes(ctx) }()
	awaitCondition(t, "the wait to be reported", 5*time.Second, func() bool { return fx.events.count("FailedMount") == 1 })
	start := time.Now()
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a cancelled wait reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait did not end when the pod was cancelled")
	}
	if time.Since(start) > time.Second {
		t.Errorf("cancellation took %v", time.Since(start))
	}
	if phase := fx.pl.GetPodStatus().Phase; phase == corev1.PodFailed {
		t.Error("a cancelled pod was failed")
	}
}

// A volume that will not work fails the pod at once with its reason (no waiting).
func TestMountVolumes_FailsWhatCannotWork(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(claimScheme(t)).WithObjects(boundClaim("pv-data"), hostPathVolume("pv-data", "/mnt/elsewhere")).Build()
	fx := newClaimFixture(t, c)
	start := time.Now()
	if fx.h.mountVolumes(context.Background()) {
		t.Fatal("a volume outside the locations was accepted")
	}
	if time.Since(start) > time.Second {
		t.Errorf("failing took %v", time.Since(start))
	}
	status := fx.pl.GetPodStatus()
	if status.Phase != corev1.PodFailed || status.Reason != "VolumeError" || !strings.Contains(status.Message, "outside the locations a volume may use") {
		t.Errorf("status %s %s: %s", status.Phase, status.Reason, status.Message)
	}
	if fx.events.count("FailedMount") != 0 {
		t.Error("a volume that cannot work was reported as one that is awaited")
	}
}

// A claim mounted read-only is read-only for every mount of it.
func TestClaimReadOnly(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
		{Name: "ro", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c", ReadOnly: true}}},
		{Name: "rw", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}}},
		{Name: "dir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}}}
	for name, want := range map[string]bool{"ro": true, "rw": false, "dir": false, "missing": false} {
		if got := claimReadOnly(pod, name); got != want {
			t.Errorf("claimReadOnly(%s) = %v, want %v", name, got, want)
		}
	}
}
