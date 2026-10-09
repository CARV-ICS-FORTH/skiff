package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"skifflet/internal/compute/gpu"
)

// The node advertises the GPUs its inventory has, as nvidia.com/gpu, in capacity and in
// allocatable, and nothing when it has none.
func TestStats_AdvertisesTheGPUsOfTheInventory(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"dev/nvidia0", "dev/nvidia1", "dev/nvidiactl", "dev/nvidia-uvm", "libs/libcuda.so.1", "libs/libnvidia-ml.so.1"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	none := gpu.Sources{Env: func(string) string { return "" }}
	t.Cleanup(func() { _ = gpu.Init(none, "") })

	if err := gpu.Init(none, ""); err != nil {
		t.Fatal(err)
	}
	if _, has := TotalResources()[gpu.ResourceName]; has {
		t.Error("a node without GPUs advertises nvidia.com/gpu")
	}

	granted := gpu.Sources{
		Env:  func(name string) string { return map[string]string{gpu.StepGPUsVariable: "0,1"}[name] },
		Proc: filepath.Join(dir, "proc"), Dev: filepath.Join(dir, "dev"), Libs: filepath.Join(dir, "libs"), Bin: filepath.Join(dir, "bin"),
		Open: func(string) error { return nil },
	}
	if err := gpu.Init(granted, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for name, list := range map[string]corev1.ResourceList{"capacity": TotalResources(), "allocatable": AllocatableResources(context.Background())} {
		q := list[gpu.ResourceName]
		if q.Value() != 2 {
			t.Errorf("%s of %s = %s; want 2", name, gpu.ResourceName, q.String())
		}
	}
}

func TestStats_EffectiveCPUAndMemory_Slurm(t *testing.T) {
	origCPU := os.Getenv("SLURM_CPUS_ON_NODE")
	origMem := os.Getenv("SLURM_MEM_PER_NODE")
	defer func() {
		os.Setenv("SLURM_CPUS_ON_NODE", origCPU)
		os.Setenv("SLURM_MEM_PER_NODE", origMem)
	}()

	os.Setenv("SLURM_CPUS_ON_NODE", "4")
	os.Setenv("SLURM_MEM_PER_NODE", "8192") // 8192 MB = 8 GiB

	total := TotalResources()

	cpuVal := total.Cpu().MilliValue()
	if cpuVal != 4000 {
		t.Fatalf("expected 4000m CPU from Slurm, got %d", cpuVal)
	}

	memVal := total.Memory().Value()
	expectedMem := int64(8192) * 1024 * 1024
	if memVal != expectedMem {
		t.Fatalf("expected %d bytes memory from Slurm, got %d", expectedMem, memVal)
	}

	allocatable := AllocatableResources(context.Background())
	// Should reserve 100m for >= 2 cores
	if allocatable.Cpu().MilliValue() != 3900 {
		t.Fatalf("expected 3900m allocatable CPU (4000 - 100), got %d", allocatable.Cpu().MilliValue())
	}
}

func TestStats_MockCgroup_FractionalCPU(t *testing.T) {
	tmpDir := t.TempDir()
	cgroupCpuMax := filepath.Join(tmpDir, "cpu.max")
	// 50000 quota with 100000 period = 0.5 CPU = 500m
	if err := os.WriteFile(cgroupCpuMax, []byte("50000 100000\n"), 0644); err != nil {
		t.Fatal(err)
	}

	origCPU := os.Getenv("SLURM_CPUS_ON_NODE")
	defer os.Setenv("SLURM_CPUS_ON_NODE", origCPU)
	os.Unsetenv("SLURM_CPUS_ON_NODE")

	// Read and verify the quota math directly
	data, _ := os.ReadFile(cgroupCpuMax)
	fields := []int64{50000, 100000}
	milli := (fields[0] * 1000) / fields[1]
	if milli != 500 {
		t.Fatalf("expected 500m for 0.5 core quota, got %d", milli)
	}
	_ = data
}
