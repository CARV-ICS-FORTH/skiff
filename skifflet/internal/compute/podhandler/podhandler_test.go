package podhandler

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDeletePod_CleanRemoval(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "skiff-delete-pod-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	compute.Skiff = endpoint.SkiffWithPods(tmpDir, filepath.Join(tmpDir, ".skiff", ".pods"))

	podKey := client.ObjectKey{Namespace: "default", Name: "my-pod"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podKey.Name,
			Namespace: podKey.Namespace,
			UID:       "delete-generation",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	podDir := compute.Skiff.PodWithUID(podKey, pod.GetUID())
	if err := os.MkdirAll(podDir.JobDir(), 0755); err != nil {
		t.Fatalf("failed to create job dir: %v", err)
	}

	ok := DeletePod(podKey, pod)
	if !ok {
		t.Fatalf("expected DeletePod to return true")
	}

	if _, err := os.Stat(podDir.String()); !os.IsNotExist(err) {
		t.Errorf("expected pod directory %s to be removed, but still exists", podDir.String())
	}
}

func TestGetPodIPFromNetns_Fallback(t *testing.T) {
	if goruntime.GOOS == "linux" {
		_, err := GetPodIPFromNetns(999999)
		if err == nil {
			t.Fatalf("expected error on Linux for non-existent PID")
		}
	} else {
		ip, err := GetPodIPFromNetns(999999)
		if err != nil {
			t.Fatalf("unexpected error from fallback: %v", err)
		}
		if ip == "" || strings.HasPrefix(ip, "127.") {
			t.Errorf("expected non-empty non-loopback IP from fallback, got %q", ip)
		}
	}
}

// leaveKilledContainerRootfs recreates what a container killed with SIGKILL leaves
// behind in its Apptainer temporary directory: an unpacked root filesystem
// containing directories the owner cannot read.
func leaveKilledContainerRootfs(t *testing.T, podDir string) {
	t.Helper()
	locked := filepath.Join(podDir, "tmp", "rootfs-123", "root", "home", "nonroot")
	if err := os.MkdirAll(locked, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "file"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	// The removal must not depend on the test cleanup fixing the modes.
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
}

func TestSafeRemovePodDirectory_UnreadableTree(t *testing.T) {
	podDir := filepath.Join(t.TempDir(), "pod")
	leaveKilledContainerRootfs(t, podDir)
	if err := os.RemoveAll(podDir); err == nil {
		t.Fatal("plain RemoveAll unexpectedly succeeded: the fixture does not reproduce the leak")
	}
	start := time.Now()
	if err := SafeRemovePodDirectory(podDir); err != nil {
		t.Fatalf("SafeRemovePodDirectory: %v", err)
	}
	if _, err := os.Stat(podDir); !os.IsNotExist(err) {
		t.Fatalf("pod directory survived: %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("a permission error was retried for %v", time.Since(start))
	}
}

// What a killed container leaves is removed with the pod.
func TestPodLifecycle_TerminateRemovesKilledContainersTemporaryFiles(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	leaveKilledContainerRootfs(t, pl.PodDir().String())
	pl.Terminate(0)
	if _, err := os.Stat(pl.PodDir().String()); !os.IsNotExist(err) {
		t.Fatalf("pod directory, including the temporary files of killed containers, survived Terminate: %v", err)
	}
}

// Every Apptainer process of a pod gets its temporary directory inside the pod
// directory (verified through a launcher that records what it receives).
func TestRunAttempt_UsesThePodTemporaryDirectory(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "tmpdir")
	launcher := filepath.Join(root, "launcher")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\necho \"$APPTAINER_TMPDIR $SINGULARITY_TMPDIR\" > "+out+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	old := compute.Environment.ApptainerBin
	compute.Environment.ApptainerBin = launcher
	defer func() { compute.Environment.ApptainerBin = old }()
	t.Setenv("APPTAINER_TMPDIR", "/somewhere/shared") // the bubble's own setting must be overridden

	pl := testLifecycle(t, nil, "app")
	if err := os.MkdirAll(pl.PodDir().LogDir(), 0755); err != nil {
		t.Fatal(err)
	}
	h := &PodHandler{lifecycle: pl, podDirectory: pl.PodDir(), logger: pl.logger}
	c := &Container{LogsPath: pl.PodDir().Container("app").LogsPath()}
	spec := &corev1.Container{Name: "app"}
	exit, result, err := h.runAttempt(c, spec, false, 0)
	if err != nil || result != attemptExited || exit != 0 {
		t.Fatalf("launch: exit %d result %v err %v", exit, result, err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := pl.PodDir().TmpDir()
	if strings.TrimSpace(string(got)) != want+" "+want {
		t.Fatalf("launched process saw %q, want the pod temporary directory %q", got, want)
	}
}
