package podhandler

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"skifflet/internal/compute/endpoint"
)

func TestTerminationMessageBound(t *testing.T) {
	for path, want := range map[string]bool{
		"/dev/termination-log": false, "/dev": false, "/dev/x/y": false, "": false, "relative": false,
		"/tekton/termination": true, "/var/log/term": true, "/devices/term": true,
	} {
		if got := terminationMessageBound(path); got != want {
			t.Fatalf("%q: %v, want %v", path, got, want)
		}
	}
	c := Container{Name: "app", ImageFilePath: "/img.sif", TerminationMessagePath: "/tekton/termination"}
	podDir := endpoint.PodPath(t.TempDir())
	args := strings.Join(c.BuildApptainerArgs(0, podDir, 3), " ")
	if !strings.Contains(args, podDir.Container("app").AttemptTerminationLog(3)+":/tekton/termination:rw") {
		t.Fatalf("the attempt's termination file is not bound at the path: %s", args)
	}
}

// exitWith runs attempt 1 of the container to the exit code, the file and the log written as
// the container would have written them.
func exitWith(t *testing.T, spec corev1.Container, code int, message, logRecords string) corev1.ContainerStatus {
	t.Helper()
	pl := testLifecycle(t, nil, spec.Name)
	a, err := pl.BeginAttempt(spec.Name, false)
	if err != nil {
		t.Fatal(err)
	}
	a.spec = &spec
	container := pl.podDir.Container(spec.Name)
	if err := os.MkdirAll(container.AttemptScratchDir(1), 0o755); err != nil {
		t.Fatal(err)
	}
	if message != "" {
		if err := os.WriteFile(container.AttemptTerminationLog(1), []byte(message), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	if logRecords != "" {
		if err := os.MkdirAll(filepath.Dir(container.AttemptLogPath(1)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(container.AttemptLogPath(1), []byte(logRecords), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code))
	if err := a.Start(cmd, NoProbes); err != nil {
		t.Fatal(err)
	}
	a.Wait(cmd)
	return containerState(pl, spec.Name)
}

func TestTerminationMessage(t *testing.T) {
	file := corev1.Container{Name: "app", TerminationMessagePath: "/tekton/termination", TerminationMessagePolicy: corev1.TerminationMessageReadFile}
	if cs := exitWith(t, file, 3, "it went wrong", ""); cs.State.Terminated.Message != "it went wrong" {
		t.Fatalf("the file's message: %q", cs.State.Terminated.Message)
	}
	long := strings.Repeat("x", maxTerminationMessage+100)
	if cs := exitWith(t, file, 0, long, ""); len(cs.State.Terminated.Message) != maxTerminationMessage {
		t.Fatalf("a long message is kept to %d bytes, got %d", maxTerminationMessage, len(cs.State.Terminated.Message))
	}

	records := "2026-10-11T10:00:00.000000000Z stdout F first\n2026-10-11T10:00:01.000000000Z stderr F last words\n"
	fallback := corev1.Container{Name: "app", TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError}
	if cs := exitWith(t, fallback, 2, "", records); cs.State.Terminated.Message != "first\nlast words\n" {
		t.Fatalf("the fallback to the log: %q", cs.State.Terminated.Message)
	}
	if cs := exitWith(t, fallback, 0, "", records); strings.Contains(cs.State.Terminated.Message, "last words") {
		t.Fatalf("a success does not fall back to the log: %q", cs.State.Terminated.Message)
	}
	readFile := corev1.Container{Name: "app", TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: corev1.TerminationMessageReadFile}
	if cs := exitWith(t, readFile, 2, "", records); strings.Contains(cs.State.Terminated.Message, "last words") {
		t.Fatalf("the File policy does not fall back to the log: %q", cs.State.Terminated.Message)
	}
}

func TestStopSignalOf(t *testing.T) {
	usr1 := corev1.Signal("SIGUSR1")
	withSignal := &corev1.Container{Lifecycle: &corev1.Lifecycle{StopSignal: &usr1}}
	for _, tc := range []struct {
		c     *corev1.Container
		image string
		want  syscall.Signal
	}{
		{&corev1.Container{}, "", syscall.SIGTERM},
		{&corev1.Container{}, "SIGQUIT", syscall.SIGQUIT},
		{&corev1.Container{}, "quit", syscall.SIGQUIT},
		{&corev1.Container{}, "9", syscall.SIGKILL},
		{withSignal, "SIGQUIT", syscall.SIGUSR1},
	} {
		if got, err := stopSignalOf(tc.c, tc.image); err != nil || got != tc.want {
			t.Fatalf("image %q: %v %v, want %v", tc.image, got, err, tc.want)
		}
	}
	if _, err := stopSignalOf(&corev1.Container{}, "SIGNOTHING"); !IsConfigError(err) {
		t.Fatalf("an unknown signal: %v", err)
	}
}

// Stop sends the container's stop signal, not SIGTERM.
func TestAttempt_StopSendsTheStopSignal(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	a.stopSignal = syscall.SIGUSR1
	marker := filepath.Join(t.TempDir(), "got")
	cmd := exec.Command("sh", "-c", fmt.Sprintf("trap 'echo usr1 > %s; exit 0' USR1; trap 'echo term > %s; exit 0' TERM; while :; do sleep 0.1; done", marker, marker))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := a.Start(cmd, NoProbes); err != nil {
		t.Fatal(err)
	}
	go a.Wait(cmd)
	time.Sleep(200 * time.Millisecond)
	a.Stop(5*time.Second, "test")
	if got, _ := os.ReadFile(marker); strings.TrimSpace(string(got)) != "usr1" {
		t.Fatalf("the container got %q, want usr1", got)
	}
}
