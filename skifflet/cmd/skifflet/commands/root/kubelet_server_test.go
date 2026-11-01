package root

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A client that stops in the middle of its handshake or its headers is let go of, and neither
// reaches the handler nor stays.
func TestKubeletServerLetsGoOfAClientThatStops(t *testing.T) {
	cfg := kubeletServer("", http.NotFoundHandler(), nil)
	if cfg.ReadHeaderTimeout <= 0 || cfg.IdleTimeout <= 0 {
		t.Fatalf("no limits: %v, %v", cfg.ReadHeaderTimeout, cfg.IdleTimeout)
	}
	if cfg.ReadTimeout != 0 || cfg.WriteTimeout != 0 {
		t.Fatalf("a limit on a whole request would end logs that are followed: %v, %v", cfg.ReadTimeout, cfg.WriteTimeout)
	}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config = kubeletServer("", http.NotFoundHandler(), nil)
	srv.Config.ReadHeaderTimeout = 200 * time.Millisecond
	srv.StartTLS()
	defer srv.Close()
	for name, stall := range map[string]func(c net.Conn){
		"handshake": func(net.Conn) {},
		"headers": func(c net.Conn) {
			tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
			if err := tc.Handshake(); err != nil {
				t.Errorf("handshake: %v", err)
			}
			tc.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n"))
		},
	} {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		stall(c)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 4096)
		for {
			_, err := c.Read(buf)
			if err == nil {
				continue
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Errorf("stopped in the %s: still connected after 3 s", name)
			}
			break
		}
	}
}
