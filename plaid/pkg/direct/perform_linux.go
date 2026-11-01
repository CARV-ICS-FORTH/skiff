//go:build linux && (amd64 || arm64)

package direct

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Once the container has a socket of the host's network, a call that is held is not let through.
// The kernel would look the descriptor and the address up again when it runs the call, and another
// thread of the container could change them after the supervisor has looked. The supervisor runs
// the call itself instead, on its own duplicate of the descriptor and its own copy of the address,
// and answers with the result: what it looked at is what runs.
//
// bind, connect and listen run on a thread of their own that first becomes the held thread as far
// as the kernel's checks go (see becomeHeld), and is never used again. That is only for the calls
// that are held, which are about a socket's addresses: read, write and the sends that are not held
// never stop. What it costs a process: a program that asks SO_PEERCRED of the other end of a Unix
// socket that connected or listened this way sees the supervisor's process (and the user and groups
// of the other end). The window is the first swap itself: a call already let through when the first
// socket is put in place. A process that is not under the supervisor and is handed such a socket
// (over a Unix socket) is not kept to it.

// sockaddrMax is the size of a struct sockaddr_storage.
const sockaddrMax = 128

func (s *Supervisor) hostSocketsExist() bool { return s.table.Len() > 0 || s.hostNet.Load() != 0 }

// perform runs a held call that would have been let through.
func (s *Supervisor) perform(n *notif) reply {
	switch int(n.Nr) {
	case unix.SYS_SENDTO, unix.SYS_SENDMSG, unix.SYS_SENDMMSG:
		return failWith(unix.EOPNOTSUPP) // only those with MSG_FASTOPEN are held
	}
	tid := int(n.Pid)
	dup, err := s.targets.getfd(tid, int(n.Args[0]))
	if errors.Is(err, unix.EBADF) {
		return failWith(unix.EBADF)
	}
	if err != nil {
		return s.guardFailed(err)
	}
	defer unix.Close(dup)
	sock, host := s.hostSocket(dup)
	switch int(n.Nr) {
	case unix.SYS_CONNECT:
		if host {
			return failWith(unix.EISCONN)
		}
		return s.performAddress(n, dup, unix.SYS_CONNECT)
	case unix.SYS_BIND:
		if host {
			return failWith(unix.EINVAL)
		}
		return s.performAddress(n, dup, unix.SYS_BIND)
	case unix.SYS_LISTEN:
		if host && sock.Kind != KindListener {
			return failWith(unix.EINVAL)
		}
		backlog := uintptr(n.Args[1])
		return s.asHeld(n, dup, false, func(fd int) reply {
			return resultOf(unix.Syscall(unix.SYS_LISTEN, uintptr(fd), backlog, 0))
		})
	case unix.SYS_GETSOCKNAME, unix.SYS_GETPEERNAME:
		if host {
			return failWith(unix.ENOTCONN)
		}
		return s.whileHeld(n, func() reply { return s.performName(n, dup) })
	case unix.SYS_IOCTL:
		if host {
			return failWith(unix.EPERM)
		}
		return s.whileHeld(n, func() reply { return s.performIoctl(n, dup) })
	}
	return s.guardFailed(errors.New("a call the supervisor does not know how to run"))
}

func resultOf(r1, _ uintptr, errno unix.Errno) reply {
	if errno != 0 {
		return failWith(errno)
	}
	return reply{val: int64(r1)}
}

// whileHeld runs what writes to the memory of the held thread under the task's lock and only if
// the call is still held, as the calls that were swapped do (see threadLocks).
func (s *Supervisor) whileHeld(n *notif, write func() reply) reply {
	t := s.threads.lock(int(n.Pid))
	defer t.unlock()
	if !s.valid(n) {
		return cancelled
	}
	return write()
}

// performAddress runs connect or bind with a copy of the address, as the held thread.
func (s *Supervisor) performAddress(n *notif, dup int, nr uintptr) reply {
	length := int(int32(n.Args[2]))
	if length < 0 || length > sockaddrMax {
		return failWith(unix.EINVAL)
	}
	addr := make([]byte, length, sockaddrMax)
	if err := readRemote(int(n.Pid), n.Args[1], addr); err != nil {
		return failWith(unix.EFAULT)
	}
	files := length > 2 && binary.NativeEndian.Uint16(addr) == unix.AF_UNIX && addr[2] != 0
	return s.asHeld(n, dup, files, func(fd int) reply {
		var p unsafe.Pointer
		if length > 0 {
			p = unsafe.Pointer(&addr[0])
		}
		return resultOf(unix.Syscall(nr, uintptr(fd), uintptr(p), uintptr(length)))
	})
}

// asHeld runs call on a duplicate of dup, on a thread that has become the held thread. What the
// thread was told is checked to be still that thread's before it is used: a call that has been given
// up may have left its number to another. A call that takes long is left to finish on its own if the
// thread gives it up, as the kernel does with a connect that is interrupted.
func (s *Supervisor) asHeld(n *notif, dup int, files bool, call func(fd int) reply) reply {
	own, err := unix.FcntlInt(uintptr(dup), unix.F_DUPFD_CLOEXEC, 0) // it may outlive this call
	if err != nil {
		return failWith(errnoOf(err))
	}
	tid, valid := int(n.Pid), s.validFunc(n)
	done := make(chan reply, 1)
	go func() {
		// Never unlocked: the thread ends with this goroutine, with all that it has become.
		runtime.LockOSThread()
		defer unix.Close(own)
		if err := becomeHeld(tid, files); err != nil {
			done <- s.guardFailed(err)
			return
		}
		if !valid() {
			done <- cancelled
			return
		}
		done <- call(own)
	}()
	for {
		select {
		case r := <-done:
			return r
		case <-time.After(200 * time.Millisecond):
			if !valid() {
				return cancelled
			}
		}
	}
}

// heldThread is what /proc/<tid>/status says of a thread, in this process's user namespace.
type heldThread struct {
	uid, gid int
	groups   []int
	caps     uint64
	umask    int
}

func readHeldThread(tid int) (heldThread, error) {
	var h heldThread
	data, err := os.ReadFile("/proc/" + strconv.Itoa(tid) + "/status")
	if err != nil {
		return h, err
	}
	seen := 0
	for _, line := range strings.Split(string(data), "\n") {
		name, value, _ := strings.Cut(line, ":")
		f := strings.Fields(value)
		switch {
		case name == "Uid" && len(f) >= 2:
			h.uid, err = strconv.Atoi(f[1])
			seen++
		case name == "Gid" && len(f) >= 2:
			h.gid, err = strconv.Atoi(f[1])
			seen++
		case name == "Groups":
			for _, g := range f {
				id, gerr := strconv.Atoi(g)
				if gerr != nil {
					return h, gerr
				}
				h.groups = append(h.groups, id)
			}
			seen++
		case name == "CapEff" && len(f) == 1:
			h.caps, err = strconv.ParseUint(f[0], 16, 64)
			seen++
		case name == "Umask" && len(f) == 1:
			var mask int64
			mask, err = strconv.ParseInt(f[0], 8, 32)
			h.umask = int(mask)
			seen++
		}
		if err != nil {
			return h, err
		}
	}
	if seen != 5 {
		return h, fmt.Errorf("the status of thread %d says %d of 5 things", tid, seen)
	}
	return h, nil
}

// becomeHeld gives this locked thread, for good, what the kernel checks of the held thread: with
// files its mount namespace, root, working directory and umask, for a path; and its user, groups
// and capabilities. Capabilities are over a user namespace: a thread in one of its own has none
// over those that a call here is checked against, as it would not have itself.
func becomeHeld(tid int, files bool) error {
	h, err := readHeldThread(tid)
	if err != nil {
		return err
	}
	proc := "/proc/" + strconv.Itoa(tid) + "/"
	theirNS, err := os.Readlink(proc + "ns/user")
	if err != nil {
		return err
	}
	ourNS, err := os.Readlink("/proc/self/ns/user")
	if err != nil {
		return err
	}
	if theirNS != ourNS {
		h.caps = 0
	}
	if files {
		if err := enterFiles(proc, h.umask); err != nil {
			return err
		}
	}
	const keep = ^uintptr(0)
	if unix.Geteuid() != h.uid || unix.Getegid() != h.gid {
		// Prctl: what it may keep for the capset below, through the change of user.
		if err := unix.Prctl(unix.PR_SET_KEEPCAPS, 1, 0, 0, 0); err != nil {
			return err
		}
	}
	ours, err := unix.Getgroups()
	if err != nil {
		return err
	}
	if !slices.Equal(sorted(ours), sorted(h.groups)) {
		list := make([]uint32, len(h.groups)+1) // (never empty, for its address)
		for i, g := range h.groups {
			list[i] = uint32(g)
		}
		if _, _, e := unix.RawSyscall(unix.SYS_SETGROUPS, uintptr(len(h.groups)), uintptr(unsafe.Pointer(&list[0])), 0); e != 0 {
			return fmt.Errorf("setgroups: %w", e)
		}
	}
	if unix.Getegid() != h.gid {
		if _, _, e := unix.RawSyscall(unix.SYS_SETRESGID, keep, uintptr(h.gid), keep); e != 0 {
			return fmt.Errorf("setresgid: %w", e)
		}
	}
	if unix.Geteuid() != h.uid {
		if _, _, e := unix.RawSyscall(unix.SYS_SETRESUID, keep, uintptr(h.uid), keep); e != 0 {
			return fmt.Errorf("setresuid: %w", e)
		}
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&header, &data[0]); err != nil {
		return err
	}
	for i := range data {
		word := uint32(h.caps >> (32 * i))
		data[i].Permitted &= word
		data[i].Effective = data[i].Permitted // (the change of user above has emptied it)
		data[i].Inheritable = 0
	}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("capset: %w", err)
	}
	return nil
}

func sorted(ids []int) []int {
	c := slices.Clone(ids)
	slices.Sort(c)
	return c
}

// enterFiles takes this thread into the mount namespace, root and working directory of the
// process at proc, as far as they are not its own, with the umask. They belong to the thread once
// it has its own (unshare), and the rest of the process keeps its.
func enterFiles(proc string, umask int) error {
	mount, err := unix.Open(proc+"ns/mnt", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(mount)
	root, err := unix.Open(proc+"root", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	cwd, err := unix.Open(proc+"cwd", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(cwd)
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unshare the thread's files: %w", err)
	}
	theirs, err := fileID(mount, "")
	if err != nil {
		return err
	}
	ours, err := fileID(unix.AT_FDCWD, "/proc/thread-self/ns/mnt")
	if err != nil {
		return err
	}
	if theirs != ours {
		if err := unix.Setns(mount, unix.CLONE_NEWNS); err != nil {
			return fmt.Errorf("enter the mount namespace: %w", err)
		}
	}
	theirs, err = fileID(root, "")
	if err != nil {
		return err
	}
	if ours, err = fileID(unix.AT_FDCWD, "/"); err != nil {
		return err
	}
	if theirs != ours {
		if err := unix.Fchdir(root); err != nil {
			return err
		}
		if err := unix.Chroot("."); err != nil {
			return fmt.Errorf("enter the root: %w", err)
		}
	}
	if err := unix.Fchdir(cwd); err != nil {
		return err
	}
	unix.Umask(umask)
	return nil
}

// fileID is the device and inode of what fd and path name (fd alone with an empty path).
func fileID(fd int, path string) ([2]uint64, error) {
	var st unix.Stat_t
	flags := 0
	if path == "" {
		flags = unix.AT_EMPTY_PATH
	}
	if err := unix.Fstatat(fd, path, &st, flags); err != nil {
		return [2]uint64{}, err
	}
	return [2]uint64{uint64(st.Dev), st.Ino}, nil
}

// copyAddress gives the held thread an address as the kernel does, truncated to its buffer, with
// the full length in its length.
func copyAddress(tid int, addrPtr, lenPtr uint64, sa []byte) unix.Errno {
	var lb [4]byte
	if err := readRemote(tid, lenPtr, lb[:]); err != nil {
		return unix.EFAULT
	}
	have := int(int32(binary.NativeEndian.Uint32(lb[:])))
	if have < 0 {
		return unix.EINVAL
	}
	if have > 0 && len(sa) > 0 {
		if err := writeRemote(tid, addrPtr, sa[:min(have, len(sa))]); err != nil {
			return unix.EFAULT
		}
	}
	binary.NativeEndian.PutUint32(lb[:], uint32(len(sa)))
	if err := writeRemote(tid, lenPtr, lb[:]); err != nil {
		return unix.EFAULT
	}
	return 0
}

func (s *Supervisor) performName(n *notif, dup int) reply {
	var sa [sockaddrMax]byte
	length := uint32(len(sa))
	_, _, e := unix.Syscall(uintptr(n.Nr), uintptr(dup), uintptr(unsafe.Pointer(&sa[0])), uintptr(unsafe.Pointer(&length)))
	if e != 0 {
		return failWith(e)
	}
	if n.Args[1] == 0 || n.Args[2] == 0 {
		return failWith(unix.EFAULT)
	}
	if e := copyAddress(int(n.Pid), n.Args[1], n.Args[2], sa[:length]); e != 0 {
		return failWith(e)
	}
	return succeed()
}

// notListening is what the kernel says of accept on a socket that does not listen, 0 if it does.
func notListening(dup int) unix.Errno {
	listening, err := unix.GetsockoptInt(dup, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	if err != nil {
		return errnoOf(err)
	}
	if listening != 0 {
		return 0
	}
	if kind, err := unix.GetsockoptInt(dup, unix.SOL_SOCKET, unix.SO_TYPE); err == nil && (kind == unix.SOCK_STREAM || kind == unix.SOCK_SEQPACKET) {
		return unix.EINVAL
	}
	return unix.EOPNOTSUPP
}

// copyPeer gives the held thread the address of the other end of an accepted connection (none for
// a Unix client without a name, or one that has left).
func copyPeer(tid int, addrPtr, lenPtr uint64, conn int) unix.Errno {
	var sa [sockaddrMax]byte
	length := uint32(len(sa))
	if _, _, e := unix.Syscall(unix.SYS_GETPEERNAME, uintptr(conn), uintptr(unsafe.Pointer(&sa[0])), uintptr(unsafe.Pointer(&length))); e != 0 {
		length = 0
	}
	return copyAddress(tid, addrPtr, lenPtr, sa[:length])
}

// The ioctls of the network that are only questions, all with a struct ifreq. Any other is refused:
// it could not be told apart from one that passes a pointer, and what changes something would
// be done with the supervisor's capabilities and not the container's.
var ifreqQuestions = map[uint32]bool{
	unix.SIOCGIFNAME: true, unix.SIOCGIFFLAGS: true, unix.SIOCGIFADDR: true, unix.SIOCGIFDSTADDR: true,
	unix.SIOCGIFBRDADDR: true, unix.SIOCGIFNETMASK: true, unix.SIOCGIFMETRIC: true, unix.SIOCGIFMTU: true,
	unix.SIOCGIFHWADDR: true, unix.SIOCGIFINDEX: true, unix.SIOCGIFTXQLEN: true,
}

func (s *Supervisor) performIoctl(n *notif, dup int) reply {
	cmd, tid := uint32(n.Args[1]), int(n.Pid)
	if cmd == unix.SIOCGIFCONF {
		return s.performIfconf(tid, n.Args[2], dup)
	}
	if !ifreqQuestions[cmd] {
		return failWith(unix.EOPNOTSUPP)
	}
	var ifr [40]byte // struct ifreq
	if err := readRemote(tid, n.Args[2], ifr[:]); err != nil {
		return failWith(unix.EFAULT)
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(dup), uintptr(cmd), uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
		return failWith(e)
	}
	if err := writeRemote(tid, n.Args[2], ifr[:]); err != nil {
		return failWith(unix.EFAULT)
	}
	return succeed()
}

// performIfconf is SIOCGIFCONF, whose struct ifconf has a length and a pointer to the list.
func (s *Supervisor) performIfconf(tid int, arg uint64, dup int) reply {
	var ifc [16]byte
	if err := readRemote(tid, arg, ifc[:]); err != nil {
		return failWith(unix.EFAULT)
	}
	length := int(int32(binary.NativeEndian.Uint32(ifc[:])))
	remote := binary.NativeEndian.Uint64(ifc[8:])
	if length < 0 || length > 1<<20 {
		return failWith(unix.EINVAL)
	}
	list := make([]byte, length+1) // (never empty, for its address)
	var local [16]byte
	binary.NativeEndian.PutUint32(local[:], uint32(length))
	if remote != 0 {
		binary.NativeEndian.PutUint64(local[8:], uint64(uintptr(unsafe.Pointer(&list[0]))))
	}
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(dup), unix.SIOCGIFCONF, uintptr(unsafe.Pointer(&local[0])))
	runtime.KeepAlive(list)
	if e != 0 {
		return failWith(e)
	}
	out := int(int32(binary.NativeEndian.Uint32(local[:])))
	if remote != 0 && out > 0 {
		if err := writeRemote(tid, remote, list[:min(out, length)]); err != nil {
			return failWith(unix.EFAULT)
		}
	}
	binary.NativeEndian.PutUint32(ifc[:], uint32(out))
	if err := writeRemote(tid, arg, ifc[:4]); err != nil {
		return failWith(unix.EFAULT)
	}
	return succeed()
}
