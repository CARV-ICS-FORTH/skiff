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

// The system calls the container's are held for. Everything else runs without a stop.
var filteredCalls = []int{
	unix.SYS_CONNECT,
	unix.SYS_BIND,
	unix.SYS_ACCEPT,
	unix.SYS_ACCEPT4,
	unix.SYS_GETSOCKNAME,
	unix.SYS_GETPEERNAME,
}

// installFilter installs, on the calling thread and so on what it starts, a filter that
// holds the system calls in calls for a supervisor, and returns the descriptor on which
// the supervisor receives them. Other architectures' calls (a 32-bit program on a
// 64-bit kernel) are not held. The calling thread must not be used for anything else
// afterwards: a filter cannot be removed.
func installFilter(calls []int) (int, error) {
	const (
		ldAbs = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq   = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		ret   = unix.BPF_RET | unix.BPF_K
	)
	prog := []unix.SockFilter{
		{Code: ldAbs, K: 4}, // seccomp_data.arch
		{Code: jeq, Jt: 1, K: auditArch},
		{Code: ret, K: unix.SECCOMP_RET_ALLOW},
		{Code: ldAbs, K: 0}, // seccomp_data.nr
	}
	for _, nr := range calls {
		prog = append(prog,
			unix.SockFilter{Code: jeq, Jf: 1, K: uint32(nr)},
			unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_USER_NOTIF})
	}
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
