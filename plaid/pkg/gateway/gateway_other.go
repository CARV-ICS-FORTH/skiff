//go:build !linux

package gateway

import (
	"fmt"
	"net"
	"os"
)

type Config struct {
	Name, Uplink                       string
	GuestAddress, GatewayIP            net.IP
	ServiceNextHop                     net.IP
	GatewayMAC                         net.HardwareAddr
	NodeCIDR, ClusterCIDR, ServiceCIDR *net.IPNet
	MTU                                int
}

type Attachment struct{ File *os.File }

func Start(Config) (*Attachment, error) { return nil, fmt.Errorf("TAP gateway requires Linux") }
func (*Attachment) Close() error        { return nil }
