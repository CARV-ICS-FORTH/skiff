package podhandler

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"skifflet/internal/compute/image"
)

// A pod that sets securityContext fields Skifflet does not apply is told which; the ones it
// applies (the user, the group, runAsNonRoot) and ones that ask for nothing are not listed.
func TestUnappliedSecurityContext(t *testing.T) {
	if got := UnappliedSecurityContext(&corev1.Pod{}); got != "" {
		t.Errorf("a pod without a security context: %q", got)
	}
	applied := &corev1.Pod{Spec: corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr(int64(1000)), RunAsGroup: ptr(int64(2000)), RunAsNonRoot: ptr(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}},
		Containers: []corev1.Container{{Name: "app", SecurityContext: &corev1.SecurityContext{
			RunAsUser: ptr(int64(1000)), AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(false), Privileged: ptr(false)}}},
	}}
	if got := UnappliedSecurityContext(applied); got != "" {
		t.Errorf("only applied or neutral fields, got %q", got)
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{FSGroup: ptr(int64(3000)), SupplementalGroups: []int64{4000},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		InitContainers: []corev1.Container{{Name: "init", SecurityContext: &corev1.SecurityContext{Privileged: ptr(true)}}},
		Containers: []corev1.Container{
			{Name: "app", SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr(true),
				Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}},
			{Name: "plain"},
		},
	}}
	got := UnappliedSecurityContext(pod)
	for _, want := range []string{"pod: fsGroup, supplementalGroups, seccompProfile", "container init: privileged", "container app: capabilities, readOnlyRootFilesystem"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not say %q", got, want)
		}
	}
	if strings.Contains(got, "plain") {
		t.Errorf("a container without a security context is listed: %q", got)
	}
}

// The user a container runs as: the pod's, else the image's, else root; runAsNonRoot refuses
// root as the kubelet does.
func TestDetermineEffectiveRunAsUser(t *testing.T) {
	imgUser := image.User{UID: 101, GID: 102, Set: true}
	for _, c := range []struct {
		name     string
		sc       *corev1.SecurityContext
		img      image.User
		uid, gid int64
		err      string
	}{
		{"nothing: root", &corev1.SecurityContext{}, image.User{}, 0, 0, ""},
		{"the image's user and its group", &corev1.SecurityContext{}, imgUser, 101, 102, ""},
		{"the pod's user over the image's (group 0 as before)", &corev1.SecurityContext{RunAsUser: ptr(int64(7))}, imgUser, 7, 0, ""},
		{"the pod's user and group", &corev1.SecurityContext{RunAsUser: ptr(int64(7)), RunAsGroup: ptr(int64(8))}, imgUser, 7, 8, ""},
		{"the pod's group over the image's, the image's user kept", &corev1.SecurityContext{RunAsGroup: ptr(int64(8))}, imgUser, 101, 8, ""},
		{"runAsNonRoot with a non-root image", &corev1.SecurityContext{RunAsNonRoot: ptr(true)}, imgUser, 101, 102, ""},
		{"runAsNonRoot with a user", &corev1.SecurityContext{RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(5))}, image.User{}, 5, 0, ""},
		{"runAsNonRoot false is nothing", &corev1.SecurityContext{RunAsNonRoot: ptr(false)}, image.User{}, 0, 0, ""},
		{"runAsNonRoot and an image that runs as root", &corev1.SecurityContext{RunAsNonRoot: ptr(true)}, image.User{}, 0, 0, "container has runAsNonRoot and image will run as root"},
		{"runAsNonRoot and an image whose USER is root", &corev1.SecurityContext{RunAsNonRoot: ptr(true)}, image.User{Set: true}, 0, 0, "container has runAsNonRoot and image will run as root"},
		{"runAsNonRoot and runAsUser 0", &corev1.SecurityContext{RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(0))}, imgUser, 0, 0, "container's runAsUser breaks non-root policy"},
		{"runAsUser 0 over a non-root image", &corev1.SecurityContext{RunAsUser: ptr(int64(0))}, imgUser, 0, 0, ""},
	} {
		uid, gid, err := DetermineEffectiveRunAsUser(c.sc, c.img)
		if c.err != "" {
			if !IsConfigError(err) || err.Error() != c.err {
				t.Errorf("%s: error = %v; want a ConfigError %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil || uid != c.uid || gid != c.gid {
			t.Errorf("%s: %d:%d, %v; want %d:%d", c.name, uid, gid, err, c.uid, c.gid)
		}
	}
}

// A ConfigError waits with CreateContainerConfigError unless it says another reason.
func TestConfigReason(t *testing.T) {
	if got := configReason(configErrorf("x")); got != "CreateContainerConfigError" {
		t.Errorf("default reason: %q", got)
	}
	if got := configReason(&ConfigError{Message: "x", Reason: "CreateContainerError"}); got != "CreateContainerError" {
		t.Errorf("its own reason: %q", got)
	}
}
