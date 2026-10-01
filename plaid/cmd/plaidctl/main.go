package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"plaid/pkg/api"
	"plaid/pkg/handoff"
	"plaid/pkg/netcfg"
	"plaid/pkg/version"
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

	client := api.NewClient(socketPath)

	switch args[0] {
	case "status":
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
		fmt.Printf("MTU:          %d\n", status.MTU)
		if status.OverlayTransport != "" {
			fmt.Printf("Overlay:      %s socket %s (tx=%d, tx errors=%d, rx=%d, rx invalid=%d, rx dropped=%d)\n",
				status.OverlayTransport, status.OverlayAddress, status.OverlayTxPackets, status.OverlayTxErrors,
				status.OverlayRxPackets, status.OverlayRxInvalid, status.OverlayRxDropped)
		}
		if status.ServiceEngine != nil {
			e := status.ServiceEngine
			fmt.Printf("Services:     %s via %s, table from %s (synced=%t): %d entries, %d backends, %d flows\n",
				status.ServiceCIDR, status.ServiceAddress, status.ServiceManager, e.Synced, e.Services, e.Backends, e.Flows)
			fmt.Printf("              flows created=%d expired=%d purged=%d; packets out=%d back=%d refused=%d\n",
				e.FlowsCreated, e.FlowsExpired, e.FlowsPurged, e.Forwarded, e.Returned, e.Rejected)
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
			fmt.Fprintln(os.Stderr, "Usage: plaidctl route <add|del> ...")
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
// namespace: it opens the overlay's UDP socket there and hands it to plaidd, which runs
// inside the bubble. It forwards nothing and exits once plaidd has acknowledged.
func overlaySocket(args []string) {
	fs := flag.NewFlagSet("overlay-socket", flag.ExitOnError)
	bind := fs.String("bind", "", "IPv4 address to bind on the host (the address the other nodes send to)")
	port := fs.Int("port", 8472, "UDP port")
	path := fs.String("handoff", "", "UNIX socket on which plaidd is waiting for the descriptor")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for plaidd to listen")
	_ = fs.Parse(args)
	ip := net.ParseIP(*bind)
	if ip == nil || ip.To4() == nil || *path == "" {
		fmt.Fprintln(os.Stderr, "Usage: plaidctl overlay-socket --bind <ipv4> [--port 8472] --handoff <socket> [--timeout 10m]")
		os.Exit(2)
	}

	// Bind first: a port that is taken is a failure right away, and holding it while
	// waiting reserves it for plaidd.
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip.To4(), Port: *port})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot open the overlay socket %s:%d: %v\n", ip, *port, err)
		os.Exit(1)
	}
	defer conn.Close()
	file, err := conn.File()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()
	err = handoff.Send(context.Background(), *path, handoff.Request{Type: handoff.TypeOverlaySocket, Bind: ip.String(), Port: *port}, file, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: overlay socket handoff failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Overlay socket %s:%d handed to plaidd\n", ip, *port)
}

func printUsage() {
	fmt.Print(`plaidctl - Control utility for Plaid daemon (plaidd)

Usage:
  plaidctl [-socket <path>] <command> [arguments]

Commands:
  version                                         Display version information
  status                                          Display daemon status & endpoints
  overlay-socket --bind <ip> --handoff <socket>   Open the overlay UDP socket here and hand it to plaidd (host helper)
  endpoint del <endpoint-id>                      Remove a container endpoint
  service list                                    List the Service engine's table and flows
  service set <file.json|->                       Replace the Service table (plaidd --disable-kubernetes only)
  route add <subnet> <remote-host> [vni] [port]   Add a cross-host overlay route
  route del <subnet>                              Remove an overlay route
  filter list                                     List active packet filter rules
  filter del <rule-id>                            Remove a packet filter rule
`)
}
