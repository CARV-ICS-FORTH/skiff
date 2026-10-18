package api

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type mockHandlers struct {
	lastAddReq   *Request
	lastDelReq   *Request
	lastAddRoute *Request
	lastDelRoute *Request
	receivedFDs  []int
	routes       []RouteInfo
}

func (m *mockHandlers) HandleAddEndpoint(req *Request, tapFDs []int) error {
	m.lastAddReq = req
	m.receivedFDs = tapFDs
	return nil
}

func (m *mockHandlers) HandleRemoveEndpoint(req *Request) error {
	m.lastDelReq = req
	return nil
}

func (m *mockHandlers) HandleAddRoute(req *Request) error {
	m.lastAddRoute = req
	return nil
}

func (m *mockHandlers) HandleRemoveRoute(req *Request) error {
	m.lastDelRoute = req
	return nil
}

func (m *mockHandlers) HandleListRoutes() ([]RouteInfo, error) {
	return m.routes, nil
}

func (m *mockHandlers) HandleGetStatus() (*Response, error) {
	return &Response{
		EndpointsCount: 42,
		RoutesCount:    3,
		NodeCIDR:       "10.244.1.0/24",
	}, nil
}

func TestAPIServerAndClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := filepath.Join(t.TempDir(), "test_plaidd.sock")
	handlers := &mockHandlers{}

	srv := NewServer(sockPath, handlers, handlers, handlers)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer srv.Close()

	client := NewClient(sockPath)
	initial, err := client.GetStatus()
	if err != nil || !initial.Success || initial.Ready {
		t.Fatalf("initializing daemon must report not ready: status=%+v err=%v", initial, err)
	}
	if err := client.AddRoute(&Request{Subnet: "10.244.2.0/24", RemoteHostIP: "192.168.1.10"}); err == nil {
		t.Fatal("route mutation was accepted before daemon initialization")
	}
	srv.MarkReady()

	// 1. Test GetStatus
	status, err := client.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus error: %v", err)
	}
	if !status.Success || !status.Ready || status.EndpointsCount != 42 || status.NodeCIDR != "10.244.1.0/24" {
		t.Errorf("Unexpected status response: %+v", status)
	}

	// 2. Test AddRoute & RemoveRoute
	err = client.AddRoute(&Request{
		Subnet:       "10.244.2.0/24",
		RemoteHostIP: "192.168.1.10",
		VNI:          1,
	})
	if err != nil {
		t.Fatalf("AddRoute error: %v", err)
	}
	if handlers.lastAddRoute == nil || handlers.lastAddRoute.Subnet != "10.244.2.0/24" {
		t.Errorf("AddRoute handler did not receive expected params")
	}

	err = client.RemoveRoute("10.244.2.0/24")
	if err != nil {
		t.Fatalf("RemoveRoute error: %v", err)
	}
	if handlers.lastDelRoute == nil || handlers.lastDelRoute.Subnet != "10.244.2.0/24" {
		t.Errorf("RemoveRoute handler did not receive expected params")
	}

	// A route is listed with the address of its node.
	handlers.routes = []RouteInfo{{Subnet: "10.244.2.0/24", RemoteHostIP: "192.168.1.10"}}
	routes, err := client.ListRoutes()
	if err != nil || len(routes) != 1 || routes[0] != handlers.routes[0] {
		t.Fatalf("ListRoutes: %v, %v", routes, err)
	}

	// 3. Test AddEndpoint with SCM_RIGHTS FD passing
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe error: %v", err)
	}
	defer rPipe.Close()
	defer wPipe.Close()

	err = client.AddEndpoint(&Request{
		PodID:        "pod-123",
		PodName:      "nginx",
		PodNamespace: "default",
		IP:           "10.244.1.15",
		MAC:          "02:00:00:00:01:15",
	}, int(rPipe.Fd()))
	if err != nil {
		t.Fatalf("AddEndpoint with FD error: %v", err)
	}

	if handlers.lastAddReq == nil || handlers.lastAddReq.PodID != "pod-123" {
		t.Fatalf("AddEndpoint handler did not receive expected request")
	}

	if len(handlers.receivedFDs) != 1 || handlers.receivedFDs[0] < 0 {
		t.Fatalf("Server did not receive one FD over SCM_RIGHTS (got %v)", handlers.receivedFDs)
	}

	// Test communicating through the passed file descriptor
	receivedFile := os.NewFile(uintptr(handlers.receivedFDs[0]), "passed-tap-fd")
	defer receivedFile.Close()

	testMsg := "hello via passed fd"
	go func() {
		_, _ = wPipe.Write([]byte(testMsg))
		_ = wPipe.Close()
	}()

	readBuf := make([]byte, 64)
	n, err := receivedFile.Read(readBuf)
	if err != nil && err != io.EOF {
		t.Fatalf("Failed to read from passed fd: %v", err)
	}
	if string(readBuf[:n]) != testMsg {
		t.Errorf("Expected msg %q, got %q", testMsg, string(readBuf[:n]))
	}

	// 4. Test RemoveEndpoint
	err = client.RemoveEndpoint("pod-123", "container-456")
	if err != nil {
		t.Fatalf("RemoveEndpoint error: %v", err)
	}
	if handlers.lastDelReq == nil || handlers.lastDelReq.PodID != "pod-123" {
		t.Errorf("RemoveEndpoint handler did not receive expected request")
	}
}

func TestLargeResponseAndFraming(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := filepath.Join(t.TempDir(), "l.sock")
	largeHandlers := &largeStatusHandler{}

	srv := NewServer(sockPath, nil, nil, largeHandlers)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer srv.Close()
	srv.MarkReady()

	client := NewClient(sockPath)
	resp, err := client.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus with large response failed: %v", err)
	}
	if !resp.Success || len(resp.Endpoints) != 200 {
		t.Fatalf("Expected 200 endpoints in large status, got %d", len(resp.Endpoints))
	}
}

type largeStatusHandler struct{}

func (h *largeStatusHandler) HandleGetStatus() (*Response, error) {
	// Generate an endpoint list well over 6,000 bytes
	endpoints := make([]string, 200)
	for i := range endpoints {
		endpoints[i] = fmt.Sprintf("endpoint-%04d-with-extra-long-description-padding-to-exceed-4096-bytes-and-verify-framing", i)
	}
	return &Response{
		Success:        true,
		EndpointsCount: len(endpoints),
		Endpoints:      endpoints,
	}, nil
}

func TestFDLeakPrevention(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := filepath.Join(t.TempDir(), "k.sock")
	srv := NewServer(sockPath, nil, nil, nil)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer srv.Close()
	srv.MarkReady()

	// 1. Send unexpected FD with a non-AddEndpoint request (e.g. remove_route)
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer wPipe.Close()

	client := NewClient(sockPath)
	// Send with passFD on a request that does not take an FD
	req := &Request{
		Action: ActionRemoveRoute,
		Subnet: "10.0.0.0/24",
	}
	// The client Send passes rPipe.Fd()
	_, err = client.Send(req, int(rPipe.Fd()))
	_ = rPipe.Close()
	if err != nil {
		t.Fatalf("Send unexpected FD request failed: %v", err)
	}

	// 2. Test Socket Lock prevents second server instance and Close() does NOT remove original socket
	srv2 := NewServer(sockPath, nil, nil, nil)
	if err := srv2.Start(ctx); err == nil {
		srv2.Close()
		t.Fatalf("Expected error when starting second server with same socket lock, got nil")
	}
	// Simulate srv2 cleanup on failure: srv2.Close() must NOT remove srv's socket
	_ = srv2.Close()
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("Failed srv2.Close() wrongfully removed srv's socket file: %v", err)
	}
}

type mockFailingHandler struct {
	mockHandlers
}

func (m *mockFailingHandler) HandleAddEndpoint(req *Request, tapFDs []int) error {
	// Handler closes the file descriptors when returning an error, as plaidd does
	for _, fd := range tapFDs {
		_ = os.NewFile(uintptr(fd), "test-tap").Close()
	}
	return fmt.Errorf("invalid endpoint configuration error")
}

func TestAddEndpointErr_FD(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := filepath.Join(t.TempDir(), "e.sock")
	failingHandler := &mockFailingHandler{}

	srv := NewServer(sockPath, failingHandler, failingHandler, failingHandler)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer srv.Close()
	srv.MarkReady()

	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer wPipe.Close()

	client := NewClient(sockPath)
	err = client.AddEndpoint(&Request{
		PodID: "test-fail-pod",
	}, int(rPipe.Fd()))
	_ = rPipe.Close()

	if err == nil {
		t.Fatalf("Expected AddEndpointWithFD to fail, got nil")
	}

	// Server must remain alive and responsive after error
	status, err := client.GetStatus()
	if err != nil || !status.Success {
		t.Fatalf("Server unresponsive after endpoint addition error: %v", err)
	}
}

// sendPipes sends the read ends of n new pipes with an AddEndpoint request and closes the
// sender's own copies of them, so that only the server holds them. It returns the write ends
// and the descriptors that the handler received.
func sendPipes(t *testing.T, n int) (writers []*os.File, h *mockHandlers) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "fds.sock")
	h = &mockHandlers{}
	srv := NewServer(sockPath, h, h, h)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.MarkReady()

	var readers []*os.File
	var fds []int
	for i := 0; i < n; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		t.Cleanup(func() { _ = w.Close() })
		readers, writers = append(readers, r), append(writers, w)
		fds = append(fds, int(r.Fd()))
	}
	if err := NewClient(sockPath).AddEndpoint(&Request{PodID: "queues"}, fds...); err != nil {
		t.Fatalf("AddEndpoint: %v", err)
	}
	for _, r := range readers {
		_ = r.Close()
	}
	t.Cleanup(func() {
		for _, fd := range h.receivedFDs {
			_ = syscall.Close(fd)
		}
	})
	return writers, h
}

// A request with the files of several queues delivers all of them, in order, to the
// handler, which owns them.
func TestAddEndpointDeliversEveryQueue(t *testing.T) {
	writers, h := sendPipes(t, 4)
	if len(h.receivedFDs) != 4 {
		t.Fatalf("the handler received %d descriptors, want 4", len(h.receivedFDs))
	}
	for i, fd := range h.receivedFDs {
		msg := fmt.Sprintf("queue %d", i)
		if _, err := writers[i].Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 16)
		n, err := syscall.Read(fd, buf)
		if err != nil || string(buf[:n]) != msg {
			t.Errorf("descriptor %d read %q, %v: want %q", i, buf[:n], err, msg)
		}
	}
}

// The server keeps MaxEndpointFDs descriptors and closes the others: the write end of a
// pipe whose read end it closed fails.
func TestAddEndpointClosesDescriptorsBeyondTheMaximum(t *testing.T) {
	writers, h := sendPipes(t, MaxEndpointFDs+4)
	if len(h.receivedFDs) != MaxEndpointFDs {
		t.Fatalf("the handler received %d descriptors, want %d", len(h.receivedFDs), MaxEndpointFDs)
	}
	for i, w := range writers {
		_, err := w.Write([]byte("x"))
		switch {
		case i < MaxEndpointFDs && err != nil:
			t.Errorf("pipe %d is kept but its read end is closed: %v", i, err)
		case i >= MaxEndpointFDs && err == nil:
			t.Errorf("pipe %d is beyond the maximum but its read end is still open", i)
		}
	}
}

// A request that fails before the handler has it does not leak the descriptors.
func TestAddEndpointWithoutHandlerClosesDescriptors(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "nohandler.sock")
	srv := NewServer(sockPath, nil, nil, nil) // not ready: every request but the status is refused
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	r, w, _ := os.Pipe()
	defer w.Close()
	if err := NewClient(sockPath).AddEndpoint(&Request{PodID: "x"}, int(r.Fd()), int(r.Fd())); err == nil {
		t.Fatal("the request was accepted by a daemon that is initializing")
	}
	_ = r.Close()
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("the server still holds the read end it was sent")
	}
}

type directRecorder struct{ reports []DirectReport }

func (d *directRecorder) HandleReportDirect(r *DirectReport) error {
	if r.PID < 0 {
		return fmt.Errorf("no such process")
	}
	d.reports = append(d.reports, *r)
	return nil
}

// A report of the direct path reaches its handler with its counters; a daemon that has no
// handler, or a handler that refuses the report, answers with an error.
func TestReportDirect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sockPath := filepath.Join(t.TempDir(), "d.sock")
	handlers := &mockHandlers{}
	srv := NewServer(sockPath, handlers, handlers, handlers)
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.MarkReady()
	client := NewClient(sockPath)

	report := DirectReport{Supervisor: "default/p/c", PID: 7, Ended: true,
		Counters: DirectCounters{Connects: 2, Accepts: 3, Fallbacks: map[string]uint64{"refused": 1}, Rejected: map[string]uint64{"bad-mac": 4}}}
	if err := client.ReportDirect(report); err == nil {
		t.Error("a report was accepted by a daemon that has no handler for it")
	}
	rec := &directRecorder{}
	srv.SetDirectHandler(rec)
	if err := client.ReportDirect(report); err != nil {
		t.Fatal(err)
	}
	if len(rec.reports) != 1 || rec.reports[0].Counters.Rejected["bad-mac"] != 4 || !rec.reports[0].Ended || rec.reports[0].Counters.Fallbacks["refused"] != 1 {
		t.Errorf("the handler got %+v", rec.reports)
	}
	report.PID = -1
	if err := client.ReportDirect(report); err == nil || !strings.Contains(err.Error(), "no such process") {
		t.Errorf("a report the handler refused: %v", err)
	}
	if resp, err := client.Send(&Request{Action: ActionReportDirect}); err != nil || resp.Success {
		t.Errorf("a request without a report: %+v, %v", resp, err)
	}
}

func TestDirectCountersAdd(t *testing.T) {
	var total DirectCounters
	total.Add(DirectCounters{Connects: 1, Errors: 2, Fallbacks: map[string]uint64{"a": 1}})
	total.Add(DirectCounters{Connects: 4, Proxied: 3, Fallbacks: map[string]uint64{"a": 2, "b": 1}, Rejected: map[string]uint64{"c": 5}})
	if total.Connects != 5 || total.Errors != 2 || total.Proxied != 3 || total.Fallbacks["a"] != 3 || total.Fallbacks["b"] != 1 || total.Rejected["c"] != 5 {
		t.Errorf("%+v", total)
	}
}
