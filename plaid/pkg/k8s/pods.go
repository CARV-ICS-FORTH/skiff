package k8s

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// DirectListenPortsAnnotation lists the ports a pod's containers listen on for the direct
// path (plaid/pkg/direct). A client's supervisor needs a server pod's list to find the host
// port of a port that shares its remainder modulo 16 with a lower one of the list (see
// direct.Ports.Slots), so plaidd keeps the lists of the cluster's pods for it.
const DirectListenPortsAnnotation = "plaid.skiff.io/direct-listen-ports"

// DirectPodSink receives the listen ports of the pods that have them, by pod address, each
// time they change.
type DirectPodSink interface {
	SetDirectPods(pods map[string]string)
}

// directPodsDebounce gathers a burst of pod changes into one push.
var directPodsDebounce = 100 * time.Millisecond

// RunDirectPods watches the cluster's pods and keeps the sink's list of the pods that have
// listen ports for the direct path current until the context ends. Only the annotation and
// the addresses of a pod are kept in the informer's cache. A pod that has ended (Succeeded,
// Failed) is left out: its address may be another pod's already.
func (c *Controller) RunDirectPods(ctx context.Context, sink DirectPodSink) {
	factory := informers.NewSharedInformerFactory(c.client, 10*time.Minute)
	informer := factory.Core().V1().Pods().Informer()
	_ = informer.SetTransform(trimPod)
	kick := make(chan struct{}, 1)
	notify := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { notify() },
		UpdateFunc: func(_, _ any) { notify() },
		DeleteFunc: func(any) { notify() },
	})
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return
	}
	fmt.Printf("[plaidd/k8s] Pod cache synchronized (listen ports of the direct path)\n")
	var last map[string]string
	for first := true; ; first = false {
		pods := directPods(informer.GetStore().List())
		if first || !maps.Equal(pods, last) {
			sink.SetDirectPods(pods)
			last = pods
		}
		select {
		case <-ctx.Done():
			return
		case <-kick:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(directPodsDebounce):
		}
	}
}

// directPods is the listen ports of the pods that have them, by address.
func directPods(objs []any) map[string]string {
	pods := make(map[string]string)
	for _, o := range objs {
		p, ok := o.(*v1.Pod)
		if !ok || p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
			continue
		}
		ports := strings.TrimSpace(p.Annotations[DirectListenPortsAnnotation])
		if ports == "" {
			continue
		}
		for _, ip := range podIPs(p) {
			pods[ip] = ports
		}
	}
	return pods
}

func podIPs(p *v1.Pod) []string {
	if len(p.Status.PodIPs) == 0 && p.Status.PodIP != "" {
		return []string{p.Status.PodIP}
	}
	ips := make([]string, 0, len(p.Status.PodIPs))
	for _, ip := range p.Status.PodIPs {
		ips = append(ips, ip.IP)
	}
	return ips
}

// trimPod keeps of a pod what RunDirectPods reads, so that the cache of every pod of the
// cluster stays small.
func trimPod(o any) (any, error) {
	p, ok := o.(*v1.Pod)
	if !ok {
		return o, nil // a tombstone
	}
	t := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, UID: p.UID, ResourceVersion: p.ResourceVersion}}
	if ports, ok := p.Annotations[DirectListenPortsAnnotation]; ok {
		t.Annotations = map[string]string{DirectListenPortsAnnotation: ports}
	}
	t.Status.Phase, t.Status.PodIP, t.Status.PodIPs = p.Status.Phase, p.Status.PodIP, p.Status.PodIPs
	return t, nil
}
