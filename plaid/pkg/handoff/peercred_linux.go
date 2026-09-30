//go:build linux

package handoff

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the user id of the process on the other end, as seen in this process's
// user namespace (a host user appears as root inside a fakeroot bubble).
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if e != nil {
			cerr = e
			return
		}
		uid = cred.Uid
	}); err != nil {
		return 0, err
	}
	return uid, cerr
}
