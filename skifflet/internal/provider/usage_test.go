package provider

import (
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/types"

	PodHandler "skifflet/internal/compute/podhandler"
	"skifflet/internal/compute/runtime"
)

func testSnapshot(at time.Time) usageSnapshot {
	start := at.Add(-time.Minute)
	return usageSnapshot{
		At: at, NodeName: "node-1", NodeStart: at.Add(-time.Hour),
		Node: runtime.Usage{CPUSeconds: 100, WorkingSetBytes: 4 << 30},
		Pods: []podUsage{{
			Namespace: "default", Name: "web", UID: types.UID("uid-web"), Start: start,
			Containers: []containerUsage{
				{Name: "app", Start: start, CPUSeconds: 12.5, WorkingSet: 100 << 20},
				{Name: "sidecar", Start: start.Add(time.Second), CPUSeconds: 0.5, WorkingSet: 20 << 20},
			},
		}},
	}
}

func metric(t *testing.T, families []*dto.MetricFamily, name string, labels ...string) *dto.Metric {
	t.Helper()
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.Metric {
			ok := true
			for i := 0; i+1 < len(labels); i += 2 {
				found := false
				for _, l := range m.Label {
					if l.GetName() == labels[i] && l.GetValue() == labels[i+1] {
						found = true
					}
				}
				ok = ok && found
			}
			if ok {
				return m
			}
		}
	}
	t.Fatalf("no metric %s %v", name, labels)
	return nil
}

// What metrics-server reads: the kubelet's names, cumulative CPU as counters (so rates can
// be computed), memory as gauges, a start time per container (so a restart is not taken for
// a counter that went backwards), a timestamp on every sample, and a pod total.
func TestResourceMetrics(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	families := resourceMetrics(testSnapshot(at))
	names := map[string]dto.MetricType{}
	for _, f := range families {
		names[f.GetName()] = f.GetType()
	}
	for name, kind := range map[string]dto.MetricType{
		"node_cpu_usage_seconds_total":       dto.MetricType_COUNTER,
		"node_memory_working_set_bytes":      dto.MetricType_GAUGE,
		"container_cpu_usage_seconds_total":  dto.MetricType_COUNTER,
		"container_memory_working_set_bytes": dto.MetricType_GAUGE,
		"container_start_time_seconds":       dto.MetricType_GAUGE,
		"pod_cpu_usage_seconds_total":        dto.MetricType_COUNTER,
		"pod_memory_working_set_bytes":       dto.MetricType_GAUGE,
		"resource_scrape_error":              dto.MetricType_GAUGE,
	} {
		if got, ok := names[name]; !ok || got != kind {
			t.Errorf("metric family %s: have %v (present %v), want %v", name, got, ok, kind)
		}
	}
	if v := metric(t, families, "node_cpu_usage_seconds_total").Counter.GetValue(); v != 100 {
		t.Errorf("node CPU %v, want 100", v)
	}
	app := []string{"container", "app", "namespace", "default", "pod", "web"}
	if v := metric(t, families, "container_cpu_usage_seconds_total", app...).Counter.GetValue(); v != 12.5 {
		t.Errorf("container CPU %v, want 12.5", v)
	}
	if v := metric(t, families, "container_memory_working_set_bytes", app...).Gauge.GetValue(); v != float64(100<<20) {
		t.Errorf("container memory %v, want %d", v, 100<<20)
	}
	if v := metric(t, families, "container_start_time_seconds", app...).Gauge.GetValue(); v != float64(at.Add(-time.Minute).Unix()) {
		t.Errorf("container start %v", v)
	}
	if v := metric(t, families, "pod_cpu_usage_seconds_total", "namespace", "default", "pod", "web").Counter.GetValue(); v != 13 {
		t.Errorf("pod CPU %v, want 13 (12.5 + 0.5)", v)
	}
	if v := metric(t, families, "pod_memory_working_set_bytes", "namespace", "default", "pod", "web").Gauge.GetValue(); v != float64(120<<20) {
		t.Errorf("pod memory %v, want %d", v, 120<<20)
	}
	for _, f := range families {
		for _, m := range f.Metric {
			if m.TimestampMs == nil || *m.TimestampMs != at.UnixMilli() {
				t.Errorf("a sample of %s has no timestamp of the reading", f.GetName())
			}
		}
	}
}

// The summary has the node, the pod with its containers and the pod's totals; a rate of
// CPU use comes from two readings (none from the first), and a container that is gone
// leaves no sample behind.
func TestSummary(t *testing.T) {
	reader := newUsageReader("node-1", PodHandler.NewPodRegistry())
	at := time.Unix(1_700_000_000, 0)
	first := reader.summary(testSnapshot(at))
	if first.Node.NodeName != "node-1" || len(first.Pods) != 1 || len(first.Pods[0].Containers) != 2 {
		t.Fatalf("summary %+v", first)
	}
	app := first.Pods[0].Containers[0]
	if app.Name != "app" || *app.CPU.UsageCoreNanoSeconds != 12_500_000_000 || *app.Memory.WorkingSetBytes != 100<<20 {
		t.Errorf("container stats %+v", app)
	}
	if app.CPU.UsageNanoCores != nil {
		t.Error("a rate from a single reading")
	}
	if first.Pods[0].PodRef.UID != "uid-web" || *first.Pods[0].CPU.UsageCoreNanoSeconds != 13_000_000_000 {
		t.Errorf("pod stats %+v", first.Pods[0])
	}

	later := testSnapshot(at.Add(10 * time.Second))
	later.Pods[0].Containers[0].CPUSeconds = 14.5 // 2 s of CPU in 10 s: 200 millicores
	second := reader.summary(later)
	if rate := second.Pods[0].Containers[0].CPU.UsageNanoCores; rate == nil || *rate != 200_000_000 {
		t.Errorf("rate %v, want 200000000 nanocores", rate)
	}
	// The pod goes away; its samples with it, so a pod of the same name starts afresh.
	gone := testSnapshot(at.Add(20 * time.Second))
	gone.Pods = nil
	reader.summary(gone)
	if len(reader.previous) != 1 || reader.previous["node"].cpuSeconds == 0 {
		t.Errorf("samples kept after the pod went: %v", reader.previous)
	}
	// A counter that went backwards (a restart) is not a negative rate.
	if reader.nanoCores("x", at, 10) != nil || reader.nanoCores("x", at.Add(time.Second), 5) != nil {
		t.Error("a rate from a counter that went backwards")
	}
}

func TestGetMetricsResourceNeedsTheNode(t *testing.T) {
	reader := newUsageReader("n", PodHandler.NewPodRegistry())
	reader.procDir = t.TempDir() // no /proc/stat here
	if _, err := reader.read(); err == nil || !strings.Contains(err.Error(), "stat") {
		t.Errorf("a node whose usage cannot be read: %v", err)
	}
}
