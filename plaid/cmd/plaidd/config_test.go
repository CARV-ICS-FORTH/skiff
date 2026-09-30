package main

import (
	"strings"
	"testing"

	"plaid/pkg/netcfg"
)

// tapConfig is an integrated (cluster) configuration; tests change what they test.
func tapConfig(change func(*Config)) Config {
	cfg := Config{
		SocketPath:        "/tmp/test-plaidd.sock",
		NodeCIDR:          "10.244.5.0/24",
		DisableKubernetes: true,
		ClusterCIDR:       "10.244.0.0/16",
		ServiceCIDR:       "10.43.0.0/16",
		Resolver:          "10.43.0.10",
		GatewayIP:         "10.244.5.1",
		GatewayMAC:        "02:00:00:00:00:01",
		BubbleMAC:         "02:00:00:00:00:02",
		GatewayMode:       "tap",
		Uplink:            "tap0",
		UplinkAddress:     "10.0.2.100",
		VXLANPort:         8472,
		VXLANVNI:          1,
	}
	if change != nil {
		change(&cfg)
	}
	return cfg
}

func TestNewPlaidDaemon_IntegratedConfiguration(t *testing.T) {
	t.Setenv(netcfg.EnvMTU, "")

	t.Run("the default deployment", func(t *testing.T) {
		d, err := NewPlaidDaemon(tapConfig(nil))
		if err != nil {
			t.Fatal(err)
		}
		if d.cfg.MTU != netcfg.DefaultMTU {
			t.Errorf("MTU %d, want the default %d", d.cfg.MTU, netcfg.DefaultMTU)
		}
	})

	t.Run("alternate networks, resolver and MTU are honoured", func(t *testing.T) {
		d, err := NewPlaidDaemon(tapConfig(func(c *Config) {
			c.NodeCIDR, c.ClusterCIDR, c.GatewayIP = "10.200.3.0/24", "10.200.0.0/16", "10.200.3.1"
			c.ServiceCIDR, c.Resolver, c.MTU = "10.201.0.0/16", "10.201.0.10", 1400
		}))
		if err != nil {
			t.Fatal(err)
		}
		status, err := d.HandleGetStatus()
		if err != nil {
			t.Fatal(err)
		}
		if status.MTU != 1400 || status.Resolver != "10.201.0.10" {
			t.Errorf("published MTU %d and resolver %q, want 1400 and 10.201.0.10", status.MTU, status.Resolver)
		}
	})

	for _, c := range []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{"resolver outside the Service network", func(c *Config) { c.Resolver = "10.9.9.9" }, "outside the Service CIDR"},
		{"resolver that is not an address", func(c *Config) { c.Resolver = "coredns" }, "invalid resolver"},
		{"IPv6 resolver", func(c *Config) { c.Resolver = "fd00::10" }, "invalid resolver"},
		{"MTU below the minimum", func(c *Config) { c.MTU = 500 }, "invalid MTU"},
		{"MTU above the maximum", func(c *Config) { c.MTU = 70000 }, "invalid MTU"},
		{"malformed cluster CIDR", func(c *Config) { c.ClusterCIDR = "10.244.0.0" }, "invalid cluster-cidr"},
		{"malformed Service CIDR", func(c *Config) { c.ServiceCIDR = "services" }, "invalid IPv4 service-cidr"},
		{"overlapping cluster and Service CIDRs", func(c *Config) { c.ServiceCIDR = "10.244.128.0/17" }, "overlap"},
		{"node CIDR outside the cluster CIDR", func(c *Config) { c.NodeCIDR, c.GatewayIP = "10.99.1.0/24", "10.99.1.1" }, "overlap"},
		{"uplink inside the pod network", func(c *Config) { c.UplinkAddress = "10.244.0.9" }, "uplink address overlaps"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewPlaidDaemon(tapConfig(c.change))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want one containing %q", err, c.want)
			}
		})
	}
}

// The environment supplies the MTU only where nothing else does.
func TestNewPlaidDaemon_MTUFromTheEnvironment(t *testing.T) {
	t.Setenv(netcfg.EnvMTU, "1400")
	d, err := NewPlaidDaemon(tapConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	if d.cfg.MTU != 1400 {
		t.Errorf("MTU %d from the environment, want 1400", d.cfg.MTU)
	}
	d, err = NewPlaidDaemon(tapConfig(func(c *Config) { c.MTU = 1300 }))
	if err != nil || d.cfg.MTU != 1300 {
		t.Errorf("an explicit MTU must win: %d, %v", d.cfg.MTU, err)
	}
}

// Standalone (slirp) mode keeps its own scope: the defaults apply without any of the
// cluster settings, and an explicit resolver is taken as it is, since there is no
// Service network for it to be in.
func TestNewPlaidDaemon_StandaloneSlirpDefaultsAndOverrides(t *testing.T) {
	t.Setenv(netcfg.EnvMTU, "")
	standalone := func(change func(*Config)) Config {
		cfg := Config{
			SocketPath:        "/tmp/test-plaidd.sock",
			NodeCIDR:          "10.244.1.0/24",
			DisableKubernetes: true,
			ClusterCIDR:       netcfg.DefaultClusterCIDR,
			GatewayIP:         "10.244.1.1",
			GatewayMAC:        "02:00:00:00:00:01",
			GatewayMode:       "slirp",
			VXLANPort:         8472,
			VXLANVNI:          1,
		}
		if change != nil {
			change(&cfg)
		}
		return cfg
	}
	d, err := NewPlaidDaemon(standalone(nil))
	if err != nil {
		t.Fatal(err)
	}
	if d.cfg.MTU != netcfg.DefaultMTU || d.cfg.ServiceCIDR != netcfg.DefaultServiceCIDR || d.cfg.Resolver != "" {
		t.Errorf("standalone defaults: MTU %d, Service CIDR %q, resolver %q", d.cfg.MTU, d.cfg.ServiceCIDR, d.cfg.Resolver)
	}
	d, err = NewPlaidDaemon(standalone(func(c *Config) { c.Resolver, c.MTU = "1.1.1.1", 1280 }))
	if err != nil {
		t.Fatalf("an explicit standalone resolver outside any Service network: %v", err)
	}
	status, _ := d.HandleGetStatus()
	if status.Resolver != "1.1.1.1" || status.MTU != 1280 {
		t.Errorf("published resolver %q and MTU %d, want the explicit 1.1.1.1 and 1280", status.Resolver, status.MTU)
	}
}
