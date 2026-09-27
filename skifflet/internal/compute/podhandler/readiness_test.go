package podhandler

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVerifyPodIP(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.244.0.0/16")

	tests := []struct {
		name    string
		ip      string
		subnet  *net.IPNet
		wantErr bool
	}{
		{"valid_ip_in_subnet", "10.244.1.5", subnet, false},
		{"valid_ip_cidr_in_subnet", "10.244.1.5/24", subnet, false},
		{"valid_ip_nil_subnet", "10.244.1.5", nil, false},
		{"valid_ip_cidr_nil_subnet", "10.244.1.5/24", nil, false},
		{"loopback_rejected", "127.0.0.1", subnet, true},
		{"unspecified_rejected", "0.0.0.0", subnet, true},
		{"outside_subnet_rejected", "192.168.1.5", subnet, true},
		{"invalid_format", "not-an-ip", subnet, true},
		{"empty_ip", "", subnet, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyPodIP(tt.ip, tt.subnet)
			if (err != nil) != tt.wantErr {
				t.Errorf("VerifyPodIP(%q) error = %v, wantErr = %v", tt.ip, err, tt.wantErr)
			}
		})
	}
}

func TestWaitForReadiness_Success(t *testing.T) {
	tmpDir := t.TempDir()
	readinessFile := filepath.Join(tmpDir, "readiness.json")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		time.Sleep(50 * time.Millisecond)
		data := `{"pause_pid": 12345, "netns_path": "/proc/12345/ns/net", "endpoint_id": "ep-1", "ip": "10.244.1.10/24", "gateway": "10.244.1.1/24"}`
		_ = os.WriteFile(readinessFile, []byte(data), 0644)
	}()

	info, err := WaitForReadiness(ctx, readinessFile, 1*time.Second)
	if err != nil {
		t.Fatalf("WaitForReadiness failed: %v", err)
	}

	if info.PausePID != 12345 || info.IP != "10.244.1.10" || info.Gateway != "10.244.1.1" || info.EndpointID != "ep-1" {
		t.Errorf("unexpected SandboxInfo: %+v", info)
	}
}

func TestWaitForReadiness_Timeout(t *testing.T) {
	tmpDir := t.TempDir()
	readinessFile := filepath.Join(tmpDir, "missing.json")

	ctx := context.Background()
	_, err := WaitForReadiness(ctx, readinessFile, 50*time.Millisecond)
	if err != ErrReadinessTimeout {
		t.Fatalf("expected ErrReadinessTimeout, got: %v", err)
	}
}

func TestVerifyNetnsDescriptor_InvalidPath(t *testing.T) {
	_, err := VerifyNetnsDescriptor("")
	if err == nil {
		t.Errorf("expected error for empty netns path")
	}

	_, err = VerifyNetnsDescriptor("/nonexistent/ns/path")
	if err == nil {
		t.Errorf("expected error for nonexistent netns path")
	}
}
