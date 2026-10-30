package api

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"
)

// Client is a client to communicate with the plaidd API server.
type Client struct {
	socketPath string
	timeout    time.Duration
}

// NewClient creates a new API client.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		timeout:    10 * time.Second,
	}
}

// Send sends a request to plaidd, passing the file descriptors given, if any, via SCM_RIGHTS.
func (c *Client) Send(req *Request, passFDs ...int) (*Response, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to plaidd API at %s: %w", c.socketPath, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(c.timeout)
	_ = conn.SetDeadline(deadline)

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("connection is not a unix domain socket")
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	if len(data) > MaxMessageSize {
		return nil, fmt.Errorf("request payload exceeds maximum size %d", MaxMessageSize)
	}

	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(data)))
	copy(frame[4:], data)

	var oob []byte
	if len(passFDs) > 0 {
		oob = syscall.UnixRights(passFDs...)
	}

	n, _, err := unixConn.WriteMsgUnix(frame, oob, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to send request to plaidd: %w", err)
	}
	if n < len(frame) {
		var written int = n
		for written < len(frame) {
			nw, err := unixConn.Write(frame[written:])
			if err != nil {
				return nil, fmt.Errorf("failed to write complete request to plaidd: %w", err)
			}
			written += nw
		}
	}

	// Read 4-byte response length header
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return nil, fmt.Errorf("failed to read response header from plaidd: %w", err)
	}

	respLen := binary.BigEndian.Uint32(lenBuf)
	if respLen > MaxMessageSize {
		return nil, fmt.Errorf("response size %d exceeds limit %d", respLen, MaxMessageSize)
	}

	respBuf := make([]byte, respLen)
	if _, err := io.ReadFull(conn, respBuf); err != nil {
		return nil, fmt.Errorf("failed to read response body from plaidd: %w", err)
	}

	var resp Response
	if err := json.Unmarshal(respBuf, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &resp, nil
}

// AddEndpoint sends an AddEndpoint request along with the opened TAP file descriptors:
// one for each queue, the first of which an older plaidd keeps and closes the rest.
func (c *Client) AddEndpoint(req *Request, tapFDs ...int) error {
	req.Action = ActionAddEndpoint
	resp, err := c.Send(req, tapFDs...)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("add endpoint failed: %s", resp.Error)
	}
	return nil
}

// RemoveEndpoint sends a RemoveEndpoint request.
func (c *Client) RemoveEndpoint(podID, containerID string) error {
	req := &Request{
		Action:      ActionRemoveEndpoint,
		PodID:       podID,
		ContainerID: containerID,
	}
	resp, err := c.Send(req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("remove endpoint failed: %s", resp.Error)
	}
	return nil
}

// AddRoute sends an AddRoute request.
func (c *Client) AddRoute(req *Request) error {
	req.Action = ActionAddRoute
	resp, err := c.Send(req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("add route failed: %s", resp.Error)
	}
	return nil
}

// RemoveRoute sends a RemoveRoute request.
func (c *Client) RemoveRoute(subnet string) error {
	req := &Request{
		Action: ActionRemoveRoute,
		Subnet: subnet,
	}
	resp, err := c.Send(req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("remove route failed: %s", resp.Error)
	}
	return nil
}

// ListRoutes returns the overlay's routes: the subnets of the remote nodes and the
// addresses of the nodes.
func (c *Client) ListRoutes() ([]RouteInfo, error) {
	resp, err := c.Send(&Request{Action: ActionListRoutes})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("list routes failed: %s", resp.Error)
	}
	return resp.Routes, nil
}

// GetStatus retrieves daemon status.
func (c *Client) GetStatus() (*Response, error) {
	req := &Request{
		Action: ActionGetStatus,
	}
	return c.Send(req)
}

// AddFilterRule sends an AddFilterRule request.
func (c *Client) AddFilterRule(req *Request) error {
	req.Action = ActionAddFilterRule
	resp, err := c.Send(req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("add filter rule failed: %s", resp.Error)
	}
	return nil
}

// RemoveFilterRule sends a RemoveFilterRule request.
func (c *Client) RemoveFilterRule(ruleID string) error {
	req := &Request{
		Action: ActionRemoveFilterRule,
		RuleID: ruleID,
	}
	resp, err := c.Send(req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("remove filter rule failed: %s", resp.Error)
	}
	return nil
}

// ListFilterRules retrieves all active filter rules.
func (c *Client) ListFilterRules() ([]string, error) {
	req := &Request{
		Action: ActionListFilterRules,
	}
	resp, err := c.Send(req)
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("list filter rules failed: %s", resp.Error)
	}
	return resp.FilterRules, nil
}

// SetServices replaces the Service engine's table.
func (c *Client) SetServices(entries []ServiceEntry) error {
	resp, err := c.Send(&Request{Action: ActionSetServices, Services: entries})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("set_services failed: %s", resp.Error)
	}
	return nil
}

// ListServices returns the Service engine's table with the flows of each entry.
func (c *Client) ListServices() (*Response, error) {
	resp, err := c.Send(&Request{Action: ActionListServices})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("list_services failed: %s", resp.Error)
	}
	return resp, nil
}

// ReportDirect tells plaidd what a supervisor of the direct path has done.
func (c *Client) ReportDirect(report DirectReport) error {
	resp, err := c.Send(&Request{Action: ActionReportDirect, Direct: &report})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("report_direct failed: %s", resp.Error)
	}
	return nil
}
