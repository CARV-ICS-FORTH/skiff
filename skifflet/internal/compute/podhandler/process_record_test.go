package podhandler

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/runtime"
)

func TestSavePIDsConcurrentStarts(t *testing.T) {
	podDir := endpoint.PodPath(filepath.Join(t.TempDir(), "pod"))
	if err := os.MkdirAll(podDir.JobDir(), 0755); err != nil {
		t.Fatal(err)
	}
	h := &PodHandler{podDirectory: podDir}
	const count = 24
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(pid int) {
			defer wg.Done()
			h.savePIDs(0, pid)
		}(10000 + i)
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(podDir.JobDir(), "pids.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record podPIDsRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.ContainerPIDs) != count {
		t.Fatalf("lost concurrent process records: got %d, want %d", len(record.ContainerPIDs), count)
	}
}

// The process record is the one crash-cleanup path: a process it names is reaped
// only while it is still the recorded process.
func TestReapRecordedProcesses(t *testing.T) {
	podDir := endpoint.PodPath(filepath.Join(t.TempDir(), "pod"))
	if err := os.MkdirAll(podDir.JobDir(), 0755); err != nil {
		t.Fatal(err)
	}
	start := func() *exec.Cmd {
		cmd := exec.Command("sleep", "30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	orphan, reused := start(), start()
	orphanStart, err := runtime.GetProcessStartTime(orphan.Process.Pid)
	if err != nil {
		t.Skipf("process start times are not available here: %v", err)
	}
	reusedStart, _ := runtime.GetProcessStartTime(reused.Process.Pid)

	h := &PodHandler{podDirectory: podDir}
	h.savePIDs(0, orphan.Process.Pid)
	h.savePIDs(0, reused.Process.Pid)
	// Make the second record name a different process than the one holding its PID now.
	h.pidsMu.Lock()
	h.pids.ContainerStartTimes[reused.Process.Pid] = reusedStart + 1
	h.pidsMu.Unlock()
	h.savePIDs(0, 0)

	ReapRecordedProcesses(podDir)

	dead := make(chan error, 1)
	go func() { dead <- orphan.Wait() }()
	select {
	case <-dead:
	case <-time.After(3 * time.Second):
		t.Fatalf("the recorded process %d (start %d) was not reaped", orphan.Process.Pid, orphanStart)
	}
	if runtime.IsProcessDead(reused.Process.Pid) {
		t.Fatal("a process whose start time no longer matches the record was killed")
	}
}
