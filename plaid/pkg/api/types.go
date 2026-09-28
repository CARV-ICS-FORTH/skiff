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
)

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
	FilterRules    []string `json:"filter_rules,omitempty"`
}
