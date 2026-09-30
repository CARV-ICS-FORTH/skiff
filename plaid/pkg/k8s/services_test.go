package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"plaid/pkg/svc"
)

type sink struct {
	mu     sync.Mutex
	tables [][]svc.Service
}

func (s *sink) SetServices(table []svc.Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tables = append(s.tables, table)
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tables)
}

func (s *sink) last() []svc.Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tables) == 0 {
		return nil
	}
	return s.tables[len(s.tables)-1]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type harness struct {
	c        *Controller
	sink     *sink
	recorder *record.FakeRecorder
	client   *fake.Clientset
	cancel   context.CancelFunc
	done     chan struct{}
}

func start(t *testing.T, objs ...runtime.Object) *harness {
	t.Helper()
	old := serviceWatchDebounce
	serviceWatchDebounce = 10 * time.Millisecond
	t.Cleanup(func() { serviceWatchDebounce = old })
	client := fake.NewSimpleClientset(objs...)
	return startWith(t, client)
}

func startWith(t *testing.T, client *fake.Clientset) *harness {
	t.Helper()
	h := &harness{c: NewController(client, "node-a"), sink: &sink{}, recorder: record.NewFakeRecorder(100), client: client, done: make(chan struct{})}
	h.c.SetEventRecorder(h.recorder)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.c.RunServices(ctx, h.sink); close(h.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("RunServices did not stop with its context")
		}
	})
	return h
}

func web() (*v1.Service, *discoveryv1.EndpointSlice) {
	s := service("default", "web", "10.43.0.50", port("", 80, v1.ProtocolTCP))
	sl := slice("default", "web", "web-a", []discoveryv1.EndpointPort{{Port: ptr.To(int32(8080))}}, ep{addr: "10.244.1.5", ready: yes()})
	return s, sl
}

func (h *harness) settle(t *testing.T) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
}

func TestTheFirstTableIsBuiltFromCompleteCaches(t *testing.T) {
	var objs []runtime.Object
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("s%d", i)
		objs = append(objs,
			service("default", name, fmt.Sprintf("10.43.0.%d", 10+i), port("", 80, v1.ProtocolTCP)),
			slice("default", name, name, []discoveryv1.EndpointPort{{Port: ptr.To(int32(80))}}, ep{addr: "10.244.1.5", ready: yes()}))
	}
	h := start(t, objs...)
	waitFor(t, "the first table", func() bool { return h.sink.count() >= 1 })
	h.settle(t)
	if h.sink.count() != 1 || len(h.sink.last()) != 5 {
		t.Fatalf("%d tables, the first with %d entries", h.sink.count(), len(h.sink.last()))
	}
	for _, e := range h.sink.last() {
		if len(e.Backends) != 1 {
			t.Errorf("an entry was built before its slice was seen: %+v", e)
		}
	}
	if st := h.c.ServiceStatus(); !st.Synced || st.Tables != 1 || st.Services != 5 || st.WatchErrors != 0 {
		t.Errorf("status %+v", st)
	}
}

func TestChangesReachTheSinkAndOnlyRealChangesDo(t *testing.T) {
	s, sl := web()
	h := start(t, s, sl)
	waitFor(t, "the first table", func() bool { return h.sink.count() == 1 })
	ctx := context.Background()

	// A new endpoint.
	sl2 := sl.DeepCopy()
	sl2.Endpoints = append(sl2.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.244.2.7"}, Conditions: discoveryv1.EndpointConditions{Ready: yes()}})
	if _, err := h.client.DiscoveryV1().EndpointSlices("default").Update(ctx, sl2, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new backend", func() bool { l := h.sink.last(); return len(l) == 1 && len(l[0].Backends) == 2 })

	// An endpoint stops being ready: still listed, not eligible.
	sl3 := sl2.DeepCopy()
	sl3.Endpoints[0].Conditions.Ready = no()
	if _, err := h.client.DiscoveryV1().EndpointSlices("default").Update(ctx, sl3, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the ineligible backend", func() bool {
		l := h.sink.last()
		return len(l) == 1 && len(l[0].Backends) == 2 && !l[0].Backends[0].Eligible
	})

	// Changes that do not change the table are not pushed: a label on the Service, an unrelated object.
	before := h.sink.count()
	s2 := s.DeepCopy()
	s2.Labels = map[string]string{"tier": "front"}
	if _, err := h.client.CoreV1().Services("default").Update(ctx, s2, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.CoreV1().Services("default").Create(ctx, service("default", "headless", "None", port("", 80, v1.ProtocolTCP)), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.settle(t)
	if h.sink.count() != before {
		t.Errorf("%d tables were pushed for changes that leave the table as it is", h.sink.count()-before)
	}

	// Deleting the Service empties the table.
	if err := h.client.CoreV1().Services("default").Delete(ctx, "web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the table to empty", func() bool { return len(h.sink.last()) == 0 && h.sink.count() > before })
}

func TestARecreatedServiceIsANewEntry(t *testing.T) {
	s, sl := web()
	h := start(t, s, sl)
	waitFor(t, "the first table", func() bool { return h.sink.count() == 1 })
	old := h.sink.last()[0].ID

	ctx := context.Background()
	if err := h.client.CoreV1().Services("default").Delete(ctx, "web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s2 := s.DeepCopy()
	s2.UID = "the-new-one"
	if _, err := h.client.CoreV1().Services("default").Create(ctx, s2, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new ID", func() bool { l := h.sink.last(); return len(l) == 1 && l[0].ID != old })
}

func TestABurstOfChangesMakesFewTables(t *testing.T) {
	s, sl := web()
	h := start(t, s, sl)
	waitFor(t, "the first table", func() bool { return h.sink.count() == 1 })
	before := h.sink.count()
	for i := 0; i < 60; i++ {
		u := sl.DeepCopy()
		u.Endpoints = nil
		for j := 0; j <= i%5; j++ {
			u.Endpoints = append(u.Endpoints, discoveryv1.Endpoint{Addresses: []string{fmt.Sprintf("10.244.3.%d", j+1)}, Conditions: discoveryv1.EndpointConditions{Ready: yes()}})
		}
		if _, err := h.client.DiscoveryV1().EndpointSlices("default").Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// 60 updates end with i = 59: five endpoints.
	waitFor(t, "the last state", func() bool { l := h.sink.last(); return len(l) == 1 && len(l[0].Backends) == 5 })
	if n := h.sink.count() - before; n > 30 {
		t.Errorf("%d tables for 60 updates", n)
	}
}

func TestUnsupportedSettingsAreReportedOncePerVersion(t *testing.T) {
	good, sl := web()
	bad := service("default", "sticky", "10.43.0.60", port("", 80, v1.ProtocolTCP))
	bad.Spec.SessionAffinity = v1.ServiceAffinityClientIP
	h := start(t, good, sl, bad)
	waitFor(t, "the first table", func() bool { return h.sink.count() == 1 })

	next := func() string {
		select {
		case e := <-h.recorder.Events:
			return e
		case <-time.After(2 * time.Second):
			t.Fatal("no event")
			return ""
		}
	}
	if e := next(); !strings.HasPrefix(e, "Warning "+ReasonSessionAffinity) {
		t.Errorf("event %q", e)
	}
	// The table has the Service that works and not the one that cannot.
	if l := h.sink.last(); len(l) != 1 || l[0].Port != 80 || l[0].VIP != ip4("10.43.0.50") {
		t.Errorf("table %+v", l)
	}

	// Rebuilds for other reasons do not repeat it.
	ctx := context.Background()
	u := sl.DeepCopy()
	u.Endpoints = append(u.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.244.2.2"}, Conditions: discoveryv1.EndpointConditions{Ready: yes()}})
	if _, err := h.client.DiscoveryV1().EndpointSlices("default").Update(ctx, u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a second table", func() bool { return h.sink.count() == 2 })
	h.settle(t)
	if len(h.recorder.Events) != 0 {
		t.Errorf("the event was repeated: %v", <-h.recorder.Events)
	}

	// A new version of the Service that is still unsupported is reported again; a fixed one is not.
	bad2 := bad.DeepCopy()
	bad2.ResourceVersion = "2"
	bad2.Labels = map[string]string{"x": "y"}
	if _, err := h.client.CoreV1().Services("default").Update(ctx, bad2, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if e := next(); !strings.HasPrefix(e, "Warning "+ReasonSessionAffinity) {
		t.Errorf("event %q", e)
	}
	fixed := bad2.DeepCopy()
	fixed.ResourceVersion = "3"
	fixed.Spec.SessionAffinity = v1.ServiceAffinityNone
	if _, err := h.client.CoreV1().Services("default").Update(ctx, fixed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the fixed Service to be served", func() bool { return len(h.sink.last()) == 2 })
	h.settle(t)
	if len(h.recorder.Events) != 0 {
		t.Errorf("an event for a Service that works: %v", <-h.recorder.Events)
	}
	if st := h.c.ServiceStatus(); st.Problems != 0 {
		t.Errorf("status %+v", st)
	}
}

func TestFailingWatchesLeaveTheTableInPlaceAndAreCounted(t *testing.T) {
	s, sl := web()
	client := fake.NewSimpleClientset(s, sl)
	fail := errors.New("the API server is unreachable")
	client.PrependWatchReactor("*", func(clienttesting.Action) (bool, watch.Interface, error) { return true, nil, fail })
	old := serviceWatchDebounce
	serviceWatchDebounce = 10 * time.Millisecond
	t.Cleanup(func() { serviceWatchDebounce = old }) // runs after the watch has stopped
	h := startWith(t, client)

	waitFor(t, "the table from the lists", func() bool { return h.sink.count() == 1 })
	waitFor(t, "watch errors to be counted", func() bool { return h.c.ServiceStatus().WatchErrors > 0 })
	st := h.c.ServiceStatus()
	if !st.Synced || !strings.Contains(st.LastWatchError, "unreachable") {
		t.Errorf("status %+v", st)
	}
	if l := h.sink.last(); len(l) != 1 || len(l[0].Backends) != 1 {
		t.Errorf("the table was lost: %+v", l)
	}
	h.settle(t)
	if h.sink.count() != 1 {
		t.Errorf("%d tables", h.sink.count())
	}
}

func TestServiceStatusBeforeTheWatchStarts(t *testing.T) {
	c := NewController(fake.NewSimpleClientset(), "n")
	if st := c.ServiceStatus(); st.Synced || st.Tables != 0 {
		t.Errorf("%+v", st)
	}
}
