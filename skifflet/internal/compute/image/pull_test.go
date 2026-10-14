package image_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"skifflet/internal/compute/image"
)

func Test_FormatPullReference_DigestPreserved(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "tagWithDigest",
			image: "registry.k8s.io/ingress-nginx/kube-webhook-certgen:v20230407@sha256:543c40fd093964bc9ab509d3e791f9989963021f1e9e4c9c7b6700b02bfb227b",
			want:  "registry.k8s.io/ingress-nginx/kube-webhook-certgen@sha256:543c40fd093964bc9ab509d3e791f9989963021f1e9e4c9c7b6700b02bfb227b",
		},
		{
			name:  "imageWithDigestNoTag",
			image: "img@sha256:123456",
			want:  "img@sha256:123456",
		},
		{
			name:  "registryWithPortAndTagAndDigest",
			image: "localhost:5000/team/app:v1@sha256:abcdef",
			want:  "localhost:5000/team/app@sha256:abcdef",
		},
		{
			name:  "tagOnlyNoDigest",
			image: "docker.io/istio/examples-bookinfo-details-v1:1.16.2",
			want:  "docker.io/istio/examples-bookinfo-details-v1:1.16.2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := image.FormatPullReference(tt.image); got != tt.want {
				t.Errorf("FormatPullReference(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func Test_ParseImageName_CollisionResistance(t *testing.T) {
	// 1. Registry with port and different tags must NOT collide
	v1 := image.ParseImageName("localhost:5000/team/app:v1")
	v2 := image.ParseImageName("localhost:5000/team/app:v2")
	if v1 == v2 {
		t.Errorf("cache key collision for v1 and v2: %s", v1)
	}

	// 2. Separators / vs _ must NOT collide
	slashPath := image.ParseImageName("team/app:latest")
	underscorePath := image.ParseImageName("team_app:latest")
	if slashPath == underscorePath {
		t.Errorf("cache key collision between slash and underscore: %s", slashPath)
	}

	// 3. Different architectures must NOT collide
	amd64 := image.ParseImageNameForArch("alpine:latest", "amd64")
	arm64 := image.ParseImageNameForArch("alpine:latest", "arm64")
	if amd64 == arm64 {
		t.Errorf("cache key collision between amd64 and arm64: %s", amd64)
	}

	// 4. Digest pinned images must have distinct keys from untagged/tagged
	plain := image.ParseImageName("alpine:latest")
	pinned := image.ParseImageName("alpine:latest@sha256:543c40fd093964bc9ab509d3e791f9989963021f1e9e4c9c7b6700b02bfb227b")
	if plain == pinned {
		t.Errorf("cache key collision between plain and digest pinned image: %s", plain)
	}
}

// The cache names and pull references of these images are also made by `skiff.sh download`,
// which puts images in the cache from outside a cluster; test/skiff-config-test.sh checks the
// script against the same examples, so the two cannot drift apart.
func Test_CacheNamesShared(t *testing.T) {
	const digest = "sha256:543c40fd093964bc9ab509d3e791f9989963021f1e9e4c9c7b6700b02bfb227b"
	names := []struct{ image, arch, want string }{
		{"registry.k8s.io/pause:3.10", "amd64", "/registry.k8s.io_pause_3.10_6f731803067f31e9_amd64.sif"},
		{"rancher/mirrored-coredns-coredns:1.14.7", "arm64", "/rancher_mirrored-coredns-coredns_1.14.7_2ff2bc0f32350611_arm64.sif"},
		{"localhost:5000/team/app:v1@" + digest, "amd64", "/localhost_5000_team_app_v1_sha256_543c40fd093964bc9ab509d3e791f9_533cb68c43dbbdf8_amd64.sif"},
		{"nginx", "arm64", "/nginx_5be1ecc7935f1dd8_arm64.sif"},
	}
	for _, n := range names {
		if got := image.ParseImageNameForArch(n.image, n.arch); got != n.want {
			t.Errorf("ParseImageNameForArch(%q, %q) = %q, want %q", n.image, n.arch, got, n.want)
		}
	}
	refs := []struct{ image, want string }{
		{"alpine:latest@" + digest, "alpine@" + digest},
		{"localhost:5000/team/app:v1@" + digest, "localhost:5000/team/app@" + digest},
		{"localhost:5000/team/app@" + digest, "localhost:5000/team/app@" + digest},
		{"nginx:1.27", "nginx:1.27"},
	}
	for _, r := range refs {
		if got := image.FormatPullReference(r.image); got != r.want {
			t.Errorf("FormatPullReference(%q) = %q, want %q", r.image, got, r.want)
		}
	}
}

func Test_CleanupTempFiles_PreservesRecentFiles(t *testing.T) {
	tmpDir := t.TempDir()

	normalFile := filepath.Join(tmpDir, "image_latest.sif")
	activeTempFile := filepath.Join(tmpDir, "image_latest.sif.tmp-active")
	oldTempFile := filepath.Join(tmpDir, "image_latest.sif.tmp-old")
	subDir := filepath.Join(tmpDir, "sub.tmp-dir")

	_ = os.WriteFile(normalFile, []byte("data"), 0644)
	_ = os.WriteFile(activeTempFile, []byte("active temp"), 0644)
	_ = os.WriteFile(oldTempFile, []byte("old temp"), 0644)
	_ = os.Mkdir(subDir, 0755)

	// Make oldTempFile appear 3 hours old
	oldTime := time.Now().Add(-3 * time.Hour)
	_ = os.Chtimes(oldTempFile, oldTime, oldTime)

	// Clean up abandoned temp files (older than 2 hours)
	if err := image.CleanupTempFiles(tmpDir); err != nil {
		t.Fatalf("CleanupTempFiles failed: %v", err)
	}

	// normal file should remain
	if _, err := os.Stat(normalFile); os.IsNotExist(err) {
		t.Errorf("normal file should not be removed")
	}
	// active temp file (< 2 hours) should NOT be removed (protects concurrent NFS pulls)
	if _, err := os.Stat(activeTempFile); os.IsNotExist(err) {
		t.Errorf("recent active temp file should be preserved")
	}
	// old temp file (> 2 hours) SHOULD be removed
	if _, err := os.Stat(oldTempFile); !os.IsNotExist(err) {
		t.Errorf("old abandoned temp file should have been removed")
	}
	// subdirectory should remain
	if _, err := os.Stat(subDir); os.IsNotExist(err) {
		t.Errorf("subdirectory should not be removed")
	}
}
