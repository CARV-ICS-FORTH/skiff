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
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"skifflet/internal/compute"
	"skifflet/internal/compute/gpu"
)

// gpuCount is the number of GPUs a container asks for: its limit of nvidia.com/gpu. A pod
// gets a GPU only by asking for it, as with the NVIDIA device plugin; the API server makes
// sure a request, if there is one, is the same number.
func gpuCount(c *corev1.Container) int {
	q := c.Resources.Limits[gpu.ResourceName]
	return int(q.Value())
}

// gpuShare says which of the pod's GPUs a container has: count of them from the offset-th.
type gpuShare struct{ offset, count int }

// planGPUs works out the GPUs a pod needs and which of them each container has, as the
// kubelet's device manager does: the application containers have different GPUs (they
// run together), and an init container has the GPUs of the pod from the first on (it runs
// alone, before them, so its GPUs are theirs afterwards). The pod needs as many as its
// largest init container or all of its application containers together, whichever is more.
func planGPUs(pod *corev1.Pod) (total int, shares map[string]gpuShare) {
	shares = map[string]gpuShare{}
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if n := gpuCount(c); n > 0 {
			shares[c.Name] = gpuShare{0, n}
			total = max(total, n)
		}
	}
	apps := 0
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if n := gpuCount(c); n > 0 {
			shares[c.Name] = gpuShare{apps, n}
			apps += n
		}
	}
	return max(total, apps), shares
}

// reserveGPUs takes the pod's GPUs from the node and gives them back when the pod's live
// resources are released (it ends, or it never started). It returns the devices of each
// container, by name. A node that has fewer free than the pod needs refuses the pod as the
// kubelet does ("UnexpectedAdmissionError"), which the scheduler's own count makes rare.
func reserveGPUs(pl *PodLifecycle, pod *corev1.Pod) (map[string][]gpu.Device, error) {
	total, shares := planGPUs(pod)
	if total == 0 {
		return nil, nil
	}
	owner := string(pod.UID)
	devices, err := gpu.Reserve(owner, total)
	if err != nil {
		return nil, err
	}
	pl.Ledger().Register("GPUs", func() error { gpu.Release(owner); return nil })
	byContainer := make(map[string][]gpu.Device, len(shares))
	for name, s := range shares {
		byContainer[name] = devices[s.offset : s.offset+s.count]
	}
	return byContainer, nil
}

// visibleDevices is the value of NVIDIA_VISIBLE_DEVICES for a container: the UUIDs of its
// GPUs, which is what the device plugin sets.
func visibleDevices(devices []gpu.Device) string {
	ids := make([]string, len(devices))
	for i, d := range devices {
		ids[i] = d.UUID
	}
	return strings.Join(ids, ",")
}

// The handlers of the RuntimeClasses K3s creates for the NVIDIA container runtime ("nvidia"
// and "nvidia-experimental", with the classes of those names), which charts of GPU workloads
// name. Skifflet runs the pods of those classes like any other: the class grants nothing,
// a pod has the GPUs it asks for with its limits.
var nvidiaHandlers = []string{"nvidia", "nvidia-experimental"}

// RuntimeHandlers finds the handler of a RuntimeClass; tests replace it.
var RuntimeHandlers = func(ctx context.Context, name string) (string, error) {
	if compute.K8SClientset == nil {
		return "", fmt.Errorf("no Kubernetes client")
	}
	rc, err := compute.K8SClientset.NodeV1().RuntimeClasses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return rc.Handler, nil
}

// CheckRuntimeClass says whether Skifflet can run a pod of the RuntimeClass: the handlers it
// has are the NVIDIA ones (the container runtime Skiff uses is the same for all pods; the
// class stands for the GPUs, which a pod gets with its limits). The kubelet refuses a pod
// whose class names a handler its runtime does not have, and so does Skifflet.
func CheckRuntimeClass(ctx context.Context, pod *corev1.Pod) error {
	name := pod.Spec.RuntimeClassName
	if name == nil || *name == "" {
		return nil
	}
	handler, err := RuntimeHandlers(ctx, *name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &RuntimeClassRefusal{fmt.Sprintf("unsupported features: Spec.RuntimeClassName (RuntimeClass %q not found)", *name)}
		}
		return fmt.Errorf("cannot read RuntimeClass %q: %w", *name, err)
	}
	if !slices.Contains(nvidiaHandlers, handler) {
		return &RuntimeClassRefusal{fmt.Sprintf("unsupported features: Spec.RuntimeClassName (RuntimeClass %q has the handler %q, and Skifflet has only %s)", *name, handler, strings.Join(nvidiaHandlers, " and "))}
	}
	return nil
}

// A RuntimeClassRefusal says that the pod's RuntimeClass is not one Skifflet runs: the pod is
// refused. Any other error of CheckRuntimeClass (the API server could not be asked) is waited out.
type RuntimeClassRefusal struct{ Message string }

func (e *RuntimeClassRefusal) Error() string { return e.Message }
