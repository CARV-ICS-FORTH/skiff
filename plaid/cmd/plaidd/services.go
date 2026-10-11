package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"plaid/pkg/api"
	"plaid/pkg/k8s"
	"plaid/pkg/svc"
)

// serviceAddress is the Service engine's address: the one given, or the .2 of the node
// network. It is an address of the node network that pods never get (they start at .4),
// and not the gateway's (.1).
func serviceAddress(given string, node *net.IPNet) (net.IP, error) {
	base := node.IP.Mask(node.Mask).To4()
	if base == nil {
		return nil, fmt.Errorf("node network %s is not IPv4", node)
	}
	addr := net.IPv4(base[0], base[1], base[2], base[3]+2).To4()
	if given != "" {
		addr = net.ParseIP(given).To4()
		if addr == nil {
			return nil, fmt.Errorf("invalid --snat-address %q", given)
		}
	}
	if !node.Contains(addr) {
		return nil, fmt.Errorf("--snat-address %s is outside the node network %s", addr, node)
	}
	if last := addr[3]; last != 2 && last != 3 {
		return nil, fmt.Errorf("--snat-address %s must be the .2 or .3 of the node network: .1 is the gateway and pods start at .4", addr)
	}
	return addr, nil
}

// sweepServices expires idle Service flows until the context ends.
func (d *PlaidDaemon) sweepServices(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.services.Sweep()
		}
	}
}

func (d *PlaidDaemon) HandleSetServices(req *api.Request) error {
	if d.services == nil {
		return fmt.Errorf("the Service engine only runs in tap mode")
	}
	if d.k8sCtrl != nil {
		return fmt.Errorf("the Service table is managed by the Kubernetes connector (start plaidd with --disable-kubernetes to set it yourself)")
	}
	table, err := tableFromEntries(req.Services, d.services.ServiceCIDR())
	if err != nil {
		return err
	}
	d.services.SetServices(table)
	fmt.Printf("[plaidd] Service table set: %d entries\n", len(table))
	return nil
}

func (d *PlaidDaemon) HandleListServices() (*api.Response, error) {
	if d.services == nil {
		return nil, fmt.Errorf("the Service engine only runs in tap mode")
	}
	resp := &api.Response{}
	d.fillServiceStatus(resp)
	infos := d.services.Services()
	resp.Services = make([]api.ServiceEntry, 0, len(infos))
	for _, info := range infos {
		resp.Services = append(resp.Services, api.ServiceEntry{
			ID:       info.ID,
			VIP:      info.VIP.String(),
			Port:     info.Port,
			Protocol: strings.ToLower(info.Proto.String()),
			Flows:    info.Flows,
			Backends: d.backendEntries(info),
		})
	}
	return resp, nil
}

func (d *PlaidDaemon) backendEntries(info svc.ServiceInfo) []api.BackendEntry {
	out := make([]api.BackendEntry, 0, len(info.Addresses))
	for _, b := range info.Addresses {
		out = append(out, api.BackendEntry{IP: net.IP(b.IP[:]).String(), Port: b.Port, Eligible: b.Eligible})
	}
	return out
}

// fillServiceStatus adds the engine's address, range, counters and manager to a status response.
func (d *PlaidDaemon) fillServiceStatus(resp *api.Response) {
	if d.services == nil {
		return
	}
	st := d.services.Stats()
	resp.ServiceAddress = d.services.SNATAddr().String()
	resp.ServiceCIDR = d.services.ServiceCIDR().String()
	resp.ServiceManager = "api"
	if d.k8sCtrl != nil {
		resp.ServiceManager = "kubernetes"
	}
	if d.nodePorts != nil {
		st := d.nodePorts.Status()
		np := &api.NodePortStatus{Desired: st.Desired, Exposed: st.Exposed, Passes: st.Passes, Removed: st.Removed, Repaired: st.Repaired, LastError: st.LastError}
		for _, f := range st.Failed {
			np.Failed = append(np.Failed, api.NodePortFailed{Proto: f.Proto, Port: f.Port, Reason: f.Reason, Attempts: f.Attempts})
		}
		resp.NodePorts = np
	}
	if d.k8sCtrl != nil {
		w := d.k8sCtrl.ServiceStatus()
		resp.ServiceWatch = &api.ServiceWatchStatus{Synced: w.Synced, Tables: w.Tables, Problems: w.Problems, WatchErrors: w.WatchErrors, LastWatchError: w.LastWatchError}
	}
	resp.ServiceEngine = &api.ServiceStats{
		Synced: st.Synced, Services: st.Services, Backends: st.Backends, Flows: st.Flows,
		FlowsCreated: st.FlowsCreated, FlowsExpired: st.FlowsExpired, FlowsPurged: st.FlowsPurged,
		Forwarded: st.Forwarded, Returned: st.Returned, SuperFrames: st.SuperFrames, Rejected: st.Rejected,
		DropsNoService: st.DropsNoService, DropsNoFlow: st.DropsNoFlow, DropsFlowLimit: st.DropsFlowLimit,
		DropsPorts: st.DropsPorts, DropsUnsupported: st.DropsUnsupported, RecentDrops: st.RecentDrops,
	}
}

// tableFromEntries checks a table given through the API and converts it. The whole table is
// refused for any entry that is wrong, so a half-applied table cannot happen.
func tableFromEntries(entries []api.ServiceEntry, serviceNet *net.IPNet) ([]svc.Service, error) {
	type key struct {
		vip   string
		port  uint16
		proto svc.Proto
	}
	seen := map[key]bool{}
	table := make([]svc.Service, 0, len(entries))
	for i, e := range entries {
		vip := net.ParseIP(e.VIP).To4()
		if vip == nil || !serviceNet.Contains(vip) {
			return nil, fmt.Errorf("entry %d: %q is not an address of the Service range %s", i, e.VIP, serviceNet)
		}
		if e.Port == 0 {
			return nil, fmt.Errorf("entry %d: port 0", i)
		}
		var proto svc.Proto
		switch strings.ToLower(e.Protocol) {
		case "tcp":
			proto = svc.TCP
		case "udp":
			proto = svc.UDP
		default:
			return nil, fmt.Errorf("entry %d: protocol %q is not tcp or udp", i, e.Protocol)
		}
		k := key{vip.String(), e.Port, proto}
		if seen[k] {
			return nil, fmt.Errorf("entry %d: %s:%d/%s appears twice", i, vip, e.Port, strings.ToLower(e.Protocol))
		}
		seen[k] = true
		s := svc.Service{ID: e.ID, Port: e.Port, Proto: proto}
		copy(s.VIP[:], vip)
		for j, b := range e.Backends {
			bip := net.ParseIP(b.IP).To4()
			if bip == nil || b.Port == 0 {
				return nil, fmt.Errorf("entry %d backend %d: %q:%d is not an IPv4 address and port", i, j, b.IP, b.Port)
			}
			be := svc.Backend{Port: b.Port, Eligible: b.Eligible}
			copy(be.IP[:], bip)
			s.Backends = append(s.Backends, be)
		}
		table = append(table, s)
	}
	return table, nil
}

// nodePortSink is the NodePort reconciler, or nil when NodePorts are not published.
func (d *PlaidDaemon) nodePortSink() k8s.NodePortSink {
	if d.nodePorts == nil {
		return nil
	}
	return d.nodePorts
}
