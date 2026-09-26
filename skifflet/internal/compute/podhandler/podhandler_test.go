package podhandler

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDeletePod_CleanRemoval(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "skiff-delete-pod-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	compute.Skiff = endpoint.SkiffWithPods(tmpDir, filepath.Join(tmpDir, ".skiff", ".pods"))

	podKey := client.ObjectKey{Namespace: "default", Name: "my-pod"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podKey.Name,
			Namespace:   podKey.Namespace,
			UID:         "delete-generation",
			Annotations: map[string]string{"skiff.io/pause-pid": "999999"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", ContainerID: "process://999998"},
			},
		},
	}

	podDir := compute.Skiff.PodWithUID(podKey, pod.GetUID())
	if err := os.MkdirAll(podDir.JobDir(), 0755); err != nil {
		t.Fatalf("failed to create job dir: %v", err)
	}

	ok := DeletePod(podKey, pod)
	if !ok {
		t.Fatalf("expected DeletePod to return true")
	}

	if _, err := os.Stat(podDir.String()); !os.IsNotExist(err) {
		t.Errorf("expected pod directory %s to be removed, but still exists", podDir.String())
	}
}

func TestGetPodIPFromNetns_Fallback(t *testing.T) {
	if goruntime.GOOS == "linux" {
		_, err := GetPodIPFromNetns(999999)
		if err == nil {
			t.Fatalf("expected error on Linux for non-existent PID")
		}
	} else {
		ip, err := GetPodIPFromNetns(999999)
		if err != nil {
			t.Fatalf("unexpected error from fallback: %v", err)
		}
		if ip == "" || strings.HasPrefix(ip, "127.") {
			t.Errorf("expected non-empty non-loopback IP from fallback, got %q", ip)
		}
	}
}
