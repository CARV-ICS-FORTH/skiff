//go:build linux && (amd64 || arm64)

package direct

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// MinKernel is the oldest Linux that has everything the supervisor uses; the interface that
// sets it is SECCOMP_IOCTL_NOTIF_ADDFD, below. The others it uses are older: the seccomp
// listener (5.0) and SECCOMP_USER_NOTIF_FLAG_CONTINUE (5.5), SO_COOKIE (4.12), the
// cookie of NETLINK_SOCK_DIAG's inet_diag_msg, process_vm_readv and writev (3.2) and
// EPOLLEXCLUSIVE (4.5); close_range (5.9) is used where it is, with a fallback (see
// closeListed).
const MinKernel = "5.9"

// kernelFeature is a kernel interface that the supervisor cannot do without: probe says
// whether the running kernel has it, by using it.
type kernelFeature struct {
	name, since string
	probe       func() error
}

var kernelFeatures = []kernelFeature{
	{"pidfd_open", "5.3", probePidfdOpen},
	{"pidfd_getfd", "5.6", probePidfdGetfd},
	{"SECCOMP_IOCTL_NOTIF_ADDFD with SECCOMP_ADDFD_FLAG_SETFD", MinKernel, probeNotifAddfd},
}

// CheckKernel says whether the kernel has what the supervisor needs, by trying each of
// the interfaces, which is more reliable than the version number (distributions backport),
// and names those it does not have. A kernel that has none of them is not told apart from
// one where a sandbox refuses the call: either way the supervisor could not work.
func CheckKernel() error { return checkKernelFeatures(kernelFeatures) }

// checkKernelFeatures is CheckKernel for the given features.
func checkKernelFeatures(features []kernelFeature) error {
	var lacks []string
	for _, f := range features {
		if err := f.probe(); err != nil {
			lacks = append(lacks, fmt.Sprintf("%s (Linux %s: %v)", f.name, f.since, err))
		}
	}
	if len(lacks) == 0 {
		return nil
	}
	return fmt.Errorf("the kernel lacks %s: the direct path needs Linux %s or newer", strings.Join(lacks, ", "), MinKernel)
}

// probePidfdOpen opens a pidfd of this process; the calls on the container's processes
// are the same.
func probePidfdOpen() error {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return err
	}
	unix.Close(fd)
	return nil
}

// probePidfdGetfd takes a duplicate of a descriptor of this process (its pidfd) through
// its pidfd: ENOSYS where there is no pidfd_getfd. Taking from oneself is always allowed,
// whatever Yama says.
func probePidfdGetfd() error {
	pidfd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return nil // the kernel without pidfd_open is told by its own probe
	}
	defer unix.Close(pidfd)
	fd, err := unix.PidfdGetfd(pidfd, pidfd, 0)
	if err != nil {
		return err
	}
	unix.Close(fd)
	return nil
}

// probeNotifAddfd asks a seccomp listener, of a filter that holds nothing, to add a
// descriptor to a held call that is not there: a kernel that has the ioctl and the flag
// answers ENOENT (or EBADF), one that has not answers EINVAL (or ENOTTY). Nothing is
// held or added. The filter is installed on a thread of its own, which
// ends with it, as Start does.
func probeNotifAddfd() error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // and never unlock: the thread ends with the filter on it
		listener, err := installFilter(nil)
		if err != nil {
			done <- err
			return
		}
		defer unix.Close(listener)
		_, err = ioctlAddfd(listener, &notifAddfd{Flags: unix.SECCOMP_ADDFD_FLAG_SETFD, Srcfd: uint32(listener)})
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) {
			done <- err
			return
		}
		done <- nil
	}()
	return <-done
}
