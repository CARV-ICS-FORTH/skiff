package podhandler

import (
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"skifflet/internal/compute"
)

func TestValidatePodCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		pod        *corev1.Pod
		wantErrSub string
	}{
		{
			name: "valid pod with supported readiness probe",
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
			name: "startup probe rejected",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", StartupProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}},
			}}}},
			wantErrSub: "StartupProbe",
		},
		{
			name: "HTTP liveness probe supported for CoreDNS",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(8080)}}},
			}}}},
		},
		{
			name: "exec liveness probe rejected",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}},
			}}}},
			wantErrSub: "LivenessProbe",
		},
		{
			name: "postStart lifecycle hook rejected",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", Lifecycle: &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}},
			}}}},
			wantErrSub: "PostStart",
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
			name: "grpc probe rejected",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "web",
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									GRPC: &corev1.GRPCAction{Port: 8080},
								},
							},
						},
					},
				},
			},
			wantErrSub: "GRPC",
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
				tt.pod.Name, tt.pod.Namespace, tt.pod.UID = "rejected", "capability-test", "rejected-capability"
				var snapshot *corev1.Pod
				owner := installOwner(t, tt.pod, func(p *corev1.Pod) { snapshot = p })
				defer func() { owner.Terminate(0); GlobalRegistry.DeleteOwner(owner) }()
				<-prepare(owner)
				if snapshot == nil || snapshot.Status.Phase != corev1.PodFailed || snapshot.Status.Reason != compute.ReasonUnsupportedFeatures {
					t.Fatalf("production launcher did not publish unsupported result: %+v", snapshot)
				}
				path := compute.Skiff.PodWithUID(client.ObjectKeyFromObject(tt.pod), tt.pod.UID)
				owner.mu.RLock()
				allocated := owner.sandbox != nil || len(owner.live) != 0
				owner.mu.RUnlock()
				if _, err := os.Stat(path.String()); !os.IsNotExist(err) || allocated || snapshot.Status.PodIP != "" {
					t.Fatal("unsupported capability allocated runtime resources")
				}
			}
		})
	}
}
