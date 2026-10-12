package provider

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The node says which container runtime runs its pods (the Apptainer in use, read once), has
// no kube-proxy version, and advertises no "storage" resource (no kubelet does).
func TestNodeSystemInfoAndCapacity(t *testing.T) {
	old := apptainerVersion
	t.Cleanup(func() { apptainerVersion = old })
	apptainerVersion = func() (string, error) { return "1.5.4", nil }
	info := (&VirtualK8S{}).NodeSystemInfo(context.Background())
	if info.ContainerRuntimeVersion != "apptainer://1.5.4" {
		t.Fatalf("container runtime %q, want apptainer://1.5.4", info.ContainerRuntimeVersion)
	}
	if info.KubeProxyVersion != "" {
		t.Fatalf("kube-proxy version %q, want none", info.KubeProxyVersion)
	}
	node := (&VirtualK8S{}).NewVirtualNode(context.Background(), "n", nil)
	if _, ok := node.Status.Capacity[corev1.ResourceStorage]; ok {
		t.Fatal("the node advertises a storage resource")
	}
}
