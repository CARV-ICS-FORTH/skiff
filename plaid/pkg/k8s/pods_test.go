package k8s

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type podSink struct {
	mu    sync.Mutex
	pushs []map[string]string
}

func (s *podSink) SetDirectPods(pods map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushs = append(s.pushs, pods)
}

func (s *podSink) last() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pushs) == 0 {
		return nil
	}
	return s.pushs[len(s.pushs)-1]
}

func pod(name, ip, ports string, phase v1.PodPhase) *v1.Pod {
	p := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Status: v1.PodStatus{Phase: phase, PodIP: ip}}
	if ports != "" {
		p.Annotations = map[string]string{DirectListenPortsAnnotation: ports, "other": "x"}
	}
	return p
}

// The pods that have listen ports for the direct path reach the sink by address, as their
// annotation says them; pods without them and pods that have ended do not, and a change of a
// pod's list is pushed.
func TestDirectPodsReachTheSink(t *testing.T) {
	old := directPodsDebounce
	directPodsDebounce = 10 * time.Millisecond
	t.Cleanup(func() { directPodsDebounce = old })
	client := fake.NewSimpleClientset(
		pod("netperf", "10.244.1.5", " 12865,5201 ", v1.PodRunning),
		pod("plain", "10.244.1.6", "", v1.PodRunning),
		pod("done", "10.244.1.7", "80", v1.PodSucceeded),
		pod("pending", "", "80", v1.PodPending),
	)
	c := NewController(client, "node-a")
	s := &podSink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.RunDirectPods(ctx, s); close(done) }()
	defer func() { cancel(); <-done }()

	want := map[string]string{"10.244.1.5": "12865,5201"}
	waitFor(t, "the first list", func() bool { return maps.Equal(s.last(), want) })
	p := pod("plain", "10.244.1.6", "8080", v1.PodRunning)
	if _, err := client.CoreV1().Pods("default").Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	want["10.244.1.6"] = "8080"
	waitFor(t, "the changed list", func() bool { return maps.Equal(s.last(), want) })
	if err := client.CoreV1().Pods("default").Delete(ctx, "netperf", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	delete(want, "10.244.1.5")
	waitFor(t, "the list without the deleted pod", func() bool { return maps.Equal(s.last(), want) })
	if got := fmt.Sprint(trimmed(t, pod("x", "10.244.1.8", "1", v1.PodRunning)).Annotations); got != "map["+DirectListenPortsAnnotation+":1]" {
		t.Errorf("the cache keeps %s of the annotations", got)
	}
}

func trimmed(t *testing.T, p *v1.Pod) *v1.Pod {
	t.Helper()
	o, err := trimPod(p)
	if err != nil {
		t.Fatal(err)
	}
	return o.(*v1.Pod)
}
