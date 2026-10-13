//go:build !linux

package tap

import (
	"errors"
	"net"
	"os"

	"plaid/pkg/packet"
)

var errNoTap = errors.New("TAP device configuration is only supported on Linux")

// TapConfig holds network configuration parameters for the TAP interface.
type TapConfig struct {
	Name    string
	IP      net.IP
	Mask    net.IPMask
	Gateway net.IP
	MAC     net.HardwareAddr
	MTU     int
}

// CreateAndConfigureTap is a stub on non-Linux platforms.
func CreateAndConfigureTap(cfg TapConfig) (*os.File, error) {
	return nil, errNoTap
}

// CreateAndConfigureTapQueues is a stub on non-Linux platforms.
func CreateAndConfigureTapQueues(cfg TapConfig, queues int) ([]*os.File, error) {
	return nil, errNoTap
}

// FileFromFD makes a file of a descriptor.
func FileFromFD(fd int, name string) *os.File { return os.NewFile(uintptr(fd), name) }

// There are no TAPs with virtio headers here.
func hasVnetHdr(*os.File) bool { return false }

func enableOffloads(*os.File) error { return errNoTap }

type vnetWriter struct{}

func newVnetWriter(*os.File) *vnetWriter { return nil }

func (*vnetWriter) write([]byte) error { return errNoTap }

func (*vnetWriter) writeSuper(packet.VnetHeader, *packet.EthernetFrame) error { return errNoTap }

// pollProbe cannot tell whether a frame is waiting.
type pollProbe struct{}

func newPollProbe(*os.File) *pollProbe { return nil }

func (*pollProbe) poll() bool { return false }
