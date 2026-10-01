package podhandler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"skifflet/internal/compute"
)

// A claim bound to a volume whose path is outside what the bubble sees fails with the
// reason and creates nothing; one inside is linked into the pod.
func TestPersistentVolumeClaimSource_VolumePathMustBeVisible(t *testing.T) {
	scheme := kruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(compute.Skiff.String(), "volumes", "job-1", "pvc-1")
	for _, tc := range []struct {
		name   string
		source corev1.PersistentVolumeSource
		want   string // "" means mounted
	}{
		{"hostPath in the state root", corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: inside}}, ""},
		{"local volume in the scratch root", corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: compute.Skiff.TmpDir()}}, ""},
		{"hostPath elsewhere", corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/home/alice/results"}}, "outside the locations a volume may use"},
		{"local volume elsewhere", corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: "/mnt/fast"}}, "outside the locations a volume may use"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "data"},
				Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-data"},
				Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
			}
			pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-data"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: tc.source}}
			old := compute.K8SClient
			compute.K8SClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, pv).Build()
			t.Cleanup(func() { compute.K8SClient = old })

			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: types.UID("uid-" + strings.ReplaceAll(tc.name, " ", "-"))}}
			h := &PodHandler{Pod: pod, podKey: client.ObjectKeyFromObject(pod), logger: logr.Discard(),
				podDirectory: compute.Skiff.PodWithUID(client.ObjectKeyFromObject(pod), pod.UID)}
			if err := os.MkdirAll(h.podDirectory.VolumeDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(h.podDirectory.String()) })

			vol := corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}
			err := h.PersistentVolumeClaimSource(context.Background(), vol)
			link := filepath.Join(h.podDirectory.VolumeDir(), "data")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if _, err := os.Lstat(link); err != nil {
					t.Fatalf("the volume was not linked into the pod: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), compute.Skiff.String()) {
				t.Fatalf("error %v, want one containing %q and naming %s", err, tc.want, compute.Skiff.String())
			}
			if _, err := os.Lstat(link); err == nil {
				t.Fatal("a volume outside the supported locations was linked into the pod")
			}
		})
	}
}
