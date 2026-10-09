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

import "sync"

// The GPUs of this node, found once when Skifflet starts: a job's grant does not change.
var (
	nodeMu   sync.RWMutex
	node     = &Inventory{}
	nodePool = NewPool(nil)
)

// Init finds the GPUs of the node and, when it has any, writes their CDI spec to specDir.
// A step without GPUs is a node without GPUs and no error; GPUs that were granted and
// cannot be used are an error (see Discover), which stops Skifflet: the job asked for GPUs
// and this node would run its pods without them.
func Init(s Sources, specDir string) error {
	inv, err := Discover(s)
	if err != nil {
		return err
	}
	if len(inv.Devices) > 0 {
		if err := inv.WriteSpec(specDir); err != nil {
			return err
		}
	}
	nodeMu.Lock()
	defer nodeMu.Unlock()
	node = inv
	nodePool = NewPool(inv.Devices)
	return nil
}

// Node returns the inventory of the node.
func Node() *Inventory {
	nodeMu.RLock()
	defer nodeMu.RUnlock()
	return node
}

// Count is the number of GPUs the node has to offer: what it advertises as nvidia.com/gpu.
func Count() int { return len(Node().Devices) }

// Reserve gives n GPUs of the node to the pod with this UID (see Pool.Reserve).
func Reserve(owner string, n int) ([]Device, error) {
	nodeMu.RLock()
	pool := nodePool
	nodeMu.RUnlock()
	return pool.Reserve(owner, n)
}

// Release gives back the GPUs of a pod.
func Release(owner string) {
	nodeMu.RLock()
	pool := nodePool
	nodeMu.RUnlock()
	pool.Release(owner)
}
