package slirpapi_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"plaid/pkg/slirpapi"
	"plaid/pkg/slirpapi/slirpfake"
)

func setup(t *testing.T) (*slirpfake.Fake, *slirpapi.Client) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	f, err := slirpfake.Start(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	c := slirpapi.New(path)
	c.Timeout = 500 * time.Millisecond
	return f, c
}

func TestAddListRemove(t *testing.T) {
	f, c := setup(t)
	ctx := context.Background()
	fixed := f.Preload(slirpapi.Entry{Proto: "tcp", HostAddr: "192.168.64.3", HostPort: 10250, GuestAddr: "10.0.2.100", GuestPort: 10250})
	id, err := c.AddHostFwd(ctx, "tcp", "192.168.64.3", 31080, "10.0.2.100", 31080)
	if err != nil || id == 0 || id == fixed {
		t.Fatalf("AddHostFwd = %d, %v", id, err)
	}
	entries, err := c.ListHostFwd(ctx)
	if err != nil || len(entries) != 2 || entries[1].HostPort != 31080 || entries[1].GuestAddr != "10.0.2.100" || entries[1].ID != id {
		t.Fatalf("ListHostFwd = %+v, %v", entries, err)
	}
	if err := c.RemoveHostFwd(ctx, id); err != nil {
		t.Fatal(err)
	}
	if entries, _ := c.ListHostFwd(ctx); len(entries) != 1 || entries[0].ID != fixed {
		t.Errorf("after removal: %+v", entries)
	}
	if err := c.RemoveHostFwd(ctx, id); err == nil {
		t.Error("removing a forward twice succeeded")
	}
}

func TestAListingInTheOlderFormat(t *testing.T) {
	f, c := setup(t)
	f.SetMode("top-level")
	f.Preload(slirpapi.Entry{Proto: "udp", HostAddr: "1.2.3.4", HostPort: 5, GuestAddr: "10.0.2.100", GuestPort: 5})
	entries, err := c.ListHostFwd(context.Background())
	if err != nil || len(entries) != 1 || entries[0].Proto != "udp" {
		t.Fatalf("%+v %v", entries, err)
	}
}

func TestABindFailureIsRecognized(t *testing.T) {
	f, c := setup(t)
	f.Take("tcp", 31080)
	_, err := c.AddHostFwd(context.Background(), "tcp", "192.168.64.3", 31080, "10.0.2.100", 31080)
	var api *slirpapi.APIError
	if !errors.As(err, &api) || !api.IsBindError() {
		t.Fatalf("error %v", err)
	}
	// The same port over the other protocol is another port.
	if _, err := c.AddHostFwd(context.Background(), "udp", "192.168.64.3", 31080, "10.0.2.100", 31080); err != nil {
		t.Errorf("udp: %v", err)
	}
	// An unrelated error is not a bind error.
	if _, err := c.ListHostFwd(context.Background()); err != nil {
		t.Fatal(err)
	}
	err = c.RemoveHostFwd(context.Background(), 999)
	if !errors.As(err, &api) || api.IsBindError() {
		t.Errorf("remove error %v", err)
	}
}

func TestEveryFailureIsBounded(t *testing.T) {
	f, c := setup(t)
	for _, mode := range []string{"hang", "garbage", "close"} {
		f.SetMode(mode)
		start := time.Now()
		if _, err := c.ListHostFwd(context.Background()); err == nil {
			t.Errorf("%s: no error", mode)
		}
		if time.Since(start) > 3*time.Second {
			t.Errorf("%s: took %v", mode, time.Since(start))
		}
	}
	f.SetMode("")
	f.SetDelay(200 * time.Millisecond)
	start := time.Now()
	if _, err := c.ListHostFwd(context.Background()); err != nil || time.Since(start) < 150*time.Millisecond {
		t.Errorf("a slow answer: %v after %v", err, time.Since(start))
	}
	// Nothing listening.
	c2 := slirpapi.New("/tmp/no-such-slirp.sock")
	if _, err := c2.ListHostFwd(context.Background()); err == nil {
		t.Error("no error without a socket")
	}
	// A cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ListHostFwd(ctx); err == nil {
		t.Error("no error for a cancelled context")
	}
}

func TestWhichAnswersCountAsABindFailure(t *testing.T) {
	for desc, want := range map[string]bool{
		"bad request: add_hostfwd: slirp_add_hostfwd failed": true, // slirp4netns 1.2.1, for a port something else holds
		"bind failed: Address already in use":                true,
		"remove_hostfwd failed: no such id":                  false,
		"unknown command":                                    false,
	} {
		if got := (&slirpapi.APIError{Desc: desc}).IsBindError(); got != want {
			t.Errorf("%q: %v", desc, got)
		}
	}
}
