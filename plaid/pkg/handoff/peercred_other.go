//go:build !linux && !darwin

package handoff

import (
	"errors"
	"net"
)

func peerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("peer credentials are not supported on this platform")
}
