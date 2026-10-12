package packet

import (
	"errors"
	"fmt"
)

const (
	VXLANHeaderLen   = 8
	VXLANFlags       = 0x08 // 'I' flag indicating valid VNI
	DefaultVXLANPort = 8472
	DefaultVNI       = 1
)

// Two of the header's reserved flag bits, which RFC 7348 says a receiver ignores and
// ParseVXLAN does (it checks the I flag only): a node that sends the first says that it
// takes TCP super-frames whole, and the second marks a datagram as a chunk of one (see
// the vxlan package). They are not bits that Linux's own VXLAN device gives a meaning.
const (
	VXLANFlagSuperOK = 0x20
	VXLANFlagChunk   = 0x10
)

var (
	ErrVXLANPacketTooShort = errors.New("vxlan packet too short")
	ErrInvalidVXLANFlags   = errors.New("invalid vxlan flags (missing I-flag)")
)

// VXLANHeader represents the 8-byte VXLAN encapsulation header.
type VXLANHeader struct {
	Flags uint8
	VNI   uint32 // 24-bit VNI
}

// VXLANPacket represents an inner Ethernet frame encapsulated with a VXLAN header.
type VXLANPacket struct {
	Header     VXLANHeader
	InnerFrame *EthernetFrame
}

// ParseVXLAN parses the UDP payload as a VXLAN packet containing an inner Ethernet frame.
func ParseVXLAN(data []byte) (*VXLANPacket, error) {
	if len(data) < VXLANHeaderLen+EthernetHeaderLen {
		return nil, ErrVXLANPacketTooShort
	}

	flags := data[0]
	if (flags & VXLANFlags) == 0 {
		return nil, ErrInvalidVXLANFlags
	}

	// 24-bit VNI in bytes 4, 5, 6
	vni := uint32(data[4])<<16 | uint32(data[5])<<8 | uint32(data[6])

	innerFrame, err := ParseEthernet(data[VXLANHeaderLen:])
	if err != nil {
		return nil, fmt.Errorf("failed to parse inner frame: %w", err)
	}

	return &VXLANPacket{
		Header: VXLANHeader{
			Flags: flags,
			VNI:   vni,
		},
		InnerFrame: innerFrame,
	}, nil
}

// AppendVXLANHeader appends the 8-byte VXLAN header for vni to dst, with the flags byte
// given, which has the I flag and may have the VXLANFlag bits.
func AppendVXLANHeader(dst []byte, flags byte, vni uint32) []byte {
	// bytes 1, 2, 3 and 7 are reserved (0)
	return append(dst, flags, 0, 0, 0, byte(vni>>16), byte(vni>>8), byte(vni), 0)
}

// MarshalVXLAN encapsulates an inner Ethernet frame with a VXLAN header.
func MarshalVXLAN(vni uint32, innerFrameBytes []byte) []byte {
	buf := AppendVXLANHeader(make([]byte, 0, VXLANHeaderLen+len(innerFrameBytes)), VXLANFlags, vni)
	return append(buf, innerFrameBytes...)
}

// Marshal encodes a VXLANPacket into raw bytes.
func (p *VXLANPacket) Marshal() ([]byte, error) {
	innerBytes, err := p.InnerFrame.Marshal()
	if err != nil {
		return nil, err
	}
	return MarshalVXLAN(p.Header.VNI, innerBytes), nil
}

func (p *VXLANPacket) String() string {
	return fmt.Sprintf("VXLAN(vni=%d, inner=%s)", p.Header.VNI, p.InnerFrame)
}
