package root

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
)

func TestWaitForPlaidUsesTheConfiguredSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "skiff-wait-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "plaidd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	old := compute.Environment
	defer func() { compute.Environment = old }()

	compute.Environment.PlaidSocket = filepath.Join(dir, "plaidd.sock")
	if err := waitForPlaid(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("the daemon at the configured socket was not found: %v", err)
	}
	compute.Environment.PlaidSocket = filepath.Join(dir, "elsewhere.sock")
	start := time.Now()
	if err := waitForPlaid(context.Background(), 700*time.Millisecond); err == nil || time.Since(start) < 500*time.Millisecond {
		t.Fatalf("a socket nobody listens on was accepted (%v after %v)", err, time.Since(start))
	}
}

// The socket comes from the flag, else from the variable the Plaid tools also read,
// else the default.
func TestPlaidSocketFlagPrecedence(t *testing.T) {
	parse := func(args ...string) string {
		var opts Opts
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		installFlags(flags, &opts)
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		return opts.DefaultHostEnvironment.PlaidSocketPath()
	}
	if got := parse(); got != endpoint.DefaultPlaidSocket {
		t.Fatalf("default %q", got)
	}
	t.Setenv(EnvPlaidSocket, "/run/site/plaidd.sock")
	if got := parse(); got != "/run/site/plaidd.sock" {
		t.Fatalf("from the environment: %q", got)
	}
	if got := parse("--plaid-socket", "/run/flag/plaidd.sock"); got != "/run/flag/plaidd.sock" {
		t.Fatalf("from the flag: %q", got)
	}
}
