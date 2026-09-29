package podhandler

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// procRoot is where the proc filesystem is mounted; tests point it at a fake tree.
var procRoot = "/proc"

// findPayload is replaced by tests.
var findPayload = payloadPID

// payloadPID returns the PID (as seen from here) of the process that is PID 1 of
// the pid namespace the Apptainer launcher created, or 0 if there is none. The
// containers are started with --pid --no-init, so the payload itself is PID 1 of
// its namespace: it is the descendant of the launcher whose innermost namespace
// PID is 1.
func payloadPID(launcher int) int {
	children := processChildren()
	queue := []int{launcher}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			if ns := readNSpid(child); len(ns) >= 2 && ns[len(ns)-1] == 1 {
				return child
			}
			queue = append(queue, child)
		}
	}
	return 0
}

// processChildren maps each PID to its children, from one scan of the proc filesystem.
func processChildren() map[int][]int {
	children := map[int][]int{}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return children
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if ppid := parentPID(pid); ppid > 0 {
			children[ppid] = append(children[ppid], pid)
		}
	}
	return children
}

// killProcessTree sends SIGKILL to a process and everything below it. The tree is
// collected first, so killing the parent does not hide the children. (A process
// that detached itself and was adopted elsewhere is not below it any more.)
func killProcessTree(root int) {
	children := processChildren()
	all := []int{root}
	for i := 0; i < len(all); i++ {
		all = append(all, children[all[i]]...)
	}
	for _, pid := range all {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func readNSpid(pid int) []int {
	data, err := os.ReadFile(procRoot + "/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return nil
	}
	return parseNSpid(string(data))
}

func parentPID(pid int) int {
	data, err := os.ReadFile(procRoot + "/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	idx := strings.LastIndex(string(data), ")")
	if idx < 0 || idx+2 >= len(data) {
		return 0
	}
	fields := strings.Fields(string(data[idx+2:]))
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

// parseNSpid extracts the "NSpid:" line of /proc/<pid>/status: the process's PID
// in each pid namespace it belongs to, outermost first.
func parseNSpid(status string) []int {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		var pids []int
		for _, field := range strings.Fields(strings.TrimPrefix(line, "NSpid:")) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				return nil
			}
			pids = append(pids, pid)
		}
		return pids
	}
	return nil
}
