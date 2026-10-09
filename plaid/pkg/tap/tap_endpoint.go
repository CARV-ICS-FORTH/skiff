package tap

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
)

// TapEndpoint implements bridge.Endpoint for a pod's TAP interface file descriptor. With offloads
// on (NewTapEndpoint) the pod's stack hands over TCP segments of up to 64 KiB with their
// checksums not done: the endpoint cuts them into complete MTU-sized frames as it reads, so that
// the bridge sees only those, and joins consecutive segments of a flow that the bridge delivers
// together (NewWriter) into one segment for the pod. A TAP without a virtio header is read and
// written as plain frames. The README (Offloads) describes the whole.
type TapEndpoint struct {
	epID       string
	epName     string
	epIP       net.IP
	epMAC      net.HardwareAddr
	file       *os.File
	closeOnce  sync.Once
	closedChan chan struct{}

	vnet    bool        // frames have a virtio header
	offload bool        // and the pod may use offloads
	vw      *vnetWriter // writes frames with an empty header; nil without vnet
	rd      []byte      // the buffer reads go to
	seg     packet.Segmenter
	one     [1][]byte
}

// OffloadStats counts the work of the offloads of all TAP endpoints.
type OffloadStats struct {
	ReadSuperFrames  uint64 // TSO segments the pods handed over
	ReadSegments     uint64 // the frames they were cut into
	ReadDropped      uint64 // frames whose virtio header did not describe them, dropped
	WriteSuperFrames uint64 // writes that joined several segments
	WriteSegments    uint64 // the segments in them
}

var stats struct {
	readSuper, readSegments, readDropped, writeSuper, writeSegments atomic.Uint64
}

// Offloads returns the counters of the TAP offloads.
func Offloads() OffloadStats {
	return OffloadStats{
		ReadSuperFrames:  stats.readSuper.Load(),
		ReadSegments:     stats.readSegments.Load(),
		ReadDropped:      stats.readDropped.Load(),
		WriteSuperFrames: stats.writeSuper.Load(),
		WriteSegments:    stats.writeSegments.Load(),
	}
}

var offloadWarning sync.Once

// NewTapEndpoint creates an Endpoint backed by an open TAP file descriptor. With
// offload, it asks the TAP for checksum and TCP segmentation offloads if it has a
// virtio header; if the kernel refuses, it says so once and goes on without.
func NewTapEndpoint(id, name string, ip net.IP, mac net.HardwareAddr, file *os.File, offload bool) *TapEndpoint {
	t := &TapEndpoint{
		epID:       id,
		epName:     name,
		epIP:       ip,
		epMAC:      mac,
		file:       file,
		closedChan: make(chan struct{}),
	}
	if hasVnetHdr(file) {
		t.vw = newVnetWriter(file)
	}
	t.vnet = t.vw != nil
	if t.vnet && offload {
		if err := enableOffloads(file); err != nil {
			offloadWarning.Do(func() {
				fmt.Printf("[plaidd/tap] TAP offloads are not available (%v): continuing without them\n", err)
			})
		} else {
			t.offload = true
		}
	}
	return t
}

func (t *TapEndpoint) ID() string            { return t.epID }
func (t *TapEndpoint) Name() string          { return t.epName }
func (t *TapEndpoint) IP() net.IP            { return t.epIP }
func (t *TapEndpoint) MAC() net.HardwareAddr { return t.epMAC }
func (t *TapEndpoint) File() *os.File        { return t.file }

// Offload reports whether the TAP has its offloads on.
func (t *TapEndpoint) Offload() bool { return t.offload }

// errClosed returns the error for a write to a closed endpoint, or nil if it is open.
func (t *TapEndpoint) errClosed() error {
	select {
	case <-t.closedChan:
		return fmt.Errorf("endpoint %s is closed", t.epID)
	default:
		return nil
	}
}

// Write sends an Ethernet frame into the TAP device (received inside the pod netns).
func (t *TapEndpoint) Write(frame []byte) error {
	if err := t.errClosed(); err != nil {
		return err
	}

	if t.vw != nil {
		return t.vw.write(frame)
	}
	_, err := t.file.Write(frame)
	return err
}

// ReadFrames waits for the pod to send and returns the Ethernet frames of what it sent:
// one, or the segments of a TSO segment. They are complete, with their checksums done,
// and valid until the next call. A packet that the TAP's virtio header does not describe
// is dropped and counted: the result is empty and not an error.
func (t *TapEndpoint) ReadFrames() ([][]byte, error) {
	if t.rd == nil {
		t.rd = make([]byte, packet.VnetHeaderLen+65535)
	}
	n, err := t.file.Read(t.rd)
	if err != nil || n == 0 {
		return nil, err
	}
	if !t.vnet {
		t.one[0] = t.rd[:n]
		return t.one[:], nil
	}
	if n < packet.VnetHeaderLen {
		stats.readDropped.Add(1)
		return nil, nil
	}
	h := packet.ParseVnetHeader(t.rd)
	frames, err := t.seg.Segment(h, t.rd[packet.VnetHeaderLen:n])
	switch {
	case err != nil:
		stats.readDropped.Add(1)
	case h.GSOType != packet.VnetGSONone:
		stats.readSuper.Add(1)
		stats.readSegments.Add(uint64(len(frames)))
	}
	return frames, nil
}

// StartReadLoop starts a goroutine reading Ethernet frames from the TAP device and processing them through the Bridge.
func (t *TapEndpoint) StartReadLoop(ctx context.Context, b *bridge.Bridge) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.closedChan:
				return
			default:
			}

			frames, err := t.ReadFrames()
			if err != nil {
				select {
				case <-t.closedChan:
					return
				case <-ctx.Done():
					return
				default:
					// TAP device closed or failed (e.g. netns deleted).
					// Deregister dead endpoint from bridge.
					_ = b.RemoveEndpoint(t.epID)
					return
				}
			}

			if len(frames) > 0 {
				_ = b.ProcessFrames(t, frames)
			}
		}
	}()
}

// Close closes the TAP device file.
func (t *TapEndpoint) Close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.closedChan)
		if t.file != nil {
			err = t.file.Close()
		}
	})
	return err
}
