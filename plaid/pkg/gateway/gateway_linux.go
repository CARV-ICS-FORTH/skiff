//go:build linux

package gateway

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"plaid/pkg/tap"
)

// Config describes only resources owned inside the bubble network namespace.
type Config struct {
	Name, Uplink                       string
	GuestAddress                       net.IP
	GatewayIP                          net.IP
	GatewayMAC                         net.HardwareAddr
	NodeCIDR, ClusterCIDR, ServiceCIDR *net.IPNet
	// ServiceNextHop is the Service engine's address: the bubble reaches Service addresses
	// through it, and Plaid answers for it.
	ServiceNextHop net.IP
	MTU            int
}

type Attachment struct {
	File                                     *os.File
	cfg                                      Config
	sysctls                                  map[string]string
	routeAdded, serviceRouteAdded            bool
	natTouched, forwardTouched, inputTouched bool
}

func run(bin string, args ...string) error {
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func rule(table, action, chain string, spec ...string) error {
	args := []string{"-w", "-t", table, action, chain}
	return run("iptables", append(args, spec...)...)
}

func ensureJump(table, parent, child string) error {
	if rule(table, "-C", parent, "-j", child) == nil {
		return nil
	}
	return rule(table, "-I", parent, "1", "-j", child)
}

func ensureChain(table, chain string) error {
	if err := rule(table, "-N", chain); err != nil {
		if check := rule(table, "-F", chain); check != nil {
			return err
		}
	}
	return nil
}

func sysctl(a *Attachment, key, value string) error {
	path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
	prior, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	a.sysctls[path] = strings.TrimSpace(string(prior))
	return os.WriteFile(path, []byte(value), 0644)
}

func Start(cfg Config) (_ *Attachment, err error) {
	if cfg.Name == "" {
		cfg.Name = "plaid0"
	}
	if cfg.Uplink == "" || cfg.GuestAddress.To4() == nil || cfg.GatewayIP.To4() == nil || cfg.NodeCIDR == nil || cfg.ClusterCIDR == nil || cfg.ServiceCIDR == nil {
		return nil, fmt.Errorf("incomplete TAP gateway configuration")
	}
	iface, err := net.InterfaceByName(cfg.Uplink)
	if err != nil {
		return nil, fmt.Errorf("uplink %s: %w", cfg.Uplink, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	found := false
	for _, addr := range addrs {
		if ip, network, e := net.ParseCIDR(addr.String()); e == nil {
			if ip.Equal(cfg.GuestAddress) {
				found = true
			}
			if network.IP.To4() != nil && (network.Contains(cfg.ClusterCIDR.IP) || cfg.ClusterCIDR.Contains(network.IP) || network.Contains(cfg.ServiceCIDR.IP) || cfg.ServiceCIDR.Contains(network.IP)) {
				return nil, fmt.Errorf("uplink network %s overlaps pod or Service CIDR", network)
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("uplink %s does not own guest address %s", cfg.Uplink, cfg.GuestAddress)
	}
	a := &Attachment{cfg: cfg, sysctls: map[string]string{}}
	defer func() {
		if err != nil {
			_ = a.Close()
		}
	}()
	a.File, err = tap.CreateAndConfigureTap(tap.TapConfig{Name: cfg.Name, IP: cfg.GatewayIP, Mask: cfg.NodeCIDR.Mask, MAC: cfg.GatewayMAC, MTU: cfg.MTU})
	if err != nil {
		return nil, err
	}
	if err = run("ip", "route", "add", cfg.ClusterCIDR.String(), "dev", cfg.Name, "scope", "link", "src", cfg.GatewayIP.String()); err != nil {
		return nil, err
	}
	a.routeAdded = true
	if cfg.ServiceNextHop != nil {
		// Service addresses are not on the link: they go to the engine's address, which is.
		if err = run("ip", "route", "add", cfg.ServiceCIDR.String(), "via", cfg.ServiceNextHop.String(), "dev", cfg.Name, "src", cfg.GatewayIP.String()); err != nil {
			return nil, err
		}
		a.serviceRouteAdded = true
	}
	if err = sysctl(a, "net.ipv4.ip_forward", "1"); err != nil {
		return nil, err
	}
	for _, key := range []string{"net.ipv4.conf.all.send_redirects", "net.ipv4.conf." + cfg.Name + ".send_redirects"} {
		if err = sysctl(a, key, "0"); err != nil {
			return nil, err
		}
	}
	if err = ensureChain("nat", "PLAID-EGRESS"); err != nil {
		return nil, err
	}
	a.natTouched = true
	if err = rule("nat", "-A", "PLAID-EGRESS", "-s", cfg.ClusterCIDR.String(), "!", "-d", cfg.ClusterCIDR.String(), "-o", cfg.Uplink, "-j", "SNAT", "--to-source", cfg.GuestAddress.String()); err != nil {
		return nil, err
	}
	if err = ensureJump("nat", "POSTROUTING", "PLAID-EGRESS"); err != nil {
		return nil, err
	}
	if err = ensureChain("filter", "PLAID-FORWARD"); err != nil {
		return nil, err
	}
	a.forwardTouched = true
	for _, spec := range [][]string{
		{"-i", cfg.Name, "-o", cfg.Name, "-j", "ACCEPT"},
		{"-i", cfg.Name, "-o", cfg.Uplink, "-j", "ACCEPT"},
		{"-i", cfg.Uplink, "-o", cfg.Name, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		// New connections that the NodePort destination NAT (PLAID-NODEPORT) has redirected to a Service.
		{"-i", cfg.Uplink, "-o", cfg.Name, "-m", "conntrack", "--ctstate", "DNAT", "-j", "ACCEPT"},
	} {
		if err = rule("filter", "-A", "PLAID-FORWARD", spec...); err != nil {
			return nil, err
		}
	}
	if err = ensureJump("filter", "FORWARD", "PLAID-FORWARD"); err != nil {
		return nil, err
	}
	if err = ensureChain("filter", "PLAID-INPUT"); err != nil {
		return nil, err
	}
	a.inputTouched = true
	if err = rule("filter", "-A", "PLAID-INPUT", "-i", cfg.Name, "-d", cfg.GatewayIP.String(), "-j", "ACCEPT"); err != nil {
		return nil, err
	}
	if err = ensureJump("filter", "INPUT", "PLAID-INPUT"); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Attachment) Close() error {
	if a == nil {
		return nil
	}
	for _, item := range []struct {
		table, parent, child string
		touched              bool
	}{{"nat", "POSTROUTING", "PLAID-EGRESS", a.natTouched}, {"filter", "FORWARD", "PLAID-FORWARD", a.forwardTouched}, {"filter", "INPUT", "PLAID-INPUT", a.inputTouched}} {
		if !item.touched {
			continue
		}
		_ = rule(item.table, "-D", item.parent, "-j", item.child)
		_ = rule(item.table, "-F", item.child)
		_ = rule(item.table, "-X", item.child)
	}
	if a.serviceRouteAdded {
		_ = run("ip", "route", "del", a.cfg.ServiceCIDR.String(), "dev", a.cfg.Name)
	}
	if a.routeAdded && a.cfg.ClusterCIDR != nil {
		_ = run("ip", "route", "del", a.cfg.ClusterCIDR.String(), "dev", a.cfg.Name)
	}
	for path, previous := range a.sysctls {
		_ = os.WriteFile(path, []byte(previous), 0644)
	}
	if a.File != nil {
		return a.File.Close()
	}
	return nil
}
