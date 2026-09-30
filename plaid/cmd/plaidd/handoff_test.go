package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"plaid/pkg/handoff"
)

func handoffDaemon(t *testing.T, bind net.IP, port int) (*PlaidDaemon, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "overlay.sock")
	d, err := NewPlaidDaemon(Config{
		SocketPath:     filepath.Join(dir, "plaidd.sock"),
		NodeCIDR:       "10.244.5.0/24",
		ClusterCIDR:    "10.244.0.0/16",
		GatewayMAC:     "02:00:00:00:00:01",
		VXLANPort:      port,
		VXLANBind:      bind.String(),
		VXLANVNI:       1,
		OverlayHandoff: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.overlay.Close() })
	return d, path
}

// helperSocket is what plaidctl overlay-socket opens: an IPv4 UDP socket bound on the host.
func helperSocket(t *testing.T) (*net.UDPConn, *os.File, int) {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.File()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); f.Close() })
	return c, f, c.LocalAddr().(*net.UDPAddr).Port
}

// openFDs counts this process's open descriptors by probing the range; unlike listing
// /dev/fd it works on every platform the tests run on.
func openFDs(t *testing.T) int {
	t.Helper()
	n := 0
	for fd := 0; fd < 4096; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			n++
		}
	}
	return n
}

func receive(t *testing.T, d *PlaidDaemon, path string, send func()) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- d.receiveOverlaySocket(context.Background()) }()
	for i := 0; i < 200; i++ {
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	send()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("receiveOverlaySocket did not return")
		return nil
	}
}

func TestOverlaySocketIsAdoptedFromTheHelper(t *testing.T) {
	c, f, port := helperSocket(t)
	d, path := handoffDaemon(t, net.IPv4(127, 0, 0, 1), port)
	err := receive(t, d, path, func() {
		req := handoff.Request{Type: handoff.TypeOverlaySocket, Bind: "127.0.0.1", Port: port}
		if err := handoff.Send(context.Background(), path, req, f, 5*time.Second); err != nil {
			t.Errorf("helper: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("receiveOverlaySocket: %v", err)
	}
	c.Close() // the helper exits
	f.Close()
	st, _ := d.HandleGetStatus()
	if st.OverlayTransport != "adopted" || st.OverlayAddress != "127.0.0.1:"+strconv.Itoa(port) {
		t.Errorf("status = transport %q, address %q", st.OverlayTransport, st.OverlayAddress)
	}
	// plaidd still owns a live socket after the helper's copy is gone: a datagram reaches it.
	sender, _ := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	defer sender.Close()
	sender.Write([]byte("x"))
	for i := 0; i < 100; i++ {
		if s, _ := d.HandleGetStatus(); s.OverlayRxInvalid == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the adopted socket did not receive after the helper closed its copy")
}

func TestOverlayHandoffRefusesWhatIsNotTheConfiguredSocket(t *testing.T) {
	_, goodFile, port := helperSocket(t)
	_, otherFile, _ := helperSocket(t)
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	tcpFile, _ := tcp.File()
	defer tcpFile.Close()
	unbound, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	unboundFile := os.NewFile(uintptr(unbound), "unbound")
	defer unboundFile.Close()

	right := handoff.Request{Type: handoff.TypeOverlaySocket, Bind: "127.0.0.1", Port: port}
	cases := []struct {
		name string
		req  handoff.Request
		file *os.File
	}{
		{"no descriptor", right, nil},
		{"another request type", handoff.Request{Type: "tap", Bind: "127.0.0.1", Port: port}, goodFile},
		{"the sender disagrees on the port", handoff.Request{Type: handoff.TypeOverlaySocket, Bind: "127.0.0.1", Port: port + 1}, goodFile},
		{"the sender disagrees on the address", handoff.Request{Type: handoff.TypeOverlaySocket, Bind: "127.0.0.2", Port: port}, goodFile},
		{"a socket on another port", right, otherFile},
		{"a TCP socket", right, tcpFile},
		{"a socket that was never bound", right, unboundFile},
	}
	for _, c := range cases {
		before := openFDs(t)
		d, path := handoffDaemon(t, net.IPv4(127, 0, 0, 1), port)
		var sendErr error
		err := receive(t, d, path, func() {
			sendErr = handoff.Send(context.Background(), path, c.req, c.file, 5*time.Second)
		})
		if err == nil || sendErr == nil {
			t.Errorf("%s: accepted (plaidd: %v, helper: %v)", c.name, err, sendErr)
		}
		if s, _ := d.HandleGetStatus(); s.OverlayTransport != "" {
			t.Errorf("%s: the overlay has a transport after a refusal: %q", c.name, s.OverlayTransport)
		}
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s: the handoff socket remains", c.name)
		}
		d.overlay.Close()
		if after := openFDs(t); after != before {
			t.Errorf("%s: %d descriptors before, %d after", c.name, before, after)
		}
	}
}

func TestOverlayHandoffRefusesSeveralDescriptors(t *testing.T) {
	_, f1, port := helperSocket(t)
	_, f2, _ := helperSocket(t)
	d, path := handoffDaemon(t, net.IPv4(127, 0, 0, 1), port)
	err := receive(t, d, path, func() {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		body := `{"type":"overlay_socket","bind":"127.0.0.1","port":` + strconv.Itoa(port) + `}`
		msg := append([]byte{0, 0, 0, byte(len(body))}, body...)
		rights := syscall.UnixRights(int(f1.Fd()), int(f2.Fd()))
		c.(*net.UnixConn).WriteMsgUnix(msg, rights, nil)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		c.Read(make([]byte, 256))
	})
	if err == nil {
		t.Fatal("two descriptors were accepted")
	}
	if s, _ := d.HandleGetStatus(); s.OverlayTransport != "" {
		t.Errorf("transport after refusal = %q", s.OverlayTransport)
	}
}

func TestOverlayHandoffIsBoundedWhenNobodyComes(t *testing.T) {
	old := overlayHandoffTimeout
	overlayHandoffTimeout = 300 * time.Millisecond
	defer func() { overlayHandoffTimeout = old }()
	_, _, port := helperSocket(t)
	d, path := handoffDaemon(t, net.IPv4(127, 0, 0, 1), port)
	start := time.Now()
	err := receive(t, d, path, func() {})
	if err == nil {
		t.Fatal("no helper came and plaidd carried on")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("waited %v", time.Since(start))
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("the handoff socket remains after the timeout")
	}
}

func TestOverlayHandoffNeedsAnIPv4Bind(t *testing.T) {
	d, _ := handoffDaemon(t, net.IPv4(127, 0, 0, 1), 8472)
	d.cfg.VXLANBind = "not-an-address"
	if err := d.receiveOverlaySocket(context.Background()); err == nil {
		t.Error("a bind address that is not IPv4 was accepted")
	}
}
