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

func podWith(mutate func(*corev1.PodSpec)) *corev1.Pod {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	mutate(&pod.Spec)
	return pod
}

func always() *corev1.ContainerRestartPolicy {
	p := corev1.ContainerRestartPolicyAlways
	return &p
}

func TestValidatePodCapabilities_RemainingUnsupportedForms(t *testing.T) {
	deadline := int64(60)
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want string // "" means accepted
	}{
		{"restartable init container", podWith(func(s *corev1.PodSpec) {
			s.InitContainers = []corev1.Container{{Name: "sidecar", RestartPolicy: always()}}
		}), "Spec.InitContainers[0].RestartPolicy"},
		{"ordinary init container", podWith(func(s *corev1.PodSpec) { s.InitContainers = []corev1.Container{{Name: "setup"}} }), ""},
		{"pod deadline", podWith(func(s *corev1.PodSpec) { s.ActiveDeadlineSeconds = &deadline }), "Spec.ActiveDeadlineSeconds"},
		{"block device", podWith(func(s *corev1.PodSpec) {
			s.Containers[0].VolumeDevices = []corev1.VolumeDevice{{Name: "raw", DevicePath: "/dev/xvda"}}
		}), "Spec.Containers[0].VolumeDevices"},
		{"NFS volume", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{{Name: "share", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nfs", Path: "/"}}}}
		}), `volume "share": NFS volumes are not supported`},
		{"CSI volume", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{{Name: "c", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "x"}}}}
		}), "CSI volumes are not supported"},
		{"generic ephemeral volume", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{{Name: "e", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}}}
		}), "Ephemeral volumes are not supported"},
		{"supported volumes", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{
				{Name: "a", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "b", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
				{Name: "c", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{}}},
				{Name: "d", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/tmp"}}},
				{Name: "e", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}}},
			}
		}), ""},
		{"projected cluster trust bundle", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{{Name: "p", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{}}, {ClusterTrustBundle: &corev1.ClusterTrustBundleProjection{Path: "x"}}}}}}}
		}), "Spec.Volumes[0].Projected.Sources[1] (ClusterTrustBundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePodCapabilities(tc.pod)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// Downward API references are checked wherever they are: the environment, a
// downwardAPI volume and a projected one. What Skifflet cannot resolve is named
// before the pod is launched, not turned into an empty value.
func TestValidatePodCapabilities_DownwardAPIReferences(t *testing.T) {
	env := func(name string, from corev1.EnvVarSource) corev1.EnvVar {
		return corev1.EnvVar{Name: name, ValueFrom: &from}
	}
	file := func(path string, item corev1.DownwardAPIVolumeFile) corev1.DownwardAPIVolumeFile {
		item.Path = path
		return item
	}
	direct := func(items ...corev1.DownwardAPIVolumeFile) corev1.Volume {
		return corev1.Volume{Name: "d", VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: items}}}
	}
	projected := func(items ...corev1.DownwardAPIVolumeFile) corev1.Volume {
		return corev1.Volume{Name: "p", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{DownwardAPI: &corev1.DownwardAPIProjection{Items: items}}}}}}
	}
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"every supported env field", podWith(func(s *corev1.PodSpec) {
			s.Containers[0].Env = []corev1.EnvVar{
				env("A", corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}),
				env("B", corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels['app']"}}),
				env("C", corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIPs"}}),
				env("D", corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.memory"}}),
			}
		}), ""},
		{"unsupported env field", podWith(func(s *corev1.PodSpec) {
			s.Containers[0].Env = []corev1.EnvVar{env("PHASE", corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.phase"}})}
		}), "Spec.Containers[0].Env[PHASE].ValueFrom.FieldRef"},
		{"unsupported env resource", podWith(func(s *corev1.PodSpec) {
			s.Containers[0].Env = []corev1.EnvVar{env("GPU", corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.nvidia.com/gpu"}})}
		}), "Env[GPU].ValueFrom.ResourceFieldRef"},
		{"file key reference", podWith(func(s *corev1.PodSpec) {
			s.Containers[0].Env = []corev1.EnvVar{env("F", corev1.EnvVarSource{FileKeyRef: &corev1.FileKeySelector{}})}
		}), "FileKeyRef"},
		{"unsupported init container env", podWith(func(s *corev1.PodSpec) {
			s.InitContainers = []corev1.Container{{Name: "i", Env: []corev1.EnvVar{env("X", corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.hostname"}})}}}
		}), "Spec.InitContainers[0].Env[X]"},
		{"volume field", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{direct(file("name", corev1.DownwardAPIVolumeFile{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}))}
		}), ""},
		{"volume resource of a container of the pod", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{direct(file("mem", corev1.DownwardAPIVolumeFile{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "app", Resource: "limits.memory"}}))}
		}), ""},
		{"volume resource of a container the pod lacks", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{direct(file("mem", corev1.DownwardAPIVolumeFile{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "ghost", Resource: "limits.memory"}}))}
		}), `Spec.Volumes[0].DownwardAPI.Items[0].ResourceFieldRef (resourceFieldRef names container "ghost"`},
		{"volume field nothing implements", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{direct(file("x", corev1.DownwardAPIVolumeFile{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.phase"}}))}
		}), "DownwardAPI.Items[0].FieldRef"},
		{"volume item without a reference", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{direct(file("x", corev1.DownwardAPIVolumeFile{}))}
		}), "neither fieldRef nor resourceFieldRef"},
		{"projected field nothing implements", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{projected(file("x", corev1.DownwardAPIVolumeFile{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.phase"}}))}
		}), "Spec.Volumes[0].Projected.Sources[0].DownwardAPI.Items[0].FieldRef"},
		{"projected resource of a container the pod lacks", podWith(func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{projected(file("x", corev1.DownwardAPIVolumeFile{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "ghost", Resource: "limits.cpu"}}))}
		}), "Projected.Sources[0].DownwardAPI.Items[0].ResourceFieldRef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePodCapabilities(tc.pod)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
}
