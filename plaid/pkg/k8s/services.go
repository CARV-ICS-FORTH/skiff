package k8s

import (
	"context"
	"fmt"
	"reflect"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"plaid/pkg/svc"
)

// ServiceSink receives the complete Service table each time it changes. It is the Service
// engine; the connector does not know more of it than that.
type ServiceSink interface {
	SetServices(table []svc.Service)
}

// ServiceStatus says how the Service watch is doing: whether the first table was built
// from complete caches, how many tables were pushed, and whether the watches have been
// failing (the table then stays as it was: stale, not empty).
type ServiceStatus struct {
	Synced         bool
	Tables         uint64
	Services       int
	Problems       int
	WatchErrors    uint64
	LastWatchError string
}

// serviceWatch is the state of RunServices.
type serviceWatch struct {
	c        *Controller
	sink     ServiceSink
	recorder record.EventRecorder

	synced      atomic.Bool
	tables      atomic.Uint64
	services    atomic.Int64
	problems    atomic.Int64
	watchErrors atomic.Uint64
	lastErr     atomic.Value // string

	svcStore, sliceStore cache.Store
	reported             map[string]bool
	last                 []svc.Service // the table last pushed (only the watch goroutine uses it)
}

// serviceWatchDebounce gathers a burst of changes (a rollout touches many slices) into one table.
var serviceWatchDebounce = 100 * time.Millisecond

// SetEventRecorder replaces the recorder of the events about Services (tests).
func (c *Controller) SetEventRecorder(r record.EventRecorder) { c.recorder = r }

// ServiceStatus reports on the Service watch.
func (c *Controller) ServiceStatus() ServiceStatus {
	w := c.watch.Load()
	if w == nil {
		return ServiceStatus{}
	}
	st := ServiceStatus{
		Synced:      w.synced.Load(),
		Tables:      w.tables.Load(),
		Services:    int(w.services.Load()),
		Problems:    int(w.problems.Load()),
		WatchErrors: w.watchErrors.Load(),
	}
	if e, ok := w.lastErr.Load().(string); ok {
		st.LastWatchError = e
	}
	return st
}

// RunServices watches Services and EndpointSlices and keeps the sink's table current until
// the context ends. The first table is built once both caches are complete, so the sink
// never sees a partial one; after that, a table is pushed whenever it differs from the
// last. A watch that fails leaves the last table in place (and is counted), and the
// informers' relist brings the cache, and so the table, back in line.
func (c *Controller) RunServices(ctx context.Context, sink ServiceSink) {
	w := &serviceWatch{c: c, sink: sink, recorder: c.recorder, reported: map[string]bool{}}
	c.watch.Store(w)
	if w.recorder == nil {
		broadcaster := record.NewBroadcaster()
		broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: c.client.CoreV1().Events("")})
		defer broadcaster.Shutdown()
		w.recorder = broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "plaid-proxy", Host: c.nodeName})
	}

	factory := informers.NewSharedInformerFactory(c.client, 10*time.Minute)
	svcInformer := factory.Core().V1().Services().Informer()
	sliceInformer := factory.Discovery().V1().EndpointSlices().Informer()
	w.svcStore, w.sliceStore = svcInformer.GetStore(), sliceInformer.GetStore()

	kick := make(chan struct{}, 1)
	notify := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { notify() },
		UpdateFunc: func(_, _ any) { notify() },
		DeleteFunc: func(any) { notify() },
	}
	for _, inf := range []cache.SharedIndexInformer{svcInformer, sliceInformer} {
		_, _ = inf.AddEventHandler(handler)
		_ = inf.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
			w.watchErrors.Add(1)
			w.lastErr.Store(err.Error())
			cache.DefaultWatchErrorHandler(ctx, r, err)
		})
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), svcInformer.HasSynced, sliceInformer.HasSynced) {
		return
	}
	fmt.Printf("[plaidd/k8s] Service and EndpointSlice caches synchronized\n")
	w.rebuild(true)
	w.synced.Store(true)

	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		}
		// Let a burst settle, then build once.
		select {
		case <-ctx.Done():
			return
		case <-time.After(serviceWatchDebounce):
		}
		select {
		case <-kick:
		default:
		}
		w.rebuild(false)
	}
}

func (w *serviceWatch) rebuild(first bool) {
	var services []*v1.Service
	for _, o := range w.svcStore.List() {
		if s, ok := o.(*v1.Service); ok {
			services = append(services, s)
		}
	}
	var slices []*discoveryv1.EndpointSlice
	for _, o := range w.sliceStore.List() {
		if s, ok := o.(*discoveryv1.EndpointSlice); ok {
			slices = append(slices, s)
		}
	}
	table, problems := BuildTable(services, slices)
	w.services.Store(int64(len(table)))
	w.problems.Store(int64(len(problems)))
	w.report(problems)

	if !first && reflect.DeepEqual(w.last, table) {
		return
	}
	w.last = table
	w.sink.SetServices(table)
	w.tables.Add(1)
}

// report records an event for each problem once per version of its Service, and forgets
// the Services that are gone.
func (w *serviceWatch) report(problems []Problem) {
	live := map[string]bool{}
	for _, p := range problems {
		key := fmt.Sprintf("%s/%s/%s", p.Service.UID, p.Reason, p.Service.ResourceVersion)
		live[key] = true
		if w.reported[key] {
			continue
		}
		w.reported[key] = true
		w.recorder.Event(p.Service, v1.EventTypeWarning, p.Reason, p.Message)
	}
	for key := range w.reported {
		if !live[key] {
			delete(w.reported, key)
		}
	}
}
