// Copyright © 2022 FORTH-ICS
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package runtime contains code for managing compute environments.
package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	ErrInvalidJob = errors.New("invalid job id")
	ErrPIDTooLow  = errors.New("pid must be greater than 1")
)

type trackedPID struct {
	pid       int
	startTime uint64
}

// IsProcessDead checks whether a process has exited or is in zombie state ('Z').
func IsProcessDead(pid int) bool {
	return isProcessDead(pid)
}

func isProcessDead(pid int) bool {
	if pid <= 1 {
		return true
	}
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	if err != nil {
		return false
	}

	// On macOS / BSD systems without /proc, syscall.Getpriority returns ESRCH for zombie processes.
	if _, err := syscall.Getpriority(0, pid); errors.Is(err, syscall.ESRCH) {
		return true
	}

	// On POSIX systems, syscall.Getpgid(pid) returns syscall.ESRCH once the process has been reaped.
	if _, err := syscall.Getpgid(pid); errors.Is(err, syscall.ESRCH) {
		return true
	}

	// Check /proc/<pid>/stat on Linux for 'Z' or 'X' state
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if idx := bytes.LastIndexByte(data, ')'); idx != -1 && idx+2 < len(data) {
			state := data[idx+2]
			if state == 'Z' || state == 'X' {
				return true
			}
		}
	}

	return false
}

// GetProcessStartTime returns the process start time (field 22 of /proc/<pid>/stat) in clock ticks since boot.
func GetProcessStartTime(pid int) (uint64, error) {
	return GetProcessStartTimeIn("/proc", pid)
}

// GetProcessStartTimeIn is GetProcessStartTime for a proc filesystem mounted at procDir.
func GetProcessStartTimeIn(procDir string, pid int) (uint64, error) {
	if pid <= 1 {
		return 0, fmt.Errorf("%w: cannot get start time for pid %d", ErrPIDTooLow, pid)
	}
	data, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", procDir, pid))
	if err != nil {
		return 0, err
	}

	idx := bytes.LastIndexByte(data, ')')
	if idx == -1 || idx+1 >= len(data) {
		return 0, fmt.Errorf("invalid stat format for pid %d", pid)
	}

	fields := strings.Fields(string(data[idx+1:]))
	if len(fields) <= 19 {
		return 0, fmt.Errorf("stat format for pid %d has insufficient fields", pid)
	}

	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse starttime for pid %d: %w", pid, err)
	}

	return startTime, nil
}

// VerifyProcessIdentity checks whether a process with the given PID matches the recorded process identity.
// If /proc is present and recordedStartTime > 0, it verifies that current start time matches recordedStartTime.
func VerifyProcessIdentity(pid int, recordedStartTime uint64) bool {
	if pid <= 1 {
		return false
	}
	// If /proc is not present (e.g. non-Linux systems), skip check
	if _, err := os.Stat("/proc"); os.IsNotExist(err) {
		return true
	}

	currStartTime, err := GetProcessStartTime(pid)
	if err != nil {
		// Process may be dead or inaccessible
		return false
	}

	if recordedStartTime > 0 {
		return currStartTime == recordedStartTime
	}

	return true
}

func verifyProcessIdentity(pid int, recordedStartTime uint64) bool {
	return VerifyProcessIdentity(pid, recordedStartTime)
}

// KillProcessByPID terminates a process by its PID using syscall.Kill with a default timeout of 35 seconds.
func KillProcessByPID(pidStr string) (string, error) {
	return KillProcessByPIDWithTimeout(pidStr, 35*time.Second)
}

// KillProcessByPIDWithTimeout sends SIGTERM to a process by its PID and polls for process exit until timeout.
// If the process does not exit within timeout, SIGKILL is sent to force termination.
func KillProcessByPIDWithTimeout(pidStr string, timeout time.Duration) (string, error) {
	return KillPodProcessesWithTimeout(pidStr, nil, timeout)
}

// sweepSecondaryPIDs sends SIGTERM to secondary container process groups (-pgid) and PIDs,
// waits briefly, and then sends SIGKILL to clean up any orphaned container processes.
// Start time identity is revalidated before both SIGTERM and SIGKILL.
func sweepSecondaryPIDs(secondaryPIDs []trackedPID) {
	if len(secondaryPIDs) == 0 {
		return
	}
	for _, sec := range secondaryPIDs {
		if sec.pid > 1 && verifyProcessIdentity(sec.pid, sec.startTime) {
			_ = syscall.Kill(-sec.pid, syscall.SIGTERM)
			_ = syscall.Kill(sec.pid, syscall.SIGTERM)
		}
	}

	time.Sleep(200 * time.Millisecond)

	for _, sec := range secondaryPIDs {
		if sec.pid > 1 && verifyProcessIdentity(sec.pid, sec.startTime) {
			_ = syscall.Kill(-sec.pid, syscall.SIGKILL)
			_ = syscall.Kill(sec.pid, syscall.SIGKILL)
		}
	}
}

// KillPodProcessesWithTimeout sends SIGTERM to primaryPIDStr and polls for process exit until timeout.
// It uses adaptive polling to reduce CPU overhead during grace periods.
// If timeout is reached, process identities are revalidated before escalating to SIGKILL.
// All PIDs <= 1 are strictly rejected at all signaling boundaries.
func KillPodProcessesWithTimeout(primaryPIDStr string, secondaryPIDStrs []string, timeout time.Duration) (string, error) {
	var secondaryPIDs []trackedPID
	for _, s := range secondaryPIDStrs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, secStartTime, err := ParseProcessJobID(s); err == nil && p > 1 {
			if verifyProcessIdentity(p, secStartTime) {
				secondaryPIDs = append(secondaryPIDs, trackedPID{pid: p, startTime: secStartTime})
			}
		}
	}

	primaryPIDStr = strings.TrimSpace(primaryPIDStr)
	if primaryPIDStr == "" {
		sweepSecondaryPIDs(secondaryPIDs)
		return "", ErrInvalidJob
	}

	primaryPID, primaryStartTime, err := ParseProcessJobID(primaryPIDStr)
	if err != nil {
		sweepSecondaryPIDs(secondaryPIDs)
		return "", fmt.Errorf("%w: invalid pid '%s': %v", ErrInvalidJob, primaryPIDStr, err)
	}
	if primaryPID <= 1 {
		sweepSecondaryPIDs(secondaryPIDs)
		return "", fmt.Errorf("%w: invalid primary pid %d: must be > 1", ErrInvalidJob, primaryPID)
	}

	// Verify PID is the expected process by start time identity to prevent stale PID signaling on recycled host PIDs
	if !verifyProcessIdentity(primaryPID, primaryStartTime) {
		sweepSecondaryPIDs(secondaryPIDs)
		return "", fmt.Errorf("%w: process '%d' start time does not match recorded identity", ErrInvalidJob, primaryPID)
	}

	/*
		Phase 1: Send SIGTERM to all secondary (application) processes first,
		allowing them to perform graceful shutdown while pod network/sandbox remains intact.
	*/
	for _, sec := range secondaryPIDs {
		if sec.pid > 1 && verifyProcessIdentity(sec.pid, sec.startTime) {
			_ = syscall.Kill(-sec.pid, syscall.SIGTERM)
			_ = syscall.Kill(sec.pid, syscall.SIGTERM)
		}
	}

	allSecondaryDead := func() bool {
		for _, sec := range secondaryPIDs {
			if !isProcessDead(sec.pid) {
				return false
			}
		}
		return true
	}

	appDeadline := time.Now().Add(timeout)
	fastUntil := time.Now().Add(1 * time.Second)

	for !allSecondaryDead() && time.Now().Before(appDeadline) {
		pollInterval := 50 * time.Millisecond
		if time.Now().After(fastUntil) {
			pollInterval = 250 * time.Millisecond
		}
		time.Sleep(pollInterval)
	}

	// Escalate SIGKILL to any surviving secondary containers with start time revalidation
	for _, sec := range secondaryPIDs {
		if sec.pid > 1 && !isProcessDead(sec.pid) && verifyProcessIdentity(sec.pid, sec.startTime) {
			_ = syscall.Kill(-sec.pid, syscall.SIGKILL)
			_ = syscall.Kill(sec.pid, syscall.SIGKILL)
		}
	}

	/*
		Phase 2: Now that application containers have finished (or been SIGKILLed),
		terminate the primary (pause) container to release the network sandbox.
	*/
	_ = syscall.Kill(-primaryPID, syscall.SIGTERM)
	if err := syscall.Kill(primaryPID, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return "", ErrInvalidJob
		}
		return "", fmt.Errorf("could not kill pause process '%d': %w", primaryPID, err)
	}

	pauseDeadline := time.Now().Add(5 * time.Second)
	for !isProcessDead(primaryPID) && time.Now().Before(pauseDeadline) {
		time.Sleep(100 * time.Millisecond)
	}

	if !isProcessDead(primaryPID) && verifyProcessIdentity(primaryPID, primaryStartTime) {
		_ = syscall.Kill(-primaryPID, syscall.SIGKILL)
		if err := syscall.Kill(primaryPID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return "", fmt.Errorf("could not SIGKILL process '%d': %w", primaryPID, err)
		}
	}

	killDeadline := time.Now().Add(5 * time.Second)
	allProcessesDead := func() bool {
		if !isProcessDead(primaryPID) {
			return false
		}
		for _, sec := range secondaryPIDs {
			if !isProcessDead(sec.pid) {
				return false
			}
		}
		return true
	}

	for {
		if allProcessesDead() {
			return "", nil
		}

		if time.Now().After(killDeadline) {
			return "", fmt.Errorf("process '%d' did not exit after SIGKILL", primaryPID)
		}

		time.Sleep(100 * time.Millisecond)
	}
}
