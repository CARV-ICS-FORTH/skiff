package podhandler

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestReadinessControlsPodCondition(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "server",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			}},
		},
	}
	condition := func() corev1.ConditionStatus {
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady {
				return c.Status
			}
		}
		return corev1.ConditionUnknown
	}
	UpdateStatusFromRuntime(pod)
	if pod.Status.Phase != corev1.PodRunning || condition() != corev1.ConditionFalse {
		t.Fatalf("running but unready container must leave Pod Ready=False: %+v", pod.Status)
	}
	pod.Status.ContainerStatuses[0].Ready = true
	UpdateStatusFromRuntime(pod)
	if condition() != corev1.ConditionTrue {
		t.Fatalf("ready container did not make Pod Ready=True: %+v", pod.Status.Conditions)
	}
}

// --- what one check does -------------------------------------------------

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portString, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func httpProbeOf(port int, path string) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(port), Path: path}}}
}

func check(t *testing.T, probe *corev1.Probe, ports []corev1.ContainerPort, podIP string) (bool, string) {
	t.Helper()
	return evaluateProbe(context.Background(), probe, ports, podIP, time.Second, nil)
}

func TestHTTPProbeStatuses(t *testing.T) {
	for status, want := range map[int]bool{200: true, 204: true, 302: true, 399: true, 400: false, 404: false, 500: false, 503: false} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status >= 300 && status < 400 {
					w.Header().Set("Location", "http://other.invalid/") // another host: not followed
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			host, port := splitHostPort(t, server.Listener.Addr().String())
			if got, detail := check(t, httpProbeOf(port, "/healthz"), nil, host); got != want {
				t.Fatalf("status %d: probe result %v (%s), want %v", status, got, detail, want)
			}
		})
	}
}

// The probe uses the pod IP unless the probe names a host, and takes its path
// (with a query) and headers as given: a Host header is the request's host, and the
// kubelet's user agent is used unless overridden.
func TestHTTPProbeRequest(t *testing.T) {
	var mu sync.Mutex
	var seen *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r
		mu.Unlock()
	}))
	defer server.Close()
	host, port := splitHostPort(t, server.Listener.Addr().String())

	probe := httpProbeOf(port, "/ready?full=1")
	probe.HTTPGet.HTTPHeaders = []corev1.HTTPHeader{{Name: "X-Probe", Value: "yes"}, {Name: "Host", Value: "app.example"}}
	if ok, detail := check(t, probe, nil, host); !ok {
		t.Fatal(detail)
	}
	mu.Lock()
	got := seen
	mu.Unlock()
	if got.URL.Path != "/ready" || got.URL.RawQuery != "full=1" {
		t.Errorf("requested %s", got.URL)
	}
	if got.Host != "app.example" {
		t.Errorf("Host %q, want the header's value", got.Host)
	}
	if got.Header.Get("X-Probe") != "yes" {
		t.Errorf("custom header lost: %v", got.Header)
	}
	if ua := got.Header.Get("User-Agent"); ua != probeUserAgent {
		t.Errorf("User-Agent %q", ua)
	}

	// The probe's own host wins over the pod IP.
	probe = httpProbeOf(port, "/")
	probe.HTTPGet.Host = host
	if ok, detail := check(t, probe, nil, "203.0.113.1"); !ok {
		t.Fatalf("probe host ignored: %s", detail)
	}
}

func TestHTTPProbeHTTPSDoesNotVerifyCertificates(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	host, port := splitHostPort(t, server.Listener.Addr().String())
	probe := httpProbeOf(port, "/")
	if ok, _ := check(t, probe, nil, host); ok {
		t.Fatal("a plain HTTP probe succeeded against a TLS server")
	}
	probe.HTTPGet.Scheme = corev1.URISchemeHTTPS
	if ok, detail := check(t, probe, nil, host); !ok {
		t.Fatalf("HTTPS probe failed: %s", detail)
	}
}

// A same-host redirect is followed; a redirect to another host is not, and counts
// as success, so a probe cannot be pointed at another service.
func TestHTTPProbeRedirects(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/new", http.StatusFound)
		case "/new":
			hits.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	host, port := splitHostPort(t, server.Listener.Addr().String())
	if ok, _ := check(t, httpProbeOf(port, "/old"), nil, host); ok || hits.Load() != 1 {
		t.Fatalf("a same-host redirect was not followed to its 503 (hits %d)", hits.Load())
	}
}

// A slow response fails the probe at its timeout instead of holding the checker.
func TestHTTPProbeTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	host, port := splitHostPort(t, server.Listener.Addr().String())
	start := time.Now()
	ok, _ := evaluateProbe(context.Background(), httpProbeOf(port, "/"), nil, host, 100*time.Millisecond, nil)
	if ok || time.Since(start) > 2*time.Second {
		t.Fatalf("slow probe: result %v after %v", ok, time.Since(start))
	}
}

func TestTCPProbe(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitHostPort(t, listener.Addr().String())
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(port)}}}
	if ok, detail := check(t, probe, nil, host); !ok {
		t.Fatalf("open port: %s", detail)
	}
	_ = listener.Close()
	if ok, _ := check(t, probe, nil, host); ok {
		t.Fatal("closed port succeeded")
	}
}

// A named port is looked up among the probed container's ports. A name that the
// container does not declare fails the probe: it is never turned into port 80 or
// into another container's port.
func TestNamedPorts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	host, port := splitHostPort(t, server.Listener.Addr().String())
	ports := []corev1.ContainerPort{{Name: "other", ContainerPort: 1}, {Name: "web", ContainerPort: int32(port)}}

	named := func(name string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromString(name)}}}
	}
	if ok, detail := check(t, named("web"), ports, host); !ok {
		t.Fatalf("declared name: %s", detail)
	}
	tcp := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("web")}}}
	if ok, detail := check(t, tcp, ports, host); !ok {
		t.Fatalf("declared name (TCP): %s", detail)
	}
	// The failure is about the name, not about a connection that was tried anyway.
	if ok, detail := check(t, named("missing"), ports, host); ok || !strings.Contains(detail, "unresolved named port") {
		t.Fatalf("undeclared name: result %v (%q), want an unresolved-name failure", ok, detail)
	}
	if ok, detail := check(t, named("web"), nil, host); ok || !strings.Contains(detail, "unresolved named port") {
		t.Fatalf("a container without ports resolved a name: %v (%q)", ok, detail)
	}
}

func TestResolvePortRange(t *testing.T) {
	for _, n := range []int{0, -1, 65536} {
		if _, err := resolvePort(intstr.FromInt(n), nil); err == nil {
			t.Errorf("port %d accepted", n)
		}
	}
	if n, err := resolvePort(intstr.FromInt(65535), nil); err != nil || n != 65535 {
		t.Errorf("port 65535: %d, %v", n, err)
	}
}

// A network probe without an address never falls back to the node itself.
func TestNetworkProbeWithoutAddressFails(t *testing.T) {
	if ok, _ := check(t, httpProbeOf(80, "/"), nil, ""); ok {
		t.Fatal("an HTTP probe without a pod IP succeeded")
	}
	tcp := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(1)}}}
	if ok, _ := check(t, tcp, nil, ""); ok {
		t.Fatal("a TCP probe without a pod IP succeeded")
	}
}

// Exec probes are run by the runner they are handed (the container's target in
// production): success is what it reports, and an empty command never succeeds.
func TestExecProbeUsesTheRunner(t *testing.T) {
	var got []string
	run := func(succeed bool) execRunner {
		return func(_ context.Context, argv []string, _ time.Duration) (bool, string) { got = argv; return succeed, "" }
	}
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"test", "-f", "/ready"}}}}
	eval := func(r execRunner) bool {
		ok, _ := evaluateProbe(context.Background(), probe, nil, "", time.Second, r)
		return ok
	}
	if !eval(run(true)) || len(got) != 3 || got[0] != "test" {
		t.Fatalf("a succeeding runner did not make the probe succeed (ran %v)", got)
	}
	if eval(run(false)) {
		t.Fatal("a failing runner made the probe succeed")
	}
	if eval(nil) {
		t.Fatal("an exec probe without anything to run it in succeeded")
	}
	probe.Exec.Command = nil
	if eval(run(true)) {
		t.Fatal("an exec probe with no command succeeded")
	}
}

// A container that is not a usable target fails its exec probe instead of running
// the command anywhere else.
func TestExecProbeFailsWhenThereIsNoTarget(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	a, err := pl.BeginAttempt("app", false) // never started: nothing to run in
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.execProbe(context.Background(), []string{"true"}, time.Second); ok {
		t.Fatal("an exec probe succeeded for an attempt without a process")
	}
}

func TestProbeSettingsDefaults(t *testing.T) {
	got := settingsOf(&corev1.Probe{})
	want := probeSettings{period: 10 * time.Second, timeout: time.Second, successThreshold: 1, failureThreshold: 3}
	if got != want {
		t.Fatalf("defaults %+v, want %+v", got, want)
	}
	got = settingsOf(&corev1.Probe{InitialDelaySeconds: 5, PeriodSeconds: 2, TimeoutSeconds: 4, SuccessThreshold: 3, FailureThreshold: 6})
	want = probeSettings{initialDelay: 5 * time.Second, period: 2 * time.Second, timeout: 4 * time.Second, successThreshold: 3, failureThreshold: 6}
	if got != want {
		t.Fatalf("settings %+v, want %+v", got, want)
	}
}

// --- when a probe changes its result -------------------------------------

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// scripted returns a check that answers from a script and then keeps giving the
// last answer.
func scripted(answers ...bool) (probeCheck, *atomic.Int32) {
	var calls atomic.Int32
	return func(context.Context, time.Duration) (bool, string) {
		n := int(calls.Add(1)) - 1
		if n >= len(answers) {
			n = len(answers) - 1
		}
		return answers[n], fmt.Sprintf("answer %d", n)
	}, &calls
}

type change struct {
	result probeResult
	detail string
}

// changesOf polls with the script until it has made n checks and reports the result changes.
func changesOf(t *testing.T, s probeSettings, initial probeResult, answers []bool, checks int) []change {
	t.Helper()
	s.period = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, calls := scripted(answers...)
	var mu sync.Mutex
	var got []change
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		pollProbe(ctx, s, time.Now(), initial, c, func(r probeResult, detail string) bool {
			mu.Lock()
			got = append(got, change{r, detail})
			mu.Unlock()
			return false
		})
	}()
	eventually(t, "the scripted checks", func() bool { return int(calls.Load()) >= checks })
	cancel()
	<-finished
	mu.Lock()
	defer mu.Unlock()
	return got
}

// Readiness starts unready, needs successThreshold successes in a row to become
// ready, and failureThreshold failures in a row to become unready again; a
// success in between restarts the failure count.
func TestPollProbeReadinessThresholds(t *testing.T) {
	s := probeSettings{timeout: time.Second, successThreshold: 2, failureThreshold: 3}
	got := changesOf(t, s, probeFailure, []bool{
		true, false, true, // not two in a row: still unready
		true,               // second in a row: ready
		false, false, true, // two failures, then a success: still ready
		false, false, false, // three failures: unready
		false, false, false, false, // further failures change nothing
	}, 13)
	if len(got) != 2 || got[0].result != probeSuccess || got[1].result != probeFailure {
		t.Fatalf("changes %+v, want ready then unready", got)
	}
	// The failure that caused the change is reported with it.
	if got[1].detail != "answer 9" {
		t.Fatalf("change reported %q", got[1].detail)
	}
}

// Liveness starts healthy and fails only after failureThreshold failures in a row.
func TestPollProbeLivenessThresholds(t *testing.T) {
	s := probeSettings{timeout: time.Second, successThreshold: 1, failureThreshold: 3}
	got := changesOf(t, s, probeSuccess, []bool{false, false, true, false, false, true, true}, 7)
	if len(got) != 0 {
		t.Fatalf("intermittent failures changed the result: %+v", got)
	}
	got = changesOf(t, s, probeSuccess, []bool{true, false, false, false}, 4)
	if len(got) != 1 || got[0].result != probeFailure {
		t.Fatalf("changes %+v, want one failure", got)
	}
}

// Startup has no result until it has succeeded once or failed failureThreshold times.
func TestPollProbeStartup(t *testing.T) {
	s := probeSettings{timeout: time.Second, successThreshold: 1, failureThreshold: 3}
	got := changesOf(t, s, probeUnknown, []bool{false, false, true}, 3)
	if len(got) != 1 || got[0].result != probeSuccess {
		t.Fatalf("two failures then a success: %+v", got)
	}
	got = changesOf(t, s, probeUnknown, []bool{false, false, false}, 3)
	if len(got) != 1 || got[0].result != probeFailure {
		t.Fatalf("three failures: %+v", got)
	}
}

// onResult ending the poll ends it: nothing is checked afterwards.
func TestPollProbeStopsWhenTold(t *testing.T) {
	c, calls := scripted(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pollProbe(context.Background(), probeSettings{period: time.Millisecond, timeout: time.Second, successThreshold: 1, failureThreshold: 1}, time.Now(), probeUnknown, c,
			func(probeResult, string) bool { return true })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("poll did not stop")
	}
	if calls.Load() != 1 {
		t.Fatalf("%d checks, want 1", calls.Load())
	}
}

// The initial delay counts from the start of the container, so time already spent
// (in a startup probe) is not waited again.
func TestPollProbeInitialDelay(t *testing.T) {
	run := func(since time.Time, delay time.Duration) time.Duration {
		started := time.Now()
		var first time.Duration
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pollProbe(ctx, probeSettings{initialDelay: delay, period: time.Hour, timeout: time.Second, successThreshold: 1, failureThreshold: 1}, since, probeUnknown,
			func(context.Context, time.Duration) (bool, string) { first = time.Since(started); return true, "" },
			func(probeResult, string) bool { return true })
		return first
	}
	if got := run(time.Now(), 150*time.Millisecond); got < 140*time.Millisecond {
		t.Fatalf("first check after %v, before the initial delay", got)
	}
	if got := run(time.Now().Add(-time.Hour), 150*time.Millisecond); got > 100*time.Millisecond {
		t.Fatalf("first check after %v although the delay had long passed", got)
	}
}

// A check that hangs is abandoned when the context ends, and its answer is never
// reported.
func TestPollProbeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	var reported atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		pollProbe(ctx, probeSettings{period: time.Millisecond, timeout: time.Hour, successThreshold: 1, failureThreshold: 1}, time.Now(), probeUnknown,
			func(ctx context.Context, _ time.Duration) (bool, string) {
				close(entered)
				<-ctx.Done()
				return false, "cancelled"
			},
			func(probeResult, string) bool { reported.Store(true); return true })
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a hanging check outlived its context")
	}
	if reported.Load() {
		t.Fatal("the answer of a cancelled check was reported")
	}
}

func TestProbeKillGrace(t *testing.T) {
	seconds := func(n int64) *int64 { return &n }
	pod := func(grace *int64) *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{TerminationGracePeriodSeconds: grace}}
	}
	for _, c := range []struct {
		name  string
		pod   *corev1.Pod
		probe *corev1.Probe
		want  time.Duration
	}{
		{"the Kubernetes default", pod(nil), &corev1.Probe{}, 30 * time.Second},
		{"the pod's", pod(seconds(10)), &corev1.Probe{}, 10 * time.Second},
		{"the probe's own", pod(seconds(10)), &corev1.Probe{TerminationGracePeriodSeconds: seconds(5)}, 5 * time.Second},
		{"never less than the kubelet's minimum", pod(seconds(0)), &corev1.Probe{}, minKillGrace},
	} {
		if got := probeKillGrace(c.pod, c.probe); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// --- probes of a running attempt ------------------------------------------

// A handler whose health the test controls. Each kind of probe handler is exercised
// through it.
type rig struct {
	handler corev1.ProbeHandler
	podIP   string
	runExec execRunner
	set     func(healthy bool)
}

func newRig(t *testing.T, kind string) *rig {
	t.Helper()
	var healthy atomic.Bool
	switch kind {
	case "http":
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !healthy.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}))
		t.Cleanup(server.Close)
		host, port := splitHostPort(t, server.Listener.Addr().String())
		return &rig{
			handler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(port), Path: "/"}},
			podIP:   host,
			set:     healthy.Store,
		}
	case "tcp":
		var mu sync.Mutex
		var listener net.Listener
		listen := func(addr string) net.Listener {
			for i := 0; ; i++ {
				l, err := net.Listen("tcp", addr)
				if err == nil {
					go func() {
						for {
							c, err := l.Accept()
							if err != nil {
								return
							}
							_ = c.Close()
						}
					}()
					return l
				}
				if i > 200 {
					t.Fatal(err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		listener = listen("127.0.0.1:0")
		addr := listener.Addr().String()
		host, port := splitHostPort(t, addr)
		t.Cleanup(func() { mu.Lock(); defer mu.Unlock(); listener.Close() })
		return &rig{
			handler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(port)}},
			podIP:   host,
			set: func(up bool) {
				mu.Lock()
				defer mu.Unlock()
				_ = listener.Close()
				if up {
					listener = listen(addr)
				}
			},
		}
	default:
		return &rig{
			handler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"check"}}},
			runExec: func(context.Context, []string, time.Duration) (bool, string) { return healthy.Load(), "unhealthy" },
			set:     healthy.Store,
		}
	}
}

func (r *rig) probe(p corev1.Probe) *corev1.Probe {
	p.ProbeHandler = r.handler
	if p.PeriodSeconds == 0 {
		p.PeriodSeconds = 1
	}
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = 100
	}
	return &p
}

// running is one attempt of a container that runs until it is stopped.
type running struct {
	pl     *PodLifecycle
	a      *Attempt
	cmd    *exec.Cmd
	exited chan int
}

// startRunning starts an attempt of a container that runs `script`, with the probes
// of spec, and waits for it in the background.
func startRunning(t *testing.T, spec *corev1.Container, r *rig, script string) *running {
	t.Helper()
	old := probeSecond
	probeSecond = 10 * time.Millisecond
	t.Cleanup(func() { probeSecond = old })

	pl := testLifecycle(t, nil, "app")
	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	spec.Name = "app"
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := a.Start(cmd, gatesOf(spec)); err != nil {
		t.Fatal(err)
	}
	run := &running{pl: pl, a: a, cmd: cmd, exited: make(chan int, 1)}
	go func() { run.exited <- a.Wait(cmd) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-a.done:
		case <-time.After(5 * time.Second):
		}
	})
	startProbes(a, r.podIP, spec, r.runExec)
	return run
}

func (r *running) status() corev1.ContainerStatus { return containerState(r.pl, "app") }

func (r *running) alive() bool {
	select {
	case <-r.a.done:
		return false
	default:
		return true
	}
}

func (r *running) started() bool { s := r.status().Started; return s != nil && *s }

const forever = "sleep 30"

// The probes of an attempt for every combination of probe kind and handler.
func TestProbeMatrix(t *testing.T) {
	for _, kind := range []string{"http", "tcp", "exec"} {
		t.Run("readiness/"+kind, func(t *testing.T) {
			r := newRig(t, kind)
			r.set(false)
			spec := &corev1.Container{ReadinessProbe: r.probe(corev1.Probe{FailureThreshold: 2})}
			run := startRunning(t, spec, r, forever)
			if s := run.status(); s.Ready || !run.started() {
				t.Fatalf("running container: ready %v started %v, want started but unready", s.Ready, run.started())
			}
			r.set(true)
			eventually(t, "ready", func() bool { return run.status().Ready })
			r.set(false)
			eventually(t, "unready again", func() bool { return !run.status().Ready })
			if !run.alive() || run.status().RestartCount != 0 {
				t.Fatal("a failing readiness probe stopped the container")
			}
			r.set(true)
			eventually(t, "ready again", func() bool { return run.status().Ready })
		})

		t.Run("liveness/"+kind, func(t *testing.T) {
			r := newRig(t, kind)
			r.set(true)
			spec := &corev1.Container{LivenessProbe: r.probe(corev1.Probe{FailureThreshold: 2})}
			run := startRunning(t, spec, r, forever)
			time.Sleep(150 * time.Millisecond)
			if !run.alive() || !run.status().Ready {
				t.Fatal("a healthy container was stopped or is not ready")
			}
			r.set(false)
			select {
			case <-run.exited:
			case <-time.After(5 * time.Second):
				t.Fatal("a container failing its liveness probe was not stopped")
			}
			if cs := run.status(); cs.State.Terminated == nil || cs.Ready {
				t.Fatalf("stopped container: %+v", cs.State)
			}
		})

		t.Run("startup/"+kind, func(t *testing.T) {
			r := newRig(t, kind)
			r.set(false)
			spec := &corev1.Container{StartupProbe: r.probe(corev1.Probe{FailureThreshold: 1000})}
			run := startRunning(t, spec, r, forever)
			time.Sleep(100 * time.Millisecond)
			if run.started() || run.status().Ready {
				t.Fatal("the container was started or ready before its startup probe succeeded")
			}
			r.set(true)
			eventually(t, "started", run.started)
			if !run.status().Ready {
				t.Fatal("a started container without a readiness probe is not ready")
			}

			// A failing startup probe is only about startup: once it has succeeded it is not run again.
			r.set(false)
			time.Sleep(100 * time.Millisecond)
			if !run.alive() || !run.status().Ready {
				t.Fatal("the startup probe acted after it had succeeded")
			}
		})
	}
}

// A slow-starting application whose liveness probe would kill it: the startup
// probe holds liveness back until the application is up.
func TestStartupProbeProtectsSlowStart(t *testing.T) {
	r := newRig(t, "http")
	r.set(false)
	spec := &corev1.Container{
		StartupProbe:   r.probe(corev1.Probe{FailureThreshold: 100}),
		LivenessProbe:  r.probe(corev1.Probe{FailureThreshold: 1}),
		ReadinessProbe: r.probe(corev1.Probe{}),
	}
	run := startRunning(t, spec, r, forever)
	time.Sleep(300 * time.Millisecond) // many liveness periods
	if !run.alive() {
		t.Fatal("liveness ran before startup completed")
	}
	if s := run.status(); s.Ready || run.started() {
		t.Fatalf("not started yet, but started %v ready %v", run.started(), s.Ready)
	}
	r.set(true)
	eventually(t, "started and ready", func() bool { return run.started() && run.status().Ready })
	if !run.alive() {
		t.Fatal("the container was stopped")
	}
	// Now that startup is over, liveness applies.
	r.set(false)
	select {
	case <-run.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("liveness did not apply after startup")
	}
}

// A container that never starts is stopped, and is not started or ready.
func TestStartupProbeFailureStopsTheContainer(t *testing.T) {
	r := newRig(t, "tcp")
	r.set(false)
	spec := &corev1.Container{StartupProbe: r.probe(corev1.Probe{FailureThreshold: 3}), ReadinessProbe: r.probe(corev1.Probe{})}
	run := startRunning(t, spec, r, forever)
	select {
	case <-run.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("a container failing its startup probe was not stopped")
	}
	if cs := run.status(); cs.State.Terminated == nil || cs.Ready || run.started() {
		t.Fatalf("after the stop: %+v started %v", cs, run.started())
	}
}

// With a readiness probe, passing the startup probe does not make the container
// ready by itself.
func TestStartupDoesNotImplyReadiness(t *testing.T) {
	r := &rig{runExec: func(_ context.Context, argv []string, _ time.Duration) (bool, string) {
		return argv[0] == "startup", ""
	}}
	probe := func(command string) *corev1.Probe {
		return &corev1.Probe{PeriodSeconds: 1, TimeoutSeconds: 100, ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{command}}}}
	}
	spec := &corev1.Container{StartupProbe: probe("startup"), ReadinessProbe: probe("readiness")}
	run := startRunning(t, spec, r, forever)
	eventually(t, "started", run.started)
	time.Sleep(50 * time.Millisecond)
	if run.status().Ready {
		t.Fatal("ready without the readiness probe having succeeded")
	}
}

// One container failing its readiness probe does not touch another container's
// process or readiness.
func TestReadinessFailureIsPerContainer(t *testing.T) {
	bad, good := newRig(t, "exec"), newRig(t, "exec")
	bad.set(false)
	good.set(true)
	old := probeSecond
	probeSecond = 10 * time.Millisecond
	defer func() { probeSecond = old }()

	pl := testLifecycle(t, nil, "bad", "good")
	start := func(name string, r *rig) (*Attempt, *exec.Cmd) {
		a, err := pl.BeginAttempt(name, false)
		if err != nil {
			t.Fatal(err)
		}
		spec := &corev1.Container{Name: name, ReadinessProbe: r.probe(corev1.Probe{})}
		cmd := sleeper("30")
		if err := a.Start(cmd, gatesOf(spec)); err != nil {
			t.Fatal(err)
		}
		go a.Wait(cmd)
		startProbes(a, "", spec, r.runExec)
		return a, cmd
	}
	_, badCmd := start("bad", bad)
	_, goodCmd := start("good", good)
	defer func() {
		_ = syscall.Kill(-badCmd.Process.Pid, syscall.SIGKILL)
		_ = syscall.Kill(-goodCmd.Process.Pid, syscall.SIGKILL)
	}()
	eventually(t, "the good container ready", func() bool { return containerState(pl, "good").Ready })
	time.Sleep(100 * time.Millisecond)
	if containerState(pl, "bad").Ready {
		t.Fatal("the failing container is ready")
	}
	if syscall.Kill(badCmd.Process.Pid, 0) != nil || syscall.Kill(goodCmd.Process.Pid, 0) != nil {
		t.Fatal("a readiness failure stopped a process")
	}
}

// --- stopping ------------------------------------------------------------

// Stop asks first and kills when the grace period has passed: a process that
// handles the termination signal exits by itself, one that ignores it is killed.
func TestAttemptStopEscalatesAfterGrace(t *testing.T) {
	spec := &corev1.Container{}
	polite := startRunning(t, spec, &rig{}, "trap 'exit 0' TERM; sleep 30 & wait")
	time.Sleep(100 * time.Millisecond) // let the shell install its trap
	start := time.Now()
	polite.a.Stop(5*time.Second, "test")
	select {
	case code := <-polite.exited:
		if code != 0 || time.Since(start) > 3*time.Second {
			t.Fatalf("a process that handles the signal: exit %d after %v", code, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the process did not stop")
	}

	stubborn := startRunning(t, &corev1.Container{}, &rig{}, "trap '' TERM; sleep 30 & wait")
	time.Sleep(100 * time.Millisecond)
	start = time.Now()
	stubborn.a.Stop(300*time.Millisecond, "test")
	select {
	case <-stubborn.exited:
		if took := time.Since(start); took < 250*time.Millisecond {
			t.Fatalf("killed after %v, before the grace period", took)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a process ignoring the signal was never killed")
	}
}

// Only the first Stop of an attempt runs a termination sequence: a second one
// (the pod's deletion while a probe's stop waits out its grace) sends no second
// signal and only enforces its own, shorter deadline.
func TestAttemptStopRunsOnce(t *testing.T) {
	terms := filepath.Join(t.TempDir(), "terms")
	run := startRunning(t, &corev1.Container{}, &rig{}, "trap 'echo t >> "+terms+"' TERM; while :; do sleep 0.1; done")
	time.Sleep(200 * time.Millisecond) // let the shell install its trap
	go run.a.Stop(time.Hour, "first")
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	run.a.Stop(0, "second") // its deadline is the kubelet's minimum, and it does not signal again
	if took := time.Since(start); took < minKillGrace-200*time.Millisecond || took > minKillGrace+3*time.Second {
		t.Fatalf("the second Stop took %v, want about %v", took, minKillGrace)
	}
	select {
	case <-run.a.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the second Stop did not kill the process at its deadline")
	}
	data, _ := os.ReadFile(terms)
	if got := strings.Count(string(data), "t"); got != 1 {
		t.Fatalf("the process was signalled %d times, want once", got)
	}
}

// Nothing a probe does outlives its attempt: when the container exits, a hanging
// exec probe is cancelled.
func TestProbesEndWithTheAttempt(t *testing.T) {
	entered := make(chan struct{}, 1)
	cancelled := make(chan struct{})
	r := &rig{
		handler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"hang"}}},
		runExec: func(ctx context.Context, _ []string, _ time.Duration) (bool, string) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-ctx.Done()
			select {
			case <-cancelled:
			default:
				close(cancelled)
			}
			return false, "cancelled"
		},
	}
	spec := &corev1.Container{ReadinessProbe: r.probe(corev1.Probe{}), LivenessProbe: r.probe(corev1.Probe{FailureThreshold: 1000})}
	run := startRunning(t, spec, r, forever)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never ran")
	}
	_ = syscall.Kill(-run.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("a probe of an exited container was not cancelled")
	}
}

// The probes of a superseded attempt cannot change the next attempt's status.
func TestOldAttemptProbesCannotPublish(t *testing.T) {
	r := newRig(t, "exec")
	r.set(true)
	spec := &corev1.Container{ReadinessProbe: r.probe(corev1.Probe{})}
	run := startRunning(t, spec, r, forever)
	eventually(t, "ready", func() bool { return run.status().Ready })
	_ = syscall.Kill(-run.cmd.Process.Pid, syscall.SIGKILL)
	<-run.exited

	next, err := run.pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := sleeper("30")
	if err := next.Start(cmd, gatesOf(spec)); err != nil {
		t.Fatal(err)
	}
	go next.Wait(cmd)
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	time.Sleep(100 * time.Millisecond) // the old attempt's probes have every chance to report
	if run.status().Ready {
		t.Fatal("the old attempt's readiness probe marked the new attempt ready")
	}
	if run.status().RestartCount != 1 {
		t.Fatalf("restart count %d, want 1", run.status().RestartCount)
	}
}

func TestStartedFollowsTheGates(t *testing.T) {
	for _, c := range []struct {
		name         string
		spec         corev1.Container
		started, rdy bool
	}{
		{"no probes", corev1.Container{}, true, true},
		{"readiness probe", corev1.Container{ReadinessProbe: &corev1.Probe{}}, true, false},
		{"startup probe", corev1.Container{StartupProbe: &corev1.Probe{}}, false, false},
	} {
		pl := testLifecycle(t, nil, "app")
		a, err := pl.BeginAttempt("app", false)
		if err != nil {
			t.Fatal(err)
		}
		cmd := sleeper("30")
		if err := a.Start(cmd, gatesOf(&c.spec)); err != nil {
			t.Fatal(err)
		}
		cs := containerState(pl, "app")
		if got := cs.Started != nil && *cs.Started; got != c.started || cs.Ready != c.rdy {
			t.Errorf("%s: started %v ready %v, want %v and %v", c.name, got, cs.Ready, c.started, c.rdy)
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		a.Wait(cmd)
		if cs := containerState(pl, "app"); cs.Started == nil || *cs.Started || cs.Ready {
			t.Errorf("%s: a terminated container is still started or ready: %+v", c.name, cs)
		}
	}
}
