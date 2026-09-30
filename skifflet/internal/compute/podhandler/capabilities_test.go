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
			name: "exec startup probe supported",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", StartupProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}},
			}}}},
		},
		{
			name: "HTTP liveness probe supported for CoreDNS",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(8080)}}},
			}}}},
		},
		{
			name: "exec liveness probe supported",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}},
			}}}},
		},
		{
			name: "exec postStart hook supported",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", Lifecycle: &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}},
			}}}},
		},
		{
			name: "HTTP and sleep preStop hooks supported",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "a", Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromString("http"), Path: "/drain"}}}},
				{Name: "b", Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: 5}}}},
			}}},
		},
		{
			name: "TCP hook handler rejected",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(80)}}},
			}}}},
			wantErrSub: "Lifecycle.PreStop.TCPSocket",
		},
		{
			name: "hook without a handler rejected",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", Lifecycle: &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{}},
			}}}},
			wantErrSub: "Lifecycle.PostStart (no handler)",
		},
		{
			name: "hooks of init containers rejected",
			pod: &corev1.Pod{Spec: corev1.PodSpec{InitContainers: []corev1.Container{{
				Name: "setup", Lifecycle: &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}},
			}}}},
			wantErrSub: "InitContainers[0].Lifecycle.PostStart",
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
			name: "named probe ports supported for every probe kind",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:           "web",
				Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
				StartupProbe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("http")}}},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromString("http")}}},
				LivenessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromString("http")}}},
			}}}},
		},
		{
			name: "grpc startup probe rejected",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", StartupProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 8080}}},
			}}}},
			wantErrSub: "StartupProbe.GRPC",
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
