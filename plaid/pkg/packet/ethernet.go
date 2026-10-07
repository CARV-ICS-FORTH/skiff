package packet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

const (
	EtherTypeIPv4 = 0x0800
	EtherTypeARP  = 0x0806
	EtherTypeIPv6 = 0x86DD

	EthernetHeaderLen = 14
)

// EthernetFrame represents a parsed L2 Ethernet frame.
type EthernetFrame struct {
	DstMAC    net.HardwareAddr
	SrcMAC    net.HardwareAddr
	EtherType uint16
	Payload   []byte
}

var (
	ErrPacketTooShort = errors.New("packet too short")
	ErrInvalidMAC     = errors.New("invalid MAC address")
)

// ParseEthernet parses raw bytes into an EthernetFrame.
func ParseEthernet(data []byte) (*EthernetFrame, error) {
	if len(data) < EthernetHeaderLen {
		return nil, ErrPacketTooShort
	}

	frame := &EthernetFrame{
		DstMAC:    net.HardwareAddr(append([]byte(nil), data[0:6]...)),
		SrcMAC:    net.HardwareAddr(append([]byte(nil), data[6:12]...)),
		EtherType: binary.BigEndian.Uint16(data[12:14]),
		Payload:   data[14:],
	}

	return frame, nil
}

// Marshal encodes an EthernetFrame into a newly allocated slice.
func (f *EthernetFrame) Marshal() ([]byte, error) {
	buf, err := f.MarshalTo(make([]byte, 0, EthernetHeaderLen+len(f.Payload)))
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// MarshalTo appends the encoded frame to dst and returns the extended slice. On error
// dst is returned as it was.
func (f *EthernetFrame) MarshalTo(dst []byte) ([]byte, error) {
	if len(f.DstMAC) != 6 || len(f.SrcMAC) != 6 {
		return dst, ErrInvalidMAC
	}

	dst = append(dst, f.DstMAC...)
	dst = append(dst, f.SrcMAC...)
	dst = binary.BigEndian.AppendUint16(dst, f.EtherType)
	return append(dst, f.Payload...), nil
}

// IsBroadcast returns true if the destination MAC is ff:ff:ff:ff:ff:ff.
func (f *EthernetFrame) IsBroadcast() bool {
	if len(f.DstMAC) != 6 {
		return false
	}
	return f.DstMAC[0] == 0xff && f.DstMAC[1] == 0xff && f.DstMAC[2] == 0xff &&
		f.DstMAC[3] == 0xff && f.DstMAC[4] == 0xff && f.DstMAC[5] == 0xff
}

// IsMulticast returns true if the destination MAC has the multicast bit set.
func (f *EthernetFrame) IsMulticast() bool {
	if len(f.DstMAC) == 0 {
		return false
	}
	return (f.DstMAC[0] & 0x01) != 0
}

func (f *EthernetFrame) String() string {
	return fmt.Sprintf("Ethernet(src=%s, dst=%s, type=0x%04x, len=%d)",
		f.SrcMAC, f.DstMAC, f.EtherType, len(f.Payload))
}
