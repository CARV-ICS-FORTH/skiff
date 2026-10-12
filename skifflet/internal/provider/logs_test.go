package provider

import (
	"context"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"skifflet/internal/compute/endpoint"
	PodHandler "skifflet/internal/compute/podhandler"
)

func query(t *testing.T, raw string) context.Context {
	t.Helper()
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return WithLogQuery(context.Background(), values)
}

// The request's own query decides, so an absent tailLines (every line) is not
// the tailLines=0 (no line) Virtual Kubelet's parsing would make of both.
func TestLogOptions_TailPresence(t *testing.T) {
	absent, err := logOptions(query(t, "timestamps=true"), vkapi.ContainerLogOpts{})
	if err != nil || absent.TailLines != nil || !absent.Timestamps {
		t.Fatalf("absent: %+v, %v", absent, err)
	}
	zero, err := logOptions(query(t, "tailLines=0"), vkapi.ContainerLogOpts{})
	if err != nil || zero.TailLines == nil || *zero.TailLines != 0 {
		t.Fatalf("tailLines=0: %+v, %v", zero, err)
	}
	all, err := logOptions(query(t, "tailLines=25&limitBytes=100&sinceSeconds=30&follow=true&previous=true"), vkapi.ContainerLogOpts{})
	if err != nil || *all.TailLines != 25 || *all.LimitBytes != 100 || *all.SinceSeconds != 30 || !all.Follow || !all.Previous {
		t.Fatalf("all: %+v, %v", all, err)
	}
	since, err := logOptions(query(t, "sinceTime=2026-01-02T03:04:05Z"), vkapi.ContainerLogOpts{})
	if err != nil || since.SinceTime == nil || !since.SinceTime.Time.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("sinceTime: %+v, %v", since, err)
	}
}

// Without the route's query (a direct call), what Virtual Kubelet parsed is used.
func TestLogOptions_FallbackToVirtualKubeletOptions(t *testing.T) {
	opts, err := logOptions(context.Background(), vkapi.ContainerLogOpts{Tail: 3, Timestamps: true, SinceSeconds: 9})
	if err != nil || *opts.TailLines != 3 || !opts.Timestamps || *opts.SinceSeconds != 9 {
		t.Fatalf("%+v, %v", opts, err)
	}
	if opts, _ := logOptions(context.Background(), vkapi.ContainerLogOpts{}); opts.TailLines != nil {
		t.Fatal("a zero tail is not a request for no lines when nothing says so")
	}
}

// What the API server refuses, so does Skifflet, with a bad request.
func TestLogOptions_InvalidCombinationsAreRefused(t *testing.T) {
	for _, raw := range []string{
		"tailLines=-1",
		"limitBytes=0",
		"limitBytes=-5",
		"sinceSeconds=0",
		"sinceSeconds=5&sinceTime=2026-01-02T03:04:05Z",
		"tailLines=abc",
		"sinceTime=yesterday",
		"follow=maybe",
	} {
		_, err := logOptions(query(t, raw), vkapi.ContainerLogOpts{})
		if err == nil || !errdefs.IsInvalidInput(err) {
			t.Errorf("%q: %v, want a bad request", raw, err)
		}
	}
}

func logPod(name string, containers ...string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID("uid-logs-" + name)},
		Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever},
	}
	for _, c := range containers {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: c, Image: "unused"})
	}
	return pod
}

func installLogOwner(t *testing.T, pod *corev1.Pod) *PodHandler.PodLifecycle {
	t.Helper()
	owner := PodHandler.NewPodLifecycle(pod, endpoint.PodPath(filepath.Join(t.TempDir(), "pod")), nil)
	owner.InitializeStatuses()
	PodHandler.GlobalRegistry.Register(owner)
	t.Cleanup(func() { forgetOwner(pod.UID) })
	return owner
}

func TestGetContainerLogs_ErrorsAreDistinct(t *testing.T) {
	pod := logPod("errors", "app")
	owner := installLogOwner(t, pod)
	vk := &VirtualK8S{}
	get := func(pod, container string, raw string) error {
		_, err := vk.GetContainerLogs(query(t, raw), "default", pod, container, vkapi.ContainerLogOpts{})
		return err
	}

	if err := get("nonexistent", "app", ""); !errdefs.IsNotFound(err) {
		t.Errorf("missing pod: %v", err)
	}
	if err := get("errors", "nonexistent", ""); !errdefs.IsNotFound(err) {
		t.Errorf("missing container: %v", err)
	}
	if err := get("errors", "app", ""); !errdefs.IsInvalidInput(err) || !strings.Contains(err.Error(), "waiting to start") {
		t.Errorf("a container that has not started: %v", err)
	}
	if err := get("errors", "app", "tailLines=-2"); !errdefs.IsInvalidInput(err) {
		t.Errorf("invalid option: %v", err)
	}

	// One attempt, ended for good: it has logs, and no previous.
	runToEnd(t, owner, "app", `echo hello`)
	rc, err := vk.GetContainerLogs(query(t, ""), "default", "errors", "app", vkapi.ContainerLogOpts{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "hello\n" {
		t.Errorf("logs %q", data)
	}
	if err := get("errors", "app", "previous=true"); !errdefs.IsInvalidInput(err) || !strings.Contains(err.Error(), "previous terminated container") {
		t.Errorf("previous with one attempt: %v", err)
	}
}

func runToEnd(t *testing.T, owner *PodHandler.PodLifecycle, container, script string) {
	t.Helper()
	a, err := owner.BeginAttempt(container, false)
	if err != nil {
		t.Fatal(err)
	}
	log, err := a.OpenLog(owner.PodDir().Container(container))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = log.Stdout(), log.Stderr()
	if err := a.Start(cmd, PodHandler.NoProbes); err != nil {
		t.Fatal(err)
	}
	log.Begin()
	a.Wait(cmd)
}

// The stream of a request stops with the request, whichever side ends it.
func TestGetContainerLogs_FollowStopsWithTheRequest(t *testing.T) {
	pod := logPod("follow", "app")
	owner := installLogOwner(t, pod)
	a, err := owner.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	log, err := a.OpenLog(owner.PodDir().Container("app"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `while :; do echo tick; sleep 0.05; done`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = log.Stdout(), log.Stderr()
	if err := a.Start(cmd, PodHandler.NoProbes); err != nil {
		t.Fatal(err)
	}
	log.Begin()
	done := make(chan struct{})
	go func() { a.Wait(cmd); close(done) }()
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done }()

	vk := &VirtualK8S{}
	// The client closes its stream.
	rc, err := vk.GetContainerLogs(query(t, "follow=true"), "default", "follow", "app", vkapi.ContainerLogOpts{})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rc, buf); err != nil || !strings.HasPrefix(string(buf), "tick") {
		t.Fatalf("read %q, %v", buf, err)
	}
	closed := make(chan struct{})
	go func() { rc.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closing a following stream did not return")
	}

	// The client's context ends.
	ctx, cancel := context.WithCancel(query(t, "follow=true"))
	rc, err = vk.GetContainerLogs(ctx, "default", "follow", "app", vkapi.ContainerLogOpts{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	readDone := make(chan struct{})
	go func() { io.Copy(io.Discard, rc); close(readDone) }()
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled request's stream did not end")
	}
	rc.Close()
}

var _ = os.Remove
