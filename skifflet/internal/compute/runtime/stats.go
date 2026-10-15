// Copyright © 2022 FORTH-ICS
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package runtime contains code for accessing compute resources.
package runtime

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"skifflet/internal/compute"
	"skifflet/internal/compute/gpu"
)

// TotalResources derives truthful capacity bounded by what the node gives the cluster (the
// bubble's script says it, from a Slurm job's allocation: NodeCPUsVariable,
// NodeMemoryVariable), cgroups, and CPU affinity.
func TotalResources() corev1.ResourceList {
	cpuMillis := getEffectiveCPUMillis()
	cpuQuantity := resource.NewMilliQuantity(cpuMillis, resource.DecimalSI)

	memBytes := getEffectiveMemoryBytes()
	memQuantity := resource.NewQuantity(int64(memBytes), resource.BinarySI)

	storagePath := "/"
	if compute.Skiff.PodsDir() != "" {
		storagePath = compute.Skiff.PodsDir()
	} else if compute.Environment.WorkingDirectory != "" {
		storagePath = compute.Environment.WorkingDirectory
	}
	storageBytes := getEffectiveStorageBytes(storagePath)
	storageQuantity := resource.NewQuantity(int64(storageBytes), resource.BinarySI)

	podsQuantity := resource.MustParse("110")

	res := corev1.ResourceList{
		corev1.ResourceCPU:              *cpuQuantity,
		corev1.ResourceMemory:           *memQuantity,
		corev1.ResourceEphemeralStorage: *storageQuantity,
		corev1.ResourcePods:             podsQuantity,
	}

	// The GPUs granted to this node and Skifflet found usable (see gpu.Init).
	if gpus := gpu.Count(); gpus > 0 {
		res[corev1.ResourceName(gpu.ResourceName)] = *resource.NewQuantity(int64(gpus), resource.DecimalSI)
	}

	return res
}

// AllocatableResources derives allocatable capacity by reserving control-plane and runtime overhead.
func AllocatableResources(ctx context.Context) corev1.ResourceList {
	total := TotalResources()
	allocatable := total.DeepCopy()

	// Reserve 100m CPU if >= 2 cores
	cpuVal := total.Cpu().MilliValue()
	if cpuVal >= 2000 {
		allocatable[corev1.ResourceCPU] = *resource.NewMilliQuantity(cpuVal-100, resource.DecimalSI)
	}

	// Reserve memory (5% of total, capped between 64Mi and 512Mi)
	memBytes := total.Memory().Value()
	resMem := memBytes / 20
	if resMem < 64*1024*1024 {
		resMem = 64 * 1024 * 1024
	}
	if resMem > 512*1024*1024 {
		resMem = 512 * 1024 * 1024
	}
	if memBytes > resMem {
		allocatable[corev1.ResourceMemory] = *resource.NewQuantity(memBytes-resMem, resource.BinarySI)
	}

	return allocatable
}

func resolveCgroupPath(controller, filename string) string {
	// 1. Try hierarchical path from /proc/self/cgroup
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			parts := strings.SplitN(line, ":", 3)
			if len(parts) == 3 {
				cgroupPath := strings.TrimSpace(parts[2])
				if cgroupPath == "" || cgroupPath == "/" {
					continue
				}
				// cgroup v2
				if parts[0] == "0" && parts[1] == "" {
					cand := filepath.Join("/sys/fs/cgroup", cgroupPath, filename)
					if _, err := os.Stat(cand); err == nil {
						return cand
					}
				}
				// cgroup v1
				controllers := strings.Split(parts[1], ",")
				for _, c := range controllers {
					if c == controller {
						cand := filepath.Join("/sys/fs/cgroup", controller, cgroupPath, filename)
						if _, err := os.Stat(cand); err == nil {
							return cand
						}
					}
				}
			}
		}
	}

	// 2. Default root paths
	candV2 := filepath.Join("/sys/fs/cgroup", filename)
	if _, err := os.Stat(candV2); err == nil {
		return candV2
	}
	candV1 := filepath.Join("/sys/fs/cgroup", controller, filename)
	if _, err := os.Stat(candV1); err == nil {
		return candV1
	}

	return ""
}

// NodeCPUsVariable and NodeMemoryVariable say what the node gives the cluster by amount: a
// number of CPUs and MiB of memory (skiff.sh, skiff_node_grant: in a Slurm job, what Slurm
// gave the job on the node). Unset or empty, the cgroup's limit or the machine's.
const (
	NodeCPUsVariable   = "SKIFF_NODE_CPUS"
	NodeMemoryVariable = "SKIFF_NODE_MEMORY"
)

func getEffectiveCPUMillis() int64 {
	var grantMilli int64

	// 1. What the node gives the cluster
	if val := os.Getenv(NodeCPUsVariable); val != "" {
		if c, err := strconv.ParseInt(val, 10, 64); err == nil && c > 0 {
			grantMilli = c * 1000
		}
	}

	// 2. Check cgroup cpu quota
	var cgroupMilli int64
	// cgroup v2
	if cgroupPath := resolveCgroupPath("cpu", "cpu.max"); cgroupPath != "" {
		if data, err := os.ReadFile(cgroupPath); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) >= 2 && fields[0] != "max" {
				quota, errQ := strconv.ParseInt(fields[0], 10, 64)
				period, errP := strconv.ParseInt(fields[1], 10, 64)
				if errQ == nil && errP == nil && period > 0 {
					cgroupMilli = (quota * 1000) / period
					if cgroupMilli <= 0 {
						cgroupMilli = 1
					}
				}
			}
		}
	}
	// cgroup v1 fallback
	if cgroupMilli == 0 {
		quotaPath := resolveCgroupPath("cpu", "cpu.cfs_quota_us")
		periodPath := resolveCgroupPath("cpu", "cpu.cfs_period_us")
		if quotaPath != "" && periodPath != "" {
			dataQ, errQ := os.ReadFile(quotaPath)
			dataP, errP := os.ReadFile(periodPath)
			if errQ == nil && errP == nil {
				quota, qErr := strconv.ParseInt(strings.TrimSpace(string(dataQ)), 10, 64)
				period, pErr := strconv.ParseInt(strings.TrimSpace(string(dataP)), 10, 64)
				if qErr == nil && pErr == nil && quota > 0 && period > 0 {
					cgroupMilli = (quota * 1000) / period
					if cgroupMilli <= 0 {
						cgroupMilli = 1
					}
				}
			}
		}
	}

	// 3. Selection logic: if both present, take tighter bound (minimum)
	if grantMilli > 0 && cgroupMilli > 0 {
		if cgroupMilli < grantMilli {
			return cgroupMilli
		}
		return grantMilli
	}
	if grantMilli > 0 {
		return grantMilli
	}
	if cgroupMilli > 0 {
		return cgroupMilli
	}

	// 4. Fallback to CPU affinity via runtime.NumCPU()
	cpus := int64(runtime.NumCPU())
	if cpus > 0 {
		return cpus * 1000
	}
	return 1000
}

func getEffectiveMemoryBytes() uint64 {
	var grantBytes uint64

	// 1. What the node gives the cluster
	if val := os.Getenv(NodeMemoryVariable); val != "" {
		if m, err := strconv.ParseUint(val, 10, 64); err == nil && m > 0 {
			grantBytes = m * 1024 * 1024 // MiB
		}
	}

	// 2. Check cgroup memory limit (cgroup v2: memory.max; cgroup v1: memory.limit_in_bytes)
	var cgroupBytes uint64
	if memMaxPath := resolveCgroupPath("memory", "memory.max"); memMaxPath != "" {
		if data, err := os.ReadFile(memMaxPath); err == nil {
			str := strings.TrimSpace(string(data))
			if str != "max" {
				if limit, err := strconv.ParseUint(str, 10, 64); err == nil && limit > 0 && limit < (1<<62) {
					cgroupBytes = limit
				}
			}
		}
	}
	if cgroupBytes == 0 {
		if memV1Path := resolveCgroupPath("memory", "memory.limit_in_bytes"); memV1Path != "" {
			if data, err := os.ReadFile(memV1Path); err == nil {
				str := strings.TrimSpace(string(data))
				if limit, err := strconv.ParseUint(str, 10, 64); err == nil && limit > 0 && limit < (1<<62) {
					cgroupBytes = limit
				}
			}
		}
	}

	// 3. Selection logic: if both present, take tighter bound (minimum)
	if grantBytes > 0 && cgroupBytes > 0 {
		if cgroupBytes < grantBytes {
			return cgroupBytes
		}
		return grantBytes
	}
	if grantBytes > 0 {
		return grantBytes
	}
	if cgroupBytes > 0 {
		return cgroupBytes
	}

	// 4. Fallback to /proc/meminfo
	return getHostTotalMemoryBytes()
}

func getHostTotalMemoryBytes() uint64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 1024 * 1024 * 1024 // 1Gi fallback
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				if kb, err := strconv.ParseUint(parts[1], 10, 64); err == nil {
					return kb * 1024 // bytes
				}
			}
		}
	}
	return 1024 * 1024 * 1024
}

func getEffectiveStorageBytes(path string) uint64 {
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	if err != nil {
		// Try root if path failed
		if errRoot := syscall.Statfs("/", &stat); errRoot != nil {
			return 10 * 1024 * 1024 * 1024 // 10Gi fallback
		}
	}

	return stat.Blocks * uint64(stat.Bsize)
}
