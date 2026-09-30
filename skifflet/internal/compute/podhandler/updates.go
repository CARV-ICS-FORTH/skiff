package podhandler

import (
	"fmt"
	"reflect"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// A running pod's labels and annotations follow the API (PodLifecycle.UpdateFromAPI).
// The other things the API lets a pod's spec change after it was created cannot be
// applied to a container that is already running, and Skifflet says so instead of
// pretending: UnappliedUpdates names each of them.

// UnappliedUpdates returns what the requested pod asks for that the running one
// does not have and Skifflet cannot apply in place: a different image, different
// resources, a pod deadline, ephemeral containers. Each entry is a sentence for the
// user, with the supported alternative. Tolerations are not listed: they only guide
// scheduling, which has already happened.
func UnappliedUpdates(running, requested *corev1.Pod) []string {
	var out []string

	containers := func(kind string, have, want []corev1.Container) {
		byName := map[string]*corev1.Container{}
		for i := range have {
			byName[have[i].Name] = &have[i]
		}
		for i := range want {
			c := &want[i]
			r, ok := byName[c.Name]
			if !ok {
				continue
			}
			if c.Image != r.Image {
				out = append(out, fmt.Sprintf("spec.%s[%s].image is %q but the container runs %q: the image of a running pod cannot be changed in place; delete the pod or roll out a new one (for example with a Deployment)", kind, c.Name, c.Image, r.Image))
			}
			if !reflect.DeepEqual(c.Resources, r.Resources) || !reflect.DeepEqual(c.ResizePolicy, r.ResizePolicy) {
				out = append(out, fmt.Sprintf("spec.%s[%s].resources differ from what the container runs with: resources of a running pod cannot be changed in place; delete the pod or roll out a new one", kind, c.Name))
			}
		}
	}
	containers("containers", running.Spec.Containers, requested.Spec.Containers)
	containers("initContainers", running.Spec.InitContainers, requested.Spec.InitContainers)

	if !equalInt64(running.Spec.ActiveDeadlineSeconds, requested.Spec.ActiveDeadlineSeconds) {
		out = append(out, "spec.activeDeadlineSeconds changed: pod deadlines are not enforced by Skifflet")
	}

	if len(requested.Spec.EphemeralContainers) > len(running.Spec.EphemeralContainers) {
		var names []string
		known := map[string]bool{}
		for _, e := range running.Spec.EphemeralContainers {
			known[e.Name] = true
		}
		for _, e := range requested.Spec.EphemeralContainers {
			if !known[e.Name] {
				names = append(names, e.Name)
			}
		}
		sort.Strings(names)
		out = append(out, fmt.Sprintf("spec.ephemeralContainers %v were added: ephemeral containers are not supported; use kubectl exec in a running container instead", names))
	}
	return out
}

func equalInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// PodEvent records an event about the pod, when events are recorded.
func (l *PodLifecycle) PodEvent(eventType, reason, format string, args ...any) {
	if podEvents == nil {
		return
	}
	podEvents(l.GetPod(), eventType, reason, fmt.Sprintf(format, args...))
}
