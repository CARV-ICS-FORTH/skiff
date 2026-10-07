package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func unixClient(path string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
}

func get(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// short returns a socket path in a short temporary directory: UNIX socket paths are limited to about 100 bytes.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "p.sock")
}

func TestStartPprof_ServesTheProfilesOnAnOwnerOnlySocket(t *testing.T) {
	path := shortSocketPath(t)
	stop, err := startPprof(path)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("the socket is %v, want a socket with mode 0600", fi.Mode())
	}

	c := unixClient(path)
	if code, body := get(t, c, "http://plaid/debug/pprof/cmdline"); code != 200 || body == "" {
		t.Fatalf("cmdline: %d %q", code, body)
	}
	if code, body := get(t, c, "http://plaid/debug/pprof/"); code != 200 || !strings.Contains(body, "goroutine") {
		t.Fatalf("index: %d", code)
	}
	for _, name := range []string{"allocs", "heap", "goroutine", "mutex", "block"} {
		if code, _ := get(t, c, "http://plaid/debug/pprof/"+name); code != 200 {
			t.Fatalf("%s: %d", name, code)
		}
	}
	// a CPU profile of a second: gzip-compressed protobuf
	if code, body := get(t, c, "http://plaid/debug/pprof/profile?seconds=1"); code != 200 || len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("profile: %d, %d bytes", code, len(body))
	}

	stop()
	stopped = true
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the socket is still there after stop: %v", err)
	}
	if _, err := net.Dial("unix", path); err == nil {
		t.Fatal("something still listens after stop")
	}
}

func TestStartPprof_ReplacesAStaleSocketButNothingElse(t *testing.T) {
	// a socket a crashed plaidd left behind (the listener is closed without unlinking it)
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	stop, err := startPprof(path)
	if err != nil {
		t.Fatalf("a stale socket should be replaced: %v", err)
	}
	if code, _ := get(t, unixClient(path), "http://plaid/debug/pprof/cmdline"); code != 200 {
		t.Fatalf("not serving after replacing a stale socket: %d", code)
	}
	stop()

	// a file that is not a socket is left alone
	file := shortSocketPath(t)
	if err := os.WriteFile(file, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := startPprof(file); err == nil {
		t.Fatal("startPprof replaced a regular file")
	}
	if got, _ := os.ReadFile(file); string(got) != "keep me" {
		t.Fatalf("the file was changed: %q", got)
	}
}

func TestStartPprof_TheMuxIsItsOwn(t *testing.T) {
	// the endpoints answer on the socket only; a request to another path is a 404, not the default mux's
	path := shortSocketPath(t)
	stop, err := startPprof(path)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if code, _ := get(t, unixClient(path), "http://plaid/other"); code != 404 {
		t.Fatalf("unexpected status for another path: %d", code)
	}
}
