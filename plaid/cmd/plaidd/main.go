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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"plaid/pkg/api"
	"plaid/pkg/bridge"
	"plaid/pkg/filter"
	"plaid/pkg/gateway"
	"plaid/pkg/ipam"
	"plaid/pkg/k8s"
	"plaid/pkg/packet"
	"plaid/pkg/slirp"
	"plaid/pkg/tap"
	"plaid/pkg/vxlan"
)

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
	cancelFunc   context.CancelFunc
	k8sCtrl      *k8s.Controller
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
	SlirpBin          string
	SlirpSocket       string
	SlirpDisableDNS   bool
	SlirpHostLoopback bool
	EnableSlirp       bool
	Routes            string
	Kubeconfig        string
	NodeName          string
	MTU               int
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.SocketPath, "api-socket", "/run/plaid/plaidd.sock", "Path to plaidd UNIX domain socket")
	flag.StringVar(&cfg.NodeCIDR, "node-cidr", "", "Pod CIDR allocated to this node (if provided, runs in static mode; if omitted, runs in Kubernetes mode)")
	flag.StringVar(&cfg.ClusterCIDR, "cluster-cidr", "10.244.0.0/16", "Overall cluster pod CIDR")
	flag.StringVar(&cfg.GatewayIP, "gateway-ip", "", "Gateway IP (defaults to first IP of node-cidr, e.g. 10.244.1.1)")
	flag.StringVar(&cfg.GatewayMAC, "gateway-mac", "02:00:00:00:00:01", "Gateway MAC address")
	flag.StringVar(&cfg.BubbleMAC, "bubble-mac", "02:00:00:00:00:02", "Bubble TAP MAC address (distinct from gateway-mac)")
	flag.StringVar(&cfg.GatewayMode, "gateway-mode", "slirp", "Gateway backend: tap, slirp, or none")
	flag.StringVar(&cfg.Uplink, "uplink", "", "Bubble uplink interface for TAP mode")
	flag.StringVar(&cfg.UplinkAddress, "uplink-address", "", "Outer slirp guest IPv4 address for TAP egress SNAT")
	flag.StringVar(&cfg.ServiceCIDR, "service-cidr", "10.43.0.0/16", "Cluster Service CIDR")
	flag.StringVar(&cfg.Resolver, "resolver", "", "Resolver IP advertised to standalone plaidtainer")
	flag.IntVar(&cfg.VXLANPort, "vxlan-port", packet.DefaultVXLANPort, "VXLAN UDP port")
	flag.IntVar(&cfg.VXLANVNI, "vxlan-vni", packet.DefaultVNI, "VXLAN VNI")
	flag.StringVar(&cfg.VXLANBind, "vxlan-bind", "0.0.0.0", "VXLAN bind address")
	flag.StringVar(&cfg.SlirpBin, "slirp-bin", "slirp4netns", "Path to slirp4netns binary")
	flag.StringVar(&cfg.SlirpSocket, "slirp-socket", "/run/plaid/slirp.sock", "UNIX socket path for slirp BESS mode")
	flag.BoolVar(&cfg.SlirpDisableDNS, "slirp-disable-dns", false, "Disable slirp built-in DNS (default: false, enabling DNS forwarder at node-cidr base+3)")
	flag.BoolVar(&cfg.SlirpHostLoopback, "slirp-host-loopback", true, "Enable access to host 127.0.0.1 via slirp")
	flag.BoolVar(&cfg.EnableSlirp, "enable-slirp", true, "Enable slirp4netns for outbound traffic")
	flag.StringVar(&cfg.Routes, "routes", "", "Initial overlay routes, comma-separated (e.g. '10.244.2.0/24=node.local')")
	flag.StringVar(&cfg.Kubeconfig, "kubeconfig", "", "Path to kubeconfig file (Kubernetes mode only; defaults to in-cluster service account)")
	flag.StringVar(&cfg.NodeName, "node-name", "", "Local node name (Kubernetes mode only; defaults to NODE_NAME env or hostname)")
	flag.IntVar(&cfg.MTU, "mtu", 1450, "Effective MTU for container TAP interfaces and slirp (default: 1450, allowing 50B VXLAN overhead on 1500B host MTU)")

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
		mode = "static"
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

	if cfg.NodeCIDR == "" {
		// Kubernetes Mode (default)
		var err error
		k8sCtrl, err = k8s.NewControllerFromConfig(k8s.Config{
			Kubeconfig: cfg.Kubeconfig,
			NodeName:   cfg.NodeName,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Kubernetes controller: %w", err)
		}

		fmt.Printf("[plaidd] Kubernetes mode active. Discovering pod CIDR for node %q...\n", k8sCtrl.NodeName())
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		_, podNet, hostIP, err := k8sCtrl.GetLocalNode(ctx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve local node CIDR from Kubernetes: %w", err)
		}

		cfg.NodeCIDR = podNet.String()
		fmt.Printf("[plaidd] Discovered node CIDR from Kubernetes: %s (host IP: %s)\n", cfg.NodeCIDR, hostIP)
	} else {
		fmt.Printf("[plaidd] Static mode active [NodeCIDR=%s]\n", cfg.NodeCIDR)
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
		cfg.ServiceCIDR = "10.43.0.0/16"
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
		if _, err := net.ParseMAC(cfg.BubbleMAC); err != nil {
			return nil, fmt.Errorf("invalid bubble-mac: %w", err)
		}
		if cfg.BubbleMAC == cfg.GatewayMAC {
			return nil, fmt.Errorf("bubble-mac must differ from gateway-mac")
		}
	}

	if cfg.MTU <= 0 {
		if envMTU := os.Getenv("PLAID_MTU"); envMTU != "" {
			if parsed, err := strconv.Atoi(envMTU); err == nil && parsed > 0 {
				cfg.MTU = parsed
			}
		}
	}
	if cfg.MTU <= 0 {
		cfg.MTU = 1450
	}
	if cfg.MTU < 576 || cfg.MTU > 65535 {
		return nil, fmt.Errorf("invalid MTU %d: must be between 576 and 65535", cfg.MTU)
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
	}, routes, b)

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
		healthErr:    make(chan error, 1),
	}

	// Connect bridge handlers
	b.SetOverlayHandler(overlay.Send)
	if slirpMgr != nil {
		b.SetOutboundHandler(slirpMgr.WriteFrame)
	}

	d.apiServer = api.NewServer(cfg.SocketPath, d, d, d)
	d.apiServer.SetFilterHandler(d)
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
		d.attachment, err = gateway.Start(gateway.Config{Name: "plaid0", Uplink: d.cfg.Uplink, GuestAddress: net.ParseIP(d.cfg.UplinkAddress), GatewayIP: net.ParseIP(d.cfg.GatewayIP), GatewayMAC: mac, NodeCIDR: nodeNet, ClusterCIDR: clusterNet, ServiceCIDR: serviceNet, MTU: d.cfg.MTU})
		if err != nil {
			startErr = err
			return fmt.Errorf("gateway setup: %w", err)
		}
		d.bubbleEP = tap.NewTapEndpoint("bubble", "plaid0", net.ParseIP(d.cfg.GatewayIP), mac, d.attachment.File)
		d.bridge.SetBubbleEndpoint(d.bubbleEP)
		go d.readBubble(ctx)
		fmt.Printf("[plaidd] TAP gateway ready: %s/%s, MAC=%s, uplink=%s, guest=%s, cluster route=%s\n", d.cfg.GatewayIP, d.cfg.NodeCIDR, mac, d.cfg.Uplink, d.cfg.UplinkAddress, d.cfg.ClusterCIDR)
	}

	if d.overlay != nil {
		if err := d.overlay.Start(ctx); err != nil {
			startErr = err
			return fmt.Errorf("failed to start overlay: %w", err)
		}
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

func (d *PlaidDaemon) Stop() {
	if d.cancelFunc != nil {
		d.cancelFunc()
	}
	if d.apiServer != nil {
		_ = d.apiServer.Close()
	}
	if d.overlay != nil {
		_ = d.overlay.Close()
	}
	if d.slirpMgr != nil {
		_ = d.slirpMgr.Close()
	}
	if d.attachment != nil {
		_ = d.attachment.Close()
		d.attachment = nil
	}
}

func (d *PlaidDaemon) readBubble(ctx context.Context) {
	buf := make([]byte, 65535)
	file := d.bubbleEP.File()
	for {
		n, err := file.Read(buf)
		if err != nil {
			if ctx.Err() == nil {
				select {
				case d.healthErr <- err:
				default:
				}
			}
			return
		}
		if n > 0 {
			_ = d.bridge.ProcessFrame(d.bubbleEP, buf[:n])
		}
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

	ep := tap.NewTapEndpoint(epID, req.PodName, epIP, epMAC, epFile)
	if err := d.bridge.AddEndpoint(ep); err != nil {
		_ = epFile.Close()
		return err
	}

	ep.StartReadLoop(context.Background(), d.bridge)
	fmt.Printf("[plaidd] Added endpoint %s (IP=%s, MAC=%s)\n", epID, epIP, epMAC)
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
		mode = "static"
	}

	return &api.Response{
		EndpointsCount: len(eps),
		RoutesCount:    len(d.routes.Routes()),
		Mode:           mode,
		NodeCIDR:       d.cfg.NodeCIDR,
		ClusterCIDR:    d.cfg.ClusterCIDR,
		GatewayIP:      d.cfg.GatewayIP,
		GatewayMode:    d.cfg.GatewayMode,
		GatewayReady:   d.cfg.GatewayMode != "tap" || d.attachment != nil,
		GatewayMAC:     d.cfg.GatewayMAC,
		BubbleMAC:      d.cfg.BubbleMAC,
		Uplink:         d.cfg.Uplink,
		UplinkAddress:  d.cfg.UplinkAddress,
		Resolver:       d.cfg.Resolver,
		MTU:            d.cfg.MTU,
		Endpoints:      epList,
	}, nil
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
