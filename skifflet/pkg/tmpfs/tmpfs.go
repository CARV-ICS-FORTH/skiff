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

// Package tmpfs mounts memory-backed directories: the volumes of a pod that are in memory
// (an emptyDir with medium Memory) and its /dev/shm.
package tmpfs

import "os"

// Options say how a tmpfs is mounted.
type Options struct {
	// Size is the most it holds, in bytes. Its pages count as the memory of the job's cgroup,
	// as they do for a pod of Kubernetes.
	Size int64
	// Mode is the permissions of its root directory.
	Mode os.FileMode
	// Restricted mounts it as a container runtime mounts /dev/shm: no set-user-id programs,
	// no device files, nothing in it executed.
	Restricted bool
}
