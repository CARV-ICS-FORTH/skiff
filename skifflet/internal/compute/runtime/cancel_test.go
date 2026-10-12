package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestHelperProcessIgnoreSIGTERM(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

func createHelperProcess(t *testing.T) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessIgnoreSIGTERM$")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start helper process: %v", err)
	}
	return cmd
}

func TestKillProcessByPID_InvalidInputs(t *testing.T) {
	invalidInputs := []string{
		"",
		"   ",
		"invalid_number",
		"0",
		"1",
		"-1",
		"-999",
		"pid://0",
		"pid://1",
		"pid://-1",
		"pid://1:12345",
	}

	for _, input := range invalidInputs {
		t.Run("input_"+input, func(t *testing.T) {
			_, err := KillPodProcessesWithTimeout(input, nil, 35*time.Second)
			if err == nil {
				t.Errorf("expected error for invalid PID %q, got nil", input)
			}
		})
	}
}

func TestKillProcessByPID_NonExistentPID(t *testing.T) {
	// A very high PID unlikely to exist
	_, err := KillPodProcessesWithTimeout("9999999", nil, 35*time.Second)
	if !errors.Is(err, ErrInvalidJob) {
		t.Errorf("expected ErrInvalidJob (ESRCH) for non-existent process, got %v", err)
	}
}

func TestKillProcessByPIDWithTimeout_GracefulExit(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sleep process: %v", err)
	}

	pidStr := strconv.Itoa(cmd.Process.Pid)
	_, err := KillPodProcessesWithTimeout(pidStr, nil, 2*time.Second)
	if err != nil {
		t.Fatalf("unexpected error killing process: %v", err)
	}

	_ = cmd.Wait()
}

func TestKillProcessByPIDWithTimeout_SIGKILLEscalation(t *testing.T) {
	cmd := createHelperProcess(t)
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})

	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	pidStr := strconv.Itoa(cmd.Process.Pid)
	// Use short timeout so test runs fast
	_, err := KillPodProcessesWithTimeout(pidStr, nil, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error killing process with escalation: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("process did not exit within timeout after SIGKILL")
	}
}

func TestKillPodProcessesWithTimeout_GroupEscalation(t *testing.T) {
	primary := createHelperProcess(t)
	secondary := createHelperProcess(t)
	time.Sleep(100 * time.Millisecond)

	doneP := make(chan struct{})
	go func() {
		_ = primary.Wait()
		close(doneP)
	}()

	doneS := make(chan struct{})
	go func() {
		_ = secondary.Wait()
		close(doneS)
	}()

	primaryPIDStr := strconv.Itoa(primary.Process.Pid)
	secondaryPIDStr := strconv.Itoa(secondary.Process.Pid)

	// Short timeout so test runs fast
	_, err := KillPodProcessesWithTimeout(primaryPIDStr, []string{secondaryPIDStr}, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error killing processes with escalation: %v", err)
	}

	select {
	case <-doneP:
	case <-time.After(3 * time.Second):
		t.Fatal("primary process did not exit within timeout after SIGKILL")
	}

	select {
	case <-doneS:
	case <-time.After(3 * time.Second):
		t.Fatal("secondary process did not exit within timeout after SIGKILL")
	}
}

func TestParseAndFormatProcessJobID(t *testing.T) {
	formatted := FormatProcessJobID(1234, 56789)
	if formatted != "pid://1234:56789" {
		t.Errorf("expected pid://1234:56789, got %s", formatted)
	}

	formattedNoTime := FormatProcessJobID(1234, 0)
	if formattedNoTime != "pid://1234" {
		t.Errorf("expected pid://1234, got %s", formattedNoTime)
	}

	pid, st, err := ParseProcessJobID("pid://12345:99999")
	if err != nil || pid != 12345 || st != 99999 {
		t.Errorf("expected pid 12345, st 99999, got pid %d, st %d, err %v", pid, st, err)
	}

	pid, st, err = ParseProcessJobID("12345")
	if err != nil || pid != 12345 || st != 0 {
		t.Errorf("expected pid 12345, st 0, got pid %d, st %d, err %v", pid, st, err)
	}

	if _, _, err := ParseProcessJobID("invalid"); err == nil {
		t.Errorf("expected ParseProcessJobID('invalid') to return an error")
	}
	if pid, st, err := ParseProcessJobID("pid://100:200"); err != nil || pid != 100 || st != 200 {
		t.Errorf("expected pid 100, st 200, got pid %d, st %d, err %v", pid, st, err)
	}
}

func TestProcessIdentityVerification_StartTimeMismatch(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sleep process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	realSt, err := GetProcessStartTime(cmd.Process.Pid)
	if err != nil {
		// Non-linux environment without /proc
		t.Skip("skipping starttime mismatch test on system without /proc")
	}

	// Supply a mismatched start time (realSt + 999999)
	mismatchedPIDStr := fmt.Sprintf("%d:%d", cmd.Process.Pid, realSt+999999)
	_, err = KillPodProcessesWithTimeout(mismatchedPIDStr, nil, 1*time.Second)
	if !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("expected ErrInvalidJob for start time mismatch, got %v", err)
	}

	// Now supply correct start time
	correctPIDStr := fmt.Sprintf("%d:%d", cmd.Process.Pid, realSt)
	_, err = KillPodProcessesWithTimeout(correctPIDStr, nil, 2*time.Second)
	if err != nil {
		t.Fatalf("unexpected error killing process with matching start time: %v", err)
	}
}

func TestKillPodProcessesWithTimeout_PrimaryDeadSweepsSecondary(t *testing.T) {
	primary := createHelperProcess(t)
	secondary := createHelperProcess(t)
	time.Sleep(100 * time.Millisecond)

	// Terminate primary process first to simulate crashed pause process (ESRCH)
	_ = primary.Process.Kill()
	_ = primary.Wait()

	doneS := make(chan struct{})
	go func() {
		_ = secondary.Wait()
		close(doneS)
	}()

	primaryPIDStr := strconv.Itoa(primary.Process.Pid)
	secondaryPIDStr := strconv.Itoa(secondary.Process.Pid)

	_, err := KillPodProcessesWithTimeout(primaryPIDStr, []string{secondaryPIDStr}, 200*time.Millisecond)
	if !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("expected ErrInvalidJob when primary process is dead, got %v", err)
	}

	select {
	case <-doneS:
	case <-time.After(3 * time.Second):
		t.Fatal("secondary container process was not terminated when primary pause process was already dead")
	}
}
