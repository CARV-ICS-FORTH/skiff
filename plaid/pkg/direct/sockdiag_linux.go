//go:build linux

package direct

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The kernel lists the TCP sockets of a network namespace to whoever asks, through the
// netlink socket diagnostics that ss(8) uses (NETLINK_SOCK_DIAG, inet_diag), and with each its
// cookie: the SO_COOKIE of the socket, which is how the table knows its sockets. Listing is
// open to every user; the namespace is the one of the process that asks, so the socket
// factory, which is in the host's, asks. What the kernel lists is the sockets that are
// hashed, which is every socket in a state from SYN_SENT to TIME_WAIT but not one that has
// been reset or has finished (TCP_CLOSE) and that the container still holds: that one is
// reported as gone, and its entry is dropped, though its descriptor is not closed.

const (
	// The layout of struct inet_diag_req_v2 and struct inet_diag_msg (linux/inet_diag.h):
	// after the family, protocol, extension and padding (and, in the request, the states
	// wanted) comes struct inet_diag_sockid, 48 bytes, which ends with the cookie as two
	// 32 bit words in the machine's order, the low one first.
	diagReqLen    = 56
	diagMsgLen    = 72
	diagCookieOff = 4 + 40 // in an inet_diag_msg: after its 4 bytes and the ports, addresses and interface

	sockDiagByFamily = 20        // SOCK_DIAG_BY_FAMILY
	allTCPStates     = 1<<12 - 1 // every state, TIME_WAIT, CLOSE_WAIT and the rest included

	dumpTimeout = 10 * time.Second // for one answer of the kernel
)

// liveTCPCookies returns the cookies of all the TCP sockets of this process's network
// namespace, IPv4 and IPv6, in any state. A dump that the kernel says changed while it was
// made (NLM_F_DUMP_INTR: a socket may have been missed, or listed twice) is an error:
// whoever asks is going to take a socket that is not in the list for a closed one.
func liveTCPCookies() (map[uint64]struct{}, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, fmt.Errorf("open a netlink socket: %w", err)
	}
	defer unix.Close(fd)
	tv := unix.NsecToTimeval(int64(dumpTimeout))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return nil, fmt.Errorf("set the netlink timeout: %w", err)
	}
	cookies := make(map[uint64]struct{})
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := dumpTCP(fd, family, cookies); err != nil {
			return nil, fmt.Errorf("list the TCP sockets of family %d: %w", family, err)
		}
	}
	return cookies, nil
}

// dumpTCP asks for the TCP sockets of a family and adds their cookies.
func dumpTCP(fd int, family uint8, cookies map[uint64]struct{}) error {
	req := make([]byte, unix.NLMSG_HDRLEN+diagReqLen)
	order := binary.NativeEndian
	order.PutUint32(req[0:], uint32(len(req)))
	order.PutUint16(req[4:], sockDiagByFamily)
	order.PutUint16(req[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	order.PutUint32(req[8:], 1) // the sequence number
	body := req[unix.NLMSG_HDRLEN:]
	body[0], body[1] = family, unix.IPPROTO_TCP
	order.PutUint32(body[4:], allTCPStates)
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 1<<18)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Header.Flags&unix.NLM_F_DUMP_INTR != 0 {
				return fmt.Errorf("the list changed while it was made")
			}
			switch m.Header.Type {
			case unix.NLMSG_DONE:
				return nil
			case unix.NLMSG_ERROR:
				if len(m.Data) >= 4 {
					if errno := int32(order.Uint32(m.Data)); errno != 0 {
						return unix.Errno(-errno)
					}
				}
				return fmt.Errorf("the kernel answered with an error message")
			case sockDiagByFamily:
				if len(m.Data) < diagMsgLen {
					return fmt.Errorf("a socket message of %d bytes", len(m.Data))
				}
				c := m.Data[diagCookieOff:]
				cookies[uint64(order.Uint32(c))|uint64(order.Uint32(c[4:]))<<32] = struct{}{}
			}
		}
	}
}
