package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"plaid/pkg/api"
)

type testAPIHandler struct {
	mu        sync.Mutex
	endpoints map[string]*api.Request
}

func (h *testAPIHandler) HandleAddEndpoint(req *api.Request, tapFDs []int) error {
	h.mu.Lock()
	h.endpoints[req.PodID] = req
	h.mu.Unlock()
	for _, fd := range tapFDs {
		_ = os.NewFile(uintptr(fd), "tap").Close()
	}
	return nil
}

func (h *testAPIHandler) HandleRemoveEndpoint(req *api.Request) error {
	h.mu.Lock()
	delete(h.endpoints, req.PodID)
	delete(h.endpoints, req.ContainerID)
	h.mu.Unlock()
	return nil
}

func (h *testAPIHandler) HandleAddRoute(req *api.Request) error {
	return nil
}

func (h *testAPIHandler) HandleRemoveRoute(req *api.Request) error {
	return nil
}

func (h *testAPIHandler) HandleListRoutes() ([]api.RouteInfo, error) {
	return nil, nil
}

func (h *testAPIHandler) SetEndpoint(key string, req *api.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.endpoints[key] = req
}

func (h *testAPIHandler) HasEndpoint(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.endpoints[key]
	return ok
}

func (h *testAPIHandler) HandleGetStatus() (*api.Response, error) {
	h.mu.Lock()
	cnt := len(h.endpoints)
	h.mu.Unlock()
	return &api.Response{
		EndpointsCount: cnt,
	}, nil
}

func TestCNIPlugin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "plaidd.sock")
	handler := &testAPIHandler{endpoints: make(map[string]*api.Request)}

	srv := api.NewServer(sockPath, handler, handler, handler)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Failed to start test API server: %v", err)
	}
	srv.MarkReady()
	defer srv.Close()

	binPath := filepath.Join(tmpDir, "plaid")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to build plaid binary: %v, output: %s", err, out)
	}

	// 1. Test VERSION
	vCmd := exec.Command(binPath)
	vCmd.Env = append(os.Environ(), "CNI_COMMAND=VERSION")
	vOut, err := vCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CNI VERSION failed: %v, output: %s", err, vOut)
	}
	var vResp map[string]interface{}
	if err := json.Unmarshal(vOut, &vResp); err != nil {
		t.Fatalf("Failed to parse VERSION response: %v", err)
	}
	if vResp["cniVersion"] != "0.4.0" {
		t.Errorf("Unexpected cniVersion: %v", vResp["cniVersion"])
	}

	// 2. Test DEL when endpoint is pre-registered under containerID but K8S_POD_NAME is different
	handler.SetEndpoint("cont-123", &api.Request{
		PodID:       "cont-123",
		ContainerID: "cont-123",
		PodName:     "test-pod",
	})

	delConf := fmt.Sprintf(`{
		"cniVersion": "0.4.0",
		"name": "cbr0",
		"type": "plaid",
		"socketPath": %q
	}`, sockPath)

	dCmd := exec.Command(binPath)
	dCmd.Env = append(os.Environ(),
		"CNI_COMMAND=DEL",
		"CNI_CONTAINERID=cont-123",
		"CNI_IFNAME=eth0",
		"CNI_ARGS=K8S_POD_NAME=test-pod-different;K8S_POD_NAMESPACE=default",
	)
	dCmd.Stdin = bytes.NewReader([]byte(delConf))
	dOut, err := dCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CNI DEL failed: %v, output: %s", err, dOut)
	}

	// Verify the endpoint was removed under its canonical containerID!
	if handler.HasEndpoint("cont-123") {
		t.Fatalf("CNI DEL failed to remove endpoint with canonical containerID cont-123 when K8S_POD_NAME was different")
	}

	// 3. Test CNI CHECK failure when endpoint not found
	cCmd := exec.Command(binPath)
	cCmd.Env = append(os.Environ(),
		"CNI_COMMAND=CHECK",
		"CNI_CONTAINERID=cont-123",
		"CNI_IFNAME=eth0",
		"CNI_NETNS="+binPath, // use existing file as netns path for stat
	)
	cCmd.Stdin = bytes.NewReader([]byte(delConf))
	cOut, err := cCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("Expected CNI CHECK to fail when endpoint not found in plaidd, but succeeded: %s", cOut)
	}

	// 4. Test Incompatible CNI Version
	badVerConf := fmt.Sprintf(`{
		"cniVersion": "9.9.9",
		"name": "cbr0",
		"type": "plaid",
		"socketPath": %q
	}`, sockPath)
	badVerCmd := exec.Command(binPath)
	badVerCmd.Env = append(os.Environ(),
		"CNI_COMMAND=ADD",
		"CNI_CONTAINERID=cont-bad-ver",
		"CNI_IFNAME=eth0",
		"CNI_NETNS="+binPath,
	)
	badVerCmd.Stdin = bytes.NewReader([]byte(badVerConf))
	badOut, err := badVerCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("Expected incompatible CNI version to fail, got success: %s", badOut)
	}

	// 5. Test API communication directly
	client := api.NewClient(sockPath)
	rPipe, _, _ := os.Pipe()
	defer rPipe.Close()

	err = client.AddEndpoint(&api.Request{
		PodID: "pod-direct",
		IP:    "10.244.1.5",
		MAC:   "02:00:00:00:00:05",
	}, int(rPipe.Fd()))
	if err != nil {
		t.Fatalf("Direct AddEndpoint failed: %v", err)
	}

	status, err := client.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if status.EndpointsCount != 1 {
		t.Errorf("Expected 1 endpoint, got %d", status.EndpointsCount)
	}

	err = client.RemoveEndpoint("pod-direct", "")
	if err != nil {
		t.Fatalf("Direct RemoveEndpoint failed: %v", err)
	}

	status, _ = client.GetStatus()
	if status.EndpointsCount != 0 {
		t.Errorf("Expected 0 endpoints after DEL, got %d", status.EndpointsCount)
	}

	// 6. Test Transactional Rollback on ADD failure
	logFile := filepath.Join(tmpDir, "ipam.log")
	mockIPAMScript := filepath.Join(tmpDir, "mock-ipam")
	scriptContent := fmt.Sprintf(`#!/bin/sh
cmd="$CNI_COMMAND"
echo "$cmd" >> %q
if [ "$cmd" = "ADD" ]; then
    echo '{"cniVersion":"0.4.0","ips":[{"version":"4","address":"10.244.1.99/24","gateway":"10.244.1.1"}]}'
    exit 0
fi
if [ "$cmd" = "DEL" ]; then
    exit 0
fi
`, logFile)
	if err := os.WriteFile(mockIPAMScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("Failed to write mock IPAM script: %v", err)
	}

	rollbackConf := fmt.Sprintf(`{
		"cniVersion": "0.4.0",
		"name": "cbr0",
		"type": "plaid",
		"socketPath": %q,
		"ipam": {
			"type": "mock-ipam"
		}
	}`, sockPath)

	failCmd := exec.Command(binPath)
	failCmd.Env = append(os.Environ(),
		"CNI_COMMAND=ADD",
		"CNI_CONTAINERID=cont-rollback",
		"CNI_IFNAME=eth0",
		"CNI_PATH="+tmpDir,
		"CNI_NETNS=/nonexistent/netns/path", // will fail at TAP creation in netns
	)
	failCmd.Stdin = bytes.NewReader([]byte(rollbackConf))
	_ = failCmd.Run() // expected to fail

	// Read log to verify ADD was followed by rollback DEL!
	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read IPAM log: %v", err)
	}
	logStr := string(logBytes)
	if !bytes.Contains(logBytes, []byte("ADD")) || !bytes.Contains(logBytes, []byte("DEL")) {
		t.Fatalf("Expected mock IPAM to receive both ADD and rollback DEL, got: %q", logStr)
	}
}
