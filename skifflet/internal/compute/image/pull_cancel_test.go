package image

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPullCancellationStopsChildHoldingOutputPipe(t *testing.T) {
	root := t.TempDir()
	script, marker := filepath.Join(root, "pull.sh"), filepath.Join(root, "pids")
	if err := os.WriteFile(script, []byte("sleep 30 &\necho \"$$ $!\" > \"$1\"\nwait $!\n"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	completed := false
	go func() { _, err := executePullWithProgress(ctx, "cancelled", "/bin/sh", script, marker); done <- err }()
	t.Cleanup(func() {
		if completed {
			return
		}
		data, _ := os.ReadFile(marker)
		fields := strings.Fields(string(data))
		if len(fields) > 1 {
			fields = fields[1:]
		}
		for _, field := range fields {
			pid, _ := strconv.Atoi(field)
			if pid > 1 {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pull child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled pull reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pull child survived cancellation and retained its output pipe")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		t.Fatalf("missing child identity: %q", data)
	}
	deadline = time.Now().Add(time.Second)
	for {
		status, err := exec.Command("ps", "-o", "stat=", "-p", fields[1]).Output()
		state := strings.TrimSpace(string(status))
		if (err != nil && state == "") || strings.HasPrefix(state, "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled pull retained a live child PID %s (%s)", fields[1], state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	completed = true
}
