package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"plaid/pkg/api"
	"plaid/pkg/direct"
	"plaid/pkg/handoff"
	"plaid/pkg/netcfg"
	"plaid/pkg/version"
	"plaid/pkg/vxlan"
)

func main() {
	var socketPath string
	flag.StringVar(&socketPath, "socket", netcfg.SocketPath(""), "Path to plaidd UNIX domain socket (default: $PLAID_SOCKET_PATH, else "+netcfg.DefaultSocketPath+")")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	if args[0] == "version" || args[0] == "--version" || args[0] == "-v" {
		fmt.Printf("plaidctl version %s (commit: %s, built: %s)\n", version.Version, version.GitCommit, version.BuildTime)
		os.Exit(0)
	}

	if args[0] == "overlay-socket" {
		overlaySocket(args[1:])
		return
	}
	if args[0] == "socket-factory" {
		socketFactory(args[1:])
		return
	}

	client := api.NewClient(socketPath)

	switch args[0] {
	case "status":
		sf := flag.NewFlagSet("status", flag.ExitOnError)
		containers := sf.Bool("containers", false, "also list the direct path's counters of each container")
		_ = sf.Parse(args[1:])
		status, err := client.GetStatus()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting status: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Node CIDR:    %s\n", status.NodeCIDR)
		fmt.Printf("Cluster CIDR: %s\n", status.ClusterCIDR)
		fmt.Printf("Gateway IP:   %s\n", status.GatewayIP)
		fmt.Printf("Gateway:      %s (ready=%t, bubble MAC=%s)\n", status.GatewayMode, status.GatewayReady, status.BubbleMAC)
		if status.Uplink != "" {
			fmt.Printf("Uplink:       %s (%s)\n", status.Uplink, status.UplinkAddress)
		}
		if status.Resolver != "" {
			fmt.Printf("Resolver:     %s\n", status.Resolver)
		}
		fmt.Printf("MTU:          %d", status.MTU)
		if status.UplinkMTU != 0 {
			fmt.Printf(" (uplink %d)", status.UplinkMTU)
		}
		fmt.Println()
		if status.OverlayTransport != "" {
			fmt.Printf("Overlay:      %s socket %s (sockets=%d rx/%d tx, rx per socket=%v, tx=%d in %d sends, tx errors=%d, rx=%d in %d reads, rx invalid=%d, rx dropped=%d, queue drops=%d, socket drops=%d, rcvbuf=%d, offload=%s, super=%s)\n",
				status.OverlayTransport, status.OverlayAddress, status.OverlayRxSockets, status.OverlayTxSockets, status.OverlayRxPerSocket,
				status.OverlayTxPackets, status.OverlayTxSends, status.OverlayTxErrors,
				status.OverlayRxPackets, status.OverlayRxReads, status.OverlayRxInvalid, status.OverlayRxDropped,
				status.OverlayRxQueueDrops, status.OverlaySocketDrops, status.OverlayRcvBuf, orNone(status.OverlayOffload), superSummary(status.OverlaySuper))
		}
		if r := status.OverlayRDMA; r != nil {
			state := "active"
			if !r.PortActive {
				state = "not active"
			}
			if r.Failed {
				state = "failed (a fatal device error: RDMA ended)"
			}
			fmt.Printf("RDMA:         %s (LID %d, MTU %d, %d Gb/s, %s; %d MiB registered; receives posted per lane %v): tx %d messages, %d bytes, rx %d messages, %d bytes; dropped %d at the in-flight cap, %d for want of a slot; receives ran low %d times; completion errors %d; links up %d, failed %d\n",
				r.Port, r.LID, r.MTU, r.RateGbps, state, r.PinnedBytes>>20, r.RecvPosted, r.TxMessages, r.TxBytes, r.RxMessages, r.RxBytes,
				r.TxFull, r.TxNoSlot, r.RxLow, r.CompletionErrors, r.LinksUp, r.LinkFailures)
			for _, p := range r.Peers {
				fmt.Printf("              %s: %s", p.Address, p.State)
				if p.QPs > 0 {
					fmt.Printf(", %d QPs", p.QPs)
				}
				if p.Failures > 0 {
					fmt.Printf(", %d failures", p.Failures)
				}
				if p.Why != "" {
					fmt.Printf(" (%s)", p.Why)
				}
				fmt.Println()
			}
		}
		if o := status.TapOffload; o != nil {
			fmt.Printf("TAP offload:  enabled=%t, on %d TAPs (read %d super-frames as %d segments, %d dropped; wrote %d super-frames of %d segments; passed %d super-frames whole, %d bytes)\n",
				o.Enabled, o.Endpoints, o.ReadSuperFrames, o.ReadSegments, o.ReadDropped, o.WriteSuperFrames, o.WriteSegments,
				o.PassedSuperFrames, o.PassedBytes)
			if status.UplinkMTU > status.MTU {
				fmt.Printf("TAP uplink:   joined %d pod super-frames into packets for the bubble; made %d large TCP frames from it super-frames; raised the MSS of %d handshakes to it, lowered it in %d to pods; fragmented %d packets into %d fragments; dropped %d above the pod MTU\n",
					o.JoinedForUplink, o.UplinkSuperFrames, o.MSSRaised, o.MSSLowered, o.Fragmented, o.Fragments, o.OversizeDropped)
			}
		}
		if p := status.TapPipeline; p != nil {
			fmt.Printf("TAP pipeline: on %d TAPs, %d queues (read: %d inline, %d queued, queue full %d times; write: %d inline, %d queued, queue full %d times)\n",
				p.Endpoints, p.Queues, p.ReadInline, p.ReadQueued, p.ReadQueueFull, p.WriteInline, p.WriteQueued, p.WriteQueueFull)
		}
		if status.ServiceEngine != nil {
			e := status.ServiceEngine
			fmt.Printf("Services:     %s via %s, table from %s (synced=%t): %d entries, %d backends, %d flows\n",
				status.ServiceCIDR, status.ServiceAddress, status.ServiceManager, e.Synced, e.Services, e.Backends, e.Flows)
			fmt.Printf("              flows created=%d expired=%d purged=%d; packets out=%d back=%d refused=%d; %d of the packets were TCP super-frames translated whole\n",
				e.FlowsCreated, e.FlowsExpired, e.FlowsPurged, e.Forwarded, e.Returned, e.Rejected, e.SuperFrames)
			if np := status.NodePorts; np != nil {
				fmt.Printf("              node ports: %d wanted, %d published (removed=%d repaired=%d passes=%d) %s\n", np.Desired, np.Exposed, np.Removed, np.Repaired, np.Passes, np.LastError)
				for _, f := range np.Failed {
					fmt.Printf("              not published: %s %d after %d attempts: %s\n", f.Proto, f.Port, f.Attempts, f.Reason)
				}
			}
			for _, d := range e.RecentDrops {
				fmt.Printf("              dropped: %s\n", d)
			}
			if w := status.ServiceWatch; w != nil {
				fmt.Printf("              watch: synced=%t, tables=%d, unsupported settings=%d, watch errors=%d %s\n",
					w.Synced, w.Tables, w.Problems, w.WatchErrors, w.LastWatchError)
			}
			fmt.Printf("              drops: no service=%d, no flow=%d, flow limit=%d, no port=%d, unsupported=%d\n",
				e.DropsNoService, e.DropsNoFlow, e.DropsFlowLimit, e.DropsPorts, e.DropsUnsupported)
		}
		if d := status.Direct; d != nil {
			printDirect(os.Stdout, d, *containers)
		}
		fmt.Printf("Endpoints:    %d\n", status.EndpointsCount)
		for _, ep := range status.Endpoints {
			fmt.Printf("  - %s\n", ep)
		}
		fmt.Printf("Routes Count: %d\n", status.RoutesCount)

	case "service":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: plaidctl service <list|set> ...")
			os.Exit(1)
		}
		switch args[1] {
		case "list":
			resp, err := client.ListServices()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error listing services: %v\n", err)
				os.Exit(1)
			}
			for _, s := range resp.Services {
				fmt.Printf("%s:%d/%s  %s  flows=%d\n", s.VIP, s.Port, s.Protocol, s.ID, s.Flows)
				for _, b := range s.Backends {
					state := "eligible"
					if !b.Eligible {
						state = "not eligible"
					}
					fmt.Printf("  -> %s:%d (%s)\n", b.IP, b.Port, state)
				}
			}
		case "set":
			if len(args) != 3 {
				fmt.Fprintln(os.Stderr, "Usage: plaidctl service set <file.json|->  (a JSON array of services; plaidd must run with --disable-kubernetes)")
				os.Exit(1)
			}
			var data []byte
			var err error
			if args[2] == "-" {
				data, err = io.ReadAll(os.Stdin)
			} else {
				data, err = os.ReadFile(args[2])
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error reading %s: %v\n", args[2], err)
				os.Exit(1)
			}
			var entries []api.ServiceEntry
			if err := json.Unmarshal(data, &entries); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %s is not a JSON array of services: %v\n", args[2], err)
				os.Exit(1)
			}
			if err := client.SetServices(entries); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Service table set: %d entries\n", len(entries))
		default:
			fmt.Fprintf(os.Stderr, "Unknown service command: %s\n", args[1])
			os.Exit(1)
		}

	case "route":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: plaidctl route <list|add|del> ...")
			os.Exit(1)
		}
		switch args[1] {
		case "add":
			if len(args) < 4 {
				fmt.Fprintln(os.Stderr, "Usage: plaidctl route add <subnet-cidr> <remote-host-ip-or-name> [vni] [port] [vtep-mac]")
				os.Exit(1)
			}
			req := &api.Request{
				Subnet:       args[2],
				RemoteHostIP: args[3],
			}
			if len(args) > 4 {
				vni, _ := strconv.Atoi(args[4])
				req.VNI = uint32(vni)
			}
			if len(args) > 5 {
				port, _ := strconv.Atoi(args[5])
				req.Port = port
			}
			if len(args) > 6 {
				req.VtepMAC = args[6]
			}
			if err := client.AddRoute(req); err != nil {
				fmt.Fprintf(os.Stderr, "Error adding route: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Route added: %s -> %s\n", req.Subnet, req.RemoteHostIP)

		case "list":
			routes, err := client.ListRoutes()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error listing routes: %v\n", err)
				os.Exit(1)
			}
			for _, r := range routes {
				fmt.Printf("%s -> %s\n", r.Subnet, r.RemoteHostIP)
			}

		case "del", "remove":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "Usage: plaidctl route del <subnet-cidr>")
				os.Exit(1)
			}
			if err := client.RemoveRoute(args[2]); err != nil {
				fmt.Fprintf(os.Stderr, "Error removing route: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Route removed: %s\n", args[2])

		default:
			fmt.Fprintf(os.Stderr, "Unknown route command: %s\n", args[1])
			os.Exit(1)
		}

	case "filter":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: plaidctl filter <list|add|del> ...")
			os.Exit(1)
		}
		switch args[1] {
		case "list":
			rules, err := client.ListFilterRules()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error listing filter rules: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Active filter rules (%d):\n", len(rules))
			for _, r := range rules {
				fmt.Printf("  - %s\n", r)
			}
		case "del":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "Usage: plaidctl filter del <rule-id>")
				os.Exit(1)
			}
			if err := client.RemoveFilterRule(args[2]); err != nil {
				fmt.Fprintf(os.Stderr, "Error deleting filter rule: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Filter rule removed: %s\n", args[2])
		default:
			fmt.Fprintf(os.Stderr, "Unknown filter command: %s\n", args[1])
			os.Exit(1)
		}

	case "endpoint":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: plaidctl endpoint <del|remove> <endpoint-id>")
			os.Exit(1)
		}
		switch args[1] {
		case "del", "remove":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "Usage: plaidctl endpoint del <endpoint-id>")
				os.Exit(1)
			}
			if err := client.RemoveEndpoint(args[2], args[2]); err != nil {
				fmt.Fprintf(os.Stderr, "Error removing endpoint: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Endpoint removed: %s\n", args[2])
		default:
			fmt.Fprintf(os.Stderr, "Unknown endpoint command: %s\n", args[1])
			os.Exit(1)
		}

	default:
		printUsage()
		os.Exit(1)
	}
}

// overlaySocket is the helper the deployment runs in the physical host's network
// namespace: it opens the overlay's UDP sockets there and hands them to plaidd, which runs
// inside the bubble. It forwards nothing and exits once plaidd has acknowledged.
func overlaySocket(args []string) {
	fs := flag.NewFlagSet("overlay-socket", flag.ExitOnError)
	bind := fs.String("bind", "", "IPv4 address to bind on the host (the address the other nodes send to)")
	port := fs.Int("port", 8472, "UDP port")
	sockets := fs.Int("sockets", vxlan.DefaultSockets, "receive sockets (1 to 16), in one SO_REUSEPORT group on the port, and four times as many send sockets")
	path := fs.String("handoff", "", "UNIX socket on which plaidd is waiting for the descriptors")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for plaidd to listen")
	_ = fs.Parse(args)
	ip := net.ParseIP(*bind)
	if ip == nil || ip.To4() == nil || *path == "" || *sockets < 1 || *sockets > vxlan.MaxSockets {
		fmt.Fprintln(os.Stderr, "Usage: plaidctl overlay-socket --bind <ipv4> [--port 8472] [--sockets 4] --handoff <socket> [--timeout 10m]")
		os.Exit(2)
	}

	// Bind first: a port that is taken is a failure right away, and holding it while
	// waiting reserves it for plaidd.
	rx, tx, err := vxlan.ListenSockets("udp4", ip.To4(), *port, *sockets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot open the overlay sockets %s:%d: %v\n", ip, *port, err)
		os.Exit(1)
	}
	var files []*os.File
	for _, conn := range append(rx, tx...) {
		defer conn.Close()
		file, err := conn.File()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer file.Close()
		files = append(files, file)
	}
	req := handoff.Request{Type: handoff.TypeOverlaySocket, Bind: ip.String(), Port: *port, RxSockets: len(rx), TxSockets: len(tx)}
	if err := handoff.Send(context.Background(), *path, req, files, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "Error: overlay socket handoff failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Overlay sockets %s:%d (%d receive, %d send) handed to plaidd\n", ip, *port, len(rx), len(tx))
}

// socketFactory is the program the deployment runs in the physical host's network
// namespace for the direct path: it makes TCP sockets there and gives them to the
// supervisors of the containers that run in the bubble, which connect and bind them. It
// runs until it is stopped.
func socketFactory(args []string) {
	fs := flag.NewFlagSet("socket-factory", flag.ExitOnError)
	path := fs.String("listen", "", "UNIX socket to serve the factory on (the user's own, mode 0600)")
	_ = fs.Parse(args)
	if *path == "" {
		fmt.Fprintln(os.Stderr, "Usage: plaidctl socket-factory --listen <socket>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	fmt.Printf("Socket factory on %s\n", *path)
	if err := direct.ServeFactory(ctx, *path); err != nil {
		fmt.Fprintf(os.Stderr, "Error: socket factory: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`plaidctl - Control utility for Plaid daemon (plaidd)

Usage:
  plaidctl [-socket <path>] <command> [arguments]

Commands:
  version                                         Display version information
  status [-containers]                            Display daemon status & endpoints (-containers: the direct path's counters per container)
  overlay-socket --bind <ip> --handoff <socket>   Open the overlay UDP sockets here and hand them to plaidd (host helper; --sockets N)
  socket-factory --listen <socket>                Make TCP sockets here for the direct path of the containers in the bubble (host helper)
  endpoint del <endpoint-id>                      Remove a container endpoint
  service list                                    List the Service engine's table and flows
  service set <file.json|->                       Replace the Service table (plaidd --disable-kubernetes only)
  route list                                      List the overlay's routes
  route add <subnet> <remote-host> [vni] [port]   Add a cross-host overlay route
  route del <subnet>                              Remove an overlay route
  filter list                                     List active packet filter rules
  filter del <rule-id>                            Remove a packet filter rule
`)
}

// printDirect writes the direct path's section of the status: the node's totals, and with
// containers the counters of each running supervisor.
func printDirect(w io.Writer, d *api.DirectStatus, containers bool) {
	fmt.Fprintf(w, "Direct path:  %d supervisors running, %d ended: %s\n", d.Running, d.Ended, directCounters(d.Total))
	fmt.Fprintf(w, "              fallbacks to the overlay or the pod network: %s\n", orNone(direct.FormatReasons(d.Total.Fallbacks)))
	fmt.Fprintf(w, "              connections turned away: %s\n", orNone(direct.FormatReasons(d.Total.Rejected)))
	if containers {
		for _, c := range d.Containers {
			fmt.Fprintf(w, "  - %s (pid %d): %s; fallbacks: %s; turned away: %s\n", c.Name, c.PID, directCounters(c.Counters),
				orNone(direct.FormatReasons(c.Counters.Fallbacks)), orNone(direct.FormatReasons(c.Counters.Rejected)))
		}
	}
}

func directCounters(c api.DirectCounters) string {
	return fmt.Sprintf("connects=%d binds=%d accepts=%d, proxied=%d (failed %d), errors=%d",
		c.Connects, c.Binds, c.Accepts, c.Proxied, c.ProxyFailed, c.Errors)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// superSummary is the overlay's super-frame counters in a few words (see api.OverlaySuperStatus).
func superSummary(o *api.OverlaySuperStatus) string {
	if o == nil {
		return "none"
	}
	state := "off"
	if o.Enabled {
		state = "on"
	}
	chunks := "of the segment size"
	if o.LinkMTU > 0 {
		chunks = fmt.Sprintf("of link MTU %d", o.LinkMTU)
	}
	return fmt.Sprintf("%s tx %d/%d chunks %s rx %d lost %d invalid %d evicted %d peers %d send failures %d",
		state, o.Tx, o.TxChunks, chunks, o.Rx, o.Partial, o.Invalid, o.Evicted, o.Peers, o.TxFailed)
}
