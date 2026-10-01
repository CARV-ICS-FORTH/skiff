package api

// Action represents the API operation requested.
type Action string

const (
	// MaxMessageSize defines the maximum framed message size (16 MB).
	MaxMessageSize = 16 * 1024 * 1024

	ActionAddEndpoint      Action = "add_endpoint"
	ActionRemoveEndpoint   Action = "remove_endpoint"
	ActionAddRoute         Action = "add_route"
	ActionRemoveRoute      Action = "remove_route"
	ActionGetStatus        Action = "get_status"
	ActionAddFilterRule    Action = "add_filter_rule"
	ActionRemoveFilterRule Action = "remove_filter_rule"
	ActionListFilterRules  Action = "list_filter_rules"
	ActionSetServices      Action = "set_services"
	ActionListServices     Action = "list_services"
)

// ServiceEntry is one Service address, port and protocol with its backends, as the
// Service engine takes it.
type ServiceEntry struct {
	ID       string         `json:"id"`
	VIP      string         `json:"vip"`
	Port     uint16         `json:"port"`
	Protocol string         `json:"protocol"` // "tcp" or "udp"
	Backends []BackendEntry `json:"backends,omitempty"`
	// Flows is the number of live flows of the entry (in listings).
	Flows int `json:"flows,omitempty"`
}

// BackendEntry is one backend; only eligible backends take new flows.
type BackendEntry struct {
	IP       string `json:"ip"`
	Port     uint16 `json:"port"`
	Eligible bool   `json:"eligible"`
}

// ServiceWatchStatus says how the Kubernetes watch that feeds the Service table is doing.
type ServiceWatchStatus struct {
	Synced         bool   `json:"synced"` // the first table was built from complete caches
	Tables         uint64 `json:"tables"` // tables pushed to the engine
	Problems       int    `json:"problems"`
	WatchErrors    uint64 `json:"watch_errors"`
	LastWatchError string `json:"last_watch_error,omitempty"`
}

// NodePortStatus says how publishing the NodePorts is going: how many are wanted and exposed,
// which are not and why.
type NodePortStatus struct {
	Desired   int              `json:"desired"`
	Exposed   int              `json:"exposed"`
	Failed    []NodePortFailed `json:"failed,omitempty"`
	Passes    uint64           `json:"passes"`
	Removed   uint64           `json:"removed"`
	Repaired  uint64           `json:"repaired"`
	LastError string           `json:"last_error,omitempty"`
}

// NodePortFailed is a node port that is not published.
type NodePortFailed struct {
	Proto    string `json:"proto"`
	Port     int    `json:"port"`
	Reason   string `json:"reason"`
	Attempts int    `json:"attempts"`
}

// ServiceStats are the Service engine's gauges and counters.
type ServiceStats struct {
	Synced           bool   `json:"synced"`
	Services         int    `json:"services"`
	Backends         int    `json:"backends"`
	Flows            int    `json:"flows"`
	FlowsCreated     uint64 `json:"flows_created"`
	FlowsExpired     uint64 `json:"flows_expired"`
	FlowsPurged      uint64 `json:"flows_purged"`
	Forwarded        uint64 `json:"forwarded"`
	Returned         uint64 `json:"returned"`
	Rejected         uint64 `json:"rejected"`
	DropsNoService   uint64 `json:"drops_no_service"`
	DropsNoFlow      uint64 `json:"drops_no_flow"`
	DropsFlowLimit   uint64 `json:"drops_flow_limit"`
	DropsPorts       uint64 `json:"drops_ports"`
	DropsUnsupported uint64 `json:"drops_unsupported"`
	// RecentDrops describes the last packets the engine dropped as unsupported.
	RecentDrops []string `json:"recent_drops,omitempty"`
}

// Request is the JSON request sent across the IPC socket.
type Request struct {
	Action Action `json:"action"`

	// AddEndpoint / RemoveEndpoint params
	PodID        string `json:"pod_id,omitempty"`
	PodName      string `json:"pod_name,omitempty"`
	PodNamespace string `json:"pod_namespace,omitempty"`
	ContainerID  string `json:"container_id,omitempty"`
	IP           string `json:"ip,omitempty"`
	MAC          string `json:"mac,omitempty"`
	NetnsPath    string `json:"netns_path,omitempty"`

	// AddRoute / RemoveRoute params
	Subnet       string `json:"subnet,omitempty"`
	RemoteHostIP string `json:"remote_host_ip,omitempty"`
	VtepMAC      string `json:"vtep_mac,omitempty"`
	VNI          uint32 `json:"vni,omitempty"`
	Port         int    `json:"port,omitempty"`

	// Filter Rule params
	RuleID       string `json:"rule_id,omitempty"`
	RuleSrcCIDR  string `json:"rule_src_cidr,omitempty"`
	RuleDstCIDR  string `json:"rule_dst_cidr,omitempty"`
	RuleProtocol uint8  `json:"rule_protocol,omitempty"`
	RuleSrcPort  uint16 `json:"rule_src_port,omitempty"`
	RuleDstPort  uint16 `json:"rule_dst_port,omitempty"`
	RuleAction   string `json:"rule_action,omitempty"`

	// SetServices params: the complete table of the Service engine.
	Services []ServiceEntry `json:"services,omitempty"`
}

// Response is the JSON response returned by plaidd.
type Response struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`

	// GetStatus fields
	EndpointsCount int      `json:"endpoints_count,omitempty"`
	RoutesCount    int      `json:"routes_count,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	NodeCIDR       string   `json:"node_cidr,omitempty"`
	ClusterCIDR    string   `json:"cluster_cidr,omitempty"`
	GatewayIP      string   `json:"gateway_ip,omitempty"`
	GatewayMode    string   `json:"gateway_mode,omitempty"`
	GatewayReady   bool     `json:"gateway_ready"`
	Ready          bool     `json:"ready"`
	GatewayMAC     string   `json:"gateway_mac,omitempty"`
	BubbleMAC      string   `json:"bubble_mac,omitempty"`
	Uplink         string   `json:"uplink,omitempty"`
	UplinkAddress  string   `json:"uplink_address,omitempty"`
	Resolver       string   `json:"resolver,omitempty"`
	MTU            int      `json:"mtu,omitempty"`
	Endpoints      []string `json:"endpoints,omitempty"`

	// The overlay transport: where its UDP socket came from, its address and counters.
	OverlayTransport string   `json:"overlay_transport,omitempty"`
	OverlayAddress   string   `json:"overlay_address,omitempty"`
	OverlayTxPackets uint64   `json:"overlay_tx_packets"`
	OverlayTxErrors  uint64   `json:"overlay_tx_errors"`
	OverlayRxPackets uint64   `json:"overlay_rx_packets"`
	OverlayRxInvalid uint64   `json:"overlay_rx_invalid"`
	OverlayRxDropped uint64   `json:"overlay_rx_dropped"`
	FilterRules      []string `json:"filter_rules,omitempty"`

	// The Service engine (tap mode): its address, range, counters and who writes its table
	// ("kubernetes" or "api"); and, from list_services, the table.
	ServiceAddress string              `json:"service_address,omitempty"`
	ServiceCIDR    string              `json:"service_cidr,omitempty"`
	ServiceEngine  *ServiceStats       `json:"service_engine,omitempty"`
	ServiceManager string              `json:"service_manager,omitempty"`
	ServiceWatch   *ServiceWatchStatus `json:"service_watch,omitempty"`
	NodePorts      *NodePortStatus     `json:"node_ports,omitempty"`
	Services       []ServiceEntry      `json:"services,omitempty"`
}
