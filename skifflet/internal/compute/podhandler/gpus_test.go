package podhandler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/gpu"
	"skifflet/internal/compute/runtime"
	"skifflet/pkg/tmpfs"
)

func gpuContainer(name string, n int) corev1.Container {
	c := corev1.Container{Name: name, Image: "unused"}
	if n > 0 {
		c.Resources.Limits = corev1.ResourceList{gpu.ResourceName: *resource.NewQuantity(int64(n), resource.DecimalSI)}
	}
	return c
}

// nodeWithGPUs gives the node GPUs 0 to n-1 (files in a directory, as the driver has them).
func nodeWithGPUs(t *testing.T, n int) {
	t.Helper()
	dir := t.TempDir()
	touch := func(rel string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list := ""
	for i := 0; i < n; i++ {
		touch("dev/nvidia" + string(rune('0'+i)))
		if i > 0 {
			list += ","
		}
		list += string(rune('0' + i))
	}
	for _, f := range []string{"dev/nvidiactl", "dev/nvidia-uvm", "libs/libcuda.so.1", "libs/libnvidia-ml.so.1"} {
		touch(f)
	}
	s := gpu.Sources{
		Env:  func(name string) string { return map[string]string{gpu.StepGPUsVariable: list}[name] },
		Proc: filepath.Join(dir, "proc"), Dev: filepath.Join(dir, "dev"), Libs: filepath.Join(dir, "libs"), Bin: filepath.Join(dir, "bin"),
		Open: func(string) error { return nil },
	}
	if n == 0 {
		s.Env = func(string) string { return "" }
	}
	if err := gpu.Init(s, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gpu.Init(gpu.Sources{Env: func(string) string { return "" }}, "") })
}

func TestPlanGPUs(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{gpuContainer("init-a", 1), gpuContainer("init-b", 3), gpuContainer("init-none", 0)},
		Containers:     []corev1.Container{gpuContainer("one", 1), gpuContainer("none", 0), gpuContainer("two", 2)},
	}}
	total, shares := planGPUs(pod)
	// The application containers run together and need 3 different GPUs; an init container
	// runs alone and may have up to 3: the pod needs the larger.
	if total != 3 {
		t.Errorf("total = %d; want 3", total)
	}
	want := map[string]gpuShare{"init-a": {0, 1}, "init-b": {0, 3}, "one": {0, 1}, "two": {1, 2}}
	if len(shares) != len(want) {
		t.Fatalf("shares = %v; want %v", shares, want)
	}
	for name, s := range want {
		if shares[name] != s {
			t.Errorf("%s has %v; want %v", name, shares[name], s)
		}
	}

	// An init container that wants more than the application containers together sets the need.
	pod = &corev1.Pod{Spec: corev1.PodSpec{InitContainers: []corev1.Container{gpuContainer("init", 4)}, Containers: []corev1.Container{gpuContainer("app", 1)}}}
	if total, _ := planGPUs(pod); total != 4 {
		t.Errorf("total = %d; want 4", total)
	}

	// Only a limit is a request for GPUs, and a pod without any needs none.
	if total, shares := planGPUs(&corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "cpu-only"}}}}); total != 0 || len(shares) != 0 {
		t.Errorf("a CPU-only pod needs %d, %v", total, shares)
	}
}

func TestReserveGPUs_AreGivenBackWithTheLiveResources(t *testing.T) {
	nodeWithGPUs(t, 2)
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{gpuContainer("a", 1), gpuContainer("b", 1)}}}
	pod.Name, pod.Namespace, pod.UID = "gpus", "gpu-test", "gpus-uid"
	owner := NewPodLifecycle(pod, endpoint.PodPath(t.TempDir()), nil)

	got, err := reserveGPUs(owner, pod)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["a"]) != 1 || len(got["b"]) != 1 || got["a"][0].Minor == got["b"][0].Minor {
		t.Errorf("containers a and b have %v; want one different GPU each", got)
	}

	// The node has none left for a second pod, which the kubelet's message says.
	other := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{gpuContainer("c", 1)}}}
	other.UID = "other-uid"
	_, err = reserveGPUs(NewPodLifecycle(other, endpoint.PodPath(t.TempDir()), nil), other)
	var unavailable *gpu.UnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "Allocate failed due to requested number of devices unavailable for nvidia.com/gpu. Requested: 1, Available: 0") {
		t.Fatalf("second pod: %v", err)
	}

	// The pod's live resources are released: the GPUs are the node's again.
	owner.ReleaseLiveResources(0)
	if _, err := reserveGPUs(NewPodLifecycle(other, endpoint.PodPath(t.TempDir()), nil), other); err != nil {
		t.Errorf("after the first pod ended: %v", err)
	}
}

func TestReserveGPUs_APodWithoutGPUsTakesNone(t *testing.T) {
	nodeWithGPUs(t, 1)
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{gpuContainer("cpu", 0)}}}
	got, err := reserveGPUs(NewPodLifecycle(pod, endpoint.PodPath(t.TempDir()), nil), pod)
	if err != nil || len(got) != 0 {
		t.Errorf("%v, %v", got, err)
	}
	if _, err := gpu.Reserve("someone", 1); err != nil {
		t.Errorf("the GPU was taken by a pod that asked for none: %v", err)
	}
}

// A pod that asks for GPUs a node does not have is refused as the kubelet refuses it, whole,
// with nothing of it created.
func TestPreparePod_RefusesGPUsTheNodeDoesNotHave(t *testing.T) {
	nodeWithGPUs(t, 0)
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{gpuContainer("app", 1)}}}
	pod.Name, pod.Namespace, pod.UID = "no-gpus", "gpu-test", "no-gpus-uid"
	var snapshot *corev1.Pod
	owner := installOwner(t, pod, func(p *corev1.Pod) { snapshot = p })
	defer func() { owner.Terminate(0); GlobalRegistry.DeleteOwner(owner) }()
	<-prepare(owner)
	if snapshot == nil || snapshot.Status.Phase != corev1.PodFailed || snapshot.Status.Reason != "UnexpectedAdmissionError" ||
		!strings.Contains(snapshot.Status.Message, "Requested: 1, Available: 0") {
		t.Fatalf("the pod was not refused as the kubelet refuses it: %+v", snapshot)
	}
	if _, err := os.Stat(compute.Skiff.PodWithUID(client.ObjectKeyFromObject(pod), pod.UID).String()); !os.IsNotExist(err) {
		t.Error("a refused pod has a directory")
	}
}

func TestBuildApptainerArgs_GPUsAndDevices(t *testing.T) {
	podDir := endpoint.PodPath(filepath.Join(t.TempDir(), "pod"))
	args := func(c *Container) string { return strings.Join(c.BuildApptainerArgs(0, podDir, 1), " ") }

	cpu := args(&Container{Name: "cpu", ExecutionMode: "exec", ImageFilePath: "/i.sif"})
	for _, no := range []string{"--nv", "--device"} {
		if strings.Contains(cpu, no) {
			t.Errorf("a container without GPUs got %s: %s", no, cpu)
		}
	}
	// Every container has a minimal /dev: the nodes of the node's devices are not its own.
	if !strings.Contains(cpu, "--contain") {
		t.Errorf("a container does not have a minimal /dev: %s", cpu)
	}
	// and starts in / (a contained container would start in its home)
	if !strings.Contains(cpu, "--pwd /") {
		t.Errorf("a container does not start in /: %s", cpu)
	}

	withGPUs := args(&Container{Name: "gpu", ExecutionMode: "exec", ImageFilePath: "/i.sif", GPUs: []int{1, 3}})
	if !strings.Contains(withGPUs, "--device nvidia.com/gpu=1 --device nvidia.com/gpu=3") || strings.Contains(withGPUs, "--nv") {
		t.Errorf("a container with GPUs 1 and 3 got: %s", withGPUs)
	}
}

func TestBuildApptainerArgs_PodShm(t *testing.T) {
	podDir := endpoint.PodPath(filepath.Join(t.TempDir(), "pod"))
	own := (&Container{Name: "a", ExecutionMode: "exec", ImageFilePath: "/i.sif"}).BuildApptainerArgs(0, podDir, 1)
	if !strings.Contains(strings.Join(own, " "), podDir.ShmDir()+":/dev/shm:rw") {
		t.Errorf("a container does not have the pod's /dev/shm: %v", own)
	}
	mounted := (&Container{Name: "a", ExecutionMode: "exec", ImageFilePath: "/i.sif", Binds: []string{"/vol/shm:/dev/shm:rw"}}).BuildApptainerArgs(0, podDir, 1)
	if strings.Contains(strings.Join(mounted, " "), podDir.ShmDir()) {
		t.Errorf("a pod's volume at /dev/shm has the pod's tmpfs under it: %v", mounted)
	}
}

func TestVisibleDevicesAreUUIDs(t *testing.T) {
	if got := visibleDevices([]gpu.Device{{Minor: 0, UUID: "GPU-a"}, {Minor: 3, UUID: "GPU-b"}}); got != "GPU-a,GPU-b" {
		t.Errorf("%q", got)
	}
}

func TestCheckRuntimeClass(t *testing.T) {
	old := RuntimeHandlers
	defer func() { RuntimeHandlers = old }()
	RuntimeHandlers = func(_ context.Context, name string) (string, error) {
		switch name {
		case "nvidia":
			return "nvidia", nil
		case "nvidia-experimental":
			return "nvidia-experimental", nil
		case "gvisor":
			return "runsc", nil
		case "broken":
			return "", errors.New("connection refused")
		}
		return "", apierrors.NewNotFound(schema.GroupResource{Group: "node.k8s.io", Resource: "runtimeclasses"}, name)
	}
	named := func(n *string) *corev1.Pod { return &corev1.Pod{Spec: corev1.PodSpec{RuntimeClassName: n}} }
	s := func(v string) *string { return &v }

	for _, n := range []*string{nil, s(""), s("nvidia"), s("nvidia-experimental")} {
		if err := CheckRuntimeClass(context.Background(), named(n)); err != nil {
			t.Errorf("%v: %v", n, err)
		}
	}
	for name, want := range map[string]string{
		"gvisor":  `RuntimeClass "gvisor" has the handler "runsc", and Skifflet has only nvidia and nvidia-experimental`,
		"missing": `RuntimeClass "missing" not found`,
		"broken":  `cannot read RuntimeClass "broken"`,
	} {
		if err := CheckRuntimeClass(context.Background(), named(s(name))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v; want one saying %q", name, err, want)
		}
	}
}

// A memory emptyDir is a tmpfs of its sizeLimit; without one, of the memory the node offers
// pods; and it is unmounted with the pod's live resources.
func TestMountMemoryVolume(t *testing.T) {
	oldMount, oldUnmount := MountTmpfs, UnmountTmpfs
	defer func() { MountTmpfs, UnmountTmpfs = oldMount, oldUnmount }()
	var mounted, unmounted []string
	var size int64
	MountTmpfs = func(dir string, o tmpfs.Options) error { mounted = append(mounted, dir); size = o.Size; return nil }
	UnmountTmpfs = func(dir string) error { unmounted = append(unmounted, dir); return nil }

	pod := &corev1.Pod{}
	h := newTestHandler(pod, endpoint.PodPath(t.TempDir()))
	limit := resource.MustParse("32Mi")
	if err := h.mountMemoryVolume("/vol/cache", &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &limit}); err != nil {
		t.Fatal(err)
	}
	if size != 32<<20 {
		t.Errorf("a volume with a limit of 32Mi is %d bytes", size)
	}
	if err := h.mountMemoryVolume("/vol/open", &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}); err != nil {
		t.Fatal(err)
	}
	if allocatable := runtime.AllocatableResources(context.Background()); size != allocatable.Memory().Value() {
		t.Errorf("a volume without a limit is %d bytes; want the allocatable memory, %d", size, allocatable.Memory().Value())
	}
	h.lifecycle.ReleaseLiveResources(0)
	if len(mounted) != 2 || len(unmounted) != 2 || unmounted[0] != "/vol/open" || unmounted[1] != "/vol/cache" {
		t.Errorf("mounted %v, unmounted %v; want both unmounted, the last first", mounted, unmounted)
	}
}

// A container with GPUs is built with their minor numbers (for --device) and with
// NVIDIA_VISIBLE_DEVICES naming their UUIDs, over the image's or the pod's own value; one
// without gets neither.
func TestBuildContainer_GPUs(t *testing.T) {
	useConfig(t, &fakeConfig{})
	h, _ := handlerFor(t,
		corev1.Container{Name: "gpu", Env: []corev1.EnvVar{{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"}}},
		corev1.Container{Name: "cpu"})
	h.gpus = map[string][]gpu.Device{"gpu": {{Minor: 1, UUID: "GPU-b"}, {Minor: 2, UUID: "GPU-c"}}}

	c, err := h.buildContainer(context.Background(), &h.Pod.Spec.Containers[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(c.GPUs) != 2 || c.GPUs[0] != 1 || c.GPUs[1] != 2 {
		t.Errorf("GPUs = %v; want [1 2]", c.GPUs)
	}
	env, err := os.ReadFile(c.EnvFilePath)
	if err != nil {
		t.Fatal(err)
	}
	// (the last assignment of a name is the one that counts)
	lines := strings.Split(strings.TrimSpace(string(env)), "\n")
	if last := lines[len(lines)-1]; last != "NVIDIA_VISIBLE_DEVICES=GPU-b,GPU-c" {
		t.Errorf("the last line of the environment is %q; want the container's GPUs after the pod's \"all\"", last)
	}

	c, err = h.buildContainer(context.Background(), &h.Pod.Spec.Containers[1])
	if err != nil {
		t.Fatal(err)
	}
	env, _ = os.ReadFile(c.EnvFilePath)
	if len(c.GPUs) != 0 || strings.Contains(string(env), "NVIDIA") {
		t.Errorf("a container that asked for none has GPUs %v and environment %q", c.GPUs, env)
	}
}
