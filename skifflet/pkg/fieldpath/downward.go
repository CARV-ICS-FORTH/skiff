package fieldpath

import (
	"fmt"
	"math"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The Downward API gives a container facts about its pod, through the environment
// (env valueFrom) or files (downwardAPI and projected volumes). Both use the
// functions here, so a reference means the same in either place, and a reference
// that cannot be honoured is an error, never an empty value.

// PodFieldValue is the value of a fieldRef, as the kubelet computes it: the
// metadata fields (name, namespace, uid, labels and annotations, whole or by key),
// spec.nodeName, spec.serviceAccountName, and the pod's addresses (status.hostIP,
// status.hostIPs, status.podIP, status.podIPs; lists are comma separated).
func PodFieldValue(pod *corev1.Pod, fieldPath string) (string, error) {
	switch fieldPath {
	case "spec.nodeName":
		return pod.Spec.NodeName, nil
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, nil
	case "status.hostIP":
		return pod.Status.HostIP, nil
	case "status.hostIPs":
		ips := make([]string, 0, len(pod.Status.HostIPs))
		for _, ip := range pod.Status.HostIPs {
			ips = append(ips, ip.IP)
		}
		if len(ips) == 0 && pod.Status.HostIP != "" {
			ips = append(ips, pod.Status.HostIP)
		}
		return strings.Join(ips, ","), nil
	case "status.podIP":
		return pod.Status.PodIP, nil
	case "status.podIPs":
		ips := make([]string, 0, len(pod.Status.PodIPs))
		for _, ip := range pod.Status.PodIPs {
			ips = append(ips, ip.IP)
		}
		if len(ips) == 0 && pod.Status.PodIP != "" {
			ips = append(ips, pod.Status.PodIP)
		}
		return strings.Join(ips, ","), nil
	}
	return ExtractFieldPathAsString(pod, fieldPath)
}

// ValidateFieldPath says whether PodFieldValue understands a field path, without a
// pod: it is what a manifest is checked with before anything is launched.
func ValidateFieldPath(fieldPath string) error {
	switch fieldPath {
	case "spec.nodeName", "spec.serviceAccountName", "status.hostIP", "status.hostIPs", "status.podIP", "status.podIPs",
		"metadata.name", "metadata.namespace", "metadata.uid", "metadata.labels", "metadata.annotations":
		return nil
	}
	if path, subscript, ok := SplitMaybeSubscriptedPath(fieldPath); ok {
		switch path {
		case "metadata.annotations":
			if errs := validation.IsQualifiedName(strings.ToLower(subscript)); len(errs) != 0 {
				return fmt.Errorf("invalid key subscript in %s: %s", fieldPath, strings.Join(errs, ";"))
			}
			return nil
		case "metadata.labels":
			if errs := validation.IsQualifiedName(subscript); len(errs) != 0 {
				return fmt.Errorf("invalid key subscript in %s: %s", fieldPath, strings.Join(errs, ";"))
			}
			return nil
		}
		return fmt.Errorf("fieldPath %q does not support subscript", fieldPath)
	}
	return fmt.Errorf("unsupported fieldPath %q", fieldPath)
}

// resourceNames are the resources a resourceFieldRef may name.
var resourceNames = map[string]bool{
	"limits.cpu": true, "limits.memory": true, "limits.ephemeral-storage": true,
	"requests.cpu": true, "requests.memory": true, "requests.ephemeral-storage": true,
}

// ValidateResourceFieldRef checks what a manifest can be checked for: the resource
// is one that can be given, and, in a volume (where the container must be named),
// that the container exists in the pod.
func ValidateResourceFieldRef(pod *corev1.Pod, ref *corev1.ResourceFieldSelector, inVolume bool) error {
	if !resourceNames[ref.Resource] && !strings.HasPrefix(ref.Resource, "limits.hugepages-") && !strings.HasPrefix(ref.Resource, "requests.hugepages-") {
		return fmt.Errorf("unsupported resource %q in resourceFieldRef", ref.Resource)
	}
	if inVolume && FindContainer(pod, ref.ContainerName) == nil {
		return fmt.Errorf("resourceFieldRef names container %q, which the pod does not have", ref.ContainerName)
	}
	return nil
}

// ResourceResolver evaluates a resourceFieldRef for a Downward API volume.
type ResourceResolver func(ref *corev1.ResourceFieldSelector) (string, error)

// FindContainer returns the pod's container (an application or an init container)
// of that name.
func FindContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

// ResourceValue is the value of a resourceFieldRef, as the kubelet computes it: the
// container's request or limit in the divisor's units, rounded up. A limit the
// container does not set is the node's allocatable amount (allocatable, which may be
// nil when it is not known: then such a reference cannot be resolved and is an
// error). own is the container the reference is in, used when the reference names
// none.
func ResourceValue(pod *corev1.Pod, own *corev1.Container, ref *corev1.ResourceFieldSelector, allocatable corev1.ResourceList) (string, error) {
	container := own
	if ref.ContainerName != "" {
		if container = FindContainer(pod, ref.ContainerName); container == nil {
			return "", fmt.Errorf("resourceFieldRef names container %q, which the pod does not have", ref.ContainerName)
		}
	}
	if container == nil {
		return "", fmt.Errorf("resourceFieldRef %q names no container", ref.Resource)
	}
	if err := ValidateResourceFieldRef(pod, ref, false); err != nil {
		return "", err
	}
	kind, name, _ := strings.Cut(ref.Resource, ".")
	resourceName := corev1.ResourceName(name)

	var quantity resource.Quantity
	switch kind {
	case "requests":
		quantity = container.Resources.Requests[resourceName]
	case "limits":
		q, set := container.Resources.Limits[resourceName]
		if !set {
			node, known := allocatable[resourceName]
			if !known {
				return "", fmt.Errorf("container %q sets no %s limit and the node's allocatable %s is not known", container.Name, name, name)
			}
			q = node
		}
		quantity = q
	}

	divisor := ref.Divisor
	if divisor.IsZero() {
		divisor = resource.MustParse("1")
	}
	if resourceName == corev1.ResourceCPU {
		return fmt.Sprint(int64(math.Ceil(float64(quantity.MilliValue()) / float64(divisor.MilliValue())))), nil
	}
	return fmt.Sprint(int64(math.Ceil(float64(quantity.Value()) / float64(divisor.Value())))), nil
}
