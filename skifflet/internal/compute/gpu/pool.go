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
	"fmt"
	"sort"
	"sync"
)

// A Pool says which pod has which GPU of the node. A pod takes the GPUs it needs when it
// starts and gives them back when it ends (or never started), as the kubelet's device
// manager does; no GPU is ever in two pods.
//
// Nothing is kept on disk: a Skifflet that stops ends the job (and the pods with it), so the
// owners of the GPUs cannot outlive the Pool.
type Pool struct {
	mu      sync.Mutex
	devices []Device
	owner   map[int]string // minor number -> the owner holding it
}

// NewPool returns a Pool of the GPUs of an inventory.
func NewPool(devices []Device) *Pool {
	d := append([]Device(nil), devices...)
	sort.Slice(d, func(i, j int) bool { return d[i].Minor < d[j].Minor })
	return &Pool{devices: d, owner: map[int]string{}}
}

// UnavailableError says that a pod asked for more GPUs than the node has free.
type UnavailableError struct{ Requested, Available int }

// Error is the message of the kubelet's device manager.
func (e *UnavailableError) Error() string {
	return fmt.Sprintf("Allocate failed due to requested number of devices unavailable for %s. Requested: %d, Available: %d, which is unexpected", ResourceName, e.Requested, e.Available)
}

// Reserve gives n GPUs to the owner (a pod's UID), the lowest minor numbers that are free.
// An owner that holds GPUs already gets them again, whatever n is: the GPUs of a pod are
// the same for as long as the pod lives.
func (p *Pool) Reserve(owner string, n int) ([]Device, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if held := p.heldLocked(owner); len(held) > 0 {
		return held, nil
	}
	var free []Device
	for _, d := range p.devices {
		if _, taken := p.owner[d.Minor]; !taken {
			free = append(free, d)
		}
	}
	if n > len(free) {
		return nil, &UnavailableError{Requested: n, Available: len(free)}
	}
	got := free[:n:n]
	for _, d := range got {
		p.owner[d.Minor] = owner
	}
	return append([]Device(nil), got...), nil
}

// Release gives back the GPUs of an owner, which may hold none.
func (p *Pool) Release(owner string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for minor, o := range p.owner {
		if o == owner {
			delete(p.owner, minor)
		}
	}
}

// Held returns the GPUs the owner holds.
func (p *Pool) Held(owner string) []Device {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.heldLocked(owner)
}

func (p *Pool) heldLocked(owner string) []Device {
	var held []Device
	for _, d := range p.devices {
		if p.owner[d.Minor] == owner {
			held = append(held, d)
		}
	}
	return held
}
