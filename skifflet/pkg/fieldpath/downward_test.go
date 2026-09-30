package fieldpath

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "team", UID: "abc-123",
			Labels: map[string]string{"app": "web", "tier": "front"}, Annotations: map[string]string{"note": "hello"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1", ServiceAccountName: "robot",
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
			}, {Name: "side"}},
			InitContainers: []corev1.Container{{Name: "setup"}},
		},
		Status: corev1.PodStatus{
			HostIP: "192.168.64.3", PodIP: "10.244.1.5",
			HostIPs: []corev1.HostIP{{IP: "192.168.64.3"}}, PodIPs: []corev1.PodIP{{IP: "10.244.1.5"}, {IP: "fd00::5"}},
		},
	}
}

func TestPodFieldValue(t *testing.T) {
	pod := testPod()
	for path, want := range map[string]string{
		"metadata.name":                "web",
		"metadata.namespace":           "team",
		"metadata.uid":                 "abc-123",
		"metadata.labels['app']":       "web",
		"metadata.annotations['note']": "hello",
		"metadata.labels['missing']":   "",
		"spec.nodeName":                "node-1",
		"spec.serviceAccountName":      "robot",
		"status.hostIP":                "192.168.64.3",
		"status.hostIPs":               "192.168.64.3",
		"status.podIP":                 "10.244.1.5",
		"status.podIPs":                "10.244.1.5,fd00::5",
	} {
		if got, err := PodFieldValue(pod, path); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	if got, _ := PodFieldValue(pod, "metadata.labels"); got != "app=\"web\"\ntier=\"front\"" {
		t.Errorf("all labels %q", got)
	}
	// Pods known only by the singular addresses still give the lists.
	bare := &corev1.Pod{Status: corev1.PodStatus{PodIP: "10.0.0.9", HostIP: "1.1.1.1"}}
	if got, _ := PodFieldValue(bare, "status.podIPs"); got != "10.0.0.9" {
		t.Errorf("podIPs from podIP: %q", got)
	}
	if got, _ := PodFieldValue(bare, "status.hostIPs"); got != "1.1.1.1" {
		t.Errorf("hostIPs from hostIP: %q", got)
	}
}

// A reference nothing implements is an error, not an empty value.
func TestPodFieldValue_UnsupportedIsAnError(t *testing.T) {
	for _, path := range []string{"spec.nodeSelector", "status.phase", "metadata.generation", "metadata.labels['bad key']", "spec.nodeName['x']", ""} {
		if got, err := PodFieldValue(testPod(), path); err == nil {
			t.Errorf("%q = %q, want an error", path, got)
		}
	}
}

func TestValidateFieldPath(t *testing.T) {
	for _, ok := range []string{"metadata.name", "metadata.labels['app']", "metadata.annotations['a.b/c']", "spec.nodeName", "status.podIPs"} {
		if err := ValidateFieldPath(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"spec.hostname", "status.phase", "metadata.labels['']", "metadata.name['x']", "metadata.labelz"} {
		if err := ValidateFieldPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestResourceValue(t *testing.T) {
	pod := testPod()
	own := &pod.Spec.Containers[0]
	q := resource.MustParse
	for _, c := range []struct {
		ref  corev1.ResourceFieldSelector
		want string
	}{
		{corev1.ResourceFieldSelector{Resource: "limits.cpu"}, "2"},                         // 1500m rounds up in whole cores
		{corev1.ResourceFieldSelector{Resource: "limits.cpu", Divisor: q("100m")}, "15"},    // in units of 100m
		{corev1.ResourceFieldSelector{Resource: "requests.cpu"}, "1"},                       // 250m rounds up
		{corev1.ResourceFieldSelector{Resource: "requests.cpu", Divisor: q("1m")}, "250"},   // millicores
		{corev1.ResourceFieldSelector{Resource: "limits.memory"}, "134217728"},              // bytes
		{corev1.ResourceFieldSelector{Resource: "limits.memory", Divisor: q("1Mi")}, "128"}, // MiB
		{corev1.ResourceFieldSelector{Resource: "requests.memory", Divisor: q("1Mi")}, "64"},
		{corev1.ResourceFieldSelector{Resource: "requests.ephemeral-storage"}, "0"},                                  // nothing requested
		{corev1.ResourceFieldSelector{Resource: "limits.memory", ContainerName: "app", Divisor: q("1Ki")}, "131072"}, // named container
	} {
		if got, err := ResourceValue(pod, own, &c.ref, nil); err != nil || got != c.want {
			t.Errorf("%s (divisor %s): %q, %v; want %q", c.ref.Resource, c.ref.Divisor.String(), got, err, c.want)
		}
	}
}

// A limit the container does not set is the node's allocatable amount, as the
// kubelet does it; without knowing that amount the reference cannot be resolved.
func TestResourceValue_NoLimit(t *testing.T) {
	pod := testPod()
	side := &pod.Spec.Containers[1]
	ref := &corev1.ResourceFieldSelector{Resource: "limits.memory", Divisor: resource.MustParse("1Mi")}
	allocatable := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi"), corev1.ResourceCPU: resource.MustParse("4")}
	if got, err := ResourceValue(pod, side, ref, allocatable); err != nil || got != "2048" {
		t.Errorf("node allocatable as the limit: %q, %v", got, err)
	}
	if got, err := ResourceValue(pod, side, &corev1.ResourceFieldSelector{Resource: "limits.cpu"}, allocatable); err != nil || got != "4" {
		t.Errorf("cpu: %q, %v", got, err)
	}
	if _, err := ResourceValue(pod, side, ref, nil); err == nil || !strings.Contains(err.Error(), "not known") {
		t.Errorf("without the node's allocatable: %v", err)
	}
	// requests need no node
	if got, err := ResourceValue(pod, side, &corev1.ResourceFieldSelector{Resource: "requests.memory"}, nil); err != nil || got != "0" {
		t.Errorf("unset request: %q, %v", got, err)
	}
}

func TestResourceValue_Errors(t *testing.T) {
	pod := testPod()
	own := &pod.Spec.Containers[0]
	for name, ref := range map[string]corev1.ResourceFieldSelector{
		"unknown resource":     {Resource: "limits.gpu"},
		"no kind":              {Resource: "cpu"},
		"container not in pod": {Resource: "limits.cpu", ContainerName: "ghost"},
	} {
		if got, err := ResourceValue(pod, own, &ref, nil); err == nil {
			t.Errorf("%s: %q, want an error", name, got)
		}
	}
	if _, err := ResourceValue(pod, nil, &corev1.ResourceFieldSelector{Resource: "limits.cpu"}, nil); err == nil {
		t.Error("a reference with no container to refer to was accepted")
	}
}

func TestValidateResourceFieldRef(t *testing.T) {
	pod := testPod()
	if err := ValidateResourceFieldRef(pod, &corev1.ResourceFieldSelector{Resource: "limits.hugepages-2Mi", ContainerName: "app"}, true); err != nil {
		t.Errorf("hugepages refused: %v", err)
	}
	if err := ValidateResourceFieldRef(pod, &corev1.ResourceFieldSelector{Resource: "limits.cpu", ContainerName: "setup"}, true); err != nil {
		t.Errorf("an init container is a container: %v", err)
	}
	if err := ValidateResourceFieldRef(pod, &corev1.ResourceFieldSelector{Resource: "limits.cpu", ContainerName: "ghost"}, true); err == nil {
		t.Error("a volume reference to a container the pod lacks was accepted")
	}
	if err := ValidateResourceFieldRef(pod, &corev1.ResourceFieldSelector{Resource: "limits.cpu"}, false); err != nil {
		t.Errorf("an env reference names no container: %v", err)
	}
	if err := ValidateResourceFieldRef(pod, &corev1.ResourceFieldSelector{Resource: "pods"}, false); err == nil {
		t.Error("an unknown resource was accepted")
	}
}
