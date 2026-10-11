package vxlan

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

var loopback = net.IPv4(127, 0, 0, 1)

// udpFile opens a socket in a SO_REUSEPORT group, as a receive socket is, and returns it
// with a file for it.
func udpFile(t *testing.T, network string, laddr *net.UDPAddr) (*net.UDPConn, *os.File) {
	t.Helper()
	pc, err := (&net.ListenConfig{Control: reusePort}).ListenPacket(context.Background(), network, laddr.String())
	if err != nil {
		t.Skipf("cannot open a %s socket: %v", network, err)
	}
	c := pc.(*net.UDPConn)
	f, err := c.File()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); f.Close() })
	return c, f
}

// plainSocket is a UDP socket on loopback and a port of its own, outside any group: a send
// socket, or a port that is in use.
func plainSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Skipf("cannot open a UDP socket: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// SocketFromFile accepts the socket that is configured, for its role, and no other.
func TestSocketFromFile(t *testing.T) {
	t.Run("the configured socket only", func(t *testing.T) {
		good, goodFile := udpFile(t, "udp4", &net.UDPAddr{IP: loopback})
		port := good.LocalAddr().(*net.UDPAddr).Port

		if conn, err := SocketFromFile(goodFile, loopback, port, Receive); err != nil {
			t.Errorf("the configured socket was refused: %v", err)
		} else {
			conn.Close()
		}

		// A wildcard-bound socket is right when the wildcard is what is configured.
		wild, wildFile := udpFile(t, "udp4", &net.UDPAddr{IP: net.IPv4zero})
		wildPort := wild.LocalAddr().(*net.UDPAddr).Port
		if conn, err := SocketFromFile(wildFile, net.IPv4zero, wildPort, Receive); err != nil {
			t.Errorf("wildcard socket refused: %v", err)
		} else {
			conn.Close()
		}

		// A UDP socket that is bound to nothing in particular.
		unbound, err := net.ListenUDP("udp4", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer unbound.Close()
		nf, _ := unbound.File()
		defer nf.Close()

		refused := []struct {
			name string
			file func() *os.File
			bind net.IP
			port int
		}{
			{"wrong port", func() *os.File { return goodFile }, loopback, port + 1},
			{"wrong address", func() *os.File { return goodFile }, net.IPv4(127, 0, 0, 2), port},
			{"wildcard where an address is configured", func() *os.File { return wildFile }, loopback, wildPort},
			{"address where the wildcard is configured", func() *os.File { return goodFile }, net.IPv4zero, port},
			{"not bound to the configured port", func() *os.File { return nf }, net.IPv4zero, 8472},
			{"TCP listener", func() *os.File {
				l, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: loopback})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { l.Close() })
				f, _ := l.File()
				t.Cleanup(func() { f.Close() })
				return f
			}, loopback, port},
			{"unix datagram socket", func() *os.File {
				dir, err := os.MkdirTemp("/tmp", "sk")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(dir) })
				c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: filepath.Join(dir, "d"), Net: "unixgram"})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close() })
				f, _ := c.File()
				t.Cleanup(func() { f.Close() })
				return f
			}, loopback, port},
			{"connected UDP socket", func() *os.File {
				c, err := net.DialUDP("udp4", &net.UDPAddr{IP: loopback, Port: port + 2}, &net.UDPAddr{IP: loopback, Port: 9})
				if err != nil {
					t.Skip("cannot open a connected socket")
				}
				t.Cleanup(func() { c.Close() })
				f, _ := c.File()
				t.Cleanup(func() { f.Close() })
				return f
			}, loopback, port + 2},
			{"IPv6 UDP socket", func() *os.File {
				_, f := udpFile(t, "udp6", &net.UDPAddr{IP: net.IPv6loopback})
				return f
			}, loopback, port},
			{"a socket that was never bound", func() *os.File {
				fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
				if err != nil {
					t.Fatal(err)
				}
				f := os.NewFile(uintptr(fd), "unbound")
				t.Cleanup(func() { f.Close() })
				return f
			}, loopback, port},
			{"a regular file", func() *os.File {
				f, err := os.Open(os.DevNull)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { f.Close() })
				return f
			}, loopback, port},
		}
		for _, c := range refused {
			conn, err := SocketFromFile(c.file(), c.bind, c.port, Receive)
			if err == nil {
				conn.Close()
				t.Errorf("%s: accepted", c.name)
			}
		}
	})
	t.Run("the role", func(t *testing.T) {
		rx, tx, err := ListenSockets("udp4", loopback, 0, 2)
		if err != nil {
			t.Skipf("cannot open the sockets: %v", err)
		}
		defer func() {
			for _, c := range append(rx, tx...) {
				c.Close()
			}
		}()
		file := func(c *net.UDPConn) *os.File {
			f, err := c.File()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { f.Close() })
			return f
		}
		port := rx[0].LocalAddr().(*net.UDPAddr).Port
		plain := plainSocket(t) // bound, not in a group
		plainFile := file(plain)
		plainPort := plain.LocalAddr().(*net.UDPAddr).Port
		connected, err := net.DialUDP("udp4", &net.UDPAddr{IP: loopback}, &net.UDPAddr{IP: loopback, Port: 9})
		if err != nil {
			t.Fatal(err)
		}
		defer connected.Close()
		unbound, err := net.ListenUDP("udp4", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer unbound.Close()

		for _, tc := range []struct {
			name string
			file *os.File
			port int
			role Role
			ok   bool
		}{
			{"a receive socket", file(rx[1]), port, Receive, true},
			{"a send socket", file(tx[0]), 0, Send, true},
			{"a send socket, with the port named", file(tx[1]), port, Send, true},
			{"a receive socket without the group", plainFile, plainPort, Receive, false},
			{"a receive socket on the wrong port", file(rx[0]), port + 1, Receive, false},
			{"a send socket as a receive socket", file(tx[0]), port, Receive, false},
			{"a send socket on another address", file(unbound), 0, Send, false},
			{"a connected send socket", file(connected), 0, Send, false},
			{"a connected receive socket", file(connected), connected.LocalAddr().(*net.UDPAddr).Port, Receive, false},
		} {
			conn, err := SocketFromFile(tc.file, loopback, tc.port, tc.role)
			if conn != nil {
				conn.Close()
			}
			if (err == nil) != tc.ok {
				t.Errorf("%s: %v", tc.name, err)
			}
		}
		if conn, err := SocketFromFile(file(tx[0]), net.IPv4(127, 0, 0, 2), 0, Send); err == nil {
			conn.Close()
			t.Error("a send socket on another address was accepted")
		}
		// A socket that was never bound has no port of its own.
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		never := os.NewFile(uintptr(fd), "never bound")
		defer never.Close()
		if conn, err := SocketFromFile(never, net.IPv4zero, 0, Send); err == nil {
			conn.Close()
			t.Error("a send socket without a port was accepted")
		}
	})
}

func waitFrames(t *testing.T, inj *mockInjector) {
	t.Helper()
	select {
	case <-inj.notify:
	case <-time.After(3 * time.Second):
		t.Fatal("no frame arrived")
	}
}

// One engine uses an adopted socket (opened elsewhere), the other its own; frames go both ways.
func TestAdoptedSocketCarriesTheOverlay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened, file := udpFile(t, "udp4", &net.UDPAddr{IP: loopback})
	port := opened.LocalAddr().(*net.UDPAddr).Port
	conn, err := SocketFromFile(file, loopback, port, Receive)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close() // the opener's copy goes away, as the helper's does

	_, sn, _ := net.ParseCIDR("10.244.2.0/24")
	_, sn2, _ := net.ParseCIDR("10.244.1.0/24")
	injA, injB := newMockInjector(), newMockInjector()
	routesA, routesB := NewRouteTable(), NewRouteTable()

	a := NewOverlayEngine(OverlayConfig{BindAddress: "127.0.0.1", Port: 0}, routesA, injA)
	if err := a.Adopt(ctx, []*net.UDPConn{conn}, []*net.UDPConn{plainSocket(t)}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer a.Close()
	if st := a.Stats(); st.Transport != TransportAdopted || st.Address != conn.LocalAddr().String() {
		t.Errorf("stats after Adopt = %+v", st)
	}
	b := NewOverlayEngine(OverlayConfig{BindAddress: "127.0.0.1", Port: 0}, routesB, injB)
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if st := b.Stats(); st.Transport != TransportBound {
		t.Errorf("Start transport = %q", st.Transport)
	}

	routesA.AddRoute(&RouteEntry{Subnet: sn, RemoteHostIP: loopback, Port: b.LocalPort()})
	routesB.AddRoute(&RouteEntry{Subnet: sn2, RemoteHostIP: loopback, Port: a.LocalPort()})
	if err := a.Send(testFrame(28), net.ParseIP("10.244.2.7")); err != nil {
		t.Fatalf("send from the adopted socket: %v", err)
	}
	waitFrames(t, injB)
	if err := b.Send(testFrame(28), net.ParseIP("10.244.1.7")); err != nil {
		t.Fatalf("send to the adopted socket: %v", err)
	}
	waitFrames(t, injA)

	if st := a.Stats(); st.TxPackets != 1 || st.RxPackets != 1 || st.TxErrors != 0 || st.RxInvalid != 0 {
		t.Errorf("counters of the adopted side = %+v", st)
	}

	// Counters see errors and junk: no route, and a datagram that is not VXLAN.
	if err := a.Send(testFrame(28), net.ParseIP("10.99.0.1")); err == nil {
		t.Error("send without a route succeeded")
	}
	junk, _ := net.DialUDP("udp4", nil, &net.UDPAddr{IP: loopback, Port: a.LocalPort()})
	junk.Write([]byte("xx"))
	junk.Close()
	for i := 0; i < 100 && a.Stats().RxInvalid == 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if st := a.Stats(); st.TxErrors != 1 || st.RxInvalid != 1 {
		t.Errorf("counters after an error and junk = %+v", st)
	}
}

func TestOverlayHasOneSocketForItsLifetime(t *testing.T) {
	ctx := context.Background()
	c1, f1 := udpFile(t, "udp4", &net.UDPAddr{IP: loopback})
	c2, f2 := udpFile(t, "udp4", &net.UDPAddr{IP: loopback})
	conn1, err := SocketFromFile(f1, loopback, c1.LocalAddr().(*net.UDPAddr).Port, Receive)
	if err != nil {
		t.Fatal(err)
	}
	conn2, err := SocketFromFile(f2, loopback, c2.LocalAddr().(*net.UDPAddr).Port, Receive)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()

	oe := NewOverlayEngine(OverlayConfig{BindAddress: "127.0.0.1", Port: 0}, NewRouteTable(), newMockInjector())
	if err := oe.Adopt(ctx, []*net.UDPConn{conn1}, []*net.UDPConn{plainSocket(t)}); err != nil {
		t.Fatal(err)
	}
	if err := oe.Adopt(ctx, []*net.UDPConn{conn2}, []*net.UDPConn{plainSocket(t)}); err == nil {
		t.Error("a second socket was adopted by a live transport")
	}
	if err := oe.Start(ctx); err == nil {
		t.Error("the engine bound its own socket next to an adopted one")
	}
	// The refused socket is still the caller's: the engine did not close it.
	if _, err := conn2.WriteToUDP([]byte("x"), &net.UDPAddr{IP: loopback, Port: conn1.LocalAddr().(*net.UDPAddr).Port}); err != nil {
		t.Errorf("the refused socket was closed: %v", err)
	}
	oe.Close()
	if err := oe.Adopt(ctx, []*net.UDPConn{conn2}, []*net.UDPConn{plainSocket(t)}); err == nil {
		t.Error("a closed engine adopted a socket")
	}
	// Closing the engine closes the adopted socket, which releases the port.
	if _, err := conn1.WriteToUDP([]byte("x"), &net.UDPAddr{IP: loopback, Port: 9}); err == nil {
		t.Error("the adopted socket is still open after Close")
	}
}
