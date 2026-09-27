package podhandler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"skifflet/internal/compute/endpoint"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetHostResolvConf(t *testing.T) {
	kubeDNS := "10.96.0.10"

	t.Run("LoopbackOnly_FilteredOut", func(t *testing.T) {
		dir := t.TempDir()
		resolvFile := filepath.Join(dir, "resolv.conf")
		content := "nameserver 127.0.0.1\nnameserver 127.0.0.53\nsearch default.svc.cluster.local\n"
		if err := os.WriteFile(resolvFile, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write fixture: %v", err)
		}

		res := getHostResolvConf(kubeDNS, resolvFile)
		if strings.Contains(res, "nameserver 127.0.0.1") || strings.Contains(res, "nameserver 127.0.0.53") {
			t.Errorf("expected loopback nameservers to be filtered out, got:\n%s", res)
		}
		if !strings.Contains(res, "search default.svc.cluster.local") {
			t.Errorf("expected search directive to be preserved, got:\n%s", res)
		}
	})

	t.Run("RealNameserver_Preserved", func(t *testing.T) {
		dir := t.TempDir()
		resolvFile := filepath.Join(dir, "resolv.conf")
		content := "nameserver 192.168.1.1\nsearch example.com\n"
		if err := os.WriteFile(resolvFile, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write fixture: %v", err)
		}

		res := getHostResolvConf(kubeDNS, resolvFile)
		if !strings.Contains(res, "nameserver 192.168.1.1") {
			t.Errorf("expected real nameserver 192.168.1.1 in resolv.conf output, got:\n%s", res)
		}
		if !strings.Contains(res, "search example.com") {
			t.Errorf("expected search directive to be preserved, got:\n%s", res)
		}
	})

	t.Run("KubeDNSIP_FilteredOut", func(t *testing.T) {
		dir := t.TempDir()
		resolvFile := filepath.Join(dir, "resolv.conf")
		content := fmt.Sprintf("nameserver %s\n", kubeDNS)
		if err := os.WriteFile(resolvFile, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write fixture: %v", err)
		}

		res := getHostResolvConf(kubeDNS, resolvFile)
		if strings.Contains(res, "nameserver "+kubeDNS) {
			t.Errorf("expected kubeDNS IP to be filtered out, got:\n%s", res)
		}
	})

	t.Run("NonExistentFile_ReturnsEmpty", func(t *testing.T) {
		res := getHostResolvConf(kubeDNS, filepath.Join(t.TempDir(), "nonexistent.conf"))
		if res != "" {
			t.Errorf("expected empty string for nonexistent file, got:\n%s", res)
		}
	})
}

func TestPrepareDNS(t *testing.T) {
	tmpDir := t.TempDir()
	podDir := endpoint.PodPath(filepath.Join(tmpDir, "test-pod"))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			DNSPolicy: corev1.DNSClusterFirst,
		},
	}

	err := PrepareDNS(pod, podDir, "10.43.0.10", "10.244.0.5")
	if err != nil {
		t.Fatalf("PrepareDNS failed: %v", err)
	}

	resolvContent, err := os.ReadFile(filepath.Join(podDir.JobDir(), "resolv.conf"))
	if err != nil {
		t.Fatalf("failed to read resolv.conf: %v", err)
	}
	if !strings.Contains(string(resolvContent), "10.43.0.10") {
		t.Errorf("resolv.conf missing kubeDNS IP: %s", string(resolvContent))
	}

	hostsContent, err := os.ReadFile(filepath.Join(podDir.JobDir(), "hosts"))
	if err != nil {
		t.Fatalf("failed to read hosts: %v", err)
	}
	if !strings.Contains(string(hostsContent), "10.244.0.5 my-pod") {
		t.Errorf("hosts missing podIP mapping: %s", string(hostsContent))
	}
}

func TestHasNameserver(t *testing.T) {
	if hasNameserver("search svc.cluster.local\noptions ndots:5\n") {
		t.Fatal("resolver without a nameserver must be rejected")
	}
	if !hasNameserver("search svc.cluster.local\nnameserver 10.0.2.3\n") {
		t.Fatal("usable outer resolver was rejected")
	}
}
