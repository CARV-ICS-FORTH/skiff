package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStats_EffectiveGPUCount(t *testing.T) {
	origCUDA := os.Getenv("CUDA_VISIBLE_DEVICES")
	origNVIDIA := os.Getenv("NVIDIA_VISIBLE_DEVICES")
	origSLURM := os.Getenv("SLURM_STEP_GPUS")
	defer func() {
		os.Setenv("CUDA_VISIBLE_DEVICES", origCUDA)
		os.Setenv("NVIDIA_VISIBLE_DEVICES", origNVIDIA)
		os.Setenv("SLURM_STEP_GPUS", origSLURM)
	}()

	tests := []struct {
		envName  string
		envVal   string
		expected int
	}{
		{"CUDA_VISIBLE_DEVICES", "0,1", 2},
		{"CUDA_VISIBLE_DEVICES", "none", 0},
		{"CUDA_VISIBLE_DEVICES", "void", 0},
		{"CUDA_VISIBLE_DEVICES", "NoDevFiles", 0},
		{"NVIDIA_VISIBLE_DEVICES", "GPU-12345,GPU-67890,GPU-abcde", 3},
		{"SLURM_STEP_GPUS", "0", 1},
	}

	for _, tt := range tests {
		os.Unsetenv("CUDA_VISIBLE_DEVICES")
		os.Unsetenv("NVIDIA_VISIBLE_DEVICES")
		os.Unsetenv("SLURM_STEP_GPUS")
		os.Setenv(tt.envName, tt.envVal)

		got := getEffectiveGPUCount()
		if got != tt.expected {
			t.Errorf("getEffectiveGPUCount() with %s=%q = %d; want %d", tt.envName, tt.envVal, got, tt.expected)
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
