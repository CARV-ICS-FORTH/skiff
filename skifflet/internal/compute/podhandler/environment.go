package podhandler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"skifflet/internal/compute"
	"skifflet/pkg/fieldpath"
)

// ConfigError is what stops a container from being configured: a ConfigMap or
// Secret it refers to is missing, a key is absent, a reference to the pod's own
// facts cannot be resolved. The kubelet reports it as the container's
// CreateContainerConfigError, keeps the container waiting and tries again, so that
// what is created later lets it start; Skifflet does the same for that one
// container and leaves the others of the pod alone.
//
// Reason is the reason the container waits with; CreateContainerConfigError when it is empty.
type ConfigError struct{ Message, Reason string }

func (e *ConfigError) Error() string { return e.Message }

func configErrorf(format string, args ...any) error {
	return &ConfigError{Message: fmt.Sprintf(format, args...)}
}

// configReason is the reason the container waits with for err, a ConfigError.
func configReason(err error) string {
	var c *ConfigError
	if errors.As(err, &c) && c.Reason != "" {
		return c.Reason
	}
	return "CreateContainerConfigError"
}

// IsConfigError says whether err is (or wraps) a ConfigError.
func IsConfigError(err error) bool {
	var c *ConfigError
	return errors.As(err, &c)
}

// ConfigSource is where a container's environment finds what it refers to.
type ConfigSource interface {
	ConfigMap(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error)
	Secret(ctx context.Context, namespace, name string) (*corev1.Secret, error)
	// Allocatable is what the node offers, the value of a resource limit a
	// container does not set.
	Allocatable(ctx context.Context, node string) (corev1.ResourceList, error)
}

// Config is the source in use; tests replace it.
var Config ConfigSource = clientsetSource{}

type clientsetSource struct{}

func (clientsetSource) ConfigMap(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error) {
	if compute.K8SClientset == nil {
		return nil, errors.New("no Kubernetes client")
	}
	return compute.K8SClientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (clientsetSource) Secret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	if compute.K8SClientset == nil {
		return nil, errors.New("no Kubernetes client")
	}
	return compute.K8SClientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (clientsetSource) Allocatable(ctx context.Context, node string) (corev1.ResourceList, error) {
	if compute.K8SClientset == nil {
		return nil, errors.New("no Kubernetes client")
	}
	n, err := compute.K8SClientset.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return n.Status.Allocatable, nil
}

// referenceError describes why a ConfigMap or Secret could not be read: it does
// not exist, or the API server could not be asked. Both keep the container waiting
// and are retried; the message tells them apart.
func referenceError(kind, name string, err error) error {
	if apierrors.IsNotFound(err) {
		return configErrorf("%s %q not found", kind, name)
	}
	return configErrorf("cannot read %s %q: %v", kind, name, err)
}

// ResolveContainerEnvironment resolves the environment a container starts with:
//  1. the variables of the namespace's Services (lowest precedence);
//  2. envFrom (ConfigMaps and Secrets, keys that are not valid names are skipped);
//  3. env (highest precedence), whose values may come from the pod's own facts (the
//     Downward API: fieldRef and resourceFieldRef, evaluated by the functions the
//     Downward API volumes use as well), a ConfigMap key or a Secret key.
//
// References marked optional that cannot be found are left out; any other
// reference that cannot be resolved is a ConfigError, never an empty value.
func ResolveContainerEnvironment(ctx context.Context, pod *corev1.Pod, container *corev1.Container, serviceEnvs []corev1.EnvVar) ([]corev1.EnvVar, error) {
	envMap := make(map[string]string)
	var order []string
	set := func(name, value string) {
		if _, exists := envMap[name]; !exists {
			order = append(order, name)
		}
		envMap[name] = value
	}

	for _, envVar := range serviceEnvs {
		set(envVar.Name, envVar.Value)
	}

	for _, from := range container.EnvFrom {
		switch {
		case from.ConfigMapRef != nil:
			cm, err := Config.ConfigMap(ctx, pod.Namespace, from.ConfigMapRef.Name)
			if err != nil {
				if optional(from.ConfigMapRef.Optional) && apierrors.IsNotFound(err) {
					continue
				}
				return nil, referenceError("configmap", from.ConfigMapRef.Name, err)
			}
			for k, v := range cm.Data {
				if key := from.Prefix + k; len(validation.IsCIdentifier(key)) == 0 {
					set(key, v)
				}
			}
		case from.SecretRef != nil:
			sec, err := Config.Secret(ctx, pod.Namespace, from.SecretRef.Name)
			if err != nil {
				if optional(from.SecretRef.Optional) && apierrors.IsNotFound(err) {
					continue
				}
				return nil, referenceError("secret", from.SecretRef.Name, err)
			}
			for k, v := range sec.Data {
				if key := from.Prefix + k; len(validation.IsCIdentifier(key)) == 0 {
					set(key, string(v))
				}
			}
		}
	}

	for _, envVar := range container.Env {
		value := envVar.Value
		if from := envVar.ValueFrom; from != nil {
			var found bool
			var err error
			value, found, err = resolveValueFrom(ctx, pod, container, envVar.Name, from)
			if err != nil {
				return nil, err
			}
			if !found {
				continue // an optional reference that is not there
			}
		}
		set(envVar.Name, value)
	}

	resolved := make([]corev1.EnvVar, len(order))
	for i, name := range order {
		resolved[i] = corev1.EnvVar{Name: name, Value: envMap[name]}
	}
	return resolved, nil
}

func optional(o *bool) bool { return o != nil && *o }

// resolveValueFrom evaluates one env valueFrom. found is false for an optional
// ConfigMap or Secret reference that is absent.
func resolveValueFrom(ctx context.Context, pod *corev1.Pod, container *corev1.Container, name string, from *corev1.EnvVarSource) (value string, found bool, err error) {
	switch {
	case from.FieldRef != nil:
		v, err := fieldpath.PodFieldValue(pod, from.FieldRef.FieldPath)
		if err != nil {
			return "", false, configErrorf("environment variable %s: %v", name, err)
		}
		return v, true, nil

	case from.ResourceFieldRef != nil:
		var allocatable corev1.ResourceList
		if strings.HasPrefix(from.ResourceFieldRef.Resource, "limits.") {
			// only a limit can fall back on the node; look it up when it is needed
			allocatable, _ = Config.Allocatable(ctx, pod.Spec.NodeName)
		}
		v, err := fieldpath.ResourceValue(pod, container, from.ResourceFieldRef, allocatable)
		if err != nil {
			return "", false, configErrorf("environment variable %s: %v", name, err)
		}
		return v, true, nil

	case from.ConfigMapKeyRef != nil:
		ref := from.ConfigMapKeyRef
		cm, err := Config.ConfigMap(ctx, pod.Namespace, ref.Name)
		if err != nil {
			if optional(ref.Optional) && apierrors.IsNotFound(err) {
				return "", false, nil
			}
			return "", false, referenceError("configmap", ref.Name, err)
		}
		v, ok := cm.Data[ref.Key]
		if !ok {
			if optional(ref.Optional) {
				return "", false, nil
			}
			return "", false, configErrorf("key %q not found in configmap %q", ref.Key, ref.Name)
		}
		return v, true, nil

	case from.SecretKeyRef != nil:
		ref := from.SecretKeyRef
		sec, err := Config.Secret(ctx, pod.Namespace, ref.Name)
		if err != nil {
			if optional(ref.Optional) && apierrors.IsNotFound(err) {
				return "", false, nil
			}
			return "", false, referenceError("secret", ref.Name, err)
		}
		v, ok := sec.Data[ref.Key]
		if !ok {
			if optional(ref.Optional) {
				return "", false, nil
			}
			return "", false, configErrorf("key %q not found in secret %q", ref.Key, ref.Name)
		}
		return string(v), true, nil
	}
	return "", false, configErrorf("environment variable %s: valueFrom names no source", name)
}

// resourceResolver is what Downward API volumes use for a resourceFieldRef: the
// container is named in the reference, and a limit it does not set is the node's.
func (h *PodHandler) resourceResolver(ctx context.Context) fieldpath.ResourceResolver {
	var allocatable corev1.ResourceList
	var fetched bool
	return func(ref *corev1.ResourceFieldSelector) (string, error) {
		if !fetched {
			fetched = true
			allocatable, _ = Config.Allocatable(ctx, h.Pod.Spec.NodeName)
		}
		return fieldpath.ResourceValue(h.Pod, nil, ref, allocatable)
	}
}
