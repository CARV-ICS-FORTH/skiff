package k8s

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"plaid/pkg/nodeport"
)

func nodePortService(name, ip string, ports ...v1.ServicePort) *v1.Service {
	s := service("default", name, ip, ports...)
	s.Spec.Type = v1.ServiceTypeNodePort
	return s
}

func np(name string, port, nodePort int32, proto v1.Protocol) v1.ServicePort {
	return v1.ServicePort{Name: name, Port: port, NodePort: nodePort, Protocol: proto}
}

func describe(m []nodeport.Mapping) string {
	var out []string
	for _, x := range m {
		out = append(out, fmt.Sprintf("%s/%d->%d.%d.%d.%d:%d", x.Proto, x.Port, x.ClusterIP[0], x.ClusterIP[1], x.ClusterIP[2], x.ClusterIP[3], x.ServicePort))
	}
	return strings.Join(out, " ")
}

func TestBuildNodePortsPublishesTheNodePortsOfServedNodePortServices(t *testing.T) {
	web := nodePortService("web", "10.43.0.50", np("http", 80, 31080, v1.ProtocolTCP), np("dns", 53, 31053, v1.ProtocolUDP), np("tls", 443, 31443, ""))
	plain := service("default", "plain", "10.43.0.51", port("", 80, v1.ProtocolTCP)) // ClusterIP only
	unassigned := nodePortService("zero", "10.43.0.52", np("x", 80, 0, v1.ProtocolTCP))
	sctp := nodePortService("sctp", "10.43.0.53", np("s", 9, 31009, v1.ProtocolSCTP), np("ok", 80, 31081, v1.ProtocolTCP))

	got, problems := BuildNodePorts([]*v1.Service{web, plain, unassigned, sctp})
	if len(problems) != 0 {
		t.Errorf("problems %+v", problems)
	}
	want := "udp/31053->10.43.0.50:53 tcp/31080->10.43.0.50:80 tcp/31081->10.43.0.53:80 tcp/31443->10.43.0.50:443"
	if describe(got) != want {
		t.Errorf("mappings:\n%s\nwant:\n%s", describe(got), want)
	}
	for _, m := range got {
		if m.Ref == nil {
			t.Error("a mapping without its Service")
		}
	}
}

func TestBuildNodePortsRefusesWhatItCannotHonor(t *testing.T) {
	local := nodePortService("local", "10.43.0.60", np("", 80, 31100, v1.ProtocolTCP))
	local.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	cluster := nodePortService("cluster", "10.43.0.61", np("", 80, 31101, v1.ProtocolTCP))
	cluster.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyCluster
	lb := nodePortService("lb", "10.43.0.62", np("", 80, 31102, v1.ProtocolTCP))
	lb.Spec.Type = v1.ServiceTypeLoadBalancer
	sticky := nodePortService("sticky", "10.43.0.63", np("", 80, 31103, v1.ProtocolTCP))
	sticky.Spec.SessionAffinity = v1.ServiceAffinityClientIP
	internal := nodePortService("internal", "10.43.0.64", np("", 80, 31104, v1.ProtocolTCP))
	internal.Spec.InternalTrafficPolicy = ptr.To(v1.ServiceInternalTrafficPolicyLocal)

	got, problems := BuildNodePorts([]*v1.Service{local, cluster, lb, sticky, internal})
	if describe(got) != "tcp/31101->10.43.0.61:80" {
		t.Errorf("mappings %s", describe(got))
	}
	reasons := map[string]string{}
	for _, p := range problems {
		reasons[p.Service.Name] = p.Reason
		if !p.Skipped || p.Message == "" {
			t.Errorf("problem %+v", p)
		}
	}
	// Session affinity and the internal policy were reported by the table builder, not again here.
	if len(reasons) != 2 || reasons["local"] != ReasonExternalPolicy || reasons["lb"] != ReasonLoadBalancer {
		t.Errorf("problems %v", reasons)
	}
}

type npSink struct {
	mu   sync.Mutex
	sets [][]nodeport.Mapping
}

func (s *npSink) SetDesired(m []nodeport.Mapping) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets = append(s.sets, m)
}

func (s *npSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.sets) }
func (s *npSink) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sets) == 0 {
		return "<none>"
	}
	return describe(s.sets[len(s.sets)-1])
}

func startWithNodePorts(t *testing.T, objs ...runtime.Object) (*harness, *npSink) {
	t.Helper()
	old := serviceWatchDebounce
	serviceWatchDebounce = 10 * time.Millisecond
	t.Cleanup(func() { serviceWatchDebounce = old })
	client := fake.NewSimpleClientset(objs...)
	h := &harness{c: NewController(client, "node-a"), sink: &sink{}, recorder: record.NewFakeRecorder(100), client: client, done: make(chan struct{})}
	sinkNP := &npSink{}
	h.c.SetEventRecorder(h.recorder)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.c.RunServices(ctx, h.sink, sinkNP); close(h.done) }()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	return h, sinkNP
}

func TestTheWatchPushesNodePortsWhenTheyChange(t *testing.T) {
	web := nodePortService("web", "10.43.0.50", np("http", 80, 31080, v1.ProtocolTCP))
	lb := nodePortService("lb", "10.43.0.62", np("", 80, 31102, v1.ProtocolTCP))
	lb.Spec.Type = v1.ServiceTypeLoadBalancer
	h, sink := startWithNodePorts(t, web, lb)
	waitFor(t, "the first set", func() bool { return sink.count() >= 1 })
	h.settle(t)
	if sink.count() != 1 || sink.last() != "tcp/31080->10.43.0.50:80" {
		t.Fatalf("%d sets, the last %s", sink.count(), sink.last())
	}
	if e := <-h.recorder.Events; !strings.HasPrefix(e, "Warning "+ReasonLoadBalancer) {
		t.Errorf("event %q", e)
	}

	ctx := context.Background()
	// A label does not change what is published; a new node port does.
	labelled := web.DeepCopy()
	labelled.Labels = map[string]string{"a": "b"}
	if _, err := h.client.CoreV1().Services("default").Update(ctx, labelled, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.settle(t)
	if sink.count() != 1 {
		t.Errorf("a label change pushed %d sets", sink.count()-1)
	}
	more := labelled.DeepCopy()
	more.Spec.Ports = append(more.Spec.Ports, np("alt", 81, 31081, v1.ProtocolTCP))
	if _, err := h.client.CoreV1().Services("default").Update(ctx, more, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second port", func() bool { return sink.last() == "tcp/31080->10.43.0.50:80 tcp/31081->10.43.0.50:81" })

	// Changing the type to ClusterIP withdraws them; deleting does too.
	plain := more.DeepCopy()
	plain.Spec.Type = v1.ServiceTypeClusterIP
	for i := range plain.Spec.Ports {
		plain.Spec.Ports[i].NodePort = 0
	}
	if _, err := h.client.CoreV1().Services("default").Update(ctx, plain, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the withdrawal", func() bool { return sink.last() == "" })
}
