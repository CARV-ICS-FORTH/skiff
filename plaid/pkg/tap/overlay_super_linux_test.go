//go:build linux

package tap

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"plaid/pkg/bridge"
	"plaid/pkg/packet"
	"plaid/pkg/vxlan"
)

// ovNode is a node of the overlay tests: a bridge with the TAP of one pod, 10.244.N.4, an
// overlay engine, and the routes to the other node.
type ovNode struct {
	n      int
	br     *bridge.Bridge
	oe     *vxlan.OverlayEngine
	pod    *TapEndpoint
	routes *vxlan.RouteTable
	gwMAC  net.HardwareAddr
	under  net.IP // the node's address on the network between the nodes
}

type bridgeInjector struct{ *bridge.Bridge }

func (i bridgeInjector) NewBatch() vxlan.InjectBatch { return i.Bridge.NewBatch() }

// newOvNode wires the pod's TAP, the bridge and an engine, whose sockets attach makes.
// The engine is not told of any route yet (see routeTo).
func newOvNode(t testing.TB, n int, pod *TapEndpoint, under string, cfg vxlan.OverlayConfig, attach func(oe *vxlan.OverlayEngine) error) *ovNode {
	t.Helper()
	nd := &ovNode{n: n, pod: pod, routes: vxlan.NewRouteTable(), gwMAC: net.HardwareAddr{2, 0, 0, 0, byte(n), 1}, under: net.ParseIP(under)}
	nodeCIDR := &net.IPNet{IP: net.IPv4(10, 244, byte(n), 0), Mask: net.CIDRMask(24, 32)}
	nd.br = bridge.NewBridge(bridge.BridgeConfig{
		GatewayMode: "tap", GatewayIP: net.IPv4(10, 244, byte(n), 1), GatewayMAC: nd.gwMAC,
		NodeCIDR: nodeCIDR, ClusterCIDR: &net.IPNet{IP: net.IPv4(10, 244, 0, 0), Mask: net.CIDRMask(16, 32)},
	})
	cfg.BindAddress, cfg.DefaultVNI = under, 1
	nd.oe = vxlan.NewOverlayEngine(cfg, nd.routes, bridgeInjector{nd.br})
	nd.br.SetOverlayBatcher(func() bridge.OverlayBatch { return nd.oe.NewBatch() })
	if err := nd.br.AddEndpoint(pod); err != nil {
		t.Fatal(err)
	}
	if err := attach(nd.oe); err != nil {
		t.Skipf("cannot start an engine on %s: %v", under, err)
	}
	t.Cleanup(func() { _ = nd.oe.Close(); _ = pod.Close() })
	return nd
}

// routeTo makes the pods of o reachable through its engine.
func (nd *ovNode) routeTo(o *ovNode) {
	nd.routes.AddRoute(&vxlan.RouteEntry{
		Subnet:       &net.IPNet{IP: net.IPv4(10, 244, byte(o.n), 0), Mask: net.CIDRMask(24, 32)},
		RemoteHostIP: o.under, VtepMAC: o.gwMAC, VNI: 1, Port: o.oe.LocalPort(),
	})
}

func (nd *ovNode) start() { nd.pod.StartReadLoop(context.Background(), nd.br) }

// ovPod is the TAP of pod 10.244.N.4 of node N: a record socket as the pipeline tests have
// them, with offloads or without, and passthrough.
func ovPod(t testing.TB, n int, offload, pipeline bool) (*TapEndpoint, *os.File) {
	t.Helper()
	ep, pod := socketTap(t, tapOpts{
		name: "pod", ip: net.IPv4(10, 244, byte(n), 4), mac: net.HardwareAddr{2, 0, 0, 0, 0, byte(4 + 10*(n-1))},
		vnet: true, offload: offload, pipeline: pipeline,
	})
	ep.SetPassthrough()
	return ep, pod
}

// retarget changes a frame of the pipeline tests (a TCP segment from pod 1.4 to 1.5) to go
// from the pod src to dst with the MAC addresses given, and makes its checksums right.
func retarget(f *packet.EthernetFrame, srcIP, dstIP net.IP, srcMAC, dstMAC net.HardwareAddr) {
	f.SrcMAC, f.DstMAC = srcMAC, dstMAC
	copy(f.Payload[12:], srcIP.To4())
	copy(f.Payload[16:], dstIP.To4())
	sumFrame(f.Payload)
}

// ackFromB is the record of the answer that pod 2.4 sends to pod 1.4: a TCP segment
// without data, which carries the mark of its node over the overlay.
func ackFromB(nd *ovNode) []byte {
	f := tcpFrame(1, 0, 0x10)
	retarget(f, net.IPv4(10, 244, 2, 4), net.IPv4(10, 244, 1, 4), nd.pod.MAC(), nd.gwMAC)
	return append(make([]byte, packet.VnetHeaderLen), mustMarshal(f)...)
}

// drain reads and discards what arrives at the pod's end until the test ends.
func drain(t testing.TB, pod *os.File) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		buf := make([]byte, 70000)
		for {
			_ = pod.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			if _, err := pod.Read(buf); err != nil {
				select {
				case <-done:
					return
				default:
				}
			}
		}
	}()
}

// A stream from a pod to a pod on another node, written as the pod's stack writes it: now
// a single frame, now a TSO segment of many, arrives whole and in order, whether the
// super-frames go whole over the overlay (the node announced it takes them) or are cut
// (before it has, or with the feature off at either end), and whether the receiving pod
// takes them whole or is given the segments, with the pipelines or without.
func TestOverlaySuperFramesKeepOrderWithCutFrames(t *testing.T) {
	type mode struct {
		name                     string
		pipeline, podOffload     bool
		senderSuper, receiverOn  bool
		wantWhole, wantReceiving bool
	}
	for _, m := range []mode{
		{"whole", false, true, true, true, true, true},
		{"whole with pipelines", true, true, true, true, true, true},
		{"into a pod without offloads", false, false, true, true, true, true},
		{"into a pod without offloads, pipelines", true, false, true, true, true, true},
		{"sender without the feature", true, true, false, true, false, false},
		{"receiver without the feature", true, true, true, false, false, false},
	} {
		t.Run(m.name, func(t *testing.T) {
			podA, aPod := ovPod(t, 1, true, m.pipeline)
			podB, bPod := ovPod(t, 2, m.podOffload, m.pipeline)
			sock := func(oe *vxlan.OverlayEngine) error { return oe.Start(context.Background()) }
			a := newOvNode(t, 1, podA, "127.0.0.1", vxlan.OverlayConfig{Offload: true, Super: m.senderSuper, RcvBuf: 4 << 20}, sock)
			b := newOvNode(t, 2, podB, "127.0.0.2", vxlan.OverlayConfig{Offload: true, Super: m.receiverOn, RcvBuf: 4 << 20}, sock)
			if !a.oe.Stats().SegmentOffload {
				t.Skip("this kernel has no UDP_GRO, which UDP_SEGMENT goes with")
			}
			a.routeTo(b)
			b.routeTo(a)
			a.start()
			b.start()
			drain(t, aPod)

			// The answers of pod B, which the node's mark goes with. They start when the stream
			// is under way, so that its first super-frames are cut and the rest go whole.
			stop, start := make(chan struct{}), make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case <-start:
				case <-stop:
					return
				}
				for {
					if _, err := bPod.Write(ackFromB(b)); err != nil {
						return
					}
					select {
					case <-stop:
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
			}()
			stopAnswers := func() { close(stop); wg.Wait() }
			defer func() {
				select {
				case <-stop:
				default:
					stopAnswers()
				}
			}()

			sizes := []int{1, 11, 1, 1, 30, 5, 1, 21, 1, 40, 2, 1}
			const rounds = 25
			total, groups := 0, rounds*len(sizes)
			for g := 0; g < groups; g++ {
				total += sizes[g%len(sizes)]
			}
			go func() {
				seq := uint32(1000)
				for g := 0; g < groups; g++ {
					if g == 3*len(sizes) {
						close(start)
					}
					n := sizes[g%len(sizes)]
					if _, err := aPod.Write(podRecordTo(seq, n, a.gwMAC, net.IPv4(10, 244, 2, 4))); err != nil {
						return
					}
					seq += uint32(n * segSize)
					if g%9 == 0 {
						time.Sleep(300 * time.Microsecond)
					}
					if g == 3*len(sizes) {
						time.Sleep(10 * time.Millisecond) // the first answer reaches the sender
					}
				}
			}()
			before := Offloads()
			readSegments(t, bPod, 1000, total, false)
			d := delta(Offloads(), before)
			sa, sb := a.oe.Stats(), b.oe.Stats()
			t.Logf("%d segments; sender: %d TSO segments cut, %d super-frames sent in %d datagrams; receiver: %d super-frames, %d joined writes", total, d.ReadSuperFrames, sa.SuperTx, sa.SuperTxChunks, sb.SuperRx, d.WriteSuperFrames)
			if sa.TxErrors != 0 || sb.SuperPartial != 0 || sb.SuperInvalid != 0 || sb.RxInvalid != 0 {
				t.Errorf("sender %+v, receiver %+v", sa, sb)
			}
			if m.wantWhole {
				if sa.SuperTx == 0 || sb.SuperRx != sa.SuperTx {
					t.Errorf("super-frames sent %d, received %d", sa.SuperTx, sb.SuperRx)
				}
				if d.ReadSuperFrames == 0 {
					t.Error("nothing was cut: the test did not mix whole and cut super-frames (the first ones go before the node is known to take them)")
				}
			} else if sa.SuperTx != 0 || sb.SuperRx != 0 || d.ReadSuperFrames == 0 {
				t.Errorf("super-frames were sent whole (%d, %d), or none was cut (%d)", sa.SuperTx, sb.SuperRx, d.ReadSuperFrames)
			}
			stopAnswers()
			_ = a.oe.Close() // nothing more arrives; the read loops end with the TAPs and give their batches back
			_ = b.oe.Close()
			_, _ = podA.Close(), podB.Close()
			waitUntil(t, "the buffers of the pipelines to be released", func() bool { return inFlight.Load() == 0 })
		})
	}
}

// -- two nodes with real kernels

// ovScenario is TestOverlaySuperFramesWithRealKernels's child: it runs in user and network
// namespaces of its own and makes five network namespaces in them, two for the nodes, joined
// by a veth pair whose MTU is the pods' plus 50, and one for each pod, with a TAP in it,
// served by the bridge of its node. A pod's stack sends 16 MiB to the other.
//
//	whole      both nodes have the feature
//	pipeline   the same, with the pipelines
//	nooffload  the receiving pod's TAP has no offloads: the frames are cut on receipt
//	recv-off   the receiving node has not: it does not announce, and the sender cuts
//	send-off   the sending node has not: it does not announce either, and cuts
func ovScenario(t *testing.T, podMTU int, mode string) {
	var nodes [2]*netns
	for i := range nodes {
		n, err := newNetns()
		if err != nil {
			t.Skipf("cannot create a network namespace: %v", err)
		}
		defer n.close()
		nodes[i] = n
	}
	nodeA, nodeB := nodes[0], nodes[1]
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	linkMTU := strconv.Itoa(podMTU + 50)
	must("veth", ipCmds(
		[]string{"link", "add", "va", "type", "veth", "peer", "name", "vb"},
		[]string{"link", "set", "va", "netns", strconv.Itoa(nodeA.tid)},
		[]string{"link", "set", "vb", "netns", strconv.Itoa(nodeB.tid)},
	))
	var sockets [2]struct{ rx, tx []*net.UDPConn }
	for i, n := range []*netns{nodeA, nodeB} {
		i, dev, addr := i, []string{"va", "vb"}[i], fmt.Sprintf("192.0.2.%d", i+1)
		must("node network", n.do(func() (err error) {
			if err = ipCmds(
				[]string{"link", "set", "lo", "up"},
				[]string{"addr", "add", addr + "/24", "dev", dev},
				[]string{"link", "set", dev, "mtu", linkMTU, "up"},
			); err != nil {
				return err
			}
			sockets[i].rx, sockets[i].tx, err = vxlan.ListenSockets("udp4", net.ParseIP(addr), 0, 4)
			return err
		}))
	}

	// The pods.
	var pods [2]*nsPod
	for i := range pods {
		pods[i] = newPod(t, TapConfig{
			Name: "pod0", IP: net.IPv4(10, 244, byte(i+1), 4), Mask: net.CIDRMask(24, 32), Gateway: net.IPv4(10, 244, byte(i+1), 1),
			MAC: net.HardwareAddr{2, 0, 0, 0, 0, byte(4 + 10*i)}, MTU: podMTU,
		}, false)
	}

	// The nodes.
	eps := make([]*TapEndpoint, 2)
	for i := range eps {
		offload := !(mode == "nooffload" && i == 1)
		eps[i] = NewTapEndpoint("pod", "pod", net.IPv4(10, 244, byte(i+1), 4), net.HardwareAddr{2, 0, 0, 0, 0, byte(4 + 10*i)}, pods[i].tap, offload)
		if !eps[i].vnet || eps[i].Offload() != offload {
			t.Skipf("this kernel does not give the TAP a virtio header or offloads (vnet %v, offload %v)", eps[i].vnet, eps[i].Offload())
		}
		if mode != "whole" {
			eps[i].SetPipeline()
		}
		eps[i].SetPassthrough()
	}
	cfg := vxlan.OverlayConfig{Offload: true, Super: true, RcvBuf: 8 << 20}
	adopt := func(i int) func(oe *vxlan.OverlayEngine) error {
		return func(oe *vxlan.OverlayEngine) error {
			return oe.Adopt(context.Background(), sockets[i].rx, sockets[i].tx)
		}
	}
	cfgA, cfgB := cfg, cfg
	cfgA.Super, cfgB.Super = mode != "send-off", mode != "recv-off"
	a := newOvNode(t, 1, eps[0], "192.0.2.1", cfgA, adopt(0))
	b := newOvNode(t, 2, eps[1], "192.0.2.2", cfgB, adopt(1))
	if !a.oe.Stats().SegmentOffload {
		t.Skip("this kernel has no UDP_GRO, which UDP_SEGMENT goes with")
	}
	a.routeTo(b)
	b.routeTo(a)
	a.start()
	b.start()

	before := Offloads()
	runStream(t, fmt.Sprintf("pod MTU %d, %s", podMTU, mode),
		func() streamResult { return serve(pods[1].lfd) },
		func() streamResult { return send(pods[0].cfd, "10.244.2.4", podPort) },
		func() string {
			return fmt.Sprintf("counters %+v, A %+v, B %+v", Offloads(), a.oe.Stats(), b.oe.Stats())
		})
	d := delta(Offloads(), before)
	sa, sb := a.oe.Stats(), b.oe.Stats()
	t.Logf("TAP counters %+v", d)
	t.Logf("sender node: %d super-frames in %d datagrams, %d sends, %d plain datagrams in all; receiver node: %d super-frames, %d reads, lost %d invalid %d evicted %d",
		sa.SuperTx, sa.SuperTxChunks, sa.TxSends, sa.TxPackets-sa.SuperTxChunks, sb.SuperRx, sb.RxReads, sb.SuperPartial, sb.SuperInvalid, sb.SuperEvicted)
	if sa.TxErrors != 0 || sb.SuperPartial != 0 || sb.SuperInvalid != 0 || sb.RxInvalid != 0 || d.ReadDropped != 0 {
		t.Errorf("errors: sender %+v, receiver %+v, TAPs %+v", sa, sb, d)
	}
	switch mode {
	case "whole", "pipeline", "nooffload":
		// Every TSO segment of the stream was sent and received whole; the sender's Segmenter
		// and the receiver's Coalescer had nothing to do (the stream's few other frames are
		// single). With a pod that takes no super-frames, the receiving side cut them.
		if sa.SuperTx < podBytes/(64<<10)/2 || sb.SuperRx != sa.SuperTx || sa.SuperTxChunks <= sa.SuperTx {
			t.Errorf("super-frames sent %d (%d datagrams), received %d", sa.SuperTx, sa.SuperTxChunks, sb.SuperRx)
		}
		// The Segmenter had nothing to cut, and the Coalescer joined only what was sent as
		// single frames (a few at the ends of the stream, and its answers).
		plain := sa.TxPackets - sa.SuperTxChunks + sb.TxPackets - sb.SuperTxChunks
		if d.ReadSuperFrames > sa.SuperTx/20+1 || d.WriteSegments > plain {
			t.Errorf("the Segmenter cut %d TSO segments and the Coalescer joined %d for %d super-frames and %d single frames", d.ReadSuperFrames, d.WriteSegments, sa.SuperTx, plain)
		}
		if bulk := uint64(podBytes / (podMTU - 52)); sa.SuperTxChunks*5 < bulk*4 {
			t.Errorf("only %d datagrams of the stream's %d were chunks of super-frames", sa.SuperTxChunks, bulk)
		}
		if mode == "nooffload" && d.PassedSuperFrames != 0 {
			t.Errorf("%d super-frames were written whole to a pod without offloads", d.PassedSuperFrames)
		}
		if mode != "nooffload" && d.PassedSuperFrames < sb.SuperRx*9/10 {
			t.Errorf("%d super-frames were written whole to the pod, %d were received", d.PassedSuperFrames, sb.SuperRx)
		}
	default:
		// cut: nothing whole, every TSO segment cut by the sender and joined again
		// for the receiver.
		if sa.SuperTx != 0 || sb.SuperRx != 0 || sa.SuperTxChunks != 0 {
			t.Errorf("super-frames were sent whole: %d, received %d", sa.SuperTx, sb.SuperRx)
		}
		if d.ReadSuperFrames == 0 || d.WriteSuperFrames == 0 {
			t.Errorf("cut %d TSO segments and joined %d writes: the stream was not cut", d.ReadSuperFrames, d.WriteSuperFrames)
		}
	}
}

// TestOverlaySuperFramesWithRealKernels is the whole thing with real kernels: two nodes,
// each with its bridge and overlay engine on one end of a veth pair, and a pod's TAP in a
// namespace of its own at each, with a TCP stream from one pod to the other. See
// ovScenario. It runs in user and network namespaces, so as to need no privileges.
func TestOverlaySuperFramesWithRealKernels(t *testing.T) {
	if mode := os.Getenv("PLAID_OVSUPER_CHILD"); mode != "" {
		mtu, name, _ := strings.Cut(mode, ":")
		podMTU, _ := strconv.Atoi(mtu)
		ovScenario(t, podMTU, name)
		return
	}
	needUserns(t)
	for _, podMTU := range []int{1450, 8950} {
		for _, mode := range []string{"whole", "pipeline", "nooffload", "recv-off", "send-off"} {
			t.Run(fmt.Sprintf("pod MTU %d %s", podMTU, mode), func(t *testing.T) {
				runInUserns(t, "TestOverlaySuperFramesWithRealKernels", fmt.Sprintf("PLAID_OVSUPER_CHILD=%d:%s", podMTU, mode), 2*time.Minute)
			})
		}
	}
}
