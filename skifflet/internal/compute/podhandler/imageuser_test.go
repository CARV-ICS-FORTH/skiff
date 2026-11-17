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

// The images of the tests are files that are not SIFs: none of them says who it runs as or
// where it starts, unless a test says so (withImageUser, withImageConfig), and none is
// unpacked: the copy a container would start from is only named (unpackedFor), unless a test
// says what the unpacking does (withUnpacking).
func init() {
	imageConfigOf = func(context.Context, *image.Image) (image.Config, error) { return image.Config{}, nil }
	unpackedImageOf = func(_ context.Context, img *image.Image) (string, error) { return unpackedFor(img.Filepath), nil }
}

// unpackedFor is where the tests say an image file is unpacked for the node.
func unpackedFor(imageFile string) string { return imageFile + ".unpacked" }

func withUnpacking(t *testing.T, unpack func(context.Context, *image.Image) (string, error)) {
	t.Helper()
	old := unpackedImageOf
	unpackedImageOf = unpack
	t.Cleanup(func() { unpackedImageOf = old })
}

func withImageUser(t *testing.T, user image.User, err error) {
	t.Helper()
	withImageConfig(t, image.Config{User: user}, err)
}

func withImageConfig(t *testing.T, config image.Config, err error) {
	t.Helper()
	old := imageConfigOf
	imageConfigOf = func(context.Context, *image.Image) (image.Config, error) { return config, err }
	t.Cleanup(func() { imageConfigOf = old })
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

// A container starts from its image as unpacked for the node: that copy is what Apptainer is
// given, where the image file is still what the container is said to run from.
func TestBuildContainerStartsFromTheUnpackedImage(t *testing.T) {
	var unpackedFiles []string
	withUnpacking(t, func(_ context.Context, img *image.Image) (string, error) {
		unpackedFiles = append(unpackedFiles, img.Filepath)
		return "/scratch/.unpacked/fixture", nil
	})
	c, err := buildWith(t, &corev1.Pod{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(c.ImageFilePath, "/fixture.sif") || len(unpackedFiles) != 1 || unpackedFiles[0] != c.ImageFilePath {
		t.Fatalf("unpacked %v for a container of image file %s, want that file, once", unpackedFiles, c.ImageFilePath)
	}
	if c.ImageRootPath != "/scratch/.unpacked/fixture" {
		t.Fatalf("the container starts from %q, want the unpacked copy", c.ImageRootPath)
	}
	args := c.BuildApptainerArgs(0, endpoint.PodPath(t.TempDir()), 1)
	given := 0
	for _, arg := range args {
		if arg == c.ImageFilePath {
			t.Errorf("Apptainer is given the image file: %v", args)
		}
		if arg == c.ImageRootPath {
			given++
		}
	}
	if given != 1 {
		t.Errorf("Apptainer is given the unpacked copy %d times, want once: %v", given, args)
	}
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
