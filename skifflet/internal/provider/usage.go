package provider

import (
	"context"
	"sort"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	PodHandler "skifflet/internal/compute/podhandler"
	"skifflet/internal/compute/runtime"
)

// What the node and its containers have used, in the two forms Kubernetes reads it:
// the kubelet's summary (/stats/summary) and its resource metrics (/metrics/resource,
// what metrics-server scrapes for `kubectl top` and autoscaling). The numbers come from the
// proc filesystem (see runtime.ProcessTreeUsage): a container is the tree of processes
// below its launcher, so they are an approximation of what a cgroup would say, and the
// node's own are those of the whole node, which a job has to itself.

type containerUsage struct {
	Name       string
	Start      time.Time
	CPUSeconds float64
	WorkingSet uint64
}

type podUsage struct {
	Namespace  string
	Name       string
	UID        types.UID
	Start      time.Time // the first of its containers
	Containers []containerUsage
}

type usageSnapshot struct {
	At       time.Time
	NodeName string
	// NodeStart is when the node's runtime started (this process).
	NodeStart time.Time
	Node      runtime.Usage
	Pods      []podUsage
}

// usageReader reads what the pods and the node use now.
type usageReader struct {
	nodeName  string
	nodeStart time.Time
	registry  *PodHandler.PodRegistry
	procDir   string
	now       func() time.Time

	mu sync.Mutex
	// previous samples, to turn cumulative CPU time into a rate (the summary reports one).
	previous map[string]sample
}

type sample struct {
	at         time.Time
	cpuSeconds float64
}

func newUsageReader(nodeName string, registry *PodHandler.PodRegistry) *usageReader {
	return &usageReader{nodeName: nodeName, nodeStart: time.Now(), registry: registry, procDir: "/proc", now: time.Now, previous: map[string]sample{}}
}

// read takes one reading. A container whose processes cannot be read (it has just
// ended) is left out of it; the node's own usage not being readable is an error.
func (r *usageReader) read() (usageSnapshot, error) {
	at := r.now()
	node, err := runtime.NodeUsage(r.procDir)
	if err != nil {
		return usageSnapshot{}, err
	}
	snapshot := usageSnapshot{At: at, NodeName: r.nodeName, NodeStart: r.nodeStart, Node: node}
	for _, lifecycle := range r.registry.All() {
		key := lifecycle.Key()
		pod := podUsage{Namespace: key.Namespace, Name: key.Name, UID: lifecycle.UID()}
		for _, running := range lifecycle.RunningContainers() {
			usage, err := runtime.ProcessTreeUsage(r.procDir, running.Launcher)
			if err != nil {
				continue
			}
			pod.Containers = append(pod.Containers, containerUsage{Name: running.Name, Start: running.Started, CPUSeconds: usage.CPUSeconds, WorkingSet: usage.WorkingSetBytes})
			if pod.Start.IsZero() || running.Started.Before(pod.Start) {
				pod.Start = running.Started
			}
		}
		if len(pod.Containers) > 0 {
			snapshot.Pods = append(snapshot.Pods, pod)
		}
	}
	sort.Slice(snapshot.Pods, func(i, j int) bool {
		if snapshot.Pods[i].Namespace != snapshot.Pods[j].Namespace {
			return snapshot.Pods[i].Namespace < snapshot.Pods[j].Namespace
		}
		return snapshot.Pods[i].Name < snapshot.Pods[j].Name
	})
	return snapshot, nil
}

// nanoCores is the rate of CPU use since the previous reading under key, in nanocores; it
// is nil the first time and when the time or the counter did not advance.
func (r *usageReader) nanoCores(key string, at time.Time, cpuSeconds float64) *uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, ok := r.previous[key]
	r.previous[key] = sample{at: at, cpuSeconds: cpuSeconds}
	if !ok || !at.After(prev.at) || cpuSeconds < prev.cpuSeconds {
		return nil
	}
	n := uint64((cpuSeconds - prev.cpuSeconds) / at.Sub(prev.at).Seconds() * 1e9)
	return &n
}

// forget drops the previous samples of what is no longer in the snapshot.
func (r *usageReader) forget(keep map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.previous {
		if !keep[key] {
			delete(r.previous, key)
		}
	}
}

func cpuStats(at time.Time, seconds float64, rate *uint64) *statsv1alpha1.CPUStats {
	total := uint64(seconds * 1e9)
	return &statsv1alpha1.CPUStats{Time: metav1.NewTime(at), UsageCoreNanoSeconds: &total, UsageNanoCores: rate}
}

func memoryStats(at time.Time, workingSet uint64) *statsv1alpha1.MemoryStats {
	usage, ws := workingSet, workingSet
	return &statsv1alpha1.MemoryStats{Time: metav1.NewTime(at), UsageBytes: &usage, WorkingSetBytes: &ws}
}

// summary builds the kubelet's /stats/summary from a reading.
func (r *usageReader) summary(s usageSnapshot) *statsv1alpha1.Summary {
	keep := map[string]bool{"node": true}
	out := &statsv1alpha1.Summary{
		Node: statsv1alpha1.NodeStats{
			NodeName:  s.NodeName,
			StartTime: metav1.NewTime(s.NodeStart),
			CPU:       cpuStats(s.At, s.Node.CPUSeconds, r.nanoCores("node", s.At, s.Node.CPUSeconds)),
			Memory:    memoryStats(s.At, s.Node.WorkingSetBytes),
		},
	}
	for _, pod := range s.Pods {
		var podCPU float64
		var podMemory uint64
		stats := statsv1alpha1.PodStats{
			PodRef:    statsv1alpha1.PodReference{Name: pod.Name, Namespace: pod.Namespace, UID: string(pod.UID)},
			StartTime: metav1.NewTime(pod.Start),
		}
		for _, c := range pod.Containers {
			key := string(pod.UID) + "/" + c.Name
			keep[key] = true
			stats.Containers = append(stats.Containers, statsv1alpha1.ContainerStats{
				Name:      c.Name,
				StartTime: metav1.NewTime(c.Start),
				CPU:       cpuStats(s.At, c.CPUSeconds, r.nanoCores(key, s.At, c.CPUSeconds)),
				Memory:    memoryStats(s.At, c.WorkingSet),
			})
			podCPU += c.CPUSeconds
			podMemory += c.WorkingSet
		}
		podKey := string(pod.UID)
		keep[podKey] = true
		stats.CPU = cpuStats(s.At, podCPU, r.nanoCores(podKey, s.At, podCPU))
		stats.Memory = memoryStats(s.At, podMemory)
		out.Pods = append(out.Pods, stats)
	}
	r.forget(keep)
	return out
}

func counter(name, help string, metrics ...*dto.Metric) *dto.MetricFamily {
	t := dto.MetricType_COUNTER
	return &dto.MetricFamily{Name: &name, Help: &help, Type: &t, Metric: metrics}
}

func gauge(name, help string, metrics ...*dto.Metric) *dto.MetricFamily {
	t := dto.MetricType_GAUGE
	return &dto.MetricFamily{Name: &name, Help: &help, Type: &t, Metric: metrics}
}

func labelled(at time.Time, value float64, counterValue bool, labels ...string) *dto.Metric {
	m := &dto.Metric{}
	for i := 0; i+1 < len(labels); i += 2 {
		name, val := labels[i], labels[i+1]
		m.Label = append(m.Label, &dto.LabelPair{Name: &name, Value: &val})
	}
	ms := at.UnixMilli()
	m.TimestampMs = &ms
	if counterValue {
		m.Counter = &dto.Counter{Value: &value}
	} else {
		m.Gauge = &dto.Gauge{Value: &value}
	}
	return m
}

// resourceMetrics builds the kubelet's /metrics/resource from a reading: the families
// metrics-server reads, under the names the kubelet gives them.
func resourceMetrics(s usageSnapshot) []*dto.MetricFamily {
	var containerCPU, containerMemory, containerStart, podCPU, podMemory []*dto.Metric
	for _, pod := range s.Pods {
		var cpu float64
		var memory uint64
		for _, c := range pod.Containers {
			labels := []string{"container", c.Name, "namespace", pod.Namespace, "pod", pod.Name}
			containerCPU = append(containerCPU, labelled(s.At, c.CPUSeconds, true, labels...))
			containerMemory = append(containerMemory, labelled(s.At, float64(c.WorkingSet), false, labels...))
			containerStart = append(containerStart, labelled(s.At, float64(c.Start.UnixNano())/1e9, false, labels...))
			cpu += c.CPUSeconds
			memory += c.WorkingSet
		}
		labels := []string{"namespace", pod.Namespace, "pod", pod.Name}
		podCPU = append(podCPU, labelled(s.At, cpu, true, labels...))
		podMemory = append(podMemory, labelled(s.At, float64(memory), false, labels...))
	}
	families := []*dto.MetricFamily{
		counter("node_cpu_usage_seconds_total", "Cumulative cpu time consumed by the node in core-seconds", labelled(s.At, s.Node.CPUSeconds, true)),
		gauge("node_memory_working_set_bytes", "Current working set of the node in bytes", labelled(s.At, float64(s.Node.WorkingSetBytes), false)),
		counter("container_cpu_usage_seconds_total", "Cumulative cpu time consumed by the container in core-seconds", containerCPU...),
		gauge("container_memory_working_set_bytes", "Current working set of the container in bytes", containerMemory...),
		gauge("container_start_time_seconds", "Start time of the container since unix epoch in seconds", containerStart...),
		counter("pod_cpu_usage_seconds_total", "Cumulative cpu time consumed by the pod in core-seconds", podCPU...),
		gauge("pod_memory_working_set_bytes", "Current working set of the pod in bytes", podMemory...),
		gauge("resource_scrape_error", "1 if there was an error while getting container metrics, 0 otherwise", labelled(s.At, 0, false)),
	}
	// A family with no sample is left out, as the kubelet does when a node runs no container: its
	// header alone is not valid exposition for the API server's proxy, which refuses the whole answer
	// ("MetricFamily has no metrics"), and so would kubectl top and metrics-server.
	present := families[:0]
	for _, family := range families {
		if len(family.Metric) > 0 {
			present = append(present, family)
		}
	}
	return present
}

// GetStatsSummary serves the kubelet's /stats/summary.
func (v *VirtualK8S) GetStatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	reader := v.usage()
	snapshot, err := reader.read()
	if err != nil {
		return nil, err
	}
	return reader.summary(snapshot), nil
}

// GetMetricsResource serves the kubelet's /metrics/resource.
func (v *VirtualK8S) GetMetricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	snapshot, err := v.usage().read()
	if err != nil {
		return nil, err
	}
	return resourceMetrics(snapshot), nil
}

func (v *VirtualK8S) usage() *usageReader {
	v.usageOnce.Do(func() { v.usageReader = newUsageReader(v.NodeName, PodHandler.GlobalRegistry) })
	return v.usageReader
}
