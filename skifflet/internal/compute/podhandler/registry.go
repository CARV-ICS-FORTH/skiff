package podhandler

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodRegistry manages active PodLifecycle instances keyed by UID with an index by ObjectKey.
type PodRegistry struct {
	mu    sync.RWMutex
	byUID map[types.UID]*PodLifecycle
	byKey map[client.ObjectKey]types.UID
}

// GlobalRegistry is the default singleton pod registry.
var GlobalRegistry = NewPodRegistry()

// NewPodRegistry creates an empty PodRegistry.
func NewPodRegistry() *PodRegistry {
	return &PodRegistry{
		byUID: make(map[types.UID]*PodLifecycle),
		byKey: make(map[client.ObjectKey]types.UID),
	}
}

// Register registers a new PodLifecycle. If a previous pod with the same ObjectKey exists under
// a different UID, the old pod is cancelled to prevent resurrection or stale interactions.
func (r *PodRegistry) Register(lifecycle *PodLifecycle) *PodLifecycle {
	r.mu.Lock()
	defer r.mu.Unlock()

	uid := lifecycle.UID()
	key := lifecycle.Key()

	// Cancel old pod generation if UID differs
	var oldLifecycle *PodLifecycle
	if oldUID, exists := r.byKey[key]; exists && oldUID != uid {
		oldLifecycle = r.byUID[oldUID]
		if oldLifecycle != nil {
			oldLifecycle.cancel()
		}
	}

	r.byUID[uid] = lifecycle
	r.byKey[key] = uid
	return oldLifecycle
}

// Get returns the PodLifecycle for a given UID.
func (r *PodRegistry) Get(uid types.UID) *PodLifecycle {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byUID[uid]
}

// GetByKey returns the currently active PodLifecycle for a given namespaced ObjectKey.
func (r *PodRegistry) GetByKey(key client.ObjectKey) *PodLifecycle {
	r.mu.RLock()
	defer r.mu.RUnlock()

	uid, ok := r.byKey[key]
	if !ok {
		return nil
	}
	return r.byUID[uid]
}

// Delete removes a PodLifecycle from the registry.
func (r *PodRegistry) Delete(uid types.UID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if lifecycle, ok := r.byUID[uid]; ok {
		key := lifecycle.Key()
		if r.byKey[key] == uid {
			delete(r.byKey, key)
		}
		delete(r.byUID, uid)
	}
}

// All returns a slice of all active PodLifecycles.
func (r *PodRegistry) All() []*PodLifecycle {
	r.mu.RLock()
	defer r.mu.RUnlock()

	res := make([]*PodLifecycle, 0, len(r.byUID))
	for _, l := range r.byUID {
		res = append(res, l)
	}
	return res
}
