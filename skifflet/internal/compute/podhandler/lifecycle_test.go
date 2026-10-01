package podhandler

import (
	"context"
	"fmt"
	"math/rand"
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

func testLifecycle(t *testing.T, notify func(*corev1.Pod), containers ...string) *PodLifecycle {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "lifecycle", Namespace: "default", UID: types.UID("uid-" + t.Name())},
		Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever},
	}
	for _, name := range containers {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: name, Image: "unused"})
	}
	pl := NewPodLifecycle(pod, endpoint.PodPath(filepath.Join(t.TempDir(), "pod")), notify)
	pl.InitializeStatuses()
	return pl
}

func sleeper(seconds string) *exec.Cmd {
	cmd := exec.Command("sleep", seconds)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func containerState(pl *PodLifecycle, name string) corev1.ContainerStatus {
	for _, cs := range pl.GetPodStatus().ContainerStatuses {
		if cs.Name == name {
			return cs
		}
	}
	return corev1.ContainerStatus{}
}

// runToExit launches one attempt of a container that exits with code and waits for it.
func runToExit(t *testing.T, pl *PodLifecycle, name string, code int) *Attempt {
	t.Helper()
	a, err := pl.BeginAttempt(name, false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code))
	if err := a.Start(cmd, probeGates{}); err != nil {
		t.Fatal(err)
	}
	if got := a.Wait(cmd); got != code {
		t.Fatalf("exit code %d, want %d", got, code)
	}
	return a
}

func TestPodLifecycle_RaceFree(t *testing.T) {
	var notifyMu sync.Mutex
	notified := 0
	pl := testLifecycle(t, func(*corev1.Pod) { notifyMu.Lock(); notified++; notifyMu.Unlock() }, "c1", "c2", "c3")

	var wg sync.WaitGroup
	// Readers, as the provider's GetPod and GetPodStatus.
	for i := 0; i < 5; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = pl.GetPod()
				time.Sleep(time.Millisecond)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = pl.GetPodStatus()
				time.Sleep(time.Millisecond)
			}
		}()
	}
	// Writers: containers starting and exiting through the production attempt path.
	for i, name := range []string{"c1", "c2", "c3"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runToExit(t, pl, name, i)
		}()
	}
	// Informer updates.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			updated := pl.GetPod()
			updated.Labels = map[string]string{"iter": fmt.Sprintf("%d", j)}
			pl.UpdateFromAPI(updated)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	wg.Wait()

	final := pl.GetPod()
	if final.Status.Phase == "" {
		t.Errorf("expected a phase after the containers exited")
	}
	for i, name := range []string{"c1", "c2", "c3"} {
		cs := containerState(pl, name)
		if cs.State.Terminated == nil || int(cs.State.Terminated.ExitCode) != i {
			t.Errorf("%s: terminated state %+v, want exit %d", name, cs.State, i)
		}
	}
	notifyMu.Lock()
	defer notifyMu.Unlock()
	if notified == 0 {
		t.Error("no status was published")
	}
}

func TestPodLifecycle_DeleteCancelsWork(t *testing.T) {
	pl := testLifecycle(t, nil, "c1")

	select {
	case <-pl.Context().Done():
		t.Fatal("context should not be done initially")
	default:
	}

	go pl.Terminate(50 * time.Millisecond)

	select {
	case <-pl.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("context was not cancelled upon Terminate")
	}
}

// Metadata follows the API, including removals, without disturbing what runs.
func TestPodLifecycle_MetadataUpdatesKeepRuntimeState(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := sleeper("30")
	if err := a.Start(cmd, probeGates{}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		a.Wait(cmd)
	}()
	before := containerState(pl, "app")

	update := func(labels, annotations map[string]string) {
		p := pl.GetPod()
		p.Labels, p.Annotations = labels, annotations
		pl.UpdateFromAPI(p)
	}
	update(map[string]string{"a": "1", "b": "1"}, map[string]string{"x": "1", "y": "1"})
	update(map[string]string{"a": "2"}, map[string]string{"x": "2"}) // change one, remove others
	got := pl.GetPod()
	if len(got.Labels) != 1 || got.Labels["a"] != "2" || len(got.Annotations) != 1 || got.Annotations["x"] != "2" {
		t.Fatalf("labels %v annotations %v: changes and removals were not applied", got.Labels, got.Annotations)
	}
	update(nil, nil)
	if got := pl.GetPod(); len(got.Labels) != 0 || len(got.Annotations) != 0 {
		t.Fatalf("removing all metadata failed: %v %v", got.Labels, got.Annotations)
	}

	// A status publication after a metadata update must not bring the old metadata back.
	update(map[string]string{"kept": "yes"}, nil)
	pl.Update(func(*corev1.PodStatus) {})
	if pl.GetPod().Labels["kept"] != "yes" {
		t.Fatal("a status publication reverted the metadata update")
	}
	after := containerState(pl, "app")
	if after.State.Running == nil || after.ContainerID != before.ContainerID || after.RestartCount != before.RestartCount {
		t.Fatalf("metadata updates disturbed the running container: %+v vs %+v", after, before)
	}
	if a2, ok := pl.RunningAttempt("app"); !ok || a2 != a {
		t.Fatal("metadata updates replaced the attempt")
	}
}

// The spec is what was launched: an updated spec does not become the running configuration.
func TestPodLifecycle_SpecChangesAreNotAppliedFromAPI(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	update := pl.GetPod()
	update.Spec.Containers[0].Image = "other:image"
	update.Labels = map[string]string{"changed": "yes"}
	pl.UpdateFromAPI(update)
	got := pl.GetPod()
	if got.Spec.Containers[0].Image != "unused" || got.Labels["changed"] != "yes" {
		t.Fatalf("image %q labels %v: only metadata may follow the API", got.Spec.Containers[0].Image, got.Labels)
	}
}

func TestPodLifecycle_SnapshotIsolation(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	updated := pl.GetPod()
	updated.Labels = map[string]string{"version": "current"}
	pl.UpdateFromAPI(updated)
	updated.Labels["version"] = "external"
	if got := pl.GetPod().Labels["version"]; got != "current" {
		t.Fatalf("API update retained caller's labels: %s", got)
	}
	first := pl.GetPod()
	first.Labels["version"] = "snapshot"
	first.Status.ContainerStatuses[0].State.Waiting.Reason = "snapshot"
	first.Spec.Containers[0].Image = "snapshot"
	second := pl.GetPod()
	if second.Labels["version"] != "current" || second.Spec.Containers[0].Image != "unused" ||
		second.Status.ContainerStatuses[0].State.Waiting.Reason != "ContainerCreating" {
		t.Fatal("a snapshot mutated lifecycle state")
	}
}

func TestPodLifecycle_PublishesInOrder(t *testing.T) {
	var mu sync.Mutex
	var last string
	// A slow, jittery consumer widens the window in which an unordered publisher
	// would deliver an older snapshot after a newer one.
	pl := testLifecycle(t, func(p *corev1.Pod) {
		time.Sleep(time.Duration(rand.Intn(3)) * 500 * time.Microsecond)
		mu.Lock()
		last = p.Status.HostIP
		mu.Unlock()
	}, "app")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pl.Update(func(s *corev1.PodStatus) { s.HostIP = fmt.Sprintf("update-%d", i) })
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if last != pl.GetPodStatus().HostIP {
		t.Fatalf("the last published status %q is not the current status %q: publication is out of order", last, pl.GetPodStatus().HostIP)
	}
}

func TestPodLifecycle_CancelDuringLaunchPreventsProcessStart(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := a.Start(sleeper("30"), probeGates{}); err != context.Canceled {
		t.Fatalf("process started after cancellation: %v", err)
	}
	if _, err := pl.BeginAttempt("app", false); err != context.Canceled {
		t.Fatalf("a revoked owner began a new attempt: %v", err)
	}
	pl.EndLaunch()
	select {
	case <-terminated:
	case <-time.After(2 * time.Second):
		t.Fatal("termination did not wait for launcher quiescence")
	}
}

func TestPodLifecycle_TerminateReapsAttemptOnce(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	pl.BeginLaunch()
	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := sleeper("30")
	if err := a.Start(cmd, probeGates{}); err != nil {
		t.Fatal(err)
	}
	waited := make(chan int, 1)
	go func() { waited <- a.Wait(cmd) }()
	pl.EndLaunch()
	pl.Terminate(300 * time.Millisecond)
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("sole process waiter did not complete")
	}
	if a.Signal(syscall.SIGKILL) {
		t.Fatal("completed attempt remained signalable")
	}
	if _, running := pl.RunningAttempt("app"); running {
		t.Fatal("completed attempt is still reported running")
	}
}

func TestPodLifecycle_StartFailureLeavesNoProcessOwnership(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	pl.BeginLaunch()
	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(exec.Command(filepath.Join(t.TempDir(), "missing-launcher")), probeGates{}); err == nil {
		t.Fatal("missing launcher unexpectedly started")
	}
	a.Fail(128)
	pl.EndLaunch()
	pl.ReleaseLiveResources(0)
	pl.Terminate(0)
	pl.mu.RLock()
	live := len(pl.live)
	pl.mu.RUnlock()
	if live != 0 {
		t.Fatal("failed start retained process ownership")
	}
	if term := containerState(pl, "app").State.Terminated; term == nil || term.ExitCode != 128 {
		t.Fatalf("failed start was not published as exit 128: %+v", containerState(pl, "app").State)
	}
}

// An old attempt (a restarted container's earlier launch, or a probe still holding
// it) can neither publish status nor signal the process of a newer attempt.
func TestAttempt_SupersededAttemptCannotPublishOrSignal(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	old := runToExit(t, pl, "app", 1)

	next, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != old.ID+1 {
		t.Fatalf("attempt ids %d then %d: not consecutive", old.ID, next.ID)
	}
	cmd := sleeper("30")
	if err := next.Start(cmd, probeGates{readiness: true}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		next.Wait(cmd)
	}()
	if cs := containerState(pl, "app"); cs.RestartCount != 1 || cs.Ready {
		t.Fatalf("second attempt: restarts %d ready %v, want 1 and unready (awaiting its readiness probe)", cs.RestartCount, cs.Ready)
	}

	old.SetReady(true)
	if containerState(pl, "app").Ready {
		t.Fatal("a late readiness result of the old attempt marked the new attempt ready")
	}
	if old.Signal(syscall.SIGKILL) {
		t.Fatal("the old attempt signalled the new attempt's process")
	}
	if syscall.Kill(cmd.Process.Pid, 0) != nil {
		t.Fatal("the new attempt's process was killed by the old attempt")
	}

	next.SetReady(true)
	if !containerState(pl, "app").Ready {
		t.Fatal("the current attempt could not publish readiness")
	}
	if a2, ok := pl.RunningAttempt("app"); !ok || a2 != next {
		t.Fatal("the running attempt is not the newest one")
	}
}

// The controller ignores status updates for a pod the API server already shows as
// Failed, so the container's exit and the pod's failure reason must arrive together.
func TestAttempt_FailPodPublishesContainerAndReasonTogether(t *testing.T) {
	var mu sync.Mutex
	var published []*corev1.Pod
	pl := testLifecycle(t, func(p *corev1.Pod) { mu.Lock(); published = append(published, p); mu.Unlock() }, "app")
	mu.Lock()
	published = nil // ignore the initial publication
	mu.Unlock()

	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	a.FailPod(128, "LogFileError", "cannot open %s", "app.logs")

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("%d publications, want exactly one", len(published))
	}
	got := published[0]
	if got.Status.Phase != corev1.PodFailed || got.Status.Reason != "LogFileError" {
		t.Fatalf("phase %s reason %q, want Failed/LogFileError in the one publication", got.Status.Phase, got.Status.Reason)
	}
	if cs := got.Status.ContainerStatuses[0]; cs.State.Terminated == nil || cs.State.Terminated.ExitCode != 128 {
		t.Fatalf("container state %+v, want terminated with 128", cs.State)
	}
}

func TestAttempt_ReadinessDoesNotOutliveTheProcess(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	a := runToExit(t, pl, "app", 0)
	a.SetReady(true)
	if cs := containerState(pl, "app"); cs.Ready || cs.State.Terminated == nil {
		t.Fatalf("readiness was published for an exited container: %+v", cs)
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

// RunningContainers lists the containers whose process runs, with the process their own
// descend from, and drops one when its process has ended (a reader of usage must not be
// handed a process that is gone).
func TestPodLifecycle_RunningContainers(t *testing.T) {
	pl := testLifecycle(t, nil, "a", "b")
	if got := pl.RunningContainers(); len(got) != 0 {
		t.Fatalf("containers running before any start: %v", got)
	}
	before := time.Now()
	a, err := pl.BeginAttempt("b", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := sleeper("30")
	if err := a.Start(cmd, NoProbes); err != nil {
		t.Fatal(err)
	}
	got := pl.RunningContainers()
	if len(got) != 1 || got[0].Name != "b" || got[0].Attempt != 1 || got[0].Launcher != cmd.Process.Pid || got[0].Init {
		t.Fatalf("running containers %+v, want container b attempt 1 launched as %d", got, cmd.Process.Pid)
	}
	if got[0].Started.Before(before) || got[0].Started.After(time.Now()) {
		t.Errorf("started at %v, want a time between %v and now", got[0].Started, before)
	}
	_ = cmd.Process.Kill()
	a.Wait(cmd)
	if got := pl.RunningContainers(); len(got) != 0 {
		t.Errorf("a container whose process ended is still listed: %+v", got)
	}
}

// Telling the node to stop stops what runs in every pod at once, by the pod's own grace
// period but never longer than the budget: a container that ends at the signal ends at
// once, one that ignores it is killed when its grace period (or the budget) is out, and
// the pods are not deleted.
func TestPodRegistry_DrainAll(t *testing.T) {
	registry := NewPodRegistry()
	start := func(name string, uid types.UID, grace int64, script string) (*PodLifecycle, *exec.Cmd) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: uid},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, TerminationGracePeriodSeconds: &grace,
				Containers: []corev1.Container{{Name: "app", Image: "unused"}}},
		}
		pl := NewPodLifecycle(pod, endpoint.PodPath(filepath.Join(t.TempDir(), name)), nil)
		pl.InitializeStatuses()
		registry.Register(pl)
		a, err := pl.BeginAttempt("app", false)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", script)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := a.Start(cmd, NoProbes); err != nil {
			t.Fatal(err)
		}
		go a.Wait(cmd)
		return pl, cmd
	}
	// Ends at the signal; ignores the signal for a pod whose grace is 1 s; ignores it
	// for a pod whose grace (30 s) is longer than the budget.
	polite, politeCmd := start("polite", "u1", 30, "trap 'exit 0' TERM; sleep 60 & wait")
	stubborn, stubbornCmd := start("stubborn", "u2", 1, "trap '' TERM; while :; do sleep 1; done")
	long, longCmd := start("long", "u3", 30, "trap '' TERM; while :; do sleep 1; done")
	time.Sleep(300 * time.Millisecond) // the shells set their traps

	began := time.Now()
	if n := registry.DrainAll(3 * time.Second); n != 3 {
		t.Fatalf("drained %d pods, want 3", n)
	}
	took := time.Since(began)
	if took > 8*time.Second {
		t.Errorf("draining took %v: a pod was waited for beyond the budget", took)
	}
	for name, cmd := range map[string]*exec.Cmd{"polite": politeCmd, "stubborn": stubbornCmd, "long": longCmd} {
		if err := cmd.Process.Signal(syscall.Signal(0)); err == nil && !cmd.ProcessState.Exited() {
			// the process is gone or a zombie that Wait has reaped by now; give it a moment
			time.Sleep(500 * time.Millisecond)
		}
		if running := len(map[string]*PodLifecycle{"polite": polite, "stubborn": stubborn, "long": long}[name].RunningContainers()); running != 0 {
			t.Errorf("%s still has %d container(s) running after the drain", name, running)
		}
	}
	// The pods are still known (the Kubernetes objects are not deleted).
	if registry.Get("u1") == nil || registry.Get("u2") == nil || registry.Get("u3") == nil {
		t.Error("draining removed pods from the registry")
	}
}
