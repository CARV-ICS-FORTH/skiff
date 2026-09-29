package podhandler

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseNSpid(t *testing.T) {
	status := "Name:\tsleep\nPid:\t8081\nNSpid:\t8081\t1\nNSsid:\t8081\t1\n"
	if got := parseNSpid(status); !reflect.DeepEqual(got, []int{8081, 1}) {
		t.Fatalf("NSpid = %v", got)
	}
	if got := parseNSpid("Name:\tx\nNSpid:\t42\n"); !reflect.DeepEqual(got, []int{42}) {
		t.Fatalf("a process in the initial namespace: %v", got)
	}
	if got := parseNSpid("Name:\tx\n"); got != nil {
		t.Fatalf("no NSpid line (non-Linux): %v", got)
	}
}

// startFakeLauncher starts what Apptainer looks like from outside: a launcher that
// runs the main process as its child, exits by itself when that child dies, and
// (like the payload as PID 1) does not react to SIGTERM.
func startFakeLauncher(t *testing.T, pl *PodLifecycle) (*Attempt, *exec.Cmd, int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "payload")
	a, err := pl.BeginAttempt("app", false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 60 & echo $! > "+pidFile+"; wait; exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := a.Start(cmd, false); err != nil {
		t.Fatal(err)
	}
	var payload int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(data)) != "" {
			payload, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if payload == 0 {
		t.Fatal("the fake launcher did not start its main process")
	}
	return a, cmd, payload
}

func withPayload(t *testing.T, pid int) {
	t.Helper()
	old := findPayload
	findPayload = func(int) int { return pid }
	t.Cleanup(func() { findPayload = old })
}

func signaledByKill(cmd *exec.Cmd) bool {
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

// A killed container's launcher must get the chance to exit on its own: that is
// when Apptainer removes the container's unpacked root filesystem.
func TestAttemptKill_StopsTheMainProcessBeforeTheLauncher(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	a, cmd, payload := startFakeLauncher(t, pl)
	withPayload(t, payload)
	waited := make(chan int, 1)
	go func() { waited <- a.Wait(cmd) }()

	a.kill()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the launcher did not exit after its main process was killed")
	}
	if signaledByKill(cmd) {
		t.Fatal("the launcher was killed instead of being left to exit by itself")
	}
}

func TestAttemptKill_FallsBackToTheProcessGroup(t *testing.T) {
	t.Run("no main process found", func(t *testing.T) {
		pl := testLifecycle(t, nil, "app")
		a, cmd, payload := startFakeLauncher(t, pl)
		defer syscall.Kill(payload, syscall.SIGKILL)
		withPayload(t, 0)
		waited := make(chan int, 1)
		go func() { waited <- a.Wait(cmd) }()
		a.kill()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			t.Fatal("the launcher survived the group kill")
		}
		if !signaledByKill(cmd) {
			t.Fatal("expected the launcher to be killed as a group")
		}
	})

	t.Run("launcher does not follow its main process", func(t *testing.T) {
		pl := testLifecycle(t, nil, "app")
		a, cmd, payload := startFakeLauncher(t, pl)
		defer syscall.Kill(payload, syscall.SIGKILL)
		decoy := exec.Command("sleep", "60")
		if err := decoy.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = decoy.Process.Kill(); _ = decoy.Wait() }()
		withPayload(t, decoy.Process.Pid) // killing it does not stop the launcher
		old := killGrace
		killGrace = 300 * time.Millisecond
		defer func() { killGrace = old }()
		waited := make(chan int, 1)
		go func() { waited <- a.Wait(cmd) }()
		start := time.Now()
		a.kill()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			t.Fatal("the launcher survived the escalation")
		}
		if !signaledByKill(cmd) || time.Since(start) < 250*time.Millisecond {
			t.Fatalf("expected escalation to the group after the grace period (killed=%v after %v)", signaledByKill(cmd), time.Since(start))
		}
	})
}

// Termination after the grace period uses the same path.
func TestReleaseLiveResources_ForceKillLetsTheLauncherCleanUp(t *testing.T) {
	pl := testLifecycle(t, nil, "app")
	pl.BeginLaunch()
	a, cmd, payload := startFakeLauncher(t, pl)
	withPayload(t, payload)
	waited := make(chan int, 1)
	go func() { waited <- a.Wait(cmd) }()
	pl.EndLaunch()
	pl.ReleaseLiveResources(200 * time.Millisecond) // TERM is ignored, so the grace period expires
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the container survived termination")
	}
	if signaledByKill(cmd) {
		t.Fatal("termination killed the launcher outright, which leaks the unpacked root filesystem")
	}
}
