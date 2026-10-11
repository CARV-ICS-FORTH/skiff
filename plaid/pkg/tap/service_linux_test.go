//go:build linux

package tap

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"plaid/pkg/bridge"
	"plaid/pkg/svc"
)

// TestServiceStreamBetweenTwoPods is TestOffloadsBetweenTwoPods through a Service: the
// first pod's stack connects to a cluster IP, whose backend is the second pod, and sends
// 16 MiB, with a real kernel's TCP on both ends, which verify every checksum they are
// given. The bridge is in tap mode with a Service engine, and the pods reach the engine
// as they do in a cluster: the first through its default route, the second answering the
// engine's address on its own subnet. The modes:
//
//	whole    both TAPs have offloads and passthrough: the engine translates the pod's TSO
//	         segments as they are, and they reach the backend whole
//	pipeline the same, with the pipelines
//	cut      the backend's TAP has no offloads: the engine translates the segments whole
//	         and the bridge cuts them after
//	off      passthrough is off: the bridge cuts the segments before the engine
//
// The counters say which of these happened. As in the other test, it runs in a user and
// network namespace of its own. What it cannot show is that the checksum field of a
// translated TSO segment is right: a receiving stack completes a checksum it is told to
// complete, whatever the field holds, so the engine and bridge tests check the field,
// completing it and verifying the result independently.
func TestServiceStreamBetweenTwoPods(t *testing.T) {
	if mode := os.Getenv("PLAID_TAP_SVC_CHILD"); mode != "" {
		servicePods(t, mode)
		return
	}
	needUserns(t)
	for _, mode := range []string{"whole", "pipeline", "cut", "off"} {
		t.Run(mode, func(t *testing.T) {
			runInUserns(t, "TestServiceStreamBetweenTwoPods", "PLAID_TAP_SVC_CHILD="+mode, time.Minute)
		})
	}
}

func servicePods(t *testing.T, mode string) {
	const vip, vipPort = "10.43.0.50", 80
	podCfg := func(n byte, gateway string) TapConfig {
		return TapConfig{Name: "pod0", IP: net.IPv4(10, 99, 0, n), Mask: net.CIDRMask(24, 32), Gateway: net.ParseIP(gateway), MAC: net.HardwareAddr{2, 0, 0, 0, 0, n}, MTU: 1450}
	}
	server, client := newPod(t, podCfg(3, ""), false), newPod(t, podCfg(2, "10.99.0.1"), false)

	_, nodeCIDR, _ := net.ParseCIDR("10.99.0.0/24")
	_, clusterCIDR, _ := net.ParseCIDR("10.99.0.0/16")
	_, serviceCIDR, _ := net.ParseCIDR("10.43.0.0/16")
	gwMAC, _ := net.ParseMAC("02:00:00:00:00:01")
	b := bridge.NewBridge(bridge.BridgeConfig{GatewayMode: "tap", GatewayIP: net.ParseIP("10.99.0.1"), GatewayMAC: gwMAC, NodeCIDR: nodeCIDR, ClusterCIDR: clusterCIDR})
	eng, err := svc.New(svc.Config{ServiceCIDR: serviceCIDR, SNATAddr: net.ParseIP("10.99.0.200")})
	if err != nil {
		t.Fatal(err)
	}
	eng.SetServices([]svc.Service{{
		ID: "stream", VIP: [4]byte{10, 43, 0, 50}, Port: vipPort, Proto: svc.TCP,
		Backends: []svc.Backend{{IP: [4]byte{10, 99, 0, 3}, Port: podPort, Eligible: true}},
	}})
	b.SetServiceEngine(eng)

	for _, spec := range []struct {
		id      string
		n       byte
		pod     *nsPod
		offload bool
	}{
		{"server", 3, server, mode != "cut"},
		{"client", 2, client, true},
	} {
		cfg := podCfg(spec.n, "")
		ep := NewTapEndpoint(spec.id, spec.id, cfg.IP, cfg.MAC, spec.pod.tap, spec.offload)
		if !ep.vnet || ep.Offload() != spec.offload {
			t.Skipf("this kernel does not give the TAP a virtio header or offloads (vnet %v, offload %v)", ep.vnet, ep.Offload())
		}
		if mode == "pipeline" {
			ep.SetPipeline()
		}
		if mode != "off" {
			ep.SetPassthrough()
		}
		if err := b.AddEndpoint(ep); err != nil {
			t.Fatal(err)
		}
		ep.StartReadLoop(context.Background(), b)
	}

	runStream(t, "stream through the Service",
		func() streamResult { return serve(server.lfd) },
		func() streamResult { return send(client.cfd, vip, vipPort) },
		func() string { return fmt.Sprintf("counters %+v, engine %+v", Offloads(), eng.Stats()) })
	st, es := Offloads(), eng.Stats()
	t.Logf("TAP offloads: %+v", st)
	t.Logf("engine: %d flows, %d packets out, %d back, %d super-frames", es.FlowsCreated, es.Forwarded, es.Returned, es.SuperFrames)
	if es.FlowsCreated != 1 || es.DropsNoFlow != 0 || es.DropsUnsupported != 0 || st.ReadDropped != 0 {
		t.Errorf("engine %+v, counters %+v", es, st)
	}
	switch mode {
	case "whole", "pipeline":
		// Every TSO segment was translated as it is and written whole: none was cut, here or after.
		if es.SuperFrames == 0 || es.SuperFrames != st.PassedSuperFrames || st.ReadSuperFrames != 0 || st.WriteSuperFrames != 0 {
			t.Errorf("TSO segments were not all translated and passed whole: engine %+v, counters %+v", es, st)
		}
	case "cut":
		// Translated whole, cut after (the TAP has no offloads): none was passed.
		if es.SuperFrames == 0 || st.PassedSuperFrames != 0 || st.ReadSuperFrames != 0 {
			t.Errorf("TSO segments were not translated whole and cut after: engine %+v, counters %+v", es, st)
		}
	case "off":
		if es.SuperFrames != 0 || st.PassedSuperFrames != 0 || st.ReadSuperFrames == 0 {
			t.Errorf("TSO segments were not cut before the engine: engine %+v, counters %+v", es, st)
		}
	}
}
