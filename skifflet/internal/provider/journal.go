package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"k8s.io/apimachinery/pkg/types"

	"skifflet/internal/compute"
	"skifflet/internal/compute/podhandler"
	"skifflet/internal/compute/runtime"
)

var (
	ErrInstanceLocked = errors.New("another skifflet instance holds the runtime lock")
)

// PodJournalRecord stores crash-recovery metadata for a pod.
type PodJournalRecord struct {
	UID            types.UID `json:"uid"`
	Generation     int64     `json:"generation"`
	Namespace      string    `json:"namespace"`
	Name           string    `json:"name"`
	PodDir         string    `json:"pod_dir"`
	PausePID       int       `json:"pause_pid"`
	PauseStartTime uint64    `json:"pause_start_time"`
	ContainerPIDs  []int     `json:"container_pids"`
	EndpointID     string    `json:"endpoint_id"`
	IP             string    `json:"ip"`
}

// InstanceLock represents an advisory lock held on the Skiff runtime directory.
type InstanceLock struct {
	file *os.File
}

// AcquireInstanceLock obtains an exclusive non-blocking lock on <skiffDir>/skifflet.lock.
func AcquireInstanceLock(skiffDir string) (*InstanceLock, error) {
	if err := os.MkdirAll(skiffDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create runtime directory for lock: %w", err)
	}

	lockPath := filepath.Join(skiffDir, "skifflet.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open instance lock file '%s': %w", lockPath, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w at '%s': %v", ErrInstanceLocked, lockPath, err)
	}

	return &InstanceLock{file: f}, nil
}

// Release releases the advisory instance lock.
func (l *InstanceLock) Release() {
	if l != nil && l.file != nil {
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		_ = l.file.Close()
	}
}

// JournalDir returns the path to the crash-recovery journal directory.
func JournalDir(skiffDir string) string {
	return filepath.Join(skiffDir, "journal")
}

// WriteJournalRecord writes or updates a pod journal entry atomically.
func WriteJournalRecord(skiffDir string, rec PodJournalRecord) error {
	jDir := JournalDir(skiffDir)
	if err := os.MkdirAll(jDir, 0755); err != nil {
		return fmt.Errorf("failed to create journal directory: %w", err)
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("failed to marshal journal record: %w", err)
	}

	targetPath := filepath.Join(jDir, string(rec.UID)+".json")
	tmpPath := targetPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write tmp journal record: %w", err)
	}

	return os.Rename(tmpPath, targetPath)
}

// RemoveJournalRecord removes a pod journal entry once the pod has cleanly terminated.
func RemoveJournalRecord(skiffDir string, uid types.UID) {
	targetPath := filepath.Join(JournalDir(skiffDir), string(uid)+".json")
	_ = os.Remove(targetPath)
}

// ReconcileSurvivingWorkloads reads crash-recovery records on startup and deterministically
// stops any orphaned processes and releases dangling network resources.
func ReconcileSurvivingWorkloads(skiffDir string) error {
	jDir := JournalDir(skiffDir)
	entries, err := os.ReadDir(jDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to read journal directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(jDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var rec PodJournalRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			_ = os.Remove(path)
			continue
		}

		compute.DefaultLogger.Info("Reconciling surviving workload from previous run",
			"uid", rec.UID, "namespace", rec.Namespace, "name", rec.Name, "pausePID", rec.PausePID)

		// 1. Terminate secondary container processes
		for _, pid := range rec.ContainerPIDs {
			if pid > 1 && !runtime.IsProcessDead(pid) {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}

		// 2. Terminate pause process if alive
		if rec.PausePID > 1 && !runtime.IsProcessDead(rec.PausePID) {
			_ = syscall.Kill(-rec.PausePID, syscall.SIGKILL)
			_ = syscall.Kill(rec.PausePID, syscall.SIGKILL)
		}

		// 3. Remove Plaid endpoint if recorded
		if rec.EndpointID != "" {
			_ = podhandler.RemovePlaidEndpoint(rec.EndpointID)
		}

		// 4. Remove journal record
		_ = os.Remove(path)
	}

	return nil
}
