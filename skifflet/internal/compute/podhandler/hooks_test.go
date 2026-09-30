package podhandler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"

	"skifflet/internal/compute"
)

// hookRig is a running attempt whose container has lifecycle hooks. The container
// is a shell script; the hooks run (through a stand-in for nsenter) on the host
// and note what they did in the same log as the container, in order.
type hookRig struct {
	pl     *PodLifecycle
	a      *Attempt
	log    string
	events *record.FakeRecorder
	proc   *fakeProc
}

func (r *hookRig) lines() []string {
	data, _ := os.ReadFile(r.log)
	return strings.Fields(string(data))
}

func (r *hookRig) waitFor(t *testing.T, want string) {
	t.Helper()
	eventually(t, "the log to say "+want, func() bool {
		for _, l := range r.lines() {
			if l == want {
				return true
			}
		}
		return false
	})
}

func (r *hookRig) exited() bool {
	select {
	case <-r.a.done:
		return true
	default:
		return false
	}
}

func (r *hookRig) event(t *testing.T) string {
	t.Helper()
	select {
	case e := <-r.events.Events:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("no event was recorded")
		return ""
	}
}

// newHookRig starts the container script with the given lifecycle. In the script,
// $LOG is the shared log. The container is started but the PostStart hook, if it has
// one, is not run: tests call runPostStart themselves.
func newHookRig(t *testing.T, lifecycle *corev1.Lifecycle, script string) *hookRig {
	t.Helper()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs a Linux proc filesystem")
	}
	fakeNsenter(t)
	// The stand-in container has no main process of its own to kill first, so
	// don't wait for one before the whole group is killed.
	oldGrace := killGrace
	killGrace = 100 * time.Millisecond
	t.Cleanup(func() { killGrace = oldGrace })
	r := &hookRig{log: filepath.Join(t.TempDir(), "log"), events: record.NewFakeRecorder(20)}
	old := compute.Events
	compute.Events = r.events
	t.Cleanup(func() { compute.Events = old })

	r.pl = testLifecycle(t, nil, "app")
	a, err := r.pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	a.spec = &corev1.Container{Name: "app", Image: "unused", Lifecycle: lifecycle}
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "LOG="+r.log)
	if err := a.Start(cmd, gatesOf(a.spec)); err != nil {
		t.Fatal(err)
	}
	go a.Wait(cmd)
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-a.done })
	r.a = a

	// The container as the resolver sees it.
	r.proc = newFakeProc(t)
	r.proc.add(a.pid, procSpec{ppid: 1, ns: map[string]string{"pid": "bubble", "mnt": "bubble", "net": "bubble"}})
	r.proc.add(fakePayload, procSpec{ppid: a.pid, nspid: []int{fakePayload, 1}, start: 555, env: []string{"PATH=" + os.Getenv("PATH"), "LOG=" + r.log},
		ns: map[string]string{"pid": "container", "mnt": "container", "net": "pod"}})
	f, err := os.Open(r.proc.nsFile("net", "pod"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	r.pl.mu.Lock()
	r.pl.sandbox = &SandboxRuntime{NetnsFile: f}
	r.pl.mu.Unlock()
	return r
}

func execHook(script string) *corev1.LifecycleHandler {
	return &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", script}}}
}

// a container that notes its termination signal and exits
const noteTERM = `trap 'echo term >> "$LOG"; exit 0' TERM; while :; do sleep 0.1; done`

// a container that ignores its termination signal
const ignoreTERM = `trap '' TERM; while :; do sleep 0.1; done`

// The hook runs first, then the signal: the application still has all it needs
// while the hook runs.
func TestStop_PreStopRunsBeforeTheSignal(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PreStop: execHook(`echo prestop-start >> "$LOG"; sleep 0.5; echo prestop-end >> "$LOG"`)}, noteTERM)
	time.Sleep(200 * time.Millisecond)
	r.a.Stop(10*time.Second, "test")
	r.waitFor(t, "term")
	if got := strings.Join(r.lines(), " "); got != "prestop-start prestop-end term" {
		t.Fatalf("order: %q", got)
	}
}

// The hook's time comes out of the grace period, and a hook that hangs is cut off
// when the grace period ends; the process then gets the kubelet's minimum before it
// is killed, and nothing the hook started remains.
func TestStop_PreStopUsesUpTheGracePeriod(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	r := newHookRig(t, &corev1.Lifecycle{PreStop: execHook(`echo $$ > ` + pidFile + `; exec sleep 60`)}, ignoreTERM)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	r.a.Stop(2*time.Second, "test")
	<-r.a.done
	took := time.Since(start)
	// 2 s of hook, then 2 s minimum after the signal
	if took < 3500*time.Millisecond || took > 8*time.Second {
		t.Fatalf("the sequence took %v, want about 4s (hook up to the grace period, then the minimum)", took)
	}
	pidGone(t, pidFile)
}

// A hook shorter than the grace period leaves the rest of it to the application.
func TestStop_TimeLeftAfterThePreStopHook(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PreStop: execHook(`sleep 1`)}, ignoreTERM)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	r.a.Stop(5*time.Second, "test")
	<-r.a.done
	if took := time.Since(start); took < 4500*time.Millisecond || took > 5800*time.Millisecond {
		t.Fatalf("took %v: a 1 s hook in a 5 s grace period leaves 4 s (killed at 5 s in all)", took)
	}
}

// With no grace period there is no time for a hook; the process still gets the
// minimum before the kill.
func TestStop_ZeroGraceRunsNoHook(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PreStop: execHook(`echo prestop >> "$LOG"`)}, ignoreTERM)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	r.a.Stop(0, "test")
	<-r.a.done
	if took := time.Since(start); took < minKillGrace-300*time.Millisecond || took > minKillGrace+4*time.Second {
		t.Fatalf("took %v, want about %v", took, minKillGrace)
	}
	for _, l := range r.lines() {
		if l == "prestop" {
			t.Fatal("a hook ran with no grace period")
		}
	}
}

// A hook that fails is reported and does not prevent the termination.
func TestStop_FailingPreStopStillTerminates(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PreStop: execHook(`exit 3`)}, noteTERM)
	time.Sleep(200 * time.Millisecond)
	r.a.Stop(10*time.Second, "test")
	r.waitFor(t, "term")
	if e := r.event(t); !strings.Contains(e, "FailedPreStopHook") || !strings.Contains(e, "exited with code 3") {
		t.Fatalf("event %q", e)
	}
}

// The pod's own deletion cancels the attempts' work, but not the hook that is part
// of deleting: the hook runs, then the signal, all within the grace period.
func TestReleaseLiveResources_RunsPreStopBeforeTheSignal(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PreStop: execHook(`echo prestop >> "$LOG"; sleep 0.3`)}, noteTERM)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	r.pl.ReleaseLiveResources(10 * time.Second)
	r.waitFor(t, "term")
	if got := strings.Join(r.lines(), " "); got != "prestop term" {
		t.Fatalf("order: %q", got)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("the deletion took %v", time.Since(start))
	}
	if !r.exited() {
		t.Fatal("the container was still running after the pod's live resources were released")
	}
}

// A container that has already ended gets no hook when the pod is deleted.
func TestReleaseLiveResources_NoPreStopForAnEndedContainer(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PreStop: execHook(`echo prestop >> "$LOG"`)}, `exit 0`)
	<-r.a.done
	r.pl.ReleaseLiveResources(5 * time.Second)
	for _, l := range r.lines() {
		if l == "prestop" {
			t.Fatal("PreStop ran for a container that had already ended")
		}
	}
}

// A hanging hook cannot stall a deletion beyond its grace period.
func TestReleaseLiveResources_HangingPreStopIsBounded(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PreStop: execHook(`exec sleep 60`)}, ignoreTERM)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	r.pl.ReleaseLiveResources(2 * time.Second)
	if took := time.Since(start); took > 12*time.Second {
		t.Fatalf("the deletion took %v with a 2 s grace period", took)
	}
	if !r.exited() {
		t.Fatal("the container survived its deletion")
	}
}

// Containers are stopped side by side, each by its own sequence, within one grace
// period.
func TestReleaseLiveResources_ContainersStopSideBySide(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PreStop: execHook(`sleep 1`)}, ignoreTERM)
	other, err := r.pl.BeginAttempt("second", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", ignoreTERM)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(cmd, probeGates{}); err != nil {
		t.Fatal(err)
	}
	go other.Wait(cmd)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	r.pl.ReleaseLiveResources(3 * time.Second)
	// side by side: about 3 s (1 s hook + 2 s...) not the sum of the two
	if took := time.Since(start); took > 9*time.Second {
		t.Fatalf("took %v", took)
	}
	if !r.exited() {
		t.Fatal("first container still running")
	}
	select {
	case <-other.done:
	default:
		t.Fatal("second container still running")
	}
}

func TestPreStop_HTTPAndSleepHandlers(t *testing.T) {
	var mu sync.Mutex
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		path = req.URL.RequestURI()
		mu.Unlock()
		f, _ := os.OpenFile(os.Getenv("HOOK_LOG"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
		f.WriteString("http\n")
		f.Close()
	}))
	defer server.Close()
	host, port := splitHostPort(t, server.Listener.Addr().String())

	t.Run("http", func(t *testing.T) {
		r := newHookRig(t, &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
			HTTPGet: &corev1.HTTPGetAction{Host: host, Port: intstr.FromString("admin"), Path: "/drain?now=1"}}}, noteTERM)
		r.a.spec.Ports = []corev1.ContainerPort{{Name: "admin", ContainerPort: int32(port)}}
		t.Setenv("HOOK_LOG", r.log)
		time.Sleep(200 * time.Millisecond)
		r.a.Stop(10*time.Second, "test")
		r.waitFor(t, "term")
		if got := strings.Join(r.lines(), " "); got != "http term" {
			t.Fatalf("order: %q", got)
		}
		mu.Lock()
		defer mu.Unlock()
		if path != "/drain?now=1" {
			t.Fatalf("requested %q", path)
		}
	})

	t.Run("a failing status is a failing hook", func(t *testing.T) {
		r := newHookRig(t, &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
			HTTPGet: &corev1.HTTPGetAction{Host: host, Port: intstr.FromInt(1), Path: "/"}}}, noteTERM)
		time.Sleep(200 * time.Millisecond)
		r.a.Stop(10*time.Second, "test")
		r.waitFor(t, "term")
		if e := r.event(t); !strings.Contains(e, "FailedPreStopHook") {
			t.Fatalf("event %q", e)
		}
	})

	t.Run("sleep", func(t *testing.T) {
		r := newHookRig(t, &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: 1}}}, noteTERM)
		time.Sleep(200 * time.Millisecond)
		start := time.Now()
		r.a.Stop(10*time.Second, "test")
		r.waitFor(t, "term")
		if took := time.Since(start); took < 900*time.Millisecond {
			t.Fatalf("the signal came after %v, before the 1 s sleep ended", took)
		}
	})
}

// PostStart: the container is Waiting until the hook has succeeded, then Running.
// The hook runs alongside the entrypoint, which has already started.
func TestPostStart_ContainerRunsOnceTheHookSucceeded(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PostStart: execHook(`sleep 0.6; echo poststart >> "$LOG"`)}, `echo entrypoint >> "$LOG"; sleep 30`)
	cs := containerState(r.pl, "app")
	if cs.State.Running != nil || cs.State.Waiting == nil || cs.State.Waiting.Reason != "PostStartHookPending" || cs.Ready {
		t.Fatalf("before the hook: %+v ready %v", cs.State, cs.Ready)
	}
	go r.a.runPostStart("", nil)
	time.Sleep(200 * time.Millisecond)
	if containerState(r.pl, "app").State.Running != nil {
		t.Fatal("the container was reported Running while its PostStart hook ran")
	}
	eventually(t, "the container to run", func() bool { return containerState(r.pl, "app").State.Running != nil })
	cs = containerState(r.pl, "app")
	if !cs.Ready || cs.Started == nil || !*cs.Started || cs.ContainerID == "" {
		t.Fatalf("after the hook: %+v", cs)
	}
	if got := strings.Join(r.lines(), " "); got != "entrypoint poststart" {
		t.Fatalf("the hook and the entrypoint: %q", got)
	}
}

// A PostStart hook that fails stops the container, with an event.
func TestPostStart_FailureStopsTheContainer(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PostStart: execHook(`exit 1`)}, `sleep 30`)
	go r.a.runPostStart("", nil)
	select {
	case <-r.a.done:
	case <-time.After(10 * time.Second):
		t.Fatal("a container whose PostStart hook failed was not stopped")
	}
	if e := r.event(t); !strings.Contains(e, "FailedPostStartHook") {
		t.Fatalf("event %q", e)
	}
	if containerState(r.pl, "app").State.Running != nil {
		t.Fatal("a container whose hook failed was reported Running")
	}
}

// A hanging PostStart hook holds the container back, and the pod's deletion still
// ends it promptly.
func TestPostStart_HangingHookDoesNotStallDeletion(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PostStart: execHook(`exec sleep 60`)}, `sleep 30`)
	done := make(chan struct{})
	go func() { r.a.runPostStart("", nil); close(done) }()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	r.pl.ReleaseLiveResources(time.Second)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("deletion took %v", time.Since(start))
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook outlived the pod")
	}
	if containerState(r.pl, "app").State.Running != nil {
		t.Fatal("the container was reported Running")
	}
}

// A hook belongs to its attempt: once the attempt has ended it cannot run in what
// replaced it.
func TestHooks_OldAttemptCannotReachTheNext(t *testing.T) {
	r := newHookRig(t, &corev1.Lifecycle{PreStop: execHook(`echo old-hook >> "$LOG"`)}, `exit 0`)
	<-r.a.done
	next, err := r.pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := sleeper("30")
	if err := next.Start(cmd, probeGates{}); err != nil {
		t.Fatal(err)
	}
	go next.Wait(cmd)
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

	err = r.a.runHandler(r.a.lifeCtx, r.a.spec.Lifecycle.PreStop, r.a.lifeCtx)
	if err == nil {
		t.Fatal("the old attempt's hook ran")
	}
	for _, l := range r.lines() {
		if l == "old-hook" {
			t.Fatal("the hook ran in the next attempt")
		}
	}
}
