//go:build linux && (amd64 || arm64)

package direct

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The kernel's structures of the user notification (include/uapi/linux/seccomp.h).

// notif is a system call held for the supervisor.
type notif struct {
	ID    uint64
	Pid   uint32 // the thread, in the supervisor's pid namespace (0 if it is not in it)
	Flags uint32
	Nr    int32
	Arch  uint32
	IP    uint64
	Args  [6]uint64
}

// notifResp is the supervisor's answer: let the call run (flagContinue), or its result.
type notifResp struct {
	ID    uint64
	Val   int64
	Errno int32 // minus the errno
	Flags uint32
}

// notifAddfd puts a descriptor of the supervisor in the held task's table.
type notifAddfd struct {
	ID         uint64
	Flags      uint32
	Srcfd      uint32
	Newfd      uint32
	NewfdFlags uint32
}

const flagContinue = 1 // SECCOMP_USER_NOTIF_FLAG_CONTINUE

// The system calls the container's are held for, whatever their arguments. Everything else
// runs without a stop, but for what installFilter says.
var filteredCalls = []int{
	unix.SYS_CONNECT,
	unix.SYS_BIND,
	unix.SYS_LISTEN,
	unix.SYS_ACCEPT,
	unix.SYS_ACCEPT4,
	unix.SYS_GETSOCKNAME,
	unix.SYS_GETPEERNAME,
}

// installFilter installs, on the calling thread and so on what it starts, a filter that
// holds the system calls in calls for a supervisor, and returns the descriptor on which
// the supervisor receives them. The calling thread must not be used for anything else
// afterwards: a filter cannot be removed.
//
// A socket that was put in place of the container's is a socket of the host's network, so the calls
// the supervisor cannot see do not run: io_uring and the calls of
// another ABI (a 32-bit program on a 64-bit kernel, x32), which fail with ENOSYS. And what could
// use one with a look at it first is held, only if it is that kind of call, so that a send or an
// ioctl of any other kind does not stop: a send that connects (MSG_FASTOPEN), and an ioctl of
// the network's (0x89xx). See Supervisor.onHostSocket.
func installFilter(calls []int) (int, error) {
	const (
		ldAbs  = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq    = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jge    = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
		jset   = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
		and    = unix.BPF_ALU | unix.BPF_AND | unix.BPF_K
		ret    = unix.BPF_RET | unix.BPF_K
		enosys = unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)
		// Where seccomp_data has the low half of an argument, on a little-endian machine.
		argLow = 16
	)
	prog := []unix.SockFilter{
		{Code: ldAbs, K: 4}, // seccomp_data.arch
		{Code: jeq, Jt: 1, K: auditArch},
		{Code: ret, K: enosys},
		{Code: ldAbs, K: 0}, // seccomp_data.nr
	}
	if abiBit != 0 {
		prog = append(prog,
			unix.SockFilter{Code: jge, Jf: 1, K: abiBit},
			unix.SockFilter{Code: ret, K: enosys})
	}
	prog = append(prog,
		unix.SockFilter{Code: jeq, Jf: 1, K: unix.SYS_IO_URING_SETUP},
		unix.SockFilter{Code: ret, K: enosys})
	for _, nr := range calls {
		prog = append(prog,
			unix.SockFilter{Code: jeq, Jf: 1, K: uint32(nr)},
			unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_USER_NOTIF})
	}
	for _, send := range []struct {
		nr  uint32
		arg uint32 // of the flags
	}{{unix.SYS_SENDTO, 3}, {unix.SYS_SENDMSG, 2}, {unix.SYS_SENDMMSG, 3}} {
		prog = append(prog,
			unix.SockFilter{Code: jeq, Jf: 4, K: send.nr},
			unix.SockFilter{Code: ldAbs, K: argLow + 8*send.arg},
			unix.SockFilter{Code: jset, Jf: 1, K: unix.MSG_FASTOPEN},
			unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_USER_NOTIF},
			unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ALLOW})
	}
	prog = append(prog,
		unix.SockFilter{Code: jeq, Jf: 6, K: unix.SYS_IOCTL},
		unix.SockFilter{Code: ldAbs, K: argLow + 8*1},
		unix.SockFilter{Code: and, K: 0xff00},
		unix.SockFilter{Code: jeq, Jt: 1, K: 0x8900}, // the network's
		unix.SockFilter{Code: jeq, Jf: 1, K: 0x8b00}, // wireless
		unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_USER_NOTIF},
		unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ALLOW})
	prog = append(prog, unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ALLOW})
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return -1, fmt.Errorf("no_new_privs: %w", err)
	}
	fd, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_NEW_LISTENER, uintptr(unsafe.Pointer(&fprog)))
	if errno != 0 {
		return -1, fmt.Errorf("seccomp: %w", errno)
	}
	return int(fd), nil
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

func ioctlRecv(listener int, n *notif) error {
	*n = notif{}
	return ioctl(listener, unix.SECCOMP_IOCTL_NOTIF_RECV, unsafe.Pointer(n))
}

func ioctlSend(listener int, r *notifResp) error {
	return ioctl(listener, unix.SECCOMP_IOCTL_NOTIF_SEND, unsafe.Pointer(r))
}

// ioctlValid says whether the held call is still waiting: it is not once the thread was
// interrupted by a signal, or was killed.
func ioctlValid(listener int, id uint64) bool {
	return ioctl(listener, unix.SECCOMP_IOCTL_NOTIF_ID_VALID, unsafe.Pointer(&id)) == nil
}

// ioctlAddfd puts fd in the held task's table (flags: SECCOMP_ADDFD_FLAG_*) and returns
// the number it got there.
func ioctlAddfd(listener int, a *notifAddfd) (int, error) {
	r, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(listener), unix.SECCOMP_IOCTL_NOTIF_ADDFD, uintptr(unsafe.Pointer(a)))
	if errno != 0 {
		return -1, errno
	}
	return int(r), nil
}
