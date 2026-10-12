package podhandler

import (
	"os"
	"path/filepath"
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

	compute.Skiff = endpoint.SkiffWithTmp(tmpDir, filepath.Join(tmpDir, ".skiff"))

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

// A process that does not exist has no pod address.
func TestObservePodIP_NoProcess(t *testing.T) {
	if _, err := observePodIP(999999, nil); err == nil {
		t.Fatal("an address was observed for a process that does not exist")
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
// directory (verified through a launcher that records what it receives): a container's
// is its attempt's own, where its root filesystem is unpacked and which is its writable
// layer, and it exists when the container starts.
func TestRunAttempt_UsesThePodTemporaryDirectory(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "tmpdir")
	launcher := filepath.Join(root, "launcher")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\necho \"$APPTAINER_TMPDIR $SINGULARITY_TMPDIR $APPTAINER_QUIET\" > "+out+"\n"), 0755); err != nil {
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
	c := &Container{Name: "app"}
	spec := &corev1.Container{Name: "app"}
	exit, result, err := h.runAttempt(c, spec, false, 0)
	if err != nil || result != attemptExited || exit != 0 {
		t.Fatalf("launch: exit %d result %v err %v", exit, result, err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := pl.PodDir().Container("app").AttemptUnpackDir(1)
	if strings.TrimSpace(string(got)) != want+" "+want+" true" {
		t.Fatalf("launched process saw %q, want the attempt's own directory %q", got, want)
	}
	if !strings.HasPrefix(want, pl.PodDir().String()+"/") || strings.HasPrefix(want, pl.PodDir().TmpDir()+"/") {
		t.Fatalf("%s is not inside the pod directory, apart from the pod's own temporary directory", want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("the attempt's unpack directory %s does not exist: %v", want, err)
	}
}

// Through the launch path: every attempt of a container gets /tmp and /var/tmp of its own
// that exist, are empty and are not the node's when the container starts, and a restart
// finds nothing of the attempt before it.
func TestRunAttempt_EveryAttemptGetsItsOwnTmp(t *testing.T) {
	root := t.TempDir()
	results := filepath.Join(root, "results")
	launcher := filepath.Join(root, "launcher")
	script := `#!/bin/sh
bind=""
nomount=""
while [ $# -gt 0 ]; do
    [ "$1" = --bind ] && bind="$2"
    [ "$1" = --no-mount ] && nomount="$2"
    shift
done
tmp=$(printf '%s' "$bind" | tr ',' '\n' | grep ':/tmp:rw$' | cut -d: -f1)
vartmp=$(printf '%s' "$bind" | tr ',' '\n' | grep ':/var/tmp:rw$' | cut -d: -f1)
echo "$tmp $vartmp $nomount $(ls -A "$tmp" | wc -l | tr -d ' ') $(ls -A "$vartmp" | wc -l | tr -d ' ') $(pwd) bind=${APPTAINER_BIND-unset}${SINGULARITY_BIND-unset}${APPTAINER_BINDPATH-unset}${SINGULARITY_BINDPATH-unset}" >> ` + results + `
touch "$tmp/marker" "$vartmp/marker"
`
	if err := os.WriteFile(launcher, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	old := compute.Environment.ApptainerBin
	compute.Environment.ApptainerBin = launcher
	defer func() { compute.Environment.ApptainerBin = old }()

	// The bubble's own binds are in its environment (Apptainer puts them there).
	for _, name := range bindVariables {
		t.Setenv(name, "/var/lib/skiff,/root/.skiff")
	}
	pl := testLifecycle(t, nil, "app")
	if err := os.MkdirAll(pl.PodDir().LogDir(), 0755); err != nil {
		t.Fatal(err)
	}
	h := &PodHandler{lifecycle: pl, podDirectory: pl.PodDir(), logger: pl.logger}
	spec := &corev1.Container{Name: "app"}
	for i := 0; i < 2; i++ {
		if _, result, err := h.runAttempt(&Container{Name: "app"}, spec, false, 0); err != nil || result != attemptExited {
			t.Fatalf("attempt %d: %v %v", i+1, result, err)
		}
	}
	data, err := os.ReadFile(results)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("the launcher ran %d times: %q", len(lines), data)
	}
	own := pl.PodDir().Container("app")
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 7 {
			t.Fatalf("attempt %d: %q", i+1, line)
		}
		if fields[0] != own.AttemptTmp(i+1) || fields[1] != own.AttemptVarTmp(i+1) {
			t.Errorf("attempt %d got /tmp %s and /var/tmp %s, want %s and %s", i+1, fields[0], fields[1], own.AttemptTmp(i+1), own.AttemptVarTmp(i+1))
		}
		if fields[2] != NoMounts || !strings.Contains(NoMounts, "tmp") || !strings.Contains(NoMounts, "cwd") {
			t.Errorf("attempt %d: --no-mount %s, want %s (the node's /tmp and the launcher's directory must not be mounted)", i+1, fields[2], NoMounts)
		}
		if fields[6] != "bind=unsetunsetunsetunset" {
			t.Errorf("attempt %d got the bubble's bind mounts from its environment: %s", i+1, fields[6])
		}
		if fields[5] != "/" {
			t.Errorf("attempt %d was launched from %s, want /", i+1, fields[5])
		}
		if fields[3] != "0" || fields[4] != "0" {
			t.Errorf("attempt %d started with files in its /tmp (%s) or /var/tmp (%s)", i+1, fields[3], fields[4])
		}
	}
	if attempts, _ := own.ScratchAttempts(); len(attempts) != 1 || attempts[0] != 2 {
		t.Errorf("after the restart the pod holds temporary files of attempts %v, want only [2]", attempts)
	}
}
