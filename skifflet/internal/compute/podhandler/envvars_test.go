package podhandler

import (
	"context"
	"testing"

	"skifflet/internal/compute"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A pod finds the API server through its Service, as with the kubelet: the variables are the
// Service's cluster IP and port, not the address Skifflet itself uses.
func TestFromServices_KubernetesServiceAsItIs(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	k8sSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: metav1.NamespaceDefault},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.43.0.1",
			Ports:     []corev1.ServicePort{{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP}},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(k8sSvc).Build()
	oldClient := compute.K8SClient
	defer func() { compute.K8SClient = oldClient }()
	compute.K8SClient = client

	envVars, err := FromServices(context.Background(), "team", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	envMap := make(map[string]string)
	for _, env := range envVars {
		envMap[env.Name] = env.Value
	}
	for key, val := range map[string]string{
		"KUBERNETES_SERVICE_HOST":       "10.43.0.1",
		"KUBERNETES_SERVICE_PORT":       "443",
		"KUBERNETES_SERVICE_PORT_HTTPS": "443",
		"KUBERNETES_PORT":               "tcp://10.43.0.1:443",
		"KUBERNETES_PORT_443_TCP_ADDR":  "10.43.0.1",
	} {
		if got, ok := envMap[key]; !ok || got != val {
			t.Errorf("env var %s = %q (set %v); want %q", key, got, ok, val)
		}
	}
}
