package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"

	"plaid/pkg/api"
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
		fmt.Printf("Endpoints:    %d\n", status.EndpointsCount)
		for _, ep := range status.Endpoints {
			fmt.Printf("  - %s\n", ep)
		}
		fmt.Printf("Routes Count: %d\n", status.RoutesCount)

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

func printUsage() {
	fmt.Print(`plaidctl - Control utility for Plaid daemon (plaidd)

Usage:
  plaidctl [-socket <path>] <command> [arguments]

Commands:
  version                                         Display version information
  status                                          Display daemon status & endpoints
  endpoint del <endpoint-id>                      Remove a container endpoint
  route add <subnet> <remote-host> [vni] [port]   Add a cross-host overlay route
  route del <subnet>                              Remove an overlay route
  filter list                                     List active packet filter rules
  filter del <rule-id>                            Remove a packet filter rule
`)
}
