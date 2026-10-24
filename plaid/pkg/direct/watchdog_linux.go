//go:build linux && (amd64 || arm64)

package direct

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// A container whose supervisor is gone is a container whose connect, bind, accept and
// address calls all fail with ENOSYS (the filter has no listener) and which goes on running:
// the supervisor's death is a death of the container's network, and the one who started the
// container (Skifflet) sees only that its launcher has exited, and starts another, while
// the old container is still there. A signal on the launcher's death (Pdeathsig) does not end
// it: Apptainer's launcher dies and the container's main process, which is in a pid namespace
// of its own, does not. So each supervisor has a watchdog, a process of its own (the program
// itself, started with WatchdogEnv set), which holds the read end of a pipe whose write end
// only the supervisor has. When the pipe closes without the supervisor having said that it is
// done, the supervisor is dead, and the watchdog ends the container as Skifflet ends one: it
// kills the container's main process, which makes the launcher exit by itself and remove
// what it made, and only then, if the launcher is still there, the process group.

// WatchdogEnv is the environment variable that makes a process started by Start a watchdog;
// a program that calls Start must call RunWatchdogIfRequested first thing in main.
const WatchdogEnv = "PLAID_DIRECT_WATCHDOG"

// watchdogGrace is how long the launcher gets to exit after its container's main process was
// killed.
const watchdogGrace = 3 * time.Second

// RunWatchdogIfRequested runs the watchdog, and does not return, if this process was started
// as one.
func RunWatchdogIfRequested() {
	if os.Getenv(WatchdogEnv) != "" {
		os.Exit(runWatchdog(os.Stdin, watchdogGrace))
	}
}

// watchdog is the supervisor's end of a watchdog.
type watchdog struct {
	cmd  *exec.Cmd
	pipe *os.File
}

// startWatchdog starts the watchdog. The supervisor then says which process it started
// (launched) and when it is done (finished).
func startWatchdog() (*watchdog, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe() // close-on-exec: the container has no way to the write end
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), WatchdogEnv+"=1")
	cmd.Stdin = r
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	r.Close()
	return &watchdog{cmd: cmd, pipe: w}, nil
}

// launched tells the watchdog which process is the container's launcher.
func (d *watchdog) launched(pid int) {
	if d != nil {
		fmt.Fprintf(d.pipe, "%d\n", pid)
	}
}

// finished tells the watchdog that the supervisor is done with its container, which is no
// reason to end anything, and sees it off.
func (d *watchdog) finished() {
	if d == nil {
		return
	}
	fmt.Fprintln(d.pipe, "done")
	d.pipe.Close()
	go d.cmd.Wait()
}

// runWatchdog reads what the supervisor says and ends the container if the supervisor ends
// without saying that it is done.
func runWatchdog(in io.Reader, grace time.Duration) int {
	// The container's process group is signalled to end it, and the watchdog is in it: it
	// ends with its supervisor's pipe, not with the signals that are meant for the container.
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	lines := bufio.NewScanner(in)
	if !lines.Scan() {
		return 0 // the supervisor ended before it had a container
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines.Text()))
	if err != nil || pid <= 1 {
		return 2
	}
	launcher, err := unix.PidfdOpen(pid, 0) // the process, not the number
	if err != nil {
		return 0
	}
	defer unix.Close(launcher)
	pgid, _ := unix.Getpgid(pid)
	if pgid != os.Getppid() {
		pgid = 0 // the supervisor did not start a process group of its own: the group is not the container's to end
	}
	for lines.Scan() {
		if strings.TrimSpace(lines.Text()) == "done" {
			return 0
		}
	}
	endContainer(launcher, pid, pgid, grace)
	return 0
}

// endContainer kills the container of a launcher: its main process first, then, if the
// launcher has not gone in grace, the launcher and its process group.
func endContainer(launcher, pid, pgid int, grace time.Duration) {
	if unix.PidfdSendSignal(launcher, 0, nil, 0) != nil {
		return // gone, and with it whatever was its
	}
	if payload := payloadPID(pid); payload > 1 {
		_ = unix.Kill(payload, syscall.SIGKILL)
		fds := []unix.PollFd{{Fd: int32(launcher), Events: unix.POLLIN}} // readable when the process has ended
		if n, _ := unix.Poll(fds, int(grace.Milliseconds())); n > 0 {
			return
		}
	}
	_ = unix.PidfdSendSignal(launcher, syscall.SIGKILL, nil, 0)
	if pgid > 1 {
		_ = unix.Kill(-pgid, syscall.SIGKILL)
	}
}

// payloadPID is the process that is pid 1 of the innermost pid namespace below the launcher
// (the containers run with --pid --no-init, so the container's main process is it), as the
// pid it has here, or 0 if there is none.
func payloadPID(launcher int) int {
	children := map[int][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if ppid := parentOf(pid); ppid > 0 {
			children[ppid] = append(children[ppid], pid)
		}
	}
	for queue := []int{launcher}; len(queue) > 0; queue = queue[1:] {
		for _, child := range children[queue[0]] {
			if ns := nspid(child); len(ns) >= 2 && ns[len(ns)-1] == 1 {
				return child
			}
			queue = append(queue, child)
		}
	}
	return 0
}

// parentOf is the parent of a process, from /proc/<pid>/stat (whose second field, the name,
// may hold spaces and brackets: the fields that follow the last bracket are after it).
func parentOf(pid int) int {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 {
		return 0
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

// nspid is the process's pid in each pid namespace it is in, outermost first.
func nspid(pid int) []int {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "NSpid:"); ok {
			var pids []int
			for _, f := range strings.Fields(rest) {
				n, err := strconv.Atoi(f)
				if err != nil {
					return nil
				}
				pids = append(pids, n)
			}
			return pids
		}
	}
	return nil
}
