package podhandler

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestValidatePodCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		pod        *corev1.Pod
		wantErrSub string
	}{
		{
			name: "valid pod",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "web",
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Port: intstr.FromInt(8080),
									},
								},
							},
						},
					},
				},
			},
			wantErrSub: "",
		},
		{
			name: "preStop lifecycle hook rejected",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "web",
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "sleep 1"}},
								},
							},
						},
					},
				},
			},
			wantErrSub: "PreStop",
		},
		{
			name: "startupProbe rejected",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "web",
							StartupProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(8080)},
								},
							},
						},
					},
				},
			},
			wantErrSub: "StartupProbe",
		},
		{
			name: "livenessProbe rejected",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "web",
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(8080)},
								},
							},
						},
					},
				},
			},
			wantErrSub: "LivenessProbe",
		},
		{
			name: "named probe port rejected",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "web",
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromString("http")},
								},
							},
						},
					},
				},
			},
			wantErrSub: "NamedPort",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePodCapabilities(tt.pod)
			if tt.wantErrSub == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrSub)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErrSub, err)
				}
			}
		})
	}
}
