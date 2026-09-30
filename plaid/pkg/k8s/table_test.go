package k8s

import (
	"fmt"
	"net"
	"testing"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"plaid/pkg/svc"
)

func service(ns, name, ip string, ports ...v1.ServicePort) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + name), ResourceVersion: "1"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeClusterIP, ClusterIP: ip, ClusterIPs: []string{ip}, Ports: ports},
	}
}

func port(name string, p int32, proto v1.Protocol) v1.ServicePort {
	return v1.ServicePort{Name: name, Port: p, Protocol: proto}
}

type ep struct {
	addr                        string
	ready, serving, terminating *bool
}

func yes() *bool { return ptr.To(true) }
func no() *bool  { return ptr.To(false) }

func slice(ns, svcName, id string, ports []discoveryv1.EndpointPort, eps ...ep) *discoveryv1.EndpointSlice {
	s := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: ns, Name: id, Labels: map[string]string{discoveryv1.LabelServiceName: svcName}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       ports,
	}
	for _, e := range eps {
		s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{
			Addresses:  []string{e.addr},
			Conditions: discoveryv1.EndpointConditions{Ready: e.ready, Serving: e.serving, Terminating: e.terminating},
		})
	}
	return s
}

func sport(name string, p int32, proto v1.Protocol) discoveryv1.EndpointPort {
	return discoveryv1.EndpointPort{Name: ptr.To(name), Port: ptr.To(p), Protocol: ptr.To(proto)}
}

func ip4(s string) [4]byte {
	var a [4]byte
	copy(a[:], net.ParseIP(s).To4())
	return a
}

// backendsOf lists "ip:port" with a marker for ineligible ones.
func backendsOf(s svc.Service) []string {
	var out []string
	for _, b := range s.Backends {
		mark := ""
		if !b.Eligible {
			mark = "!"
		}
		out = append(out, fmt.Sprintf("%s%s:%d", mark, net.IP(b.IP[:]), b.Port))
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBuildTableMapsPortsByNameAndProtocol(t *testing.T) {
	s := service("default", "app", "10.43.0.50", port("http", 80, v1.ProtocolTCP), port("dns", 53, v1.ProtocolUDP), port("dns-tcp", 53, v1.ProtocolTCP))
	sl := slice("default", "app", "app-a", []discoveryv1.EndpointPort{
		sport("http", 8080, v1.ProtocolTCP), sport("dns", 5353, v1.ProtocolUDP), sport("dns-tcp", 5353, v1.ProtocolTCP),
	}, ep{addr: "10.244.1.5", ready: yes()}, ep{addr: "10.244.2.6"}) // an unset ready condition is ready

	table, problems := BuildTable([]*v1.Service{s}, []*discoveryv1.EndpointSlice{sl})
	if len(problems) != 0 || len(table) != 3 {
		t.Fatalf("%d entries, problems %v", len(table), problems)
	}
	want := map[string][]string{
		"10.43.0.50:53/UDP": {"10.244.1.5:5353", "10.244.2.6:5353"},
		"10.43.0.50:53/TCP": {"10.244.1.5:5353", "10.244.2.6:5353"},
		"10.43.0.50:80/TCP": {"10.244.1.5:8080", "10.244.2.6:8080"},
	}
	for _, e := range table {
		k := fmt.Sprintf("%s:%d/%s", net.IP(e.VIP[:]), e.Port, e.Proto)
		if !equal(backendsOf(e), want[k]) {
			t.Errorf("%s: backends %v, want %v", k, backendsOf(e), want[k])
		}
		if e.ID == "" {
			t.Errorf("%s has no ID", k)
		}
	}
	// Entries come out in a fixed order.
	if table[0].Port != 53 || table[0].Proto != svc.TCP || table[1].Proto != svc.UDP || table[2].Port != 80 {
		t.Errorf("order %v %v %v", table[0].Port, table[1].Port, table[2].Port)
	}
}

func TestBuildTableUnnamedPortAndSlicesThatDoNotMatch(t *testing.T) {
	s := service("default", "app", "10.43.0.50", port("", 80, v1.ProtocolTCP))
	good := slice("default", "app", "good", []discoveryv1.EndpointPort{{Port: ptr.To(int32(8080))}}, ep{addr: "10.244.1.5", ready: yes()})
	others := []*discoveryv1.EndpointSlice{
		slice("default", "other", "other", []discoveryv1.EndpointPort{{Port: ptr.To(int32(8080))}}, ep{addr: "10.244.9.1", ready: yes()}),                               // another Service
		slice("elsewhere", "app", "ns", []discoveryv1.EndpointPort{{Port: ptr.To(int32(8080))}}, ep{addr: "10.244.9.2", ready: yes()}),                                  // another namespace
		slice("default", "app", "udp", []discoveryv1.EndpointPort{{Port: ptr.To(int32(8080)), Protocol: ptr.To(v1.ProtocolUDP)}}, ep{addr: "10.244.9.3", ready: yes()}), // another protocol
		slice("default", "app", "named", []discoveryv1.EndpointPort{sport("web", 8080, v1.ProtocolTCP)}, ep{addr: "10.244.9.4", ready: yes()}),                          // another port name
		func() *discoveryv1.EndpointSlice {
			s := slice("default", "app", "v6", []discoveryv1.EndpointPort{{Port: ptr.To(int32(8080))}}, ep{addr: "fd00::5", ready: yes()})
			s.AddressType = discoveryv1.AddressTypeIPv6
			return s
		}(),
		{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unlabelled"}, AddressType: discoveryv1.AddressTypeIPv4}, // no Service
	}
	table, _ := BuildTable([]*v1.Service{s}, append(others, good))
	if len(table) != 1 || !equal(backendsOf(table[0]), []string{"10.244.1.5:8080"}) {
		t.Fatalf("table %+v", table)
	}
}

func TestBuildTableMergesSlicesAndCountsAnEndpointOnce(t *testing.T) {
	s := service("default", "app", "10.43.0.50", port("", 80, v1.ProtocolTCP))
	ports := []discoveryv1.EndpointPort{{Port: ptr.To(int32(8080))}}
	a := slice("default", "app", "a", ports, ep{addr: "10.244.1.5", ready: yes()}, ep{addr: "10.244.1.6", ready: no()})
	b := slice("default", "app", "b", ports, ep{addr: "10.244.1.7", ready: yes()}, ep{addr: "10.244.1.6", ready: yes()}) // .6 moved
	table, _ := BuildTable([]*v1.Service{s}, []*discoveryv1.EndpointSlice{b, a})
	if !equal(backendsOf(table[0]), []string{"10.244.1.5:8080", "10.244.1.6:8080", "10.244.1.7:8080"}) {
		t.Errorf("backends %v", backendsOf(table[0]))
	}
}

func TestBuildTableEligibility(t *testing.T) {
	ports := []discoveryv1.EndpointPort{{Port: ptr.To(int32(8080))}}
	cases := []struct {
		name string
		pnra bool
		eps  []ep
		want []string
	}{
		{"ready and not ready", false,
			[]ep{{addr: "10.244.1.1", ready: yes()}, {addr: "10.244.1.2", ready: no()}},
			[]string{"10.244.1.1:8080", "!10.244.1.2:8080"}},
		{"unset means ready", false,
			[]ep{{addr: "10.244.1.1"}, {addr: "10.244.1.2", ready: no(), serving: no()}},
			[]string{"10.244.1.1:8080", "!10.244.1.2:8080"}},
		{"terminating next to a ready one stays ineligible", false,
			[]ep{{addr: "10.244.1.1", ready: yes()}, {addr: "10.244.1.2", ready: no(), serving: yes(), terminating: yes()}},
			[]string{"10.244.1.1:8080", "!10.244.1.2:8080"}},
		{"terminating and serving is used when nothing is ready", false,
			[]ep{{addr: "10.244.1.1", ready: no(), serving: yes(), terminating: yes()}, {addr: "10.244.1.2", ready: no(), serving: no(), terminating: yes()}, {addr: "10.244.1.3", ready: no()}},
			[]string{"10.244.1.1:8080", "!10.244.1.2:8080", "!10.244.1.3:8080"}},
		{"nothing eligible", false,
			[]ep{{addr: "10.244.1.1", ready: no()}},
			[]string{"!10.244.1.1:8080"}},
		{"publishNotReadyAddresses counts unready ones", true,
			[]ep{{addr: "10.244.1.1", ready: no()}, {addr: "10.244.1.2", ready: yes()}},
			[]string{"10.244.1.1:8080", "10.244.1.2:8080"}},
	}
	for _, c := range cases {
		s := service("default", "app", "10.43.0.50", port("", 80, v1.ProtocolTCP))
		s.Spec.PublishNotReadyAddresses = c.pnra
		table, _ := BuildTable([]*v1.Service{s}, []*discoveryv1.EndpointSlice{slice("default", "app", "a", ports, c.eps...)})
		if got := backendsOf(table[0]); !equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBuildTableFormsWithoutAnEntry(t *testing.T) {
	headless := service("default", "h", "None", port("", 80, v1.ProtocolTCP))
	external := service("default", "x", "", port("", 80, v1.ProtocolTCP))
	external.Spec.Type = v1.ServiceTypeExternalName
	pending := service("default", "p", "", port("", 80, v1.ProtocolTCP))
	table, problems := BuildTable([]*v1.Service{headless, external, pending}, nil)
	if len(table) != 0 || len(problems) != 0 {
		t.Errorf("table %+v problems %+v", table, problems)
	}
}

func TestBuildTableAddressFamilies(t *testing.T) {
	dual := service("default", "d", "fd00::10", port("", 80, v1.ProtocolTCP))
	dual.Spec.ClusterIPs = []string{"fd00::10", "10.43.0.7"}
	v6 := service("default", "v6", "fd00::11", port("", 80, v1.ProtocolTCP))
	table, problems := BuildTable([]*v1.Service{dual, v6}, nil)
	if len(table) != 1 || net.IP(table[0].VIP[:]).String() != "10.43.0.7" {
		t.Errorf("table %+v", table)
	}
	if len(problems) != 1 || problems[0].Reason != ReasonNoIPv4ClusterIP || problems[0].Service != v6 || !problems[0].Skipped {
		t.Errorf("problems %+v", problems)
	}
}

func TestBuildTableRefusesSettingsItDoesNotHonor(t *testing.T) {
	affinity := service("default", "a", "10.43.0.1", port("", 80, v1.ProtocolTCP))
	affinity.Spec.SessionAffinity = v1.ServiceAffinityClientIP
	local := service("default", "l", "10.43.0.2", port("", 80, v1.ProtocolTCP))
	local.Spec.InternalTrafficPolicy = ptr.To(v1.ServiceInternalTrafficPolicyLocal)
	cluster := service("default", "c", "10.43.0.3", port("", 80, v1.ProtocolTCP))
	cluster.Spec.InternalTrafficPolicy = ptr.To(v1.ServiceInternalTrafficPolicyCluster)
	sctp := service("default", "s", "10.43.0.4", port("sctp", 9, v1.ProtocolSCTP), port("ok", 80, v1.ProtocolTCP))
	ext := service("default", "e", "10.43.0.5", port("", 80, v1.ProtocolTCP))
	ext.Spec.ExternalIPs = []string{"203.0.113.9"}

	table, problems := BuildTable([]*v1.Service{affinity, local, cluster, sctp, ext}, nil)
	got := map[string]Problem{}
	for _, p := range problems {
		got[p.Service.Name] = p
	}
	for name, reason := range map[string]string{"a": ReasonSessionAffinity, "l": ReasonInternalPolicy, "s": ReasonProtocol, "e": ReasonExternalIPs} {
		if p, ok := got[name]; !ok || p.Reason != reason || p.Message == "" {
			t.Errorf("%s: problem %+v", name, p)
		}
	}
	if len(got) != 4 {
		t.Errorf("problems %+v", got)
	}
	if !got["a"].Skipped || !got["l"].Skipped || !got["s"].Skipped || got["e"].Skipped {
		t.Error("the wrong things were skipped")
	}
	// Served: the plain one, the SCTP Service's TCP port, and the externalIPs one (its cluster IP only).
	var served []string
	for _, e := range table {
		served = append(served, fmt.Sprintf("%s:%d", net.IP(e.VIP[:]), e.Port))
	}
	if !equal(served, []string{"10.43.0.3:80", "10.43.0.4:80", "10.43.0.5:80"}) {
		t.Errorf("served %v", served)
	}
}

// The API Service: a selectorless Service whose slice names an address that is no pod.
func TestBuildTableSelectorlessServiceWithANonPodEndpoint(t *testing.T) {
	api := service("default", "kubernetes", "10.43.0.1", port("https", 443, v1.ProtocolTCP))
	sl := slice("default", "kubernetes", "kubernetes", []discoveryv1.EndpointPort{sport("https", 6443, v1.ProtocolTCP)}, ep{addr: "192.168.64.3", ready: yes()})
	table, _ := BuildTable([]*v1.Service{api}, []*discoveryv1.EndpointSlice{sl})
	if len(table) != 1 || table[0].VIP != ip4("10.43.0.1") || table[0].Port != 443 || !equal(backendsOf(table[0]), []string{"192.168.64.3:6443"}) {
		t.Errorf("%+v", table)
	}
}

func TestBuildTableServiceIDsIncludeTheUID(t *testing.T) {
	a := service("default", "app", "10.43.0.50", port("", 80, v1.ProtocolTCP))
	b := service("default", "app", "10.43.0.50", port("", 80, v1.ProtocolTCP))
	b.UID = "another"
	ta, _ := BuildTable([]*v1.Service{a}, nil)
	tb, _ := BuildTable([]*v1.Service{b}, nil)
	if ta[0].ID == tb[0].ID {
		t.Error("a recreated Service has the ID of the old one")
	}
}

func TestBuildTableIsDeterministic(t *testing.T) {
	var services []*v1.Service
	var slices []*discoveryv1.EndpointSlice
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("s%02d", i)
		services = append(services, service("default", name, fmt.Sprintf("10.43.0.%d", 100-i), port("", 80, v1.ProtocolTCP)))
		slices = append(slices, slice("default", name, name, []discoveryv1.EndpointPort{{Port: ptr.To(int32(80))}},
			ep{addr: "10.244.1.9", ready: yes()}, ep{addr: "10.244.1.3", ready: yes()}, ep{addr: "10.244.1.5", ready: no()}))
	}
	first, _ := BuildTable(services, slices)
	for i := 0; i < 5; i++ {
		// Reversed input gives the same table.
		rs, rl := append([]*v1.Service(nil), services...), append([]*discoveryv1.EndpointSlice(nil), slices...)
		for a, b := 0, len(rs)-1; a < b; a, b = a+1, b-1 {
			rs[a], rs[b] = rs[b], rs[a]
			rl[a], rl[b] = rl[b], rl[a]
		}
		again, _ := BuildTable(rs, rl)
		if fmt.Sprint(first) != fmt.Sprint(again) {
			t.Fatal("the table depends on the order of the input")
		}
	}
}
