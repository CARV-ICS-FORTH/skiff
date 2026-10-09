// Copyright © 2026 FORTH-ICS
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

// Package gpu is what Skifflet does for NVIDIA GPUs in place of the kubelet's device
// manager and the NVIDIA device plugin: it finds the GPUs the job granted to the node
// (Discover), says which of them a pod has (Pool), and describes them to Apptainer as a
// CDI spec (Inventory.CDISpec), so that a container sees the GPUs it asked for and no
// others.
package gpu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ResourceName is the extended resource a pod asks GPUs with (resources.limits).
const ResourceName = "nvidia.com/gpu"

// StepGPUsVariable is where Slurm says which GPUs it gave the step the bubble runs in:
// the indices of the node's GPUs, which are the minor numbers of their device files
// (/dev/nvidia<N>). It is set only for a step that was given GPUs, which is the point: a
// job that has GPUs and a step that was not given them (the job's own variable,
// SLURM_JOB_GPUS, is set all the same) cannot use them, as the device cgroup keeps the
// step out of them.
const StepGPUsVariable = "SLURM_STEP_GPUS"

// Device is one GPU of the node.
type Device struct {
	// Minor is the number of its device file, /dev/nvidia<Minor>.
	Minor int
	// UUID is its identity ("GPU-..."), what NVIDIA_VISIBLE_DEVICES names it with.
	UUID string
	// Model is its product name, as the driver reports it.
	Model string
}

// Inventory is what a node has for GPU containers: the GPUs it was granted and the driver
// files that make them usable, as the bubble sees them.
type Inventory struct {
	// Devices are the granted GPUs that can be used, by minor number.
	Devices []Device
	// Driver is the version of the kernel driver.
	Driver string
	// Nodes are the device files every GPU container needs besides its own GPUs'.
	Nodes []string
	// Libraries are the driver's libraries (names in Sources.Libs).
	Libraries []string
	// Binaries are the driver's programs (nvidia-smi), by path.
	Binaries []string

	libs string
}

// Sources are the places an inventory is read from; tests give theirs.
type Sources struct {
	// Env gives the value of an environment variable.
	Env func(string) string
	// Proc is the driver's directory in /proc (/proc/driver/nvidia).
	Proc string
	// Dev is where the device files are (/dev).
	Dev string
	// Libs is where the bubble finds the driver's libraries (/.singularity.d/libs, where
	// Apptainer puts them for a container started with --nv, which is how the bubble starts
	// when its step has GPUs).
	Libs string
	// Bin is where Apptainer puts the driver's programs (/usr/bin).
	Bin string
	// Open says whether a device file can be opened by the process.
	Open func(path string) error
}

// DefaultSources are the places of a real node.
func DefaultSources() Sources {
	return Sources{
		Env:  os.Getenv,
		Proc: "/proc/driver/nvidia",
		Dev:  "/dev",
		Libs: "/.singularity.d/libs",
		Bin:  "/usr/bin",
		Open: func(path string) error {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return f.Close()
		},
	}
}

// commonNodes are the device files of the driver that are not a GPU's own. The first two
// are needed to use any GPU; the others exist when the driver has the feature.
var commonNodes = []string{"nvidiactl", "nvidia-uvm", "nvidia-uvm-tools", "nvidia-modeset"}

// driverPrograms are the programs of the driver Apptainer binds into a container started
// with --nv (its nvliblist.conf); the container gets those the node has.
var driverPrograms = []string{"nvidia-smi", "nvidia-debugdump", "nvidia-persistenced", "nvidia-cuda-mps-control", "nvidia-cuda-mps-server"}

// Discover finds the GPUs the step was given and what a container needs to use them. A
// node whose step has none has an empty inventory and no error. A GPU Slurm granted and
// the node cannot use (its device file cannot be opened, or the driver's libraries are
// not in the bubble) is an error that names it: advertising a GPU a pod cannot use would
// be a lie to the scheduler.
func Discover(s Sources) (*Inventory, error) {
	inv := &Inventory{libs: s.Libs}
	granted := strings.TrimSpace(s.Env(StepGPUsVariable))
	if granted == "" {
		return inv, nil
	}
	minors, err := ParseIndices(granted)
	if err != nil {
		return nil, fmt.Errorf("%s=%q: %w", StepGPUsVariable, granted, err)
	}

	info := readGPUInfo(s.Proc)
	for _, minor := range minors {
		node := filepath.Join(s.Dev, "nvidia"+strconv.Itoa(minor))
		if err := s.Open(node); err != nil {
			return nil, fmt.Errorf("GPU %d is granted to the step by Slurm and %s cannot be opened: %w", minor, node, err)
		}
		d := info[minor]
		d.Minor = minor
		if d.UUID == "" {
			// The driver tells what it knows in /proc; without it the GPU is still usable, and
			// is named by its minor number where a UUID would have gone.
			d.UUID = strconv.Itoa(minor)
		}
		inv.Devices = append(inv.Devices, d)
	}

	for _, name := range commonNodes {
		path := filepath.Join(s.Dev, name)
		if _, err := os.Stat(path); err == nil {
			inv.Nodes = append(inv.Nodes, path)
		} else if name == "nvidiactl" || name == "nvidia-uvm" {
			return nil, fmt.Errorf("the GPUs are granted and %s, which every GPU container needs, is missing: %w", path, err)
		}
	}
	for _, path := range inv.Nodes {
		if err := s.Open(path); err != nil {
			return nil, fmt.Errorf("%s cannot be opened: %w", path, err)
		}
	}

	entries, err := os.ReadDir(s.Libs)
	if err != nil {
		return nil, fmt.Errorf("the GPUs are granted and the driver's libraries are not in the bubble (%s: %w): the bubble must be started with --nv", s.Libs, err)
	}
	for _, e := range entries {
		inv.Libraries = append(inv.Libraries, e.Name())
	}
	if !contains(inv.Libraries, "libcuda.so.1") || !contains(inv.Libraries, "libnvidia-ml.so.1") {
		return nil, fmt.Errorf("the GPUs are granted and the driver's libraries in %s lack libcuda.so.1 or libnvidia-ml.so.1: the bubble must be started with --nv on a node with the NVIDIA driver", s.Libs)
	}
	for _, name := range driverPrograms {
		path := filepath.Join(s.Bin, name)
		if _, err := os.Stat(path); err == nil {
			inv.Binaries = append(inv.Binaries, path)
		}
	}
	inv.Driver = readDriverVersion(s.Proc)
	return inv, nil
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// ParseIndices reads a list of GPU indices as Slurm writes it: numbers separated by
// commas, with ranges ("0,2-3").
func ParseIndices(list string) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, isRange := strings.Cut(part, "-")
		lo, err := strconv.Atoi(from)
		if err != nil || lo < 0 {
			return nil, fmt.Errorf("%q is not a GPU index", part)
		}
		hi := lo
		if isRange {
			if hi, err = strconv.Atoi(to); err != nil || hi < lo {
				return nil, fmt.Errorf("%q is not a range of GPU indices", part)
			}
		}
		for i := lo; i <= hi; i++ {
			if !seen[i] {
				seen[i] = true
				out = append(out, i)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no GPU index")
	}
	sort.Ints(out)
	return out, nil
}

// readGPUInfo reads what the driver says about each GPU in /proc/driver/nvidia/gpus/<bus
// id>/information, by minor number. A process needs nothing but read access for it, not
// even the driver's programs.
func readGPUInfo(proc string) map[int]Device {
	out := map[int]Device{}
	files, _ := filepath.Glob(filepath.Join(proc, "gpus", "*", "information"))
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var d Device
		minor := -1
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			switch strings.TrimSpace(key) {
			case "Model":
				d.Model = value
			case "GPU UUID":
				d.UUID = value
			case "Device Minor":
				if n, err := strconv.Atoi(value); err == nil {
					minor = n
				}
			}
		}
		if minor >= 0 {
			d.Minor = minor
			out[minor] = d
		}
	}
	return out
}

// readDriverVersion reads the version of the kernel module from /proc/driver/nvidia/version
// ("NVRM version: NVIDIA UNIX x86_64 Kernel Module  580.178.04  Tue Jul ...").
func readDriverVersion(proc string) string {
	data, err := os.ReadFile(filepath.Join(proc, "version"))
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(first)
	for i, f := range fields {
		if f == "Module" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// Labels are the node labels the NVIDIA GPU feature discovery gives a node with GPUs, as far
// as the driver's files tell: that it has them, how many and of what product (with the
// product name written as a label value, "Quadro-P1000").
func (inv *Inventory) Labels() map[string]string {
	if inv == nil || len(inv.Devices) == 0 {
		return nil
	}
	labels := map[string]string{
		"nvidia.com/gpu.present": "true",
		"nvidia.com/gpu.count":   strconv.Itoa(len(inv.Devices)),
	}
	if model := inv.Devices[0].Model; model != "" {
		labels["nvidia.com/gpu.product"] = labelValue(model)
	}
	if inv.Driver != "" {
		labels["nvidia.com/cuda.driver-version.full"] = inv.Driver
	}
	return labels
}

// labelValue turns a product name into a valid label value: spaces become dashes and
// anything else outside [A-Za-z0-9._-] is dropped.
func labelValue(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-_.")
}
