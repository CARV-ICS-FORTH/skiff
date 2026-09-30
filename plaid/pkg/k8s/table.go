package k8s

import (
	"fmt"
	"net"
	"sort"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"plaid/pkg/svc"
)

// Problem is something about a Service that the engine cannot do, for an event on the
// Service. Skipped says whether the Service (or the port) is left out of the table: a
// Service is never served with a setting the engine does not honor.
type Problem struct {
	Service *v1.Service
	Reason  string
	Message string
	Skipped bool
}

// Reasons of the events about Services.
const (
	ReasonSessionAffinity      = "SessionAffinityNotSupported"
	ReasonInternalPolicy       = "InternalTrafficPolicyNotSupported"
	ReasonProtocol             = "ProtocolNotSupported"
	ReasonNoIPv4ClusterIP      = "NoIPv4ClusterIP"
	ReasonExternalIPs          = "ExternalIPsNotSupported"
	serviceNameLabel           = discoveryv1.LabelServiceName
	defaultProtocol            = v1.ProtocolTCP
	unsupportedSessionAffinity = "sessionAffinity %s: Plaid does not keep a client on one backend; the Service is not served"
)

// BuildTable turns Services and EndpointSlices into the Service engine's table.
//
// What gets an entry: each TCP and UDP port of a Service with an IPv4 cluster IP, served
// with the settings Plaid honors. Headless Services and ExternalName Services have no
// cluster IP to translate and get nothing (their DNS names resolve to pod or external
// addresses directly). A Service with session affinity or an internal traffic policy of
// Local is not served at all, with a Problem: serving it as if those were not set would
// claim behavior Plaid does not have.
//
// Backends come from the slices labelled with the Service's name in its namespace, for
// the IPv4 address type, matched to the Service port by name and protocol (the slice
// states the resolved target port). An endpoint is eligible for new flows as Kubernetes
// 1.35's kube-proxy treats it: when it is ready (an unset ready condition means ready,
// and a Service that publishes not-ready addresses counts all of them); and when no
// endpoint is ready, those that are serving while terminating, so a rolling update does
// not leave a Service without backends. Every other endpoint is kept in the entry as
// ineligible: flows already pinned to it continue. An endpoint listed in more than one
// slice counts once, and is ready if any listing says so.
func BuildTable(services []*v1.Service, slices []*discoveryv1.EndpointSlice) ([]svc.Service, []Problem) {
	type key struct{ ns, name string }
	byService := map[key][]*discoveryv1.EndpointSlice{}
	for _, s := range slices {
		if s.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		if name := s.Labels[serviceNameLabel]; name != "" {
			k := key{s.Namespace, name}
			byService[k] = append(byService[k], s)
		}
	}

	var table []svc.Service
	var problems []Problem
	for _, s := range services {
		if s.Spec.Type == v1.ServiceTypeExternalName || s.Spec.ClusterIP == "" || s.Spec.ClusterIP == v1.ClusterIPNone {
			continue
		}
		vip := clusterIPv4(s)
		if vip == nil {
			problems = append(problems, Problem{Service: s, Reason: ReasonNoIPv4ClusterIP, Skipped: true,
				Message: fmt.Sprintf("the Service has cluster IPs %v and none is IPv4: Plaid serves IPv4 only", s.Spec.ClusterIPs)})
			continue
		}
		if s.Spec.SessionAffinity == v1.ServiceAffinityClientIP {
			problems = append(problems, Problem{Service: s, Reason: ReasonSessionAffinity, Skipped: true,
				Message: fmt.Sprintf(unsupportedSessionAffinity, s.Spec.SessionAffinity)})
			continue
		}
		if p := s.Spec.InternalTrafficPolicy; p != nil && *p == v1.ServiceInternalTrafficPolicyLocal {
			problems = append(problems, Problem{Service: s, Reason: ReasonInternalPolicy, Skipped: true,
				Message: "internalTrafficPolicy Local: Plaid does not restrict a Service to the node's own backends; the Service is not served"})
			continue
		}
		if len(s.Spec.ExternalIPs) > 0 {
			problems = append(problems, Problem{Service: s, Reason: ReasonExternalIPs,
				Message: "externalIPs are not served; the cluster IP is"})
		}
		for _, port := range s.Spec.Ports {
			proto := port.Protocol
			if proto == "" {
				proto = defaultProtocol
			}
			var p svc.Proto
			switch proto {
			case v1.ProtocolTCP:
				p = svc.TCP
			case v1.ProtocolUDP:
				p = svc.UDP
			default:
				problems = append(problems, Problem{Service: s, Reason: ReasonProtocol, Skipped: true,
					Message: fmt.Sprintf("port %d uses protocol %s: Plaid serves TCP and UDP; the port is not served", port.Port, proto)})
				continue
			}
			entry := svc.Service{
				ID:       fmt.Sprintf("%s/%s/%s:%d/%s", s.Namespace, s.Name, s.UID, port.Port, lower(proto)),
				Port:     uint16(port.Port),
				Proto:    p,
				Backends: backends(s, port, proto, byService[key{s.Namespace, s.Name}]),
			}
			copy(entry.VIP[:], vip)
			table = append(table, entry)
		}
	}
	sort.Slice(table, func(i, j int) bool {
		a, b := table[i], table[j]
		if c := compare4(a.VIP, b.VIP); c != 0 {
			return c < 0
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Proto < b.Proto
	})
	return table, problems
}

func lower(p v1.Protocol) string {
	if p == v1.ProtocolUDP {
		return "udp"
	}
	return "tcp"
}

func compare4(a, b [4]byte) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// clusterIPv4 is the Service's IPv4 cluster IP, or nil.
func clusterIPv4(s *v1.Service) net.IP {
	candidates := append([]string{s.Spec.ClusterIP}, s.Spec.ClusterIPs...)
	for _, c := range candidates {
		if ip := net.ParseIP(c).To4(); ip != nil {
			return ip
		}
	}
	return nil
}

type endpointState struct {
	ready, serving, terminating bool
}

func backends(s *v1.Service, port v1.ServicePort, proto v1.Protocol, slices []*discoveryv1.EndpointSlice) []svc.Backend {
	type ep struct {
		ip   [4]byte
		port uint16
	}
	states := map[ep]*endpointState{}
	var order []ep
	for _, slice := range slices {
		slicePort, ok := matchingPort(slice, port.Name, proto)
		if !ok {
			continue
		}
		for _, e := range slice.Endpoints {
			addr := firstIPv4(e.Addresses)
			if addr == nil {
				continue
			}
			c := e.Conditions
			ready := c.Ready == nil || *c.Ready
			serving := ready
			if c.Serving != nil {
				serving = *c.Serving
			}
			terminating := c.Terminating != nil && *c.Terminating
			k := ep{port: slicePort}
			copy(k.ip[:], addr)
			if st, seen := states[k]; seen {
				st.ready = st.ready || ready
				st.serving = st.serving || serving
				st.terminating = st.terminating && terminating
				continue
			}
			states[k] = &endpointState{ready: ready, serving: serving, terminating: terminating}
			order = append(order, k)
		}
	}
	anyReady := false
	for _, st := range states {
		if st.ready || s.Spec.PublishNotReadyAddresses {
			anyReady = true
			break
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if c := compare4(order[i].ip, order[j].ip); c != 0 {
			return c < 0
		}
		return order[i].port < order[j].port
	})
	out := make([]svc.Backend, 0, len(order))
	for _, k := range order {
		st := states[k]
		eligible := st.ready || s.Spec.PublishNotReadyAddresses
		if !anyReady && st.serving && st.terminating {
			eligible = true
		}
		out = append(out, svc.Backend{IP: k.ip, Port: k.port, Eligible: eligible})
	}
	return out
}

// matchingPort finds the slice's entry for a Service port (by name and protocol) and
// returns the port number the endpoints listen on.
func matchingPort(slice *discoveryv1.EndpointSlice, name string, proto v1.Protocol) (uint16, bool) {
	for _, p := range slice.Ports {
		pn := ""
		if p.Name != nil {
			pn = *p.Name
		}
		pp := defaultProtocol
		if p.Protocol != nil {
			pp = *p.Protocol
		}
		if pn == name && pp == proto && p.Port != nil && *p.Port > 0 && *p.Port < 65536 {
			return uint16(*p.Port), true
		}
	}
	return 0, false
}

func firstIPv4(addresses []string) net.IP {
	for _, a := range addresses {
		if ip := net.ParseIP(a).To4(); ip != nil {
			return ip
		}
	}
	return nil
}
