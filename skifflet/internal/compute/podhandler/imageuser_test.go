package podhandler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/image"
)

// The images of the tests are files that are not SIFs: none of them says who it runs as,
// unless a test says so (withImageUser).
func init() {
	imageUserOf = func(context.Context, *image.Image) (image.User, error) { return image.User{}, nil }
}

func withImageUser(t *testing.T, user image.User, err error) {
	t.Helper()
	old := imageUserOf
	imageUserOf = func(context.Context, *image.Image) (image.User, error) { return user, err }
	t.Cleanup(func() { imageUserOf = old })
}

func buildWith(t *testing.T, pod *corev1.Pod) (Container, error) {
	t.Helper()
	podDir := endpoint.PodPath(t.TempDir())
	if err := os.MkdirAll(podDir.JobDir(), 0755); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(t.TempDir(), "fixture.sif")
	if err := os.WriteFile(imagePath, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	pod.Name, pod.Namespace, pod.UID = "p", "ns", "uid-1"
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "app", Image: imagePath, ImagePullPolicy: corev1.PullNever})
	h := newTestHandler(pod, podDir)
	return h.buildContainer(context.Background(), &pod.Spec.Containers[0])
}

// A container runs as the user its image says when the pod does not say.
func TestBuildContainerRunsAsTheImagesUser(t *testing.T) {
	withImageUser(t, image.User{UID: 101, GID: 102, Set: true}, nil)
	c, err := buildWith(t, &corev1.Pod{})
	if err != nil || c.RunAsUser != 101 || c.RunAsGroup != 102 {
		t.Fatalf("image user: %d:%d, %v", c.RunAsUser, c.RunAsGroup, err)
	}
	c, err = buildWith(t, &corev1.Pod{Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr(int64(7)), RunAsGroup: ptr(int64(8))}}})
	if err != nil || c.RunAsUser != 7 || c.RunAsGroup != 8 {
		t.Fatalf("the pod's user over the image's: %d:%d, %v", c.RunAsUser, c.RunAsGroup, err)
	}
}

// runAsNonRoot refuses a container that would run as root, as the kubelet does, naming the pod.
func TestBuildContainerRunAsNonRootRefusesRoot(t *testing.T) {
	withImageUser(t, image.User{}, nil)
	_, err := buildWith(t, &corev1.Pod{Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr(true)}}})
	want := `container has runAsNonRoot and image will run as root (pod: "p_ns(uid-1)", container: app)`
	if !IsConfigError(err) || err.Error() != want || configReason(err) != "CreateContainerConfigError" {
		t.Fatalf("got %v; want %q", err, want)
	}
	// with an image that runs as a user, the same pod runs
	withImageUser(t, image.User{UID: 65532, GID: 65532, Set: true}, nil)
	c, err := buildWith(t, &corev1.Pod{Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr(true)}}})
	if err != nil || c.RunAsUser != 65532 {
		t.Fatalf("a non-root image under runAsNonRoot: %d, %v", c.RunAsUser, err)
	}
}

// A USER that the image does not have fails the container, with the runtime's reason.
func TestBuildContainerUnknownImageUser(t *testing.T) {
	withImageUser(t, image.User{}, &image.NotFoundError{Message: "unable to find user ghost: no matching entries in passwd file"})
	_, err := buildWith(t, &corev1.Pod{})
	if !IsConfigError(err) || !strings.Contains(err.Error(), "unable to find user ghost") || configReason(err) != "CreateContainerError" {
		t.Fatalf("got %v (reason %s)", err, configReason(err))
	}
}
