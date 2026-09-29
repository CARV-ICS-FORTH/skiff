package podhandler

import (
	"os"
	"strconv"
	"strings"
)

// findPayload is replaced by tests.
var findPayload = payloadPID

// payloadPID returns the PID (as seen from here) of the process that is PID 1 of
// the pid namespace the Apptainer launcher created, or 0 if there is none. The
// containers are started with --pid --no-init, so the payload itself is PID 1 of
// its namespace: it is the descendant of the launcher whose innermost namespace
// PID is 1.
func payloadPID(launcher int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	children := map[int][]int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if ppid := parentPID(pid); ppid > 0 {
			children[ppid] = append(children[ppid], pid)
		}
	}
	queue := []int{launcher}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			if data, err := os.ReadFile("/proc/" + strconv.Itoa(child) + "/status"); err == nil {
				if ns := parseNSpid(string(data)); len(ns) >= 2 && ns[len(ns)-1] == 1 {
					return child
				}
			}
			queue = append(queue, child)
		}
	}
	return 0
}

func parentPID(pid int) int {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
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
