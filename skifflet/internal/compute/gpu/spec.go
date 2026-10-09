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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// CDIKind is the kind of the devices of the spec: a container asks for one as
// --device nvidia.com/gpu=<minor number>.
const CDIKind = "nvidia.com/gpu"

// CDIDir is where Apptainer looks for specs (the Container Device Interface's default).
const CDIDir = "/etc/cdi"

// DeviceName is the name of a GPU in the spec, and so what follows "=" in --device.
func DeviceName(d Device) string { return strconv.Itoa(d.Minor) }

// The parts of a CDI spec (github.com/cncf-tags/container-device-interface): only those the
// spec of a node needs.
type (
	cdiSpec struct {
		Version        string   `json:"cdiVersion"`
		Kind           string   `json:"kind"`
		Devices        []cdiDev `json:"devices"`
		ContainerEdits cdiEdits `json:"containerEdits"`
	}
	cdiDev struct {
		Name           string   `json:"name"`
		ContainerEdits cdiEdits `json:"containerEdits"`
	}
	cdiEdits struct {
		DeviceNodes []cdiNode  `json:"deviceNodes,omitempty"`
		Mounts      []cdiMount `json:"mounts,omitempty"`
	}
	cdiNode struct {
		Path string `json:"path"`
	}
	cdiMount struct {
		HostPath      string   `json:"hostPath"`
		ContainerPath string   `json:"containerPath"`
		Options       []string `json:"options"`
	}
)

// containerLibs is where Apptainer looks for libraries a container's loader does not know:
// it ends the LD_LIBRARY_PATH of every container with it (whatever the image set before it
// stays). The driver's libraries are put there, one by one, as --nv puts them.
const containerLibs = "/.singularity.d/libs"

// CDISpec is the CDI spec of the node: one device per GPU (its file), and for every device
// the files all GPU containers need (the driver's control files, its libraries and its
// programs, read-only). A container that is given GPU devices sees these and the files of
// its own GPUs, and nothing else of the driver: with --contain, whose /dev has no other
// node, a GPU that was not given is not there, which the driver's libraries report as no
// device (nvidia-smi: "No devices were found").
func (inv *Inventory) CDISpec() ([]byte, error) {
	spec := cdiSpec{Version: "0.5.0", Kind: CDIKind}
	for _, d := range inv.Devices {
		spec.Devices = append(spec.Devices, cdiDev{
			Name:           DeviceName(d),
			ContainerEdits: cdiEdits{DeviceNodes: []cdiNode{{Path: "/dev/nvidia" + strconv.Itoa(d.Minor)}}},
		})
	}
	for _, path := range inv.Nodes {
		spec.ContainerEdits.DeviceNodes = append(spec.ContainerEdits.DeviceNodes, cdiNode{Path: "/dev/" + filepath.Base(path)})
	}
	options := []string{"ro", "nosuid", "nodev", "bind"}
	libs := append([]string(nil), inv.Libraries...)
	sort.Strings(libs)
	for _, name := range libs {
		// A name in the directory is often a link to the library; the container gets the
		// library under every name it has there.
		real, err := filepath.EvalSymlinks(filepath.Join(inv.libs, name))
		if err != nil {
			return nil, fmt.Errorf("driver library %s: %w", name, err)
		}
		spec.ContainerEdits.Mounts = append(spec.ContainerEdits.Mounts, cdiMount{HostPath: real, ContainerPath: containerLibs + "/" + name, Options: options})
	}
	for _, path := range inv.Binaries {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("driver program %s: %w", path, err)
		}
		spec.ContainerEdits.Mounts = append(spec.ContainerEdits.Mounts, cdiMount{HostPath: real, ContainerPath: path, Options: options})
	}
	return json.MarshalIndent(spec, "", "  ")
}

// WriteSpec writes the spec of the node as dir/nvidia.json, replacing the one there. The
// file appears whole (it is written to another name and renamed), so an Apptainer that
// starts a container at that moment reads one spec or the other.
func (inv *Inventory) WriteSpec(dir string) error {
	data, err := inv.CDISpec()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".nvidia.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "nvidia.json"))
}
