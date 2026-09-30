package podhandler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeConfig serves ConfigMaps, Secrets and the node's allocatable resources from
// memory. It is read by the goroutine that prepares a container while a test
// changes it, so what it serves is guarded.
type fakeConfig struct {
	mu          sync.Mutex
	configMaps  map[string]*corev1.ConfigMap
	secrets     map[string]*corev1.Secret
	allocatable corev1.ResourceList
	failWith    error // every read fails with this (a transient error) when set
	reads       int
}

// setConfigMaps replaces the ConfigMaps served.
func (f *fakeConfig) setConfigMaps(m map[string]*corev1.ConfigMap) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configMaps = m
}

func (f *fakeConfig) ConfigMap(_ context.Context, ns, name string) (*corev1.ConfigMap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.failWith != nil {
		return nil, f.failWith
	}
	if cm, ok := f.configMaps[ns+"/"+name]; ok {
		return cm, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
}

func (f *fakeConfig) Secret(_ context.Context, ns, name string) (*corev1.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.failWith != nil {
		return nil, f.failWith
	}
	if s, ok := f.secrets[ns+"/"+name]; ok {
		return s, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
}

func (f *fakeConfig) Allocatable(context.Context, string) (corev1.ResourceList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.allocatable == nil {
		return nil, errors.New("node unknown")
	}
	return f.allocatable, nil
}

func useConfig(t *testing.T, f *fakeConfig) {
	t.Helper()
	old := Config
	Config = f
	t.Cleanup(func() { Config = old })
}

func envOf(t *testing.T, pod *corev1.Pod, c *corev1.Container, service []corev1.EnvVar) map[string]string {
	t.Helper()
	resolved, err := ResolveContainerEnvironment(context.Background(), pod, c, service)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range resolved {
		out[e.Name] = e.Value
	}
	return out
}

func fieldEnv(name, path string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: path}}}
}

func resourceEnv(name, res, divisor string) corev1.EnvVar {
	ref := &corev1.ResourceFieldSelector{Resource: res}
	if divisor != "" {
		ref.Divisor = resource.MustParse(divisor)
	}
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: ref}}
}

func envPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "team", UID: "u-1",
			Labels: map[string]string{"app": "web"}, Annotations: map[string]string{"note": "hi"},
		},
		Spec: corev1.PodSpec{NodeName: "node-1", ServiceAccountName: "robot"},
		Status: corev1.PodStatus{
			PodIP: "10.244.1.5", HostIP: "192.168.64.3",
			PodIPs: []corev1.PodIP{{IP: "10.244.1.5"}}, HostIPs: []corev1.HostIP{{IP: "192.168.64.3"}},
		},
	}
}

// Every pod fact the Downward API offers reaches the environment: what used to be
// an empty value (node name, service account, labels and annotations by key, the
// address lists) is now the fact.
func TestEnvironment_DownwardAPIFieldRefs(t *testing.T) {
	useConfig(t, &fakeConfig{})
	c := &corev1.Container{Name: "app", Env: []corev1.EnvVar{
		fieldEnv("NODE", "spec.nodeName"), fieldEnv("SA", "spec.serviceAccountName"),
		fieldEnv("APP", "metadata.labels['app']"), fieldEnv("NOTE", "metadata.annotations['note']"),
		fieldEnv("IPS", "status.podIPs"), fieldEnv("HOSTS", "status.hostIPs"),
		fieldEnv("IP", "status.podIP"), fieldEnv("NAME", "metadata.name"),
	}}
	got := envOf(t, envPod(), c, nil)
	for k, want := range map[string]string{
		"NODE": "node-1", "SA": "robot", "APP": "web", "NOTE": "hi", "IPS": "10.244.1.5", "HOSTS": "192.168.64.3", "IP": "10.244.1.5", "NAME": "web",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

func TestEnvironment_ResourceFieldRefs(t *testing.T) {
	useConfig(t, &fakeConfig{allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}})
	pod := envPod()
	c := &corev1.Container{Name: "app",
		Resources: corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")},
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		Env: []corev1.EnvVar{
			resourceEnv("CPU_LIMIT", "limits.cpu", ""),
			resourceEnv("CPU_MILLI", "limits.cpu", "1m"),
			resourceEnv("MEM_REQUEST_MI", "requests.memory", "1Mi"),
			resourceEnv("MEM_LIMIT_MI", "limits.memory", "1Mi"), // no limit set: the node's 4Gi
		}}
	got := envOf(t, pod, c, nil)
	for k, want := range map[string]string{"CPU_LIMIT": "2", "CPU_MILLI": "1500", "MEM_REQUEST_MI": "64", "MEM_LIMIT_MI": "4096"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// What cannot be resolved is a configuration error naming the variable, not an empty value.
func TestEnvironment_UnresolvableReferencesAreConfigErrors(t *testing.T) {
	useConfig(t, &fakeConfig{}) // the node's allocatable is unknown
	for name, env := range map[string]corev1.EnvVar{
		"a field nothing implements":           fieldEnv("BAD", "status.phase"),
		"a label subscript that is not a key":  fieldEnv("BAD", "metadata.labels['bad key']"),
		"a limit with nothing to fall back on": resourceEnv("BAD", "limits.memory", ""),
		"an unknown resource":                  resourceEnv("BAD", "limits.gpu", ""),
		"a value from nowhere":                 {Name: "BAD", ValueFrom: &corev1.EnvVarSource{}},
	} {
		_, err := ResolveContainerEnvironment(context.Background(), envPod(), &corev1.Container{Name: "app", Env: []corev1.EnvVar{env}}, nil)
		if !IsConfigError(err) || !strings.Contains(err.Error(), "BAD") {
			t.Errorf("%s: %v, want a configuration error naming the variable", name, err)
		}
	}
}

func cmKey(name, key string, optional bool) corev1.EnvVar {
	return corev1.EnvVar{Name: "V", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key, Optional: &optional}}}
}

func secretKey(name, key string, optional bool) corev1.EnvVar {
	return corev1.EnvVar{Name: "V", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key, Optional: &optional}}}
}

func TestEnvironment_ConfigMapsAndSecrets(t *testing.T) {
	useConfig(t, &fakeConfig{
		configMaps: map[string]*corev1.ConfigMap{"team/settings": {Data: map[string]string{"mode": "fast", "not-a-name": "skipped", "OTHER": "x"}}},
		secrets:    map[string]*corev1.Secret{"team/creds": {Data: map[string][]byte{"token": []byte("s3cret")}}},
	})
	pod := envPod()

	t.Run("found", func(t *testing.T) {
		got := envOf(t, pod, &corev1.Container{Name: "app", Env: []corev1.EnvVar{cmKey("settings", "mode", false)}}, nil)
		if got["V"] != "fast" {
			t.Fatalf("configmap key: %q", got["V"])
		}
		got = envOf(t, pod, &corev1.Container{Name: "app", Env: []corev1.EnvVar{secretKey("creds", "token", false)}}, nil)
		if got["V"] != "s3cret" {
			t.Fatalf("secret key: %q", got["V"])
		}
	})

	t.Run("envFrom with a prefix skips keys that are not names, and env overrides it", func(t *testing.T) {
		c := &corev1.Container{Name: "app",
			EnvFrom: []corev1.EnvFromSource{{Prefix: "CFG_", ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}}}},
			Env:     []corev1.EnvVar{{Name: "CFG_mode", Value: "explicit"}}}
		got := envOf(t, pod, c, nil)
		if got["CFG_mode"] != "explicit" || got["CFG_OTHER"] != "x" {
			t.Fatalf("env %v", got)
		}
		if _, present := got["CFG_not-a-name"]; present {
			t.Fatal("a key that is not a valid variable name was exported")
		}
	})

	t.Run("missing and required", func(t *testing.T) {
		for name, env := range map[string]corev1.EnvVar{
			"configmap":     cmKey("gone", "mode", false),
			"configmap key": cmKey("settings", "gone", false),
			"secret":        secretKey("gone", "token", false),
			"secret key":    secretKey("creds", "gone", false),
		} {
			_, err := ResolveContainerEnvironment(context.Background(), pod, &corev1.Container{Name: "app", Env: []corev1.EnvVar{env}}, nil)
			if !IsConfigError(err) || !strings.Contains(err.Error(), "not found") {
				t.Errorf("%s: %v, want a configuration error", name, err)
			}
		}
		_, err := ResolveContainerEnvironment(context.Background(), pod, &corev1.Container{Name: "app",
			EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "gone"}}}}}, nil)
		if !IsConfigError(err) {
			t.Errorf("envFrom secret: %v", err)
		}
	})

	t.Run("missing and optional", func(t *testing.T) {
		c := &corev1.Container{Name: "app",
			Env:     []corev1.EnvVar{cmKey("gone", "mode", true), {Name: "KEEP", Value: "1"}},
			EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "gone"}, Optional: boolPtr(true)}}}}
		got := envOf(t, pod, c, nil)
		if _, present := got["V"]; present || got["KEEP"] != "1" {
			t.Fatalf("an optional reference that is absent must leave the variable out: %v", got)
		}
		got = envOf(t, pod, &corev1.Container{Name: "app", Env: []corev1.EnvVar{cmKey("settings", "gone", true)}}, nil)
		if _, present := got["V"]; present {
			t.Fatal("an optional key that is absent was exported")
		}
	})
}

func boolPtr(b bool) *bool { return &b }

// A failure to read (the API server unreachable) keeps the container waiting like a
// missing ConfigMap, but says what happened: it must not read as "not found", even
// for an optional reference (which is only optional when the object is absent).
func TestEnvironment_UnreachableAPIServerIsNotAMissingConfigMap(t *testing.T) {
	useConfig(t, &fakeConfig{failWith: errors.New("connection refused")})
	_, err := ResolveContainerEnvironment(context.Background(), envPod(), &corev1.Container{Name: "app", Env: []corev1.EnvVar{cmKey("settings", "mode", true)}}, nil)
	if !IsConfigError(err) || !strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "not found") {
		t.Fatalf("error %v", err)
	}
}
