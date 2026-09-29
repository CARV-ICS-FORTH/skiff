package podhandler

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestHTTPReadinessUsesPodIP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	host, portString, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatal(err)
	}
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(port)}}}
	if !evaluateProbe(context.Background(), probe, host, time.Second, nil) {
		t.Fatal("HTTP readiness probe could not reach the PodIP")
	}
}

// Exec probes are run by the runner they are handed (the container's target in
// production): success is exit code 0, and an empty command never succeeds.
func TestExecProbeUsesTheRunner(t *testing.T) {
	var got []string
	run := func(succeed bool) func(context.Context, []string, time.Duration) bool {
		return func(_ context.Context, argv []string, _ time.Duration) bool { got = argv; return succeed }
	}
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"test", "-f", "/ready"}}}}
	if !evaluateProbe(context.Background(), probe, "", time.Second, run(true)) || len(got) != 3 || got[0] != "test" {
		t.Fatalf("a succeeding runner did not make the probe succeed (ran %v)", got)
	}
	if evaluateProbe(context.Background(), probe, "", time.Second, run(false)) {
		t.Fatal("a failing runner made the probe succeed")
	}
	if evaluateProbe(context.Background(), probe, "", time.Second, nil) {
		t.Fatal("an exec probe without anything to run it in succeeded")
	}
	probe.Exec.Command = nil
	if evaluateProbe(context.Background(), probe, "", time.Second, run(true)) {
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
	if a.execProbe(context.Background(), []string{"true"}, time.Second) {
		t.Fatal("an exec probe succeeded for an attempt without a process")
	}
}
