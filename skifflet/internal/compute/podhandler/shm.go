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

package podhandler

import (
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"

	"skifflet/internal/compute/runtime"
	"skifflet/pkg/tmpfs"
)

// MountTmpfs and UnmountTmpfs make and remove the memory volumes of pods (a pod's /dev/shm
// and its emptyDirs of medium Memory); tests replace them, since they run without the
// authority to mount.
var (
	MountTmpfs   = tmpfs.Mount
	UnmountTmpfs = tmpfs.Unmount
)

// ShmSize is the size of a pod's /dev/shm: the 64 MiB the container runtimes of Kubernetes
// give a pod, whatever its memory. A pod that needs more (a data loader of PyTorch, an MPI
// job) mounts an emptyDir of medium Memory at /dev/shm, with the size it needs.
const ShmSize = 64 << 20

// mountShm mounts the pod's /dev/shm, a tmpfs every container of the pod has (as the
// containers of a pod share the IPC namespace and with it /dev/shm), unless the container
// mounts a volume there itself (see BuildApptainerArgs). It is unmounted with the rest of
// the pod's live resources.
func (h *PodHandler) mountShm() error {
	dir := h.podDirectory.ShmDir()
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	if err := MountTmpfs(dir, tmpfs.Options{Size: ShmSize, Mode: os.ModeSticky | 0o777, Restricted: true}); err != nil {
		return err
	}
	h.lifecycle.Ledger().Register("/dev/shm", func() error { return UnmountTmpfs(dir) })
	return nil
}

// mountMemoryVolume makes an emptyDir of medium Memory a tmpfs, as the kubelet does: as
// large as its sizeLimit says, else as much as the node can give a pod (its allocatable
// memory). What a pod writes there counts as the memory of the job, and is gone with the
// pod.
func (h *PodHandler) mountMemoryVolume(dir string, source *corev1.EmptyDirVolumeSource) error {
	size := int64(0)
	if source.SizeLimit != nil {
		size = source.SizeLimit.Value()
	}
	if size <= 0 {
		allocatable := runtime.AllocatableResources(h.lifecycle.Context())
		memory := allocatable[corev1.ResourceMemory]
		size = memory.Value()
	}
	if err := MountTmpfs(dir, tmpfs.Options{Size: size, Mode: 0o777}); err != nil {
		return err
	}
	h.lifecycle.Ledger().Register("memory volume "+filepath.Base(dir), func() error { return UnmountTmpfs(dir) })
	return nil
}
