package podhandler

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"skifflet/internal/compute"
)

// Removing a pod's endpoint talks to the configured plaidd socket, not a fixed one.
func TestRemovePlaidEndpointUsesTheConfiguredSocket(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "skiff-plaid-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "custom.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	requests := make(chan map[string]string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var header [4]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(header[:]))
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		request := map[string]string{}
		_ = json.Unmarshal(body, &request)
		requests <- request
		reply := []byte(`{"success":true}`)
		binary.BigEndian.PutUint32(header[:], uint32(len(reply)))
		_, _ = conn.Write(append(header[:], reply...))
	}()

	old := compute.Environment
	compute.Environment.PlaidSocket = socket
	defer func() { compute.Environment = old }()
	if err := RemovePlaidEndpoint("endpoint-1"); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	if request["action"] != "remove_endpoint" || request["pod_id"] != "endpoint-1" || request["container_id"] != "endpoint-1" {
		t.Fatalf("plaidd received %v", request)
	}
}
