package downwardapi

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team", Labels: map[string]string{"app": "web"}}}
}

func mode() *int32 { m := int32(0644); return &m }

func TestCollectData_FieldsAndResources(t *testing.T) {
	resolver := func(ref *corev1.ResourceFieldSelector) (string, error) {
		if ref.ContainerName == "ghost" {
			return "", errors.New("no such container")
		}
		return "128", nil
	}
	items := []corev1.DownwardAPIVolumeFile{
		{Path: "name", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		{Path: "app", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels['app']"}},
		{Path: "mem", ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "app", Resource: "limits.memory"}},
	}
	data, err := CollectData(items, pod(), mode(), resolver)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"name": "web", "app": "web", "mem": "128"} {
		if got := string(data[path].Data); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

// A reference that cannot be honoured is an error of the volume, naming the path.
func TestCollectData_UnresolvableIsAnError(t *testing.T) {
	failing := func(*corev1.ResourceFieldSelector) (string, error) { return "", errors.New("no such container") }
	for name, tc := range map[string]struct {
		item     corev1.DownwardAPIVolumeFile
		resolver func(*corev1.ResourceFieldSelector) (string, error)
	}{
		"a field nothing implements":     {corev1.DownwardAPIVolumeFile{Path: "p", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.phase"}}, nil},
		"a resource that fails":          {corev1.DownwardAPIVolumeFile{Path: "p", ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "ghost", Resource: "limits.cpu"}}, failing},
		"a resource with no resolver":    {corev1.DownwardAPIVolumeFile{Path: "p", ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "app", Resource: "limits.cpu"}}, nil},
		"an item that refers to nothing": {corev1.DownwardAPIVolumeFile{Path: "p"}, nil},
	} {
		_, err := CollectData([]corev1.DownwardAPIVolumeFile{tc.item}, pod(), mode(), tc.resolver)
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := CollectData(nil, pod(), nil, nil); err == nil || !strings.Contains(err.Error(), "defaultMode") {
		t.Errorf("without a default mode: %v", err)
	}
}
