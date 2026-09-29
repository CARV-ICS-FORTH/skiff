package podhandler

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"

	"skifflet/internal/compute"
)

type podNetworkStatus struct {
	Success      bool   `json:"success"`
	Ready        bool   `json:"ready"`
	NodeCIDR     string `json:"node_cidr"`
	GatewayMode  string `json:"gateway_mode"`
	GatewayReady bool   `json:"gateway_ready"`
}

func readPodNetworkStatus(ctx context.Context) (podNetworkStatus, error) {
	var status podNetworkStatus
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", compute.Environment.PlaidSocketPath())
	if err != nil {
		return status, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	payload := []byte(`{"action":"get_status"}`)
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	if _, err := conn.Write(frame); err != nil {
		return status, err
	}
	var length [4]byte
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		return status, err
	}
	n := binary.BigEndian.Uint32(length[:])
	if n == 0 || n > 64*1024 {
		return status, fmt.Errorf("invalid Plaid status size %d", n)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(conn, data); err != nil {
		return status, err
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return status, err
	}
	if !status.Success {
		return status, fmt.Errorf("Plaid status request failed")
	}
	return status, nil
}

// waitForPodNetwork waits for Plaid's initialized TAP datapath and returns the
// node subnet assigned to this daemon.
func waitForPodNetwork(ctx context.Context) (*net.IPNet, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var lastErr error
	for {
		status, err := readPodNetworkStatus(ctx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil && status.Ready && status.GatewayReady && status.GatewayMode == "tap" {
			_, subnet, parseErr := net.ParseCIDR(status.NodeCIDR)
			if parseErr == nil && subnet.IP.To4() != nil {
				return subnet, nil
			}
			lastErr = fmt.Errorf("invalid Plaid node CIDR %q", status.NodeCIDR)
		} else if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("Plaid TAP datapath is not ready")
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for Plaid pod network: %w (last observation: %v)", ctx.Err(), lastErr)
		case <-time.After(500 * time.Millisecond):
		}
	}
}
