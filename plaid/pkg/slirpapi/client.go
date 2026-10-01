// Package slirpapi is a client of slirp4netns's API socket, which adds, lists and removes
// the forwards of host ports to the guest (the bubble). A request is one JSON message on
// a connection of its own; the answer is one JSON message, which is either a result or an
// error with a description. slirp4netns stops moving packets while it serves a request,
// so the client asks for as little as it can and every request is bounded.
package slirpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Entry is one forward of a host address and port to a guest address and port.
type Entry struct {
	ID        int    `json:"id"`
	Proto     string `json:"proto"`
	HostAddr  string `json:"host_addr"`
	HostPort  int    `json:"host_port"`
	GuestAddr string `json:"guest_addr"`
	GuestPort int    `json:"guest_port"`
}

// maxAnswer bounds what is read from slirp4netns for one request.
const maxAnswer = 4 << 20

// APIError is an error slirp4netns reported for a request.
type APIError struct {
	Desc string
}

func (e *APIError) Error() string { return "slirp4netns: " + e.Desc }

// IsBindError reports whether adding a forward failed the way a host port that something
// else holds fails. slirp4netns 1.2.1 answers a refused add_hostfwd with "bad request:
// add_hostfwd: slirp_add_hostfwd failed" whatever the reason (a port that is taken is the
// one that happens in practice; the text does not say), so that answer counts too.
func (e *APIError) IsBindError() bool {
	d := strings.ToLower(e.Desc)
	return strings.Contains(d, "bind") || strings.Contains(d, "in use") || strings.Contains(d, "slirp_add_hostfwd failed")
}

// Client talks to one API socket.
type Client struct {
	Path    string
	Timeout time.Duration
}

// New creates a client with a five second bound per request.
func New(path string) *Client { return &Client{Path: path, Timeout: 5 * time.Second} }

func (c *Client) do(ctx context.Context, command string, args any) (json.RawMessage, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Path)
	if err != nil {
		return nil, fmt.Errorf("connect to the slirp4netns API at %s: %w", c.Path, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	req := map[string]any{"execute": command}
	if args != nil {
		req["arguments"] = args
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("slirp4netns %s: %w", command, err)
	}
	// One answer, then the connection ends: read what arrives until it does, or until the
	// bytes already form one complete JSON value.
	var answer []byte
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		answer = append(answer, buf[:n]...)
		if len(answer) > maxAnswer {
			return nil, errors.New("slirp4netns: the answer is too large")
		}
		if json.Valid(answer) {
			break
		}
		if err != nil {
			if err == io.EOF && len(answer) > 0 {
				return nil, fmt.Errorf("slirp4netns %s: incomplete answer", command)
			}
			if err == io.EOF {
				return nil, fmt.Errorf("slirp4netns %s: no answer", command)
			}
			return nil, fmt.Errorf("slirp4netns %s: %w", command, err)
		}
	}
	var msg struct {
		Return  json.RawMessage `json:"return"`
		Entries json.RawMessage `json:"entries"`
		Error   *struct {
			Desc string `json:"desc"`
		} `json:"error"`
	}
	if err := json.Unmarshal(answer, &msg); err != nil {
		return nil, fmt.Errorf("slirp4netns %s: malformed answer: %w", command, err)
	}
	switch {
	case msg.Error != nil:
		return nil, &APIError{Desc: msg.Error.Desc}
	case msg.Return != nil:
		return msg.Return, nil
	case msg.Entries != nil:
		// list_hostfwd answers {"entries": [...]} with slirp4netns 1.2.
		return answer, nil
	}
	return nil, fmt.Errorf("slirp4netns %s: unexpected answer %s", command, strings.TrimSpace(string(answer)))
}

// AddHostFwd asks slirp4netns to listen on hostAddr:hostPort and forward to the guest.
// It returns the ID of the forward.
func (c *Client) AddHostFwd(ctx context.Context, proto, hostAddr string, hostPort int, guestAddr string, guestPort int) (int, error) {
	raw, err := c.do(ctx, "add_hostfwd", map[string]any{
		"proto": proto, "host_addr": hostAddr, "host_port": hostPort, "guest_addr": guestAddr, "guest_port": guestPort,
	})
	if err != nil {
		return 0, err
	}
	var r struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, fmt.Errorf("slirp4netns add_hostfwd: malformed result %s", raw)
	}
	return r.ID, nil
}

// RemoveHostFwd removes a forward by ID.
func (c *Client) RemoveHostFwd(ctx context.Context, id int) error {
	_, err := c.do(ctx, "remove_hostfwd", map[string]any{"id": id})
	return err
}

// ListHostFwd returns every forward slirp4netns has.
func (c *Client) ListHostFwd(ctx context.Context) ([]Entry, error) {
	raw, err := c.do(ctx, "list_hostfwd", nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("slirp4netns list_hostfwd: malformed result: %w", err)
	}
	return r.Entries, nil
}
