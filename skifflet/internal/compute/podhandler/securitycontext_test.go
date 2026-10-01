package podhandler

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
