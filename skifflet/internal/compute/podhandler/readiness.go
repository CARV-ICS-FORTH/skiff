package podhandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

var (
	ErrReadinessTimeout = errors.New("timeout waiting for launcher readiness handshake")
	ErrInvalidNetns     = errors.New("invalid or duplicate network namespace")
	ErrInvalidPodIP     = errors.New("invalid pod ip address")
)

// SandboxInfo contains verified network sandbox information returned by the launcher handshake.
type SandboxInfo struct {
	PausePID   int    `json:"pause_pid"`
	NetnsPath  string `json:"netns_path"`
	EndpointID string `json:"endpoint_id"`
	IP         string `json:"ip"`
	Gateway    string `json:"gateway"`
}

// WaitForReadiness waits for the launcher to write its readiness JSON handshake to readinessFile.
func WaitForReadiness(ctx context.Context, readinessFile string, timeout time.Duration) (*SandboxInfo, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			data, err := os.ReadFile(readinessFile)
			if err == nil && len(data) > 0 {
				var info SandboxInfo
				if jsonErr := json.Unmarshal(data, &info); jsonErr == nil && info.PausePID > 1 && info.IP != "" {
					info.IP = strings.Split(info.IP, "/")[0]
					if info.Gateway != "" {
						info.Gateway = strings.Split(info.Gateway, "/")[0]
					}
					return &info, nil
				}
			}
			if time.Now().After(deadline) {
				return nil, ErrReadinessTimeout
			}
		}
	}
}

// VerifyNetnsDescriptor opens and verifies that the container network namespace exists
// and differs from both the current process network namespace and host /proc/1/ns/net.
func VerifyNetnsDescriptor(netnsPath string) (*os.File, error) {
	if netnsPath == "" {
		return nil, errors.New("empty netns path")
	}

	netnsFile, err := os.Open(netnsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open netns path '%s': %w", netnsPath, err)
	}

	targetStat, err := netnsFile.Stat()
	if err != nil {
		_ = netnsFile.Close()
		return nil, fmt.Errorf("failed to stat netns '%s': %w", netnsPath, err)
	}

	// On Linux systems with /proc, verify namespace inode distinctness
	targetSys, ok := targetStat.Sys().(*syscall.Stat_t)
	if !ok {
		return netnsFile, nil
	}

	// 1. Verify it differs from host /proc/1/ns/net
	if hostStat, err := os.Stat("/proc/1/ns/net"); err == nil {
		if hostSys, ok := hostStat.Sys().(*syscall.Stat_t); ok {
			if targetSys.Ino == hostSys.Ino && targetSys.Dev == hostSys.Dev {
				_ = netnsFile.Close()
				return nil, fmt.Errorf("%w: netns %s has identical inode to host /proc/1/ns/net (%d)",
					ErrInvalidNetns, netnsPath, targetSys.Ino)
			}
		}
	}

	// 2. Verify it differs from self /proc/self/ns/net (bubble namespace)
	if selfStat, err := os.Stat("/proc/self/ns/net"); err == nil {
		if selfSys, ok := selfStat.Sys().(*syscall.Stat_t); ok {
			if targetSys.Ino == selfSys.Ino && targetSys.Dev == selfSys.Dev {
				_ = netnsFile.Close()
				return nil, fmt.Errorf("%w: netns %s has identical inode to self /proc/self/ns/net (%d)",
					ErrInvalidNetns, netnsPath, targetSys.Ino)
			}
		}
	}

	return netnsFile, nil
}

// VerifyPodIP validates that the given IP is a non-loopback, non-zero IPv4 address
// belonging to the expected pod subnet if configured.
func VerifyPodIP(ipStr string, expectedSubnet *net.IPNet) error {
	if ipStr == "" {
		return fmt.Errorf("%w: empty IP", ErrInvalidPodIP)
	}

	cleanIP := strings.Split(ipStr, "/")[0]
	parsedIP := net.ParseIP(cleanIP)
	if parsedIP == nil {
		return fmt.Errorf("%w: cannot parse IP '%s'", ErrInvalidPodIP, ipStr)
	}

	ip4 := parsedIP.To4()
	if ip4 == nil {
		return fmt.Errorf("%w: not an IPv4 address '%s'", ErrInvalidPodIP, ipStr)
	}

	if ip4.IsLoopback() {
		return fmt.Errorf("%w: loopback address '%s' cannot be PodIP", ErrInvalidPodIP, ipStr)
	}

	if ip4.IsUnspecified() {
		return fmt.Errorf("%w: unspecified address '%s' cannot be PodIP", ErrInvalidPodIP, ipStr)
	}

	if expectedSubnet != nil && !expectedSubnet.Contains(parsedIP) {
		return fmt.Errorf("%w: IP '%s' is not in assigned pod subnet %s", ErrInvalidPodIP, ipStr, expectedSubnet.String())
	}

	return nil
}
