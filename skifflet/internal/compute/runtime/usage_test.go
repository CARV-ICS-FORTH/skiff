package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeProc writes the files of a process in a fake proc filesystem. The command name is
// given as is (it may contain spaces and parentheses).
func writeProc(t *testing.T, dir string, pid, ppid int, comm string, utime, stime, cutime, cstime uint64, pssKB int) {
	t.Helper()
	p := filepath.Join(dir, fmt.Sprint(pid))
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := fmt.Sprintf("%d (%s) S %d 1 1 0 -1 4194560 100 0 0 0 %d %d %d %d 20 0 1 0 12345 1000000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0\n", pid, comm, ppid, utime, stime, cutime, cstime)
	if err := os.WriteFile(filepath.Join(p, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if pssKB >= 0 {
		rollup := fmt.Sprintf("00400000-7fff00000000 ---p 00000000 00:00 0 [rollup]\nRss:     %d kB\nPss:     %d kB\nPss_Anon: 1 kB\n", pssKB*2, pssKB)
		if err := os.WriteFile(filepath.Join(p, "smaps_rollup"), []byte(rollup), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A container's usage is the sum over the tree below its launcher: processes outside it
// are not counted, CPU time includes what finished children handed up, memory is the
// proportional share, and a command name with spaces and parentheses does not confuse the parser.
func TestProcessTreeUsage(t *testing.T) {
	proc := t.TempDir()
	writeProc(t, proc, 100, 1, "apptainer", 200, 100, 0, 0, 1000)   // the launcher: 3.0 s
	writeProc(t, proc, 101, 100, "sh -c (x)", 50, 50, 100, 0, 2000) // a child that waited for one: 2.0 s
	writeProc(t, proc, 102, 101, "worker", 300, 0, 0, 0, 4096)      // its child: 3.0 s
	writeProc(t, proc, 103, 1, "unrelated", 99999, 0, 0, 0, 99999)  // not in the tree
	writeProc(t, proc, 104, 102, "no-rollup", 0, 0, 0, 0, -1)       // no smaps_rollup: statm
	if err := os.WriteFile(filepath.Join(proc, "104", "statm"), []byte("100 25 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	usage, err := ProcessTreeUsage(proc, 100)
	if err != nil {
		t.Fatal(err)
	}
	if usage.CPUSeconds != 8.0 {
		t.Errorf("CPU %.2f s, want 8.00 (3 + 2 + 3 + 0, without the unrelated process)", usage.CPUSeconds)
	}
	want := uint64(1000+2000+4096)*1024 + 25*uint64(os.Getpagesize())
	if usage.WorkingSetBytes != want || usage.Processes != 4 {
		t.Errorf("memory %d over %d processes, want %d over 4", usage.WorkingSetBytes, usage.Processes, want)
	}
	// A process that ends is not an error; a root that is gone is.
	if err := os.RemoveAll(filepath.Join(proc, "102")); err != nil {
		t.Fatal(err)
	}
	if _, err := ProcessTreeUsage(proc, 100); err != nil {
		t.Errorf("a vanished descendant: %v", err)
	}
	if _, err := ProcessTreeUsage(proc, 4242); err == nil {
		t.Error("a root that is not there was reported as usage, not as an error")
	}
}

// The CPU time of a tree does not go down when a process ends and its parent waits for it.
func TestProcessTreeUsage_CPUDoesNotGoDownWhenAChildIsReaped(t *testing.T) {
	proc := t.TempDir()
	writeProc(t, proc, 100, 1, "launcher", 100, 0, 0, 0, 10)
	writeProc(t, proc, 101, 100, "child", 400, 100, 0, 0, 10)
	before, _ := ProcessTreeUsage(proc, 100)
	// The child ends and the launcher waits for it: its time moves to the launcher's cutime.
	if err := os.RemoveAll(filepath.Join(proc, "101")); err != nil {
		t.Fatal(err)
	}
	writeProc(t, proc, 100, 1, "launcher", 100, 0, 500, 0, 10)
	after, _ := ProcessTreeUsage(proc, 100)
	if after.CPUSeconds < before.CPUSeconds {
		t.Errorf("CPU went from %.2f s to %.2f s when a child was reaped", before.CPUSeconds, after.CPUSeconds)
	}
}

func TestNodeUsage(t *testing.T) {
	proc := t.TempDir()
	// user nice system idle iowait irq softirq steal guest guest_nice
	stat := "cpu  1000 100 400 90000 500 50 50 100 0 0\ncpu0 500 50 200 45000 250 25 25 50 0 0\nintr 1\n"
	meminfo := "MemTotal:       16000000 kB\nMemFree:         1000000 kB\nMemAvailable:    6000000 kB\n"
	if err := os.WriteFile(filepath.Join(proc, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "meminfo"), []byte(meminfo), 0o644); err != nil {
		t.Fatal(err)
	}
	usage, err := NodeUsage(proc)
	if err != nil {
		t.Fatal(err)
	}
	if usage.CPUSeconds != 17.0 { // (1000 + 100 + 400 + 50 + 50 + 100) / 100: no idle, no iowait
		t.Errorf("CPU %.2f s, want 17.00", usage.CPUSeconds)
	}
	if usage.WorkingSetBytes != uint64(10000000)*1024 {
		t.Errorf("memory %d, want %d", usage.WorkingSetBytes, uint64(10000000)*1024)
	}
	if _, err := NodeUsage(t.TempDir()); err == nil {
		t.Error("a proc without stat was reported as usage, not as an error")
	}
}
