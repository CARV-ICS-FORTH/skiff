package vxlan

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"plaid/pkg/packet"
)

// Encryption of the overlay's datagrams with a key of the cluster (OverlayConfig.Key), in AES-GCM.
// On the wire a datagram is the VXLAN header, who sent it and since when and for which node (12
// bytes), all of it authenticated and not encrypted, a nonce of 12 bytes, the encrypted frame and a
// tag of 16 bytes. A node is named by its pod subnet, which its route says to a sender and which it
// knows itself, and takes only what is for it.
//
// A nonce must never be used twice under the key, by any node or after any restart: it is 5 random
// bytes that an engine chooses when it is made and a counter of 7 bytes. Every engine counts from
// 1, so two with the same prefix would repeat nonces: 40 random bytes' worth of prefixes make that
// unlikely for the few thousand starts in the life of a cluster's key.
//
// A receiver drops what it has seen. For each sender, known by the pod subnet it authenticates and not by
// the address a datagram claims, it takes only the latest start, by the time in seconds the sender
// says, so a run that has ended cannot be played again, however often the sender started since. In
// that second it keeps a window of the counters of each prefix, and drops what it has seen or what
// is older than the window. The window is wide because the datagrams of one sender arrive on several
// sockets, which are read at their own pace.
//
// A receiver that starts again has seen nothing, and until the sender's next datagrams move its
// window along it cannot tell what is played to it from that start. And a sender whose clock went
// back across a start is refused until the clock has passed it again (counted, and logged once).
const (
	overlayKeyLen      = 32
	overlayFreshLen    = 12 // the sender's subnet, the second it started and the receiver's subnet
	overlayAuthedLen   = packet.VXLANHeaderLen + overlayFreshLen
	overlayNonceLen    = 12
	overlayRoomLen     = overlayFreshLen + overlayNonceLen // between the VXLAN header and the frame
	overlayTagLen      = 16
	overlayCipherBytes = overlayRoomLen + overlayTagLen

	replayWindowBits = 1 << 16
	replayPrefixes   = 4
)

// overlayCipher is what an engine with a key has to encrypt and decrypt with.
type overlayCipher struct {
	aead    cipher.AEAD
	started uint32 // the second it started
	local   uint32 // its node's pod subnet
	prefix  [5]byte
	sent    atomic.Uint64

	mu      sync.RWMutex
	senders map[uint32]*replaySender
}

func newOverlayCipher(key []byte, local *net.IPNet) (*overlayCipher, error) {
	if local == nil {
		return nil, errors.New("the overlay's key needs the node's pod subnet")
	}
	if len(key) != overlayKeyLen {
		return nil, fmt.Errorf("the overlay's key has %d bytes, not %d", len(key), overlayKeyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	c := &overlayCipher{aead: aead, local: subnetName(local), started: uint32(time.Now().Unix()), senders: make(map[uint32]*replaySender)}
	if _, err := rand.Read(c.prefix[:]); err != nil {
		return nil, err
	}
	return c, nil
}

// subnetName is what a node is called in a datagram: its subnet's address.
func subnetName(subnet *net.IPNet) uint32 {
	if subnet == nil || subnet.IP.To4() == nil {
		return 0
	}
	return binary.BigEndian.Uint32(subnet.IP.To4())
}

// ReadKey reads OverlayConfig.Key from a file of 64 hexadecimal digits.
func ReadKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != overlayKeyLen {
		return nil, fmt.Errorf("the overlay's key in %s is not %d hexadecimal digits", path, 2*overlayKeyLen)
	}
	return key, nil
}

// seal encrypts in place the datagram at b[start:], which has overlayRoomLen bytes of room after
// its VXLAN header, for the node of the subnet named to, and returns b with the tag behind it.
func (c *overlayCipher) seal(b []byte, start int, to uint32) []byte {
	b = slices.Grow(b, overlayTagLen)
	header := b[start : start+overlayAuthedLen]
	binary.BigEndian.PutUint32(header[packet.VXLANHeaderLen:], c.local)
	binary.BigEndian.PutUint32(header[packet.VXLANHeaderLen+4:], c.started)
	binary.BigEndian.PutUint32(header[packet.VXLANHeaderLen+8:], to)
	nonce := b[start+overlayAuthedLen : start+overlayAuthedLen+overlayNonceLen]
	copy(nonce, c.prefix[:])
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], c.sent.Add(1))
	copy(nonce[5:], count[1:])
	plain := b[start+overlayAuthedLen+overlayNonceLen:]
	sealed := c.aead.Seal(plain[:0], nonce, plain, header)
	return b[:start+overlayAuthedLen+overlayNonceLen+len(sealed)]
}

// open decrypts a datagram in place, and returns it as a datagram without a key has it: the VXLAN
// header and the frame. It is false for one that does not authenticate or is not fresh.
func (c *overlayCipher) open(datagram []byte) ([]byte, bool) {
	if len(datagram) < packet.VXLANHeaderLen+overlayCipherBytes {
		return nil, false
	}
	authed := datagram[:overlayAuthedLen]
	nonce := datagram[overlayAuthedLen : overlayAuthedLen+overlayNonceLen]
	body := datagram[overlayAuthedLen+overlayNonceLen:]
	if _, err := c.aead.Open(body[:0], nonce, body, authed); err != nil {
		return nil, false
	}
	if binary.BigEndian.Uint32(authed[packet.VXLANHeaderLen+8:]) != c.local {
		return nil, false
	}
	var count [8]byte
	copy(count[1:], nonce[5:])
	var prefix [5]byte
	copy(prefix[:], nonce)
	name := binary.BigEndian.Uint32(authed[packet.VXLANHeaderLen:])
	started := binary.BigEndian.Uint32(authed[packet.VXLANHeaderLen+4:])
	if !c.sender(name).fresh(started, prefix, binary.BigEndian.Uint64(count[:])) {
		return nil, false
	}
	copy(datagram[overlayRoomLen:], datagram[:packet.VXLANHeaderLen]) // (the header moves up to meet the frame)
	return datagram[overlayRoomLen : len(datagram)-overlayTagLen], true
}

func (c *overlayCipher) sender(name uint32) *replaySender {
	c.mu.RLock()
	s := c.senders[name]
	c.mu.RUnlock()
	if s != nil {
		return s
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s = c.senders[name]; s == nil {
		s = &replaySender{}
		c.senders[name] = s
	}
	return s
}

// replaySender is what a receiver knows of a sender: the second of its latest start, and the windows
// of the prefixes seen in it (more than one only for starts within a second); a new prefix takes the
// place of the one that was used longest ago.
type replaySender struct {
	mu      sync.Mutex
	started uint32
	windows [replayPrefixes]*replayWindow
	last    int
}

func (s *replaySender) fresh(started uint32, prefix [5]byte, count uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if started < s.started {
		return false
	}
	if started > s.started {
		s.started, s.windows, s.last = started, [replayPrefixes]*replayWindow{}, 0
	}
	w := s.windows[s.last]
	if w == nil || w.prefix != prefix {
		w = nil
		for i, other := range s.windows {
			if other != nil && other.prefix == prefix {
				w, s.last = other, i
				break
			}
		}
		if w == nil {
			oldest := (s.last + 1) % replayPrefixes
			w = &replayWindow{prefix: prefix}
			s.windows[oldest], s.last = w, oldest
		}
	}
	return w.fresh(count)
}

// replayWindow is the counters seen of one prefix: those below top, as far as the window goes.
type replayWindow struct {
	prefix [5]byte
	top    uint64 // one above the highest counter seen
	seen   [replayWindowBits / 64]uint64
}

func (w *replayWindow) fresh(count uint64) bool {
	if count >= w.top {
		// Forget what the window moves past.
		if count-w.top >= replayWindowBits {
			clear(w.seen[:])
		} else {
			for c := w.top; c <= count; c++ {
				w.seen[(c%replayWindowBits)/64] &^= 1 << (c % 64)
			}
		}
		w.top = count + 1
	} else if w.top-count >= replayWindowBits {
		return false
	}
	word, bit := (count%replayWindowBits)/64, uint64(1)<<(count%64)
	if w.seen[word]&bit != 0 {
		return false
	}
	w.seen[word] |= bit
	return true
}
