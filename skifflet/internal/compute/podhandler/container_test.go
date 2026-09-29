package podhandler

import (
	"os"
	"path/filepath"
	"skifflet/internal/compute/endpoint"
	"strings"
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

	resolved, err := ResolveContainerEnvironment(pod, container, serviceEnvs)
	if err != nil {
		t.Fatalf("unexpected error resolving env: %v", err)
	}

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

func TestResolveContainerEnvironment_MissingNonOptional(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
	}

	isOptional := false
	container := &corev1.Container{
		Name: "test-container",
		Env: []corev1.EnvVar{
			{
				Name: "REQUIRED_CONFIG",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "missing-cm"},
						Key:                  "key",
						Optional:             &isOptional,
					},
				},
			},
		},
	}

	_, err := ResolveContainerEnvironment(pod, container, nil)
	if err == nil {
		t.Fatalf("expected error resolving missing non-optional configmap, got nil")
	}
}

func TestBuildContainerRejectsMissingSourceSubPath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source corev1.VolumeSource
	}{
		{"configmap", corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
		{"secret", corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{}}},
		{"hostpath", corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{}}},
		{"projected", corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}},
		{"downwardapi", corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{}}},
	} {
		for _, subPath := range []string{"missing.conf", "missing-dir/"} {
			t.Run(tc.name+"/"+subPath, func(t *testing.T) {
				podDir := endpoint.PodPath(t.TempDir())
				for _, dir := range []string{podDir.JobDir(), filepath.Join(podDir.VolumeDir(), "config")} {
					if err := os.MkdirAll(dir, 0755); err != nil {
						t.Fatal(err)
					}
				}
				pod := &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "config", VolumeSource: tc.source}}}}
				h := newTestHandler(pod, podDir)
				imagePath := filepath.Join(t.TempDir(), "fixture.sif")
				if err := os.WriteFile(imagePath, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				container := &corev1.Container{Name: "app", Image: imagePath, ImagePullPolicy: corev1.PullNever, VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/config", SubPath: subPath}}}
				_, err := h.buildContainer(container)
				if err == nil || !strings.Contains(err.Error(), "subPath source does not exist") {
					t.Errorf("missing source should fail before image preparation: %v", err)
				}
				if _, err := os.Stat(filepath.Join(podDir.VolumeDir(), "config", subPath)); !os.IsNotExist(err) {
					t.Fatalf("fabricated missing source: %v", err)
				}
			})
		}
	}
}

func TestBuildContainerPreservesSupportedSubPaths(t *testing.T) {
	for _, existing := range []bool{true, false} {
		name := "existing-config-file"
		source := corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}
		if !existing {
			name = "emptydir-dotted-directory"
			source = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
		}
		t.Run(name, func(t *testing.T) {
			podDir := endpoint.PodPath(t.TempDir())
			volumeDir := filepath.Join(podDir.VolumeDir(), "config")
			for _, dir := range []string{podDir.JobDir(), volumeDir} {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			subPath := "config.value"
			sourcePath := filepath.Join(volumeDir, subPath)
			if existing {
				if err := os.WriteFile(sourcePath, []byte("original-value"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			imagePath := filepath.Join(t.TempDir(), "fixture.sif")
			if err := os.WriteFile(imagePath, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			h := newTestHandler(&corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "config", VolumeSource: source}}}}, podDir)
			container, err := h.buildContainer(&corev1.Container{Name: "app", Image: imagePath, ImagePullPolicy: corev1.PullNever, VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/config", SubPath: subPath}}})
			if err != nil {
				t.Fatal(err)
			}
			if len(container.Binds) != 1 || container.Binds[0] != sourcePath+":/config:rw" {
				t.Fatalf("wrong subpath bind: %v", container.Binds)
			}
			if existing {
				data, err := os.ReadFile(sourcePath)
				if err != nil || string(data) != "original-value" {
					t.Fatalf("existing source changed: %q, %v", data, err)
				}
			} else {
				info, err := os.Stat(sourcePath)
				if err != nil || !info.IsDir() {
					t.Fatalf("emptyDir subpath was not a directory: %v", err)
				}
			}
		})
	}
}
