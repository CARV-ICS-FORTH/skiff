package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"plaid/pkg/handoff"
	"plaid/pkg/vxlan"
)

func handoffDaemon(t *testing.T, bind net.IP, port int) (*PlaidDaemon, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "overlay.sock")
	d, err := NewPlaidDaemon(tapConfig(func(c *Config) {
		c.SocketPath, c.VXLANPort, c.VXLANBind, c.OverlayHandoff = filepath.Join(dir, "plaidd.sock"), port, bind.String(), path
	}))
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

// helperSockets is what plaidctl overlay-socket opens for --sockets n: the receive sockets
// of the group and four times as many send sockets, as descriptors, receive ones first.
func helperSockets(t *testing.T, n int) (files []*os.File, port int) {
	t.Helper()
	rx, tx, err := vxlan.ListenSockets("udp4", net.IPv4(127, 0, 0, 1), 0, n)
	if err != nil {
		t.Skipf("cannot open the sockets: %v", err)
	}
	for _, c := range append(rx, tx...) {
		f, err := c.File()
		if err != nil {
			t.Fatal(err)
		}
		c := c
		t.Cleanup(func() { c.Close(); f.Close() })
		files = append(files, f)
	}
	return files, rx[0].LocalAddr().(*net.UDPAddr).Port
}

func sockReq(port, rx, tx int) handoff.Request {
	return handoff.Request{Type: handoff.TypeOverlaySocket, Bind: "127.0.0.1", Port: port, RxSockets: rx, TxSockets: tx}
}

func TestOverlaySocketsAreAdoptedFromTheHelper(t *testing.T) {
	for _, n := range []int{1, 4, 16} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			files, port := helperSockets(t, n)
			d, path := handoffDaemon(t, net.IPv4(127, 0, 0, 1), port)
			err := receive(t, d, path, func() {
				if err := handoff.Send(context.Background(), path, sockReq(port, n, 4*n), files, 5*time.Second); err != nil {
					t.Errorf("helper: %v", err)
				}
			})
			if err != nil {
				t.Fatalf("receiveOverlaySocket: %v", err)
			}
			st, _ := d.HandleGetStatus()
			if st.OverlayTransport != "adopted" || st.OverlayAddress != "127.0.0.1:"+strconv.Itoa(port) ||
				st.OverlayRxSockets != n || st.OverlayTxSockets != 4*n || len(st.OverlayRxPerSocket) != n {
				t.Errorf("status = %q %q, %d rx / %d tx sockets, %v", st.OverlayTransport, st.OverlayAddress, st.OverlayRxSockets, st.OverlayTxSockets, st.OverlayRxPerSocket)
			}
			// The helper's copies go away; plaidd's own keep receiving, on every source port.
			for _, f := range files {
				f.Close()
			}
			var delivered uint64
			for i := 0; i < 64; i++ {
				c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
				if err != nil {
					t.Fatal(err)
				}
				c.Write([]byte("x"))
				c.Close()
			}
			for i := 0; i < 200 && delivered != 64; i++ {
				s, _ := d.HandleGetStatus()
				delivered = s.OverlayRxInvalid
				time.Sleep(10 * time.Millisecond)
			}
			if delivered != 64 {
				t.Errorf("%d of 64 datagrams reached the adopted sockets", delivered)
			}
		})
	}
}

func TestOverlayHandoffRefusesWrongSockets(t *testing.T) {
	loop := net.IPv4(127, 0, 0, 1)
	good, port := helperSockets(t, 2) // 2 receive, 8 send
	other, _ := helperSockets(t, 2)   // another group, on another port
	connected, err := net.DialUDP("udp4", &net.UDPAddr{IP: loop}, &net.UDPAddr{IP: loop, Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	defer connected.Close()
	connectedFile, _ := connected.File()
	defer connectedFile.Close()
	unbound, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unbound.Close()
	unboundFile, _ := unbound.File()
	defer unboundFile.Close()
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: loop})
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	tcpFile, _ := tcp.File()
	defer tcpFile.Close()

	with := func(i int, f *os.File) []*os.File {
		fs := append([]*os.File(nil), good...)
		fs[i] = f
		return fs
	}
	// A receive socket without the group: a plain socket on a port of its own.
	_, ungroupedFile, ungroupedPort := helperSocket(t)
	ungrouped := append([]*os.File{ungroupedFile}, good[1:5]...)

	cases := []struct {
		name   string
		req    handoff.Request
		files  []*os.File
		port   int    // the port plaidd is configured for
		reason string // part of the refusal
	}{
		{"fewer descriptors than counted", sockReq(port, 2, 8), good[:9], port, "expected 10 descriptors, got 9"},
		{"more descriptors than counted", sockReq(port, 2, 7), good, port, "expected 9 descriptors, got 10"},
		{"another request type", handoff.Request{Type: "tap", Bind: "127.0.0.1", Port: port, RxSockets: 2, TxSockets: 8}, good, port, "unexpected request"},
		{"no receive socket", sockReq(port, 0, 8), good[2:], port, "0 receive"},
		{"no send socket", sockReq(port, 2, 0), good[:2], port, "2 receive and 0 send"},
		{"too many receive sockets", sockReq(port, 17, 0), good, port, "17 receive"},
		{"a send socket where a receive socket belongs", sockReq(port, 2, 8), append(append([]*os.File(nil), good[2:]...), good[:2]...), port, "descriptor 0"},
		{"a receive socket on another port", sockReq(port, 2, 8), with(1, other[0]), port, "descriptor 1: UDP socket is bound to port"},
		{"a TCP socket", sockReq(port, 2, 8), with(0, tcpFile), port, "descriptor 0: descriptor is not a datagram socket"},
		{"a send socket on another address", sockReq(port, 2, 8), with(5, unboundFile), port, "descriptor 5: UDP socket is bound to 0.0.0.0, want 127.0.0.1"},
		{"a connected send socket", sockReq(port, 2, 8), with(9, connectedFile), port, "descriptor 9: UDP socket is connected"},
		{"a receive socket outside the group", sockReq(ungroupedPort, 1, 4), ungrouped, ungroupedPort, "descriptor 0: UDP socket is not in a SO_REUSEPORT group"},
		{"the sender disagrees on the port", sockReq(port+1, 2, 8), good, port, "the sender bound"},
		{"the sender disagrees on the address", handoff.Request{Type: handoff.TypeOverlaySocket, Bind: "127.0.0.2", Port: port, RxSockets: 2, TxSockets: 8}, good, port, "the sender bound"},
	}
	for _, c := range cases {
		before := openFDs(t)
		d, path := handoffDaemon(t, loop, c.port)
		var sendErr error
		err := receive(t, d, path, func() {
			sendErr = handoff.Send(context.Background(), path, c.req, c.files, 5*time.Second)
		})
		if err == nil || sendErr == nil {
			t.Errorf("%s: accepted (plaidd: %v, helper: %v)", c.name, err, sendErr)
		} else if !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: refused for another reason than %q: %v", c.name, c.reason, err)
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

// Without a handoff plaidd opens the sockets itself, --overlay-sockets of them.
func TestStandaloneOverlayOpensItsOwnSockets(t *testing.T) {
	for _, tc := range []struct{ flag, rx int }{{1, 1}, {4, 4}, {16, 16}, {0, vxlan.DefaultSockets}} {
		dir, err := os.MkdirTemp("/tmp", "pd")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		d, err := NewPlaidDaemon(tapConfig(func(c *Config) {
			c.SocketPath, c.VXLANPort, c.VXLANBind, c.OverlaySockets = filepath.Join(dir, "plaidd.sock"), 0, "127.0.0.1", tc.flag
		}))
		if err != nil {
			t.Fatal(err)
		}
		if err := d.overlay.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		st, _ := d.HandleGetStatus()
		if st.OverlayTransport != "bound" || st.OverlayRxSockets != tc.rx || st.OverlayTxSockets != 4*tc.rx {
			t.Errorf("--overlay-sockets=%d: %q, %d rx / %d tx sockets", tc.flag, st.OverlayTransport, st.OverlayRxSockets, st.OverlayTxSockets)
		}
		d.overlay.Close()
	}
}
