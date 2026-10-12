package podhandler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
)

// While the pod lives, a ConfigMap volume follows its ConfigMap and a Downward API volume the
// pod's labels, as with the kubelet.
func TestSyncVolumes_FollowConfigMapsAndLabels(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "ns"}, Data: map[string]string{"k": "one"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	oldClient, oldPeriod := compute.K8SClient, volumeSyncPeriod
	compute.K8SClient, volumeSyncPeriod = cl, 50*time.Millisecond
	t.Cleanup(func() { compute.K8SClient, volumeSyncPeriod = oldClient, oldPeriod })

	mode := int32(0644)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "u", Labels: map[string]string{"tier": "front"}},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{
			{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}, DefaultMode: &mode}}},
			{Name: "info", VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{DefaultMode: &mode,
				Items: []corev1.DownwardAPIVolumeFile{{Path: "labels", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels"}}}}}},
		}},
	}
	podDir := endpoint.PodPath(t.TempDir())
	if err := os.MkdirAll(podDir.VolumeDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(pod, podDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, vol := range h.Pod.Spec.Volumes {
		if err := h.mountVolumeSource(ctx, vol); err != nil {
			t.Fatal(err)
		}
	}
	read := func(rel string) string {
		data, _ := os.ReadFile(filepath.Join(podDir.VolumeDir(), rel))
		return string(data)
	}
	if read("cfg/k") != "one" || read("info/labels") != `tier="front"` {
		t.Fatalf("mounted: %q %q", read("cfg/k"), read("info/labels"))
	}
	go h.syncVolumes(ctx)

	cm.Data["k"] = "two"
	if err := cl.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	changed := pod.DeepCopy()
	changed.Labels["tier"] = "back"
	h.lifecycle.UpdateFromAPI(changed)
	eventually(t, "the volumes to follow", func() bool { return read("cfg/k") == "two" && read("info/labels") == `tier="back"` })
}
