package podhandler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"skifflet/internal/compute/endpoint"
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
