package nodeport

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// Chain is the nat chain the rules live in; it is jumped to from PREROUTING for traffic
// that came in on the uplink.
const Chain = "PLAID-NODEPORT"

// command runs a program with input; replaceable for tests.
type command func(ctx context.Context, stdin string, name string, args ...string) error

func runCommand(ctx context.Context, stdin string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Iptables is the bubble's kernel side of NodePorts: a destination NAT, for what arrives on
// the uplink addressed to the bubble at a node port, to the Service's address and port.
// What the NAT starts is carried by the kernel's connection tracking, which also restores
// the addresses of the replies; the Service engine only ever sees a client and a Service.
type Iptables struct {
	Uplink    string // the interface slirp4netns delivers on
	GuestAddr string // the bubble's address on it, where the forwards point
	run       command
	jumped    bool
}

// NewIptables creates the rules manager.
func NewIptables(uplink, guestAddr string) *Iptables {
	return &Iptables{Uplink: uplink, GuestAddr: guestAddr, run: runCommand}
}

// Render gives the iptables-restore input for a set of rules. The chain is declared in the
// same transaction, so restoring replaces its whole content at once.
func (i *Iptables) Render(rules []Rule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*nat\n:%s - [0:0]\n", Chain)
	for _, r := range rules {
		fmt.Fprintf(&b, "-A %s -d %s -p %s -m %s --dport %d -j DNAT --to-destination %s:%d\n",
			Chain, i.GuestAddr, r.Proto, r.Proto, r.Port, net.IP(r.DestIP[:]), r.DestPort)
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// Apply replaces the rules.
func (i *Iptables) Apply(ctx context.Context, rules []Rule) error {
	if !i.jumped {
		// The chain must exist before the jump does; an empty one is declared by a restore.
		if err := i.run(ctx, i.Render(nil), "iptables-restore", "-w", "--noflush"); err != nil {
			return err
		}
		if err := i.run(ctx, "", "iptables", "-w", "-t", "nat", "-C", "PREROUTING", "-i", i.Uplink, "-j", Chain); err != nil {
			if err := i.run(ctx, "", "iptables", "-w", "-t", "nat", "-I", "PREROUTING", "1", "-i", i.Uplink, "-j", Chain); err != nil {
				return err
			}
		}
		i.jumped = true
	}
	return i.run(ctx, i.Render(rules), "iptables-restore", "-w", "--noflush")
}

// Close removes the jump and the chain.
func (i *Iptables) Close(ctx context.Context) {
	if !i.jumped {
		return
	}
	_ = i.run(ctx, "", "iptables", "-w", "-t", "nat", "-D", "PREROUTING", "-i", i.Uplink, "-j", Chain)
	_ = i.run(ctx, "", "iptables", "-w", "-t", "nat", "-F", Chain)
	_ = i.run(ctx, "", "iptables", "-w", "-t", "nat", "-X", Chain)
	i.jumped = false
}
