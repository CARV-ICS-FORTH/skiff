package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The descriptors Skifflet gives `plaidtainer enter`: the program itself, a pipe with the
// container's environment, and the container's mount namespace, root and working directory.
const (
	enterProgramFD = 3
	enterEnvFD     = 4
	enterMountFD   = 5
	enterRootFD    = 6
	enterCwdFD     = 7
)

// enterIfRequested is `plaidtainer enter MASK COMMAND [ARG...]`, the last step of entering a
// container (kubectl exec, probes, hooks). Skifflet's nsenter runs outside the container with all
// the capabilities of the bubble, which it needs to enter, so nothing of the container may reach
// it: it has no environment, and this program gives the command the container's, read from
// a descriptor, not from arguments, which any user of the node can read. What the command runs
// with is limited to the capabilities of MASK, in hexadecimal, or left as it is with "-" (a
// container in a user namespace of its own, or a privileged one).
//
// nsenter enters the other namespaces and starts this program while its files are still the
// bubble's, so that it looks up no path inside the container. This program is what goes in, from
// descriptors that Skifflet opened, and then gives up what it may not keep. It needs nothing of
// the image. Nothing runs if any of this cannot be done.
//
// `plaidtainer entered MASK COMMAND [ARG...]` is the same for a container in a user namespace of
// its own, where nsenter has entered the rest: joining that namespace takes the only thread of a
// program, which this one does not have, and what nsenter runs there runs as the container's user.
func enterIfRequested() {
	if len(os.Args) < 4 || (os.Args[1] != "enter" && os.Args[1] != "entered") {
		return
	}
	env, err := readEnterEnv()
	runtime.LockOSThread() // what follows belongs to a thread, and the exec is from this one
	if err == nil && os.Args[1] == "enter" {
		err = enterContainer()
	}
	if err == nil && os.Args[2] != "-" {
		var mask uint64
		if mask, err = strconv.ParseUint(os.Args[2], 16, 64); err == nil {
			err = keepCaps(mask)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "enter %s: %v\n", os.Args[2], err)
		os.Exit(126)
	}
	os.Exit(execLikeNsenter(os.Args[3:], env))
}

// readEnterEnv reads the NUL-separated environment, to its end, and closes the descriptor.
func readEnterEnv() ([]string, error) {
	f := os.NewFile(enterEnvFD, "environment")
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read the environment: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	return strings.Split(string(data), "\x00"), nil
}

// enterContainer takes this thread into the container's mount namespace, root and working
// directory. They belong to the thread once it has its own. The container's command does not get
// the descriptors.
func enterContainer() error {
	if err := unix.Unshare(unix.CLONE_FS); err != nil { // a mount namespace is for a thread that shares none of this
		return fmt.Errorf("unshare the thread's files: %w", err)
	}
	if err := unix.Setns(enterMountFD, unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("enter the mount namespace: %w", err)
	}
	if err := unix.Fchdir(enterRootFD); err != nil {
		return fmt.Errorf("go to the root: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("enter the root: %w", err)
	}
	if err := unix.Fchdir(enterCwdFD); err != nil {
		return fmt.Errorf("go to the working directory: %w", err)
	}
	for _, fd := range []int{enterMountFD, enterRootFD, enterCwdFD} {
		unix.CloseOnExec(fd)
	}
	return nil
}

// keepCaps limits this thread to the capabilities of mask, for good. The bounding set is what
// counts: an exec as root is given everything the set has.
func keepCaps(mask uint64) error {
	for c := uintptr(0); c < 64; c++ {
		if mask&(1<<c) != 0 {
			continue
		}
		err := unix.Prctl(unix.PR_CAPBSET_DROP, c, 0, 0, 0)
		if errors.Is(err, unix.EINVAL) {
			break // past the last capability of this kernel
		}
		if err != nil {
			return fmt.Errorf("drop capability %d: %w", c, err)
		}
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("clear the ambient capabilities: %w", err)
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&header, &data[0]); err != nil {
		return fmt.Errorf("read the capabilities: %w", err)
	}
	for i := range data {
		word := uint32(mask >> (32 * i))
		data[i].Effective &= word
		data[i].Permitted &= word
		data[i].Inheritable &= word
	}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("set the capabilities: %w", err)
	}
	return nil
}

// execLikeNsenter runs argv with env as nsenter would have: found in env's PATH, /bin and
// /usr/bin when there is none, and failing with the exit codes of a shell. It returns only on
// failure.
func execLikeNsenter(argv, env []string) int {
	searchPath := "/bin:/usr/bin"
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			searchPath = value
		}
	}
	os.Setenv("PATH", searchPath)
	path, err := exec.LookPath(argv[0])
	if errors.Is(err, exec.ErrDot) {
		err = nil // a relative entry of PATH: not for this program to refuse
	}
	if err == nil {
		unix.CloseOnExec(enterProgramFD)
		err = unix.Exec(path, argv, env)
		if errors.Is(err, unix.ENOEXEC) { // a script with no #!: the C library's execvp, which nsenter has, gives it to a shell
			err = unix.Exec("/bin/sh", append([]string{"/bin/sh", path}, argv[1:]...), env)
		}
	}
	fmt.Fprintf(os.Stderr, "failed to execute %s: %v\n", argv[0], err)
	if errors.Is(err, fs.ErrPermission) {
		return 126
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
		return 127
	}
	return 126
}
