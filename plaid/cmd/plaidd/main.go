package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"plaid/pkg/netcfg"
	"plaid/pkg/nodeport"
	"sort"
	"strings"
	"syscall"
	"time"

	"plaid/pkg/api"
	"plaid/pkg/bridge"
	"plaid/pkg/filter"
	"plaid/pkg/gateway"
	"plaid/pkg/handoff"
	"plaid/pkg/ipam"
	"plaid/pkg/k8s"
	"plaid/pkg/packet"
	"plaid/pkg/slirp"
	"plaid/pkg/slirpapi"
	"plaid/pkg/svc"
	"plaid/pkg/tap"
	"plaid/pkg/vxlan"
)

// bridgeInjector lets the overlay inject received frames into the bridge in batches.
type bridgeInjector struct{ *bridge.Bridge }

func (i bridgeInjector) NewBatch() vxlan.InjectBatch { return i.Bridge.NewBatch() }

type PlaidDaemon struct {
	cfg          Config
	bridge       *bridge.Bridge
	routes       *vxlan.RouteTable
	overlay      *vxlan.OverlayEngine
	slirpMgr     *slirp.SlirpManager
	attachment   *gateway.Attachment
	bubbleEP     *tap.TapEndpoint
	healthErr    chan error
	filterEngine *filter.Engine
	apiServer    *api.Server
	pprofStop    func() // profiling server, when --pprof-socket is set
	cancelFunc   context.CancelFunc
	k8sCtrl      *k8s.Controller
	services     *svc.Engine // tap mode only
	nodePorts    *nodeport.Reconciler
}

type Config struct {
	SocketPath        string
	NodeCIDR          string
	ClusterCIDR       string
	GatewayIP         string
	GatewayMAC        string
	BubbleMAC         string
	GatewayMode       string
	Uplink            string
	UplinkAddress     string
	ServiceCIDR       string
	Resolver          string
	VXLANPort         int
	VXLANVNI          int
	VXLANBind         string
	OverlayHandoff    string
	Offload           bool
	UplinkMTU         int
	SlirpBin          string
	SlirpSocket       string
	SlirpDisableDNS   bool
	SlirpHostLoopback bool
	EnableSlirp       bool
	Routes            string
	Kubeconfig        string
	NodeName          string
	MTU               int
	// PprofSocket, when set, is a UNIX socket on which plaidd serves Go's profiling endpoints (see startPprof).
	PprofSocket string
	// DisableKubernetes keeps plaidd away from the Kubernetes API altogether: no Node,
	// Service or EndpointSlice watches, no Node condition.
	DisableKubernetes bool
	// SNATAddress is the Service engine's address (tap mode): empty means the node
	// network's .2.
	SNATAddress string
	// NodePorts: the API socket of the slirp4netns that serves the bubble (empty: NodePorts are
	// not published), the physical host's address the forwards listen on, and the NodePort range.
	NodePortSlirpSocket string
	NodePortHostAddress string
	NodePortRange       string
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.SocketPath, "api-socket", netcfg.SocketPath(""), "Path to plaidd UNIX domain socket (default: $PLAID_SOCKET_PATH, else "+netcfg.DefaultSocketPath+")")
	flag.StringVar(&cfg.NodeCIDR, "node-cidr", "", "Pod CIDR of this node (default: the one Kubernetes allocated to the Node; required with --disable-kubernetes)")
	flag.BoolVar(&cfg.DisableKubernetes, "disable-kubernetes", false, "Never contact the Kubernetes API: no Node, Service or EndpointSlice watches (standalone Plaid; requires --node-cidr)")
	flag.StringVar(&cfg.NodePortSlirpSocket, "nodeport-slirp-socket", "", "API socket of the slirp4netns that serves this bubble: NodePort Services are published through it on --nodeport-host-address (tap mode; empty: not published)")
	flag.StringVar(&cfg.NodePortHostAddress, "nodeport-host-address", "", "Physical host address on which NodePorts are published")
	flag.StringVar(&cfg.NodePortRange, "nodeport-range", "30000-32767", "The Kubernetes NodePort range Plaid publishes")
	flag.StringVar(&cfg.SNATAddress, "snat-address", "", "Address of the Service engine in tap mode, which backends see as the source of Service traffic and answer (default: the .2 of the node network)")
	flag.StringVar(&cfg.ClusterCIDR, "cluster-cidr", netcfg.DefaultClusterCIDR, "Overall cluster pod CIDR")
	flag.StringVar(&cfg.GatewayIP, "gateway-ip", "", "Gateway IP (defaults to first IP of node-cidr, e.g. 10.244.1.1)")
	flag.StringVar(&cfg.GatewayMAC, "gateway-mac", "02:00:00:00:00:01", "Gateway MAC address")
	flag.StringVar(&cfg.BubbleMAC, "bubble-mac", "02:00:00:00:00:02", "Bubble TAP MAC address (distinct from gateway-mac)")
	flag.StringVar(&cfg.GatewayMode, "gateway-mode", "slirp", "Gateway backend: tap, slirp, or none")
	flag.StringVar(&cfg.Uplink, "uplink", "", "Bubble uplink interface for TAP mode")
	flag.StringVar(&cfg.UplinkAddress, "uplink-address", "", "Outer slirp guest IPv4 address for TAP egress SNAT")
	flag.StringVar(&cfg.ServiceCIDR, "service-cidr", netcfg.DefaultServiceCIDR, "Cluster Service CIDR")
	flag.StringVar(&cfg.Resolver, "resolver", "", "Resolver IP advertised to standalone plaidtainer")
	flag.IntVar(&cfg.VXLANPort, "vxlan-port", packet.DefaultVXLANPort, "VXLAN UDP port")
	flag.IntVar(&cfg.VXLANVNI, "vxlan-vni", packet.DefaultVNI, "VXLAN VNI")
	flag.StringVar(&cfg.VXLANBind, "vxlan-bind", "0.0.0.0", "VXLAN bind address (with --overlay-handoff: the address the handed-over socket must be bound to)")
	flag.StringVar(&cfg.OverlayHandoff, "overlay-handoff", "", "Path of the UNIX socket on which to receive the overlay's UDP socket, opened on the physical host by `plaidctl overlay-socket` (default: plaidd opens the VXLAN port itself)")
	flag.BoolVar(&cfg.Offload, "offload", true, "Use the kernel's offloads where it has them: checksum and TCP segmentation offload on the pods' and the bubble's TAPs, UDP_SEGMENT and UDP_GRO on the overlay socket. Datagrams on the wire are the same and nodes with and without them interoperate; false is one frame and one datagram per system call, and a pod's TCP super-frames are cut instead of being written whole to the other pods and the bubble of this node")
	flag.IntVar(&cfg.UplinkMTU, "uplink-mtu", 0, "MTU of the bubble's TAP (plaid0) and of its link to slirp4netns in tap mode, which may be much larger than the pods' (at most 65520), so that slirp4netns, which pays for every packet, moves fewer: what a pod sends is joined into packets of up to this size, and larger packets from the bubble are cut for the pods. 0: the pod MTU")
	flag.StringVar(&cfg.SlirpBin, "slirp-bin", "slirp4netns", "Path to slirp4netns binary")
	flag.StringVar(&cfg.SlirpSocket, "slirp-socket", "/run/plaid/slirp.sock", "UNIX socket path for slirp BESS mode")
	flag.BoolVar(&cfg.SlirpDisableDNS, "slirp-disable-dns", false, "Disable slirp built-in DNS (default: false, enabling DNS forwarder at node-cidr base+3)")
	flag.BoolVar(&cfg.SlirpHostLoopback, "slirp-host-loopback", true, "Enable access to host 127.0.0.1 via slirp")
	flag.BoolVar(&cfg.EnableSlirp, "enable-slirp", true, "Enable slirp4netns for outbound traffic")
	flag.StringVar(&cfg.Routes, "routes", "", "Initial overlay routes, comma-separated (e.g. '10.244.2.0/24=node.local')")
	flag.StringVar(&cfg.Kubeconfig, "kubeconfig", "", "Path to kubeconfig file (Kubernetes mode only; defaults to in-cluster service account)")
	flag.StringVar(&cfg.NodeName, "node-name", "", "Local node name (Kubernetes mode only; defaults to NODE_NAME env or hostname)")
	flag.StringVar(&cfg.PprofSocket, "pprof-socket", os.Getenv(EnvPprofSocket), "Path of a UNIX socket on which to serve Go profiling endpoints (CPU, allocations, goroutines, mutex, block, trace), for finding where plaidd's CPU goes without perf (default: $"+EnvPprofSocket+"; empty: off)")
	flag.IntVar(&cfg.MTU, "mtu", 0, "Effective MTU for container TAP interfaces and slirp (default: $PLAID_MTU, else 1450, allowing 50B VXLAN overhead on 1500B host MTU)")

	flag.Parse()

	daemon, err := NewPlaidDaemon(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing plaidd: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	daemon.cancelFunc = cancel

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	if err := daemon.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting plaidd: %v\n", err)
		os.Exit(1)
	}

	mode := "kubernetes"
	if daemon.k8sCtrl == nil {
		mode = "no-kubernetes"
	}
	fmt.Printf("plaidd is running [Mode=%s, NodeCIDR=%s, ClusterCIDR=%s, VXLANPort=%d, APISocket=%s]\n",
		mode, daemon.cfg.NodeCIDR, daemon.cfg.ClusterCIDR, daemon.cfg.VXLANPort, daemon.cfg.SocketPath)

	select {
	case sig := <-sigChan:
		fmt.Printf("Received signal %s, shutting down plaidd...\n", sig)
	case err := <-daemon.healthErr:
		fmt.Fprintf(os.Stderr, "Fatal gateway failure: %v\n", err)
		daemon.Stop()
		os.Exit(1)
	}
	daemon.Stop()
}

func NewPlaidDaemon(cfg Config) (*PlaidDaemon, error) {
	var k8sCtrl *k8s.Controller

	if cfg.DisableKubernetes {
		if cfg.NodeCIDR == "" {
			return nil, fmt.Errorf("--disable-kubernetes needs --node-cidr: there is no Node to take it from")
		}
		fmt.Printf("[plaidd] Kubernetes disabled [NodeCIDR=%s]\n", cfg.NodeCIDR)
	} else {
		var err error
		k8sCtrl, err = k8s.NewControllerFromConfig(k8s.Config{
			Kubeconfig: cfg.Kubeconfig,
			NodeName:   cfg.NodeName,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Kubernetes controller: %w", err)
		}
		if cfg.NodeCIDR == "" {
			fmt.Printf("[plaidd] Kubernetes active. Discovering pod CIDR for node %q...\n", k8sCtrl.NodeName())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			_, podNet, hostIP, err := k8sCtrl.GetLocalNode(ctx)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("failed to retrieve local node CIDR from Kubernetes: %w", err)
			}
			cfg.NodeCIDR = podNet.String()
			fmt.Printf("[plaidd] Discovered node CIDR from Kubernetes: %s (host IP: %s)\n", cfg.NodeCIDR, hostIP)
		} else {
			fmt.Printf("[plaidd] Kubernetes active [NodeCIDR=%s]\n", cfg.NodeCIDR)
		}
	}

	nodeNet, err := ipam.ValidateNodeCIDR(cfg.NodeCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid node-cidr: %w", err)
	}

	_, clusterNet, err := net.ParseCIDR(cfg.ClusterCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid cluster-cidr: %w", err)
	}
	if clusterNet.IP.To4() == nil {
		return nil, fmt.Errorf("IPv6 is not supported; cluster CIDR %q must be IPv4", cfg.ClusterCIDR)
	}
	if cfg.ServiceCIDR == "" {
		cfg.ServiceCIDR = netcfg.DefaultServiceCIDR
	}
	_, serviceNet, err := net.ParseCIDR(cfg.ServiceCIDR)
	if err != nil || serviceNet.IP.To4() == nil {
		return nil, fmt.Errorf("invalid IPv4 service-cidr %q", cfg.ServiceCIDR)
	}
	if overlaps(nodeNet, serviceNet) || overlaps(clusterNet, serviceNet) || !clusterNet.Contains(nodeNet.IP) {
		return nil, fmt.Errorf("node, cluster, and Service CIDRs are inconsistent or overlap")
	}
	if cfg.GatewayMode == "" {
		if cfg.EnableSlirp {
			cfg.GatewayMode = "slirp"
		} else {
			cfg.GatewayMode = "none"
		}
	}
	if cfg.GatewayMode != "tap" && cfg.GatewayMode != "slirp" && cfg.GatewayMode != "none" {
		return nil, fmt.Errorf("invalid gateway-mode %q", cfg.GatewayMode)
	}
	if cfg.GatewayMode == "tap" {
		if net.ParseIP(cfg.UplinkAddress).To4() == nil || cfg.Uplink == "" {
			return nil, fmt.Errorf("tap mode requires uplink and uplink-address")
		}
		if serviceNet.Contains(net.ParseIP(cfg.UplinkAddress)) || clusterNet.Contains(net.ParseIP(cfg.UplinkAddress)) {
			return nil, fmt.Errorf("uplink address overlaps pod or Service CIDR")
		}
		// In a cluster the resolver is the cluster DNS Service, an address of the
		// Service network.
		if cfg.Resolver != "" {
			if err := netcfg.ValidateResolver(cfg.Resolver, serviceNet); err != nil {
				return nil, err
			}
		}
		if _, err := net.ParseMAC(cfg.BubbleMAC); err != nil {
			return nil, fmt.Errorf("invalid bubble-mac: %w", err)
		}
		if cfg.BubbleMAC == cfg.GatewayMAC {
			return nil, fmt.Errorf("bubble-mac must differ from gateway-mac")
		}
	}

	cfg.MTU = netcfg.EffectiveMTU(cfg.MTU)
	if err := netcfg.ValidateMTU(cfg.MTU); err != nil {
		return nil, err
	}
	if cfg.GatewayMode == "tap" {
		if cfg.UplinkMTU, err = netcfg.UplinkMTU(cfg.UplinkMTU, cfg.MTU); err != nil {
			return nil, err
		}
	} else if cfg.UplinkMTU != 0 {
		return nil, fmt.Errorf("uplink-mtu is for the tap gateway mode")
	}

	gwIP := net.ParseIP(cfg.GatewayIP)
	if gwIP == nil {
		// Default gateway is network IP + 1
		gwIP = make(net.IP, len(nodeNet.IP))
		copy(gwIP, nodeNet.IP.To4())
		gwIP[3]++
	}
	cfg.GatewayIP = gwIP.String()
	if cfg.GatewayMode == "tap" {
		expected := net.IPv4(nodeNet.IP[0], nodeNet.IP[1], nodeNet.IP[2], 1)
		if !gwIP.Equal(expected) {
			return nil, fmt.Errorf("tap gateway must be first usable node address %s", expected)
		}
	}

	gwMAC, err := net.ParseMAC(cfg.GatewayMAC)
	if err != nil {
		return nil, fmt.Errorf("invalid gateway-mac: %w", err)
	}

	b := bridge.NewBridge(bridge.BridgeConfig{
		GatewayIP:   gwIP,
		GatewayMAC:  gwMAC,
		GatewayMode: cfg.GatewayMode,
		NodeCIDR:    nodeNet,
		ClusterCIDR: clusterNet,
		FDBTTL:      5 * time.Minute,
	})

	routes := vxlan.NewRouteTable()
	b.SetRouteChecker(func(ip net.IP) bool { _, err := routes.Lookup(ip); return err == nil })
	if cfg.Routes != "" {
		for _, rStr := range strings.Split(cfg.Routes, ",") {
			rStr = strings.TrimSpace(rStr)
			if rStr == "" {
				continue
			}
			parts := strings.SplitN(rStr, "=", 2)
			if len(parts) == 2 {
				subnetStr := strings.TrimSpace(parts[0])
				hostStr := strings.TrimSpace(parts[1])
				_, sn, err := net.ParseCIDR(subnetStr)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: invalid initial route subnet %s: %v\n", subnetStr, err)
					continue
				}
				rIP, err := resolveHostIP(hostStr)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: cannot resolve initial route host %s: %v\n", hostStr, err)
					continue
				}
				routes.AddRoute(&vxlan.RouteEntry{
					Subnet:       sn,
					RemoteHostIP: rIP,
				})
				fmt.Printf("[plaidd] Initial route added: %s -> %s (%s)\n", subnetStr, hostStr, rIP)
			}
		}
	}

	overlay := vxlan.NewOverlayEngine(vxlan.OverlayConfig{
		BindAddress: cfg.VXLANBind,
		Port:        cfg.VXLANPort,
		DefaultVNI:  uint32(cfg.VXLANVNI),
		Offload:     cfg.Offload,
	}, routes, bridgeInjector{b})

	var slirpMgr *slirp.SlirpManager
	if cfg.GatewayMode == "slirp" {
		slirpMgr = slirp.NewSlirpManager(slirp.SlirpConfig{
			BinaryPath:          cfg.SlirpBin,
			SocketPath:          cfg.SlirpSocket,
			CIDR:                cfg.NodeCIDR,
			MTU:                 cfg.MTU,
			DisableDNS:          cfg.SlirpDisableDNS,
			DisableHostLoopback: !cfg.SlirpHostLoopback,
		}, b)
	}

	var services *svc.Engine
	if cfg.GatewayMode == "tap" {
		snat, err := serviceAddress(cfg.SNATAddress, nodeNet)
		if err != nil {
			return nil, err
		}
		cfg.SNATAddress = snat.String()
		services, err = svc.New(svc.Config{ServiceCIDR: serviceNet, SNATAddr: snat})
		if err != nil {
			return nil, err
		}
		b.SetServiceEngine(services)
	}

	var nodePorts *nodeport.Reconciler
	if cfg.NodePortSlirpSocket != "" {
		if cfg.GatewayMode != "tap" {
			return nil, fmt.Errorf("--nodeport-slirp-socket needs the tap gateway")
		}
		host := net.ParseIP(cfg.NodePortHostAddress)
		if host == nil || host.To4() == nil {
			return nil, fmt.Errorf("--nodeport-host-address %q is not an IPv4 address", cfg.NodePortHostAddress)
		}
		var low, high int
		if n, err := fmt.Sscanf(cfg.NodePortRange, "%d-%d", &low, &high); err != nil || n != 2 || low < 1 || high > 65535 || low > high {
			return nil, fmt.Errorf("--nodeport-range %q is not a range of ports like 30000-32767", cfg.NodePortRange)
		}
		var reporter nodeport.Reporter
		if k8sCtrl != nil {
			reporter = k8sCtrl.Reporter()
		}
		nodePorts = nodeport.New(nodeport.Config{HostAddr: host.String(), ServiceCIDR: serviceNet, RangeLow: low, RangeHigh: high},
			slirpapi.New(cfg.NodePortSlirpSocket), reporter)
	}

	filterEngine := filter.NewEngine(filter.ActionAccept)
	b.SetFilterHandler(filterEngine.Filter)

	d := &PlaidDaemon{
		cfg:          cfg,
		bridge:       b,
		routes:       routes,
		overlay:      overlay,
		slirpMgr:     slirpMgr,
		filterEngine: filterEngine,
		k8sCtrl:      k8sCtrl,
		services:     services,
		nodePorts:    nodePorts,
		healthErr:    make(chan error, 1),
	}

	// Connect bridge handlers
	b.SetOverlayBatcher(func() bridge.OverlayBatch { return overlay.NewBatch() })
	if slirpMgr != nil {
		b.SetOutboundHandler(slirpMgr.WriteFrame)
	}

	d.apiServer = api.NewServer(cfg.SocketPath, d, d, d)
	d.apiServer.SetFilterHandler(d)
	d.apiServer.SetServiceHandler(d)
	return d, nil
}

func (d *PlaidDaemon) Start(ctx context.Context) error {
	var startErr error
	defer func() {
		if startErr != nil {
			d.Stop()
		}
	}()
	// Ensure socket directory exists with permissive access before acquiring lock
	socketDir := filepath.Dir(d.cfg.SocketPath)
	if err := os.MkdirAll(socketDir, 0777); err != nil {
		startErr = err
		return fmt.Errorf("failed to create runtime socket directory %s: %w", socketDir, err)
	}
	_ = os.Chmod(socketDir, 0777)

	// Acquire API socket lock and start listener before mutating network state, IPAM config, or binding overlay ports.
	// This guarantees that a duplicate daemon fails immediately without disturbing an existing live daemon.
	if err := d.apiServer.Start(ctx); err != nil {
		startErr = err
		return fmt.Errorf("failed to start API server: %w", err)
	}

	if d.cfg.PprofSocket != "" {
		stop, err := startPprof(d.cfg.PprofSocket)
		if err != nil {
			startErr = err
			return err
		}
		d.pprofStop = stop
	}

	// Initialize runtime dir, permissions, IPAM config, and binary
	if err := initRuntimeDir(d.cfg.SocketPath, d.cfg.NodeCIDR, d.cfg.GatewayIP); err != nil {
		startErr = err
		return fmt.Errorf("failed to initialize runtime dir: %w", err)
	}

	if d.cfg.GatewayMode == "tap" {
		_, nodeNet, _ := net.ParseCIDR(d.cfg.NodeCIDR)
		_, clusterNet, _ := net.ParseCIDR(d.cfg.ClusterCIDR)
		_, serviceNet, _ := net.ParseCIDR(d.cfg.ServiceCIDR)
		mac, _ := net.ParseMAC(d.cfg.BubbleMAC)
		var err error
		d.attachment, err = gateway.Start(gateway.Config{Name: "plaid0", Uplink: d.cfg.Uplink, GuestAddress: net.ParseIP(d.cfg.UplinkAddress), ServiceNextHop: net.ParseIP(d.cfg.SNATAddress), GatewayIP: net.ParseIP(d.cfg.GatewayIP), GatewayMAC: mac, NodeCIDR: nodeNet, ClusterCIDR: clusterNet, ServiceCIDR: serviceNet, MTU: d.cfg.UplinkMTU})
		if err != nil {
			startErr = err
			return fmt.Errorf("gateway setup: %w", err)
		}
		d.bubbleEP = tap.NewTapEndpoint("bubble", "plaid0", net.ParseIP(d.cfg.GatewayIP), mac, d.attachment.File, d.cfg.Offload)
		d.bubbleEP.SetUplinkMTU(d.cfg.MTU, d.cfg.UplinkMTU)
		if d.cfg.UplinkMTU > d.cfg.MTU {
			d.bubbleEP.SetPassthrough() // its large TCP frames, made TSO segments, go whole to the pods that take them
		}
		// The bubble's TAP is not pipelined: its traffic is the pods' traffic to and from
		// slirp4netns, a ping-pong with a user-space TCP stack that the hand-offs only slow
		// down.
		d.bridge.SetBubbleEndpoint(d.bubbleEP)
		d.bubbleEP.StartReadLoopFunc(ctx, d.bridge, func(err error) {
			select {
			case d.healthErr <- err:
			default:
			}
		})
		fmt.Printf("[plaidd] TAP gateway ready: %s/%s, MAC=%s, uplink=%s, guest=%s, cluster route=%s, TAP offloads=%v\n", d.cfg.GatewayIP, d.cfg.NodeCIDR, mac, d.cfg.Uplink, d.cfg.UplinkAddress, d.cfg.ClusterCIDR, d.bubbleEP.Offload())
	}

	if d.services != nil {
		go d.sweepServices(ctx)
	}
	if d.nodePorts != nil {
		go d.nodePorts.Run(ctx)
	}

	if d.cfg.OverlayHandoff != "" {
		if err := d.receiveOverlaySocket(ctx); err != nil {
			startErr = err
			return fmt.Errorf("failed to attach the overlay socket: %w", err)
		}
	} else if err := d.overlay.Start(ctx); err != nil {
		startErr = err
		return fmt.Errorf("failed to start overlay: %w", err)
	}

	if d.slirpMgr != nil {
		go func() {
			if err := d.slirpMgr.Start(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to start slirp4netns: %v\n", err)
			}
		}()
	}

	if d.k8sCtrl != nil {
		if err := d.k8sCtrl.SetNodeNetworkReady(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to set NodeNetworkUnavailable condition: %v\n", err)
		} else {
			fmt.Printf("[plaidd/k8s] Marked NodeNetworkUnavailable=False on node %s\n", d.k8sCtrl.NodeName())
		}

		if d.services != nil {
			go d.k8sCtrl.RunServices(ctx, d.services, d.nodePortSink())
		}

		gwMAC, _ := net.ParseMAC(d.cfg.GatewayMAC)
		go d.k8sCtrl.Run(ctx,
			func(nodeName string, sn *net.IPNet, hostIP net.IP) {
				entry := &vxlan.RouteEntry{
					Subnet:       sn,
					RemoteHostIP: hostIP,
					VtepMAC:      gwMAC,
					VNI:          uint32(d.cfg.VXLANVNI),
					Port:         d.cfg.VXLANPort,
				}
				d.routes.AddRoute(entry)
				fmt.Printf("[plaidd/k8s] Added route for node %s: %s -> %s\n", nodeName, sn, hostIP)
			},
			func(nodeName string, sn *net.IPNet) {
				d.routes.RemoveRoute(sn.String())
				fmt.Printf("[plaidd/k8s] Removed route for node %s: %s\n", nodeName, sn)
			},
		)
	}

	d.apiServer.MarkReady()
	return nil
}

// overlayHandoffTimeout bounds the wait for the helper once plaidd is listening for it
// (a variable so tests can shorten it).
// The helper is already polling for the socket, so it arrives within a moment.
var overlayHandoffTimeout = 2 * time.Minute

// receiveOverlaySocket waits for the UDP socket opened on the physical host and adopts
// it. It runs before the daemon reports Ready and does not depend on anything that
// waits for Ready: the handoff has a socket of its own, apart from the API.
func (d *PlaidDaemon) receiveOverlaySocket(ctx context.Context) error {
	bind := net.ParseIP(d.cfg.VXLANBind)
	if bind == nil || bind.To4() == nil {
		return fmt.Errorf("--vxlan-bind %q is not an IPv4 address", d.cfg.VXLANBind)
	}
	if err := os.MkdirAll(filepath.Dir(d.cfg.OverlayHandoff), 0700); err != nil {
		return err
	}
	fmt.Printf("[plaidd] Waiting for the overlay socket (%s:%d) on %s\n", bind, d.cfg.VXLANPort, d.cfg.OverlayHandoff)
	err := handoff.Await(ctx, d.cfg.OverlayHandoff, overlayHandoffTimeout, func(req handoff.Request, files []*os.File) error {
		if req.Type != handoff.TypeOverlaySocket {
			return fmt.Errorf("unexpected request %q", req.Type)
		}
		if len(files) != 1 {
			return fmt.Errorf("expected one descriptor, got %d", len(files))
		}
		if req.Port != d.cfg.VXLANPort || !net.ParseIP(req.Bind).Equal(bind) {
			return fmt.Errorf("the sender bound %s:%d, plaidd is configured for %s:%d", req.Bind, req.Port, bind, d.cfg.VXLANPort)
		}
		conn, err := vxlan.SocketFromFile(files[0], bind, d.cfg.VXLANPort)
		if err != nil {
			return err
		}
		if err := d.overlay.Adopt(ctx, conn); err != nil {
			_ = conn.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	st := d.overlay.Stats()
	fmt.Printf("[plaidd] Overlay socket attached: %s (receive buffer %d bytes)\n", st.Address, st.RcvBuf)
	return nil
}

func (d *PlaidDaemon) Stop() {
	if d.cancelFunc != nil {
		d.cancelFunc()
	}
	if d.apiServer != nil {
		_ = d.apiServer.Close()
	}
	if d.pprofStop != nil {
		d.pprofStop()
		d.pprofStop = nil
	}
	if d.overlay != nil {
		_ = d.overlay.Close()
	}
	// With the overlay stopped, nothing more is delivered to the TAPs: close them, which
	// ends their pipelines' goroutines.
	for _, ep := range d.tapEndpoints() {
		_ = ep.Close()
	}
	if d.slirpMgr != nil {
		_ = d.slirpMgr.Close()
	}
	if d.attachment != nil {
		_ = d.attachment.Close()
		d.attachment = nil
	}
}

func overlaps(a, b *net.IPNet) bool { return a.Contains(b.IP) || b.Contains(a.IP) }

// API Server Callbacks

func (d *PlaidDaemon) HandleAddEndpoint(req *api.Request, tapFD int) error {
	if tapFD < 0 {
		return fmt.Errorf("missing TAP file descriptor")
	}

	epFile := os.NewFile(uintptr(tapFD), req.PodName)
	epIP := net.ParseIP(req.IP)
	_, nodeNet, _ := net.ParseCIDR(d.cfg.NodeCIDR)
	if epIP == nil || epIP.To4() == nil || !nodeNet.Contains(epIP) || epIP.Equal(net.ParseIP(d.cfg.GatewayIP)) || epIP.To4()[3] < 4 || epIP.To4()[3] > 254 {
		_ = epFile.Close()
		return fmt.Errorf("endpoint IP %q is outside the allocatable node range", req.IP)
	}
	epMAC, err := net.ParseMAC(req.MAC)
	if err != nil {
		_ = epFile.Close()
		return fmt.Errorf("invalid MAC: %w", err)
	}

	epID := req.PodID
	if epID == "" {
		epID = req.ContainerID
	}

	ep := tap.NewTapEndpoint(epID, req.PodName, epIP, epMAC, epFile, d.cfg.Offload)
	ep.SetPipeline()
	ep.SetPassthrough()
	if err := d.bridge.AddEndpoint(ep); err != nil {
		_ = ep.Close() // closes epFile and stops the goroutines SetPipeline started
		return err
	}

	ep.StartReadLoop(context.Background(), d.bridge)
	fmt.Printf("[plaidd] Added endpoint %s (IP=%s, MAC=%s, TAP offloads=%v)\n", epID, epIP, epMAC, ep.Offload())
	return nil
}

func (d *PlaidDaemon) HandleRemoveEndpoint(req *api.Request) error {
	epID := req.PodID
	if epID == "" {
		epID = req.ContainerID
	}

	err := d.bridge.RemoveEndpoint(epID)
	if errors.Is(err, bridge.ErrEndpointNotFound) {
		// Idempotent success per CNI specification
		return nil
	}
	if err == nil {
		fmt.Printf("[plaidd] Removed endpoint %s\n", epID)
	}
	return err
}

func resolveHostIP(host string) (net.IP, error) {
	ip := net.ParseIP(host)
	if ip != nil {
		return ip, nil
	}
	ips, err := net.LookupIP(host)
	if err == nil {
		for _, candidate := range ips {
			if candidate.To4() != nil {
				return candidate.To4(), nil
			}
		}
	}
	return nil, fmt.Errorf("could not resolve host %q to IPv4", host)
}

func (d *PlaidDaemon) HandleAddRoute(req *api.Request) error {
	_, sn, err := net.ParseCIDR(req.Subnet)
	if err != nil {
		return fmt.Errorf("invalid subnet CIDR: %w", err)
	}

	remoteIP, err := resolveHostIP(req.RemoteHostIP)
	if err != nil {
		return fmt.Errorf("invalid remote host %s: %w", req.RemoteHostIP, err)
	}

	var vtepMAC net.HardwareAddr
	if req.VtepMAC != "" {
		vtepMAC, err = net.ParseMAC(req.VtepMAC)
		if err != nil {
			return fmt.Errorf("invalid VTEP MAC: %w", err)
		}
	}

	entry := &vxlan.RouteEntry{
		Subnet:       sn,
		RemoteHostIP: remoteIP,
		VtepMAC:      vtepMAC,
		VNI:          req.VNI,
		Port:         req.Port,
	}

	d.routes.AddRoute(entry)
	fmt.Printf("[plaidd] Added route %s -> Host %s (VTEP %s)\n", req.Subnet, remoteIP, vtepMAC)
	return nil
}

func (d *PlaidDaemon) HandleRemoveRoute(req *api.Request) error {
	deleted := d.routes.RemoveRoute(req.Subnet)
	if !deleted {
		return fmt.Errorf("route %s not found", req.Subnet)
	}
	fmt.Printf("[plaidd] Removed route %s\n", req.Subnet)
	return nil
}

func (d *PlaidDaemon) HandleGetStatus() (*api.Response, error) {
	eps := d.bridge.Endpoints()
	epList := make([]string, len(eps))
	for i, ep := range eps {
		epList[i] = fmt.Sprintf("%s (%s / %s)", ep.ID(), ep.IP(), ep.MAC())
	}
	sort.Strings(epList)

	mode := "kubernetes"
	if d.k8sCtrl == nil {
		mode = "no-kubernetes"
	}

	ov := d.overlay.Stats()
	resp := &api.Response{
		OverlayTransport:    ov.Transport,
		OverlayAddress:      ov.Address,
		OverlayTxPackets:    ov.TxPackets,
		OverlayTxSends:      ov.TxSends,
		OverlayTxErrors:     ov.TxErrors,
		OverlayRxReads:      ov.RxReads,
		OverlayRxPackets:    ov.RxPackets,
		OverlayRxInvalid:    ov.RxInvalid,
		OverlayRxDropped:    ov.RxDropped,
		OverlayRxQueueDrops: ov.RxQueueDrops,
		OverlayRcvBuf:       ov.RcvBuf,
		OverlaySocketDrops:  ov.SocketDrops,
		OverlayOffload:      overlayOffload(ov),
		TapOffload:          d.tapOffload(),
		TapPipeline:         d.tapPipeline(),
		EndpointsCount:      len(eps),
		RoutesCount:         len(d.routes.Routes()),
		Mode:                mode,
		NodeCIDR:            d.cfg.NodeCIDR,
		ClusterCIDR:         d.cfg.ClusterCIDR,
		GatewayIP:           d.cfg.GatewayIP,
		GatewayMode:         d.cfg.GatewayMode,
		GatewayReady:        d.cfg.GatewayMode != "tap" || d.attachment != nil,
		GatewayMAC:          d.cfg.GatewayMAC,
		BubbleMAC:           d.cfg.BubbleMAC,
		Uplink:              d.cfg.Uplink,
		UplinkAddress:       d.cfg.UplinkAddress,
		Resolver:            d.cfg.Resolver,
		MTU:                 d.cfg.MTU,
		UplinkMTU:           d.cfg.UplinkMTU,
		Endpoints:           epList,
	}
	d.fillServiceStatus(resp)
	return resp, nil
}

func (d *PlaidDaemon) HandleAddFilterRule(req *api.Request) error {
	var srcCIDR, dstCIDR *net.IPNet
	var err error
	if req.RuleSrcCIDR != "" {
		_, srcCIDR, err = net.ParseCIDR(req.RuleSrcCIDR)
		if err != nil {
			return fmt.Errorf("invalid rule_src_cidr: %w", err)
		}
	}
	if req.RuleDstCIDR != "" {
		_, dstCIDR, err = net.ParseCIDR(req.RuleDstCIDR)
		if err != nil {
			return fmt.Errorf("invalid rule_dst_cidr: %w", err)
		}
	}

	action := filter.Action(req.RuleAction)
	if action == "" {
		action = filter.ActionDrop
	}

	r := &filter.Rule{
		ID:       req.RuleID,
		SrcCIDR:  srcCIDR,
		DstCIDR:  dstCIDR,
		Protocol: req.RuleProtocol,
		SrcPort:  req.RuleSrcPort,
		DstPort:  req.RuleDstPort,
		Action:   action,
	}

	d.filterEngine.AddRule(r)
	fmt.Printf("[plaidd] Added filter rule: %s\n", r)
	return nil
}

func (d *PlaidDaemon) HandleRemoveFilterRule(req *api.Request) error {
	deleted := d.filterEngine.RemoveRule(req.RuleID)
	if !deleted {
		return fmt.Errorf("filter rule %s not found", req.RuleID)
	}
	fmt.Printf("[plaidd] Removed filter rule: %s\n", req.RuleID)
	return nil
}

func (d *PlaidDaemon) HandleListFilterRules() (*api.Response, error) {
	rules := d.filterEngine.Rules()
	descriptions := make([]string, len(rules))
	for i, r := range rules {
		descriptions[i] = r.String()
	}
	return &api.Response{
		FilterRules: descriptions,
	}, nil
}

func initRuntimeDir(socketPath, nodeCIDR, gwIP string) error {
	socketDir := filepath.Dir(socketPath)
	if err := os.MkdirAll(socketDir, 0777); err != nil {
		return fmt.Errorf("failed to create runtime socket directory %s: %w", socketDir, err)
	}
	_ = os.Chmod(socketDir, 0777)

	ipamConf, err := ipam.BuildHostLocalIPAMConfig(socketDir, nodeCIDR, gwIP)
	if err != nil {
		return fmt.Errorf("failed to build host-local IPAM configuration: %w", err)
	}

	ipamFile := filepath.Join(socketDir, "ipam.json")
	if err := os.WriteFile(ipamFile, ipamConf, 0666); err != nil {
		return fmt.Errorf("failed to write IPAM config file %s: %w", ipamFile, err)
	}
	_ = os.Chmod(ipamFile, 0666)
	return nil
}

// overlayOffload names the overlay's offloads that are on.
func overlayOffload(st vxlan.Stats) string {
	var on []string
	if st.SegmentOffload {
		on = append(on, "UDP_SEGMENT")
	}
	if st.ReceiveOffload {
		on = append(on, "UDP_GRO")
	}
	return strings.Join(on, ",")
}

// tapEndpoints returns the TAP endpoints of the pods and of the bubble.
func (d *PlaidDaemon) tapEndpoints() []*tap.TapEndpoint {
	var taps []*tap.TapEndpoint
	for _, ep := range d.bridge.Endpoints() {
		if t, ok := ep.(*tap.TapEndpoint); ok {
			taps = append(taps, t)
		}
	}
	if d.bubbleEP != nil {
		taps = append(taps, d.bubbleEP)
	}
	return taps
}

// tapOffload reports the TAP offloads: how many endpoints have them on and the work
// they have done.
func (d *PlaidDaemon) tapOffload() *api.TapOffloadStatus {
	st := &api.TapOffloadStatus{Enabled: d.cfg.Offload}
	for _, t := range d.tapEndpoints() {
		if t.Offload() {
			st.Endpoints++
		}
	}
	c := tap.Offloads()
	st.ReadSuperFrames, st.ReadSegments, st.ReadDropped = c.ReadSuperFrames, c.ReadSegments, c.ReadDropped
	st.WriteSuperFrames, st.WriteSegments = c.WriteSuperFrames, c.WriteSegments
	st.PassedSuperFrames, st.PassedBytes = c.PassedSuperFrames, c.PassedBytes
	st.JoinedForUplink, st.UplinkSuperFrames = c.JoinedForUplink, c.UplinkSuperFrames
	st.Fragmented, st.Fragments, st.OversizeDropped = c.Fragmented, c.Fragments, c.OversizeDropped
	return st
}

// tapPipeline reports the TAP pipelines: how many endpoints have one and how often a
// stage had to wait for the next.
func (d *PlaidDaemon) tapPipeline() *api.TapPipelineStatus {
	st := &api.TapPipelineStatus{}
	for _, t := range d.tapEndpoints() {
		if t.Pipeline() {
			st.Endpoints++
		}
	}
	c := tap.Pipeline()
	st.ReadInline, st.ReadQueued, st.ReadQueueFull = c.ReadInline, c.ReadQueued, c.ReadQueueFull
	st.WriteInline, st.WriteQueued, st.WriteQueueFull = c.WriteInline, c.WriteQueued, c.WriteQueueFull
	return st
}
