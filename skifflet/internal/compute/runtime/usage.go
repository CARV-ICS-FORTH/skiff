package runtime

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// What a container or the node has used, read from the proc filesystem. Skiff has no
// cgroup of its own per container (a container is a process tree of the job, which Slurm
// accounts as a whole), so usage is what the tree of processes of the container shows:
// an approximation, said so wherever it is shown. CPU time is exact for the processes
// there are, and memory is their proportional share of the pages they hold (a page two
// processes share counts half for each), so a container is not charged for what its
// workers share, but file pages it holds mapped count like anonymous ones.

// ClockTicksPerSecond is USER_HZ, the unit of the CPU times in /proc: fixed at 100 by the
// kernel's interface to user space on every architecture.
const ClockTicksPerSecond = 100

// Usage is what a set of processes used.
type Usage struct {
	// CPUSeconds is cumulative: the user and system time of the processes and of the
	// children they have waited for, which does not go down when a process ends (its time
	// moves to its parent's, which is in the set).
	CPUSeconds float64
	// WorkingSetBytes is the memory the processes hold now (see above).
	WorkingSetBytes uint64
	// Processes is how many processes there are.
	Processes int
}

type procStat struct {
	ppid  int
	ticks uint64 // utime + stime + cutime + cstime
}

// readProcStat parses /proc/PID/stat: the fields after the command name, which may contain
// spaces and parentheses, so it is cut at the last closing one.
func readProcStat(procDir string, pid int) (procStat, error) {
	data, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	idx := bytes.LastIndexByte(data, ')')
	if idx == -1 || idx+1 >= len(data) {
		return procStat{}, fmt.Errorf("invalid stat format for pid %d", pid)
	}
	// fields[0] is the state (field 3 of the file): ppid is fields[1], utime fields[11],
	// stime fields[12], cutime fields[13] and cstime fields[14].
	fields := strings.Fields(string(data[idx+1:]))
	if len(fields) < 15 {
		return procStat{}, fmt.Errorf("stat of pid %d has too few fields", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return procStat{}, err
	}
	var ticks uint64
	for _, f := range fields[11:15] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return procStat{}, err
		}
		ticks += v
	}
	return procStat{ppid: ppid, ticks: ticks}, nil
}

// processMemory is the proportional set size of a process (from smaps_rollup), or its
// resident set when the kernel offers no rollup; zero when it has gone.
func processMemory(procDir string, pid int) uint64 {
	if data, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "smaps_rollup")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if rest, ok := strings.CutPrefix(line, "Pss:"); ok {
				fields := strings.Fields(rest)
				if len(fields) >= 1 {
					if kb, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
						return kb * 1024
					}
				}
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "statm"))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}

// ProcessTreeUsage is what root and every process descended from it used. A process that
// ends while it is read is left out of that reading, not an error; an error is only a root
// that is not there.
func ProcessTreeUsage(procDir string, root int) (Usage, error) {
	if _, err := readProcStat(procDir, root); err != nil {
		return Usage{}, err
	}
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return Usage{}, err
	}
	stats := map[int]procStat{}
	children := map[int][]int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		st, err := readProcStat(procDir, pid)
		if err != nil {
			continue
		}
		stats[pid] = st
		children[st.ppid] = append(children[st.ppid], pid)
	}
	var usage Usage
	var ticks uint64
	seen := map[int]bool{}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		st, ok := stats[pid]
		if !ok {
			continue
		}
		ticks += st.ticks
		usage.WorkingSetBytes += processMemory(procDir, pid)
		usage.Processes++
		queue = append(queue, children[pid]...)
	}
	usage.CPUSeconds = float64(ticks) / ClockTicksPerSecond
	return usage, nil
}

// NodeUsage is what the node as a whole used: the busy CPU time of all processors, and
// the memory in use (total less what is available).
func NodeUsage(procDir string) (Usage, error) {
	data, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return Usage{}, err
	}
	var usage Usage
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[0] != "cpu" {
			continue
		}
		// user nice system idle iowait irq softirq steal: everything but idle and iowait.
		var busy uint64
		for _, i := range []int{1, 2, 3, 6, 7, 8} {
			if i >= len(fields) {
				continue
			}
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return Usage{}, err
			}
			busy += v
		}
		usage.CPUSeconds = float64(busy) / ClockTicksPerSecond
		found = true
		break
	}
	if !found {
		return Usage{}, fmt.Errorf("no cpu line in %s/stat", procDir)
	}
	meminfo, err := os.ReadFile(filepath.Join(procDir, "meminfo"))
	if err != nil {
		return Usage{}, err
	}
	var total, available uint64
	var haveTotal, haveAvailable bool
	for _, line := range strings.Split(string(meminfo), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "MemTotal":
			total, haveTotal = kb*1024, true
		case "MemAvailable":
			available, haveAvailable = kb*1024, true
		}
	}
	if !haveTotal || !haveAvailable || available > total {
		return Usage{}, fmt.Errorf("no usable MemTotal and MemAvailable in %s/meminfo", procDir)
	}
	usage.WorkingSetBytes = total - available
	return usage, nil
}
