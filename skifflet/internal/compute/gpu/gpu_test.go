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

package gpu

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeNode is a node's driver files in a directory: GPUs with the given minors (their /proc
// information and /dev files), the control files, libraries (one a link, as in the bubble)
// and nvidia-smi.
func fakeNode(t *testing.T, minors ...int) Sources {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proc, dev, libs, bin := filepath.Join(root, "proc"), filepath.Join(root, "dev"), filepath.Join(root, "libs"), filepath.Join(root, "bin")
	write(filepath.Join(proc, "version"), "NVRM version: NVIDIA UNIX x86_64 Kernel Module  580.178.04  Tue Jul  7 12:38:21 UTC 2026\nGCC version:  gcc version 13.3.0\n")
	for _, m := range minors {
		write(filepath.Join(proc, "gpus", fmt.Sprintf("0000:%02x:00.0", m), "information"),
			fmt.Sprintf("Model: \t\t Quadro P1000\nIRQ:   \t\t 120\nGPU UUID: \t GPU-0000-%d\nBus Location: \t 0000:84:00.0\nDevice Minor: \t %d\nGPU Excluded:\t No\n", m, m))
		write(filepath.Join(dev, fmt.Sprintf("nvidia%d", m)), "")
	}
	for _, n := range []string{"nvidiactl", "nvidia-uvm", "nvidia-uvm-tools"} {
		write(filepath.Join(dev, n), "")
	}
	write(filepath.Join(libs, "libcuda.so.580.178.04"), "cuda")
	write(filepath.Join(libs, "libnvidia-ml.so.580.178.04"), "nvml")
	for _, l := range [][2]string{{"libcuda.so.1", "libcuda.so.580.178.04"}, {"libcuda.so", "libcuda.so.1"}, {"libnvidia-ml.so.1", "libnvidia-ml.so.580.178.04"}} {
		if err := os.Symlink(l[1], filepath.Join(libs, l[0])); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(bin, "nvidia-smi"), "smi")
	return Sources{
		Env:  func(string) string { return "" },
		Proc: proc, Dev: dev, Libs: libs, Bin: bin,
		Open: func(path string) error { _, err := os.Stat(path); return err },
	}
}

func granted(s Sources, list string) Sources {
	s.Env = func(name string) string {
		if name == StepGPUsVariable {
			return list
		}
		return ""
	}
	return s
}

func TestDiscover_NoGrantIsNoGPUs(t *testing.T) {
	for _, list := range []string{"", "  "} {
		inv, err := Discover(granted(fakeNode(t, 0), list))
		if err != nil || len(inv.Devices) != 0 {
			t.Errorf("SLURM_STEP_GPUS=%q: %+v, %v; want an empty inventory", list, inv, err)
		}
	}
}

func TestDiscover_GrantedGPUs(t *testing.T) {
	inv, err := Discover(granted(fakeNode(t, 0, 1, 2), "1-2"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Device{{Minor: 1, UUID: "GPU-0000-1", Model: "Quadro P1000"}, {Minor: 2, UUID: "GPU-0000-2", Model: "Quadro P1000"}}
	if !reflect.DeepEqual(inv.Devices, want) {
		t.Errorf("devices = %+v; want only the granted %+v", inv.Devices, want)
	}
	if inv.Driver != "580.178.04" {
		t.Errorf("driver = %q", inv.Driver)
	}
	if want := []string{"nvidiactl", "nvidia-uvm", "nvidia-uvm-tools"}; len(inv.Nodes) != 3 || filepath.Base(inv.Nodes[0]) != want[0] {
		t.Errorf("nodes = %v; want the control files %v (nvidia-modeset is absent)", inv.Nodes, want)
	}
	if len(inv.Libraries) != 5 || len(inv.Binaries) != 1 {
		t.Errorf("libraries = %v, binaries = %v", inv.Libraries, inv.Binaries)
	}
}

func TestDiscover_AGPUWithoutProcInformationIsNamedByItsMinor(t *testing.T) {
	s := fakeNode(t, 0)
	if err := os.RemoveAll(filepath.Join(s.Proc, "gpus")); err != nil {
		t.Fatal(err)
	}
	inv, err := Discover(granted(s, "0"))
	if err != nil || len(inv.Devices) != 1 || inv.Devices[0].UUID != "0" {
		t.Errorf("%+v, %v", inv, err)
	}
}

func TestDiscover_GrantedAndUnusableIsAnError(t *testing.T) {
	missing := fakeNode(t, 0)
	notOpenable := fakeNode(t, 0)
	notOpenable.Open = func(path string) error {
		if strings.HasSuffix(path, "nvidia0") {
			return errors.New("permission denied")
		}
		return nil
	}
	noLibs := fakeNode(t, 0)
	if err := os.RemoveAll(noLibs.Libs); err != nil {
		t.Fatal(err)
	}
	noCuda := fakeNode(t, 0)
	if err := os.Remove(filepath.Join(noCuda.Libs, "libcuda.so.1")); err != nil {
		t.Fatal(err)
	}
	noCtl := fakeNode(t, 0)
	if err := os.Remove(filepath.Join(noCtl.Dev, "nvidiactl")); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		s    Sources
		list string
		want string
	}{
		"a GPU whose file does not exist":   {missing, "3", "/nvidia3 cannot be opened"},
		"a GPU the step cannot open":        {notOpenable, "0", "nvidia0 cannot be opened"},
		"a bubble without the driver files": {noLibs, "0", "must be started with --nv"},
		"a bubble without libcuda":          {noCuda, "0", "lack libcuda.so.1"},
		"a node without the control file":   {noCtl, "0", "nvidiactl"},
		"a list that is not one":            {fakeNode(t, 0), "0,x", "not a GPU index"},
	} {
		if _, err := Discover(granted(c.s, c.list)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v; want one saying %q", name, err, c.want)
		}
	}
}

func TestParseIndices(t *testing.T) {
	for in, want := range map[string][]int{"0": {0}, "0,1": {0, 1}, "3,0-1,1": {0, 1, 3}, " 2 , 4-5 ": {2, 4, 5}} {
		if got, err := ParseIndices(in); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", ",", "a", "-1", "3-1", "1-"} {
		if got, err := ParseIndices(in); err == nil {
			t.Errorf("%q: %v; want an error", in, got)
		}
	}
}

func TestPool(t *testing.T) {
	p := NewPool([]Device{{Minor: 2}, {Minor: 0}, {Minor: 1}})

	a, err := p.Reserve("a", 2)
	if err != nil || len(a) != 2 || a[0].Minor != 0 || a[1].Minor != 1 {
		t.Fatalf("a = %v, %v; want the two lowest", a, err)
	}
	b, err := p.Reserve("b", 1)
	if err != nil || b[0].Minor != 2 {
		t.Fatalf("b = %v, %v; want the last", b, err)
	}

	_, err = p.Reserve("c", 1)
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || unavailable.Requested != 1 || unavailable.Available != 0 {
		t.Fatalf("c: %v; want UnavailableError for 1 of 0", err)
	}
	if !strings.Contains(err.Error(), "Allocate failed due to requested number of devices unavailable for nvidia.com/gpu. Requested: 1, Available: 0") {
		t.Errorf("message %q is not the kubelet's", err)
	}

	// An owner that has GPUs gets the same again.
	if again, err := p.Reserve("a", 1); err != nil || !reflect.DeepEqual(again, a) {
		t.Errorf("a again = %v, %v; want %v", again, err, a)
	}

	// Released GPUs are free for the next, and an owner that holds none releases nothing.
	p.Release("a")
	p.Release("nobody")
	if got := p.heldLocked("a"); len(got) != 0 {
		t.Errorf("a still holds %v", got)
	}
	c, err := p.Reserve("c", 2)
	if err != nil || c[0].Minor != 0 || c[1].Minor != 1 {
		t.Errorf("c = %v, %v; want the GPUs a had", c, err)
	}
	if _, err := p.Reserve("d", 1); err == nil {
		t.Error("d got a GPU while all are held")
	}
	if _, err := p.Reserve("e", 0); err != nil {
		t.Errorf("a request for none failed: %v", err)
	}
}

func TestCDISpec(t *testing.T) {
	inv, err := Discover(granted(fakeNode(t, 0, 1), "1"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := inv.CDISpec()
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Kind    string
		Devices []struct {
			Name           string
			ContainerEdits struct{ DeviceNodes []struct{ Path string } }
		}
		ContainerEdits struct {
			DeviceNodes []struct{ Path string }
			Mounts      []struct{ HostPath, ContainerPath string }
		}
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Kind != "nvidia.com/gpu" || len(spec.Devices) != 1 || spec.Devices[0].Name != "1" || spec.Devices[0].ContainerEdits.DeviceNodes[0].Path != "/dev/nvidia1" {
		t.Errorf("devices = %+v; want one, named by the minor 1, with its file", spec)
	}
	if len(spec.ContainerEdits.DeviceNodes) != 3 {
		t.Errorf("common nodes = %v", spec.ContainerEdits.DeviceNodes)
	}
	// Every library name is mounted, under /.singularity.d/libs, from the real file.
	got := map[string]string{}
	for _, m := range spec.ContainerEdits.Mounts {
		got[m.ContainerPath] = filepath.Base(m.HostPath)
	}
	if got["/.singularity.d/libs/libcuda.so.1"] != "libcuda.so.580.178.04" || got["/.singularity.d/libs/libcuda.so"] != "libcuda.so.580.178.04" || got["/.singularity.d/libs/libnvidia-ml.so.1"] != "libnvidia-ml.so.580.178.04" {
		t.Errorf("mounts = %v", got)
	}
	if len(spec.ContainerEdits.Mounts) != 6 {
		t.Errorf("%d mounts; want the 5 library names and nvidia-smi", len(spec.ContainerEdits.Mounts))
	}
}

func TestWriteSpecIsWhole(t *testing.T) {
	inv, err := Discover(granted(fakeNode(t, 0), "0"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "cdi")
	for i := 0; i < 2; i++ { // twice: the second replaces the first
		if err := inv.WriteSpec(dir); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "nvidia.json" {
		t.Errorf("%v; want only nvidia.json", entries)
	}
}

func TestInitAndNodeState(t *testing.T) {
	s := granted(fakeNode(t, 0, 1), "0,1")
	if err := Init(s, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Init(Sources{Env: func(string) string { return "" }}, "") })
	if Count() != 2 {
		t.Fatalf("count = %d", Count())
	}
	if _, err := Reserve("pod", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve("other", 1); err == nil {
		t.Error("a third GPU was given")
	}
	Release("pod")
	if _, err := Reserve("other", 1); err != nil {
		t.Errorf("after the release: %v", err)
	}
	// A node without GPUs: nothing to reserve, nothing advertised, no labels.
	if err := Init(Sources{Env: func(string) string { return "" }}, ""); err != nil || Count() != 0 || Node().Labels() != nil {
		t.Errorf("empty node: %v, %d, %v", err, Count(), Node().Labels())
	}
}

func TestLabels(t *testing.T) {
	inv, _ := Discover(granted(fakeNode(t, 0), "0"))
	want := map[string]string{
		"nvidia.com/gpu.present":              "true",
		"nvidia.com/gpu.count":                "1",
		"nvidia.com/gpu.product":              "Quadro-P1000",
		"nvidia.com/cuda.driver-version.full": "580.178.04",
	}
	if got := inv.Labels(); !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v; want %v", got, want)
	}
	if got := labelValue(" NVIDIA A100-SXM4-40GB (x) "); got != "NVIDIA-A100-SXM4-40GB-x" {
		t.Errorf("labelValue = %q", got)
	}
}
