package packet

import "encoding/binary"

// VnetHeaderLen is the size of the struct virtio_net_hdr that a TAP opened with
// IFF_VNET_HDR puts before every frame it returns and expects before every frame it is
// given.
const VnetHeaderLen = 10

// VnetHeader flags and GSO types (VIRTIO_NET_HDR_F_* and VIRTIO_NET_HDR_GSO_*).
const (
	VnetNeedsCsum = 1 // the checksum of the transport header is not complete
	VnetDataValid = 2 // the kernel has checked the checksum of a frame it hands over

	VnetGSONone  = 0
	VnetGSOTCPv4 = 1
	VnetGSOTCPv6 = 4
	VnetGSOECN   = 0x80 // added to a TCP type when the first segment has CWR
)

// VnetHeader is a virtio_net_hdr. For a frame the kernel hands over it says what the
// sender left undone: with NeedsCsum, the sum from CsumStart is to be written at
// CsumStart+CsumOffset; with a GSO type, the frame is a TCP segment of up to 64 KiB that
// stands for several of GSOSize bytes of payload each. For a frame written to the TAP it
// says the same of the frame.
type VnetHeader struct {
	Flags      uint8
	GSOType    uint8
	HdrLen     uint16
	GSOSize    uint16
	CsumStart  uint16
	CsumOffset uint16
}

// The kernel uses the byte order of the host for the header (a TAP is not little-endian
// or big-endian unless asked).

// ParseVnetHeader decodes the first VnetHeaderLen bytes of b.
func ParseVnetHeader(b []byte) VnetHeader {
	e := binary.NativeEndian
	return VnetHeader{
		Flags: b[0], GSOType: b[1],
		HdrLen: e.Uint16(b[2:]), GSOSize: e.Uint16(b[4:]),
		CsumStart: e.Uint16(b[6:]), CsumOffset: e.Uint16(b[8:]),
	}
}

// Put encodes h into the first VnetHeaderLen bytes of b.
func (h VnetHeader) Put(b []byte) {
	e := binary.NativeEndian
	b[0], b[1] = h.Flags, h.GSOType
	e.PutUint16(b[2:], h.HdrLen)
	e.PutUint16(b[4:], h.GSOSize)
	e.PutUint16(b[6:], h.CsumStart)
	e.PutUint16(b[8:], h.CsumOffset)
}
