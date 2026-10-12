package provider

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	PodHandler "skifflet/internal/compute/podhandler"
	"skifflet/pkg/tmpfs"
)

// The List barrier models descheduling after a successful API response. The
// ConfigMap boundary holds B after registration, before network/process setup.
type initialPreparationClient struct {
	client.Client
	lists            atomic.Int32
	staleAllocations atomic.Int32
	listed           chan context.Context
	resumeA          chan struct{}
	preparingB       chan context.Context
	resumeB          chan struct{}
	panicAfterList   bool
}

func (c *initialPreparationClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if c.lists.Add(1) == 1 {
		c.listed <- ctx
		<-c.resumeA
		if c.panicAfterList {
			panic("controlled preparation panic")
		}
	}
	return nil
}

func (c *initialPreparationClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key.Name == "old-volume" {
		c.staleAllocations.Add(1)
	} else {
		c.preparingB <- ctx
		<-c.resumeB
	}
	// Stop at this boundary; never reach the real network or process launcher.
	return errors.New("controlled volume setup failure")
}

type initialPreparationFixture struct {
	v                  *VirtualK8S
	c                  *initialPreparationClient
	a, b               *corev1.Pod
	done               [2]chan struct{}
	operations         []<-chan struct{}
	releaseA, releaseB sync.Once
}

func awaitPreparation(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func newInitialPreparationFixture(t *testing.T) *initialPreparationFixture {
	t.Helper()
	oldClient, oldSkiff, oldRegistry := compute.K8SClient, compute.Skiff, PodHandler.GlobalRegistry
	oldMount, oldUnmount := PodHandler.MountTmpfs, PodHandler.UnmountTmpfs
	// (the tests run without the authority to mount a tmpfs)
	PodHandler.MountTmpfs = func(string, tmpfs.Options) error { return nil }
	PodHandler.UnmountTmpfs = func(string) error { return nil }
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	f := &initialPreparationFixture{
		v:    &VirtualK8S{},
		c:    &initialPreparationClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), listed: make(chan context.Context, 1), resumeA: make(chan struct{}), preparingB: make(chan context.Context, 1), resumeB: make(chan struct{})},
		done: [2]chan struct{}{make(chan struct{}), make(chan struct{})},
	}
	root := t.TempDir()
	compute.K8SClient = f.c
	compute.Skiff = endpoint.SkiffWithTmp(root, root+"/scratch")
	PodHandler.GlobalRegistry = PodHandler.NewPodRegistry()
	var dispatched int
	f.v.dispatchPreparation = func(run func()) {
		done := f.done[dispatched]
		dispatched++
		go func() { defer close(done); run() }()
	}
	zero := int64(0)
	mode := int32(0600)
	f.a = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a1", Name: "initial", UID: types.UID("old")}, Spec: corev1.PodSpec{
		TerminationGracePeriodSeconds: &zero,
		Containers:                    []corev1.Container{{Name: "app", Image: "unused"}},
		Volumes:                       []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "old-volume"}, DefaultMode: &mode}}}},
	}}
	f.b = f.a.DeepCopy()
	f.b.UID = "new"
	f.b.Spec.Volumes[0].ConfigMap.Name = "new-volume"
	t.Cleanup(func() {
		// Release dependencies and join ALL production work before restoring globals.
		f.v.cancelFuncs.Range(func(_, value any) bool { value.(context.CancelFunc)(); return true })
		f.releaseA.Do(func() { close(f.c.resumeA) })
		f.releaseB.Do(func() { close(f.c.resumeB) })
		for _, done := range f.operations {
			awaitPreparation(t, done, "provider operation cleanup")
		}
		for i := 0; i < dispatched; i++ {
			awaitPreparation(t, f.done[i], "launcher cleanup")
		}
		for _, owner := range PodHandler.GlobalRegistry.All() {
			owner.Terminate(0)
			forgetOwner(owner.UID())
		}
		compute.K8SClient, compute.Skiff, PodHandler.GlobalRegistry = oldClient, oldSkiff, oldRegistry
		PodHandler.MountTmpfs, PodHandler.UnmountTmpfs = oldMount, oldUnmount
	})
	return f
}

func (f *initialPreparationFixture) startA(t *testing.T) context.Context {
	t.Helper()
	if err := f.v.CreatePod(context.Background(), f.a); err != nil {
		t.Fatal(err)
	}
	select {
	case ctx := <-f.c.listed:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("A did not complete service lookup")
		return nil
	}
}

func TestInitialPreparationReplacement(t *testing.T) {
	f := newInitialPreparationFixture(t)
	aCtx := f.startA(t)
	updated := make(chan struct{})
	f.operations = append(f.operations, updated)
	go func() {
		defer close(updated)
		if err := f.v.UpdatePod(context.Background(), f.b); err != nil {
			t.Error(err)
		}
	}()
	var bCtx context.Context
	select {
	case bCtx = <-f.c.preparingB:
	case <-time.After(5 * time.Second):
		t.Fatal("B did not reach preparation boundary")
	}
	awaitPreparation(t, aCtx.Done(), "A revocation")
	key := client.ObjectKeyFromObject(f.b)
	owner := PodHandler.GlobalRegistry.GetByKey(key)
	if owner == nil || owner.UID() != f.b.UID {
		t.Fatal("B owner missing at preparation boundary")
	}
	before, err := f.v.GetPod(context.Background(), key.Namespace, key.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.releaseA.Do(func() { close(f.c.resumeA) })
	select {
	case <-f.done[0]:
	case <-bCtx.Done():
		t.Error("cancelled A preparation cancelled replacement B")
		// Let the broken baseline finish its erroneous B termination before joining.
		f.releaseB.Do(func() { close(f.c.resumeB) })
	case <-time.After(5 * time.Second):
		t.Fatal("A preparation failed to quiesce")
	}
	awaitPreparation(t, f.done[0], "A preparation")
	awaitPreparation(t, updated, "replacement transition")
	if owner.Context().Err() != nil || bCtx.Err() != nil {
		t.Error("B execution was revoked by stale A")
	}
	if PodHandler.GlobalRegistry.GetByKey(key) != owner || PodHandler.GlobalRegistry.Get(f.b.UID) != owner {
		t.Error("stale A displaced or removed B registry owner")
	}
	current, err := f.v.GetPod(context.Background(), key.Namespace, key.Name)
	if err != nil || current.UID != f.b.UID || !reflect.DeepEqual(current.Status, before.Status) {
		t.Errorf("B provider snapshot changed: %v, %v", current, err)
	}
	if f.c.staleAllocations.Load() != 0 {
		t.Error("revoked A reached volume allocation boundary")
	}
	if _, err := os.Stat(compute.Skiff.PodWithUID(key, f.a.UID).String()); !os.IsNotExist(err) {
		t.Errorf("A files survived cleanup: %v", err)
	}
	// A stale API delete must also leave the same B owner active.
	if err := f.v.DeletePod(context.Background(), f.a); err != nil {
		t.Fatal(err)
	}
	if PodHandler.GlobalRegistry.GetByKey(key) != owner || owner.Context().Err() != nil {
		t.Error("stale delete affected B")
	}
}

func TestInitialPreparationDeletion(t *testing.T) {
	f := newInitialPreparationFixture(t)
	aCtx := f.startA(t)
	deleted := make(chan struct{})
	f.operations = append(f.operations, deleted)
	go func() {
		defer close(deleted)
		if err := f.v.DeletePod(context.Background(), f.a); err != nil {
			t.Error(err)
		}
	}()
	awaitPreparation(t, aCtx.Done(), "deletion revocation")
	f.releaseA.Do(func() { close(f.c.resumeA) })
	awaitPreparation(t, f.done[0], "deleted A preparation")
	awaitPreparation(t, deleted, "A deletion")
	key := client.ObjectKeyFromObject(f.a)
	if PodHandler.GlobalRegistry.GetByKey(key) != nil || PodHandler.GlobalRegistry.Get(f.a.UID) != nil {
		t.Error("deleted A resurrected its registry owner")
	}
	if pod, err := f.v.GetPod(context.Background(), key.Namespace, key.Name); err == nil {
		t.Errorf("deleted A resurrected provider snapshot: %v", pod)
	}
	if f.c.staleAllocations.Load() != 0 {
		t.Error("deleted A reached volume allocation boundary")
	}
	if _, err := os.Stat(compute.Skiff.PodWithUID(key, f.a.UID).String()); !os.IsNotExist(err) {
		t.Errorf("deleted A recreated files: %v", err)
	}
}

func TestInitialPreparationDuplicateCreate(t *testing.T) {
	f := newInitialPreparationFixture(t)
	ctx := f.startA(t)
	owner := PodHandler.GlobalRegistry.Get(f.a.UID)
	if owner == nil {
		t.Fatal("owner was not installed before service lookup")
	}
	if err := f.v.CreatePod(context.Background(), f.a.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if PodHandler.GlobalRegistry.Get(f.a.UID) != owner {
		t.Fatal("duplicate create replaced the execution owner")
	}
	deleted := make(chan struct{})
	f.operations = append(f.operations, deleted)
	go func() {
		defer close(deleted)
		if err := f.v.DeletePod(context.Background(), f.a); err != nil {
			t.Error(err)
		}
	}()
	awaitPreparation(t, ctx.Done(), "duplicate owner revocation")
	f.releaseA.Do(func() { close(f.c.resumeA) })
	awaitPreparation(t, f.done[0], "original preparation")
	awaitPreparation(t, deleted, "duplicate deletion")
	if f.c.lists.Load() != 1 || f.c.staleAllocations.Load() != 0 {
		t.Fatal("duplicate create dispatched another preparation")
	}
}

func TestInitialPreparationDeleteBeforeDispatch(t *testing.T) {
	f := newInitialPreparationFixture(t)
	var queued func()
	f.v.dispatchPreparation = func(run func()) { queued = run }
	if err := f.v.CreatePod(context.Background(), f.a); err != nil {
		t.Fatal(err)
	}
	owner := PodHandler.GlobalRegistry.Get(f.a.UID)
	if owner == nil {
		t.Fatal("owner missing before dispatch")
	}
	deleted := make(chan struct{})
	f.operations = append(f.operations, deleted)
	go func() {
		defer close(deleted)
		if err := f.v.DeletePod(context.Background(), f.a); err != nil {
			t.Error(err)
		}
	}()
	awaitPreparation(t, owner.Context().Done(), "owner revocation before dispatch")
	select {
	case <-deleted:
		t.Error("deletion finished before the queued preparation quiesced")
	default:
	}
	queued()
	awaitPreparation(t, deleted, "deletion after queued preparation")
	if f.c.lists.Load() != 0 || f.c.staleAllocations.Load() != 0 {
		t.Fatal("revoked queued preparation entered allocation")
	}
}

func TestInitialPreparationPanicQuiesces(t *testing.T) {
	f := newInitialPreparationFixture(t)
	f.c.panicAfterList = true
	f.startA(t)
	owner := PodHandler.GlobalRegistry.Get(f.a.UID)
	f.releaseA.Do(func() { close(f.c.resumeA) })
	awaitPreparation(t, f.done[0], "panicked preparation")
	current, err := f.v.GetPod(context.Background(), f.a.Namespace, f.a.Name)
	if err != nil || current.Status.Reason != "PanicError" {
		t.Fatalf("panic result missing: %v, %v", current, err)
	}
	deleted := make(chan struct{})
	f.operations = append(f.operations, deleted)
	go func() {
		defer close(deleted)
		if err := f.v.DeletePod(context.Background(), f.a); err != nil {
			t.Error(err)
		}
	}()
	awaitPreparation(t, deleted, "deletion after preparation panic")
	if owner.Context().Err() == nil || PodHandler.GlobalRegistry.Get(f.a.UID) != nil {
		t.Fatal("panicked owner was not cleaned up")
	}
}
