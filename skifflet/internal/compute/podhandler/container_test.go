package podhandler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolveContainerEnvironment_FieldRefAndCIdentifier(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "test-ns",
			UID:       "uid-1234",
		},
		Status: corev1.PodStatus{
			PodIP:  "10.244.0.42",
			HostIP: "192.168.64.3",
		},
	}

	container := &corev1.Container{
		Name: "test-container",
		Env: []corev1.EnvVar{
			{Name: "MY_POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
			{Name: "MY_HOST_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
			{Name: "MY_POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "MY_POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "MY_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
			{Name: "OVERRIDE_VAR", Value: "explicit-wins"},
			{Name: "invalid-hyphen-var", Value: "should-be-omitted-from-envfile"},
		},
	}

	serviceEnvs := []corev1.EnvVar{
		{Name: "KUBERNETES_SERVICE_HOST", Value: "10.43.0.1"},
		{Name: "OVERRIDE_VAR", Value: "service-loses"},
	}

	resolved := ResolveContainerEnvironment(pod, container, serviceEnvs)

	resMap := make(map[string]string)
	for _, e := range resolved {
		resMap[e.Name] = e.Value
	}

	if resMap["MY_POD_IP"] != "10.244.0.42" {
		t.Fatalf("expected MY_POD_IP=10.244.0.42, got %q", resMap["MY_POD_IP"])
	}
	if resMap["MY_HOST_IP"] != "192.168.64.3" {
		t.Fatalf("expected MY_HOST_IP=192.168.64.3, got %q", resMap["MY_HOST_IP"])
	}
	if resMap["MY_POD_NAME"] != "test-pod" {
		t.Fatalf("expected MY_POD_NAME=test-pod, got %q", resMap["MY_POD_NAME"])
	}
	if resMap["MY_POD_NAMESPACE"] != "test-ns" {
		t.Fatalf("expected MY_POD_NAMESPACE=test-ns, got %q", resMap["MY_POD_NAMESPACE"])
	}
	if resMap["MY_POD_UID"] != "uid-1234" {
		t.Fatalf("expected MY_POD_UID=uid-1234, got %q", resMap["MY_POD_UID"])
	}
	if resMap["OVERRIDE_VAR"] != "explicit-wins" {
		t.Fatalf("expected OVERRIDE_VAR=explicit-wins, got %q", resMap["OVERRIDE_VAR"])
	}
	if resMap["KUBERNETES_SERVICE_HOST"] != "10.43.0.1" {
		t.Fatalf("expected KUBERNETES_SERVICE_HOST=10.43.0.1, got %q", resMap["KUBERNETES_SERVICE_HOST"])
	}
}
