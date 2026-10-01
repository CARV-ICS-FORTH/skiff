package nodeport

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recorded struct {
	stdin string
	name  string
	args  string
}

func fakeRun(log *[]recorded, fail func(name, args string) error) command {
	return func(_ context.Context, stdin, name string, args ...string) error {
		a := strings.Join(args, " ")
		*log = append(*log, recorded{stdin, name, a})
		if fail != nil {
			return fail(name, a)
		}
		return nil
	}
}

func TestRenderReplacesTheWholeChain(t *testing.T) {
	i := NewIptables("tap0", "10.0.2.100")
	got := i.Render([]Rule{
		{Proto: "tcp", Port: 31080, DestIP: [4]byte{10, 43, 0, 50}, DestPort: 80},
		{Proto: "udp", Port: 31353, DestIP: [4]byte{10, 43, 0, 51}, DestPort: 5353},
	})
	want := `*nat
:PLAID-NODEPORT - [0:0]
-A PLAID-NODEPORT -d 10.0.2.100 -p tcp -m tcp --dport 31080 -j DNAT --to-destination 10.43.0.50:80
-A PLAID-NODEPORT -d 10.0.2.100 -p udp -m udp --dport 31353 -j DNAT --to-destination 10.43.0.51:5353
COMMIT
`
	if got != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", got, want)
	}
	if empty := i.Render(nil); empty != "*nat\n:PLAID-NODEPORT - [0:0]\nCOMMIT\n" {
		t.Errorf("an empty set renders %q", empty)
	}
}

func TestApplyCreatesTheChainAndTheJumpOnceAndThenOnlyRestores(t *testing.T) {
	var log []recorded
	i := NewIptables("tap0", "10.0.2.100")
	i.run = fakeRun(&log, func(name, args string) error {
		if name == "iptables" && strings.Contains(args, " -C ") {
			return errors.New("no such rule") // the jump is not there yet
		}
		return nil
	})
	rules := []Rule{{Proto: "tcp", Port: 31080, DestIP: [4]byte{10, 43, 0, 50}, DestPort: 80}}
	if err := i.Apply(context.Background(), rules); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range log {
		kinds = append(kinds, r.name+" "+r.args)
	}
	want := []string{
		"iptables-restore -w --noflush",
		"iptables -w -t nat -C PREROUTING -i tap0 -j PLAID-NODEPORT",
		"iptables -w -t nat -I PREROUTING 1 -i tap0 -j PLAID-NODEPORT",
		"iptables-restore -w --noflush",
	}
	if strings.Join(kinds, "|") != strings.Join(want, "|") {
		t.Errorf("commands:\n%s", strings.Join(kinds, "\n"))
	}
	if !strings.Contains(log[len(log)-1].stdin, "--dport 31080") {
		t.Error("the last restore does not carry the rules")
	}
	log = nil
	if err := i.Apply(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].name != "iptables-restore" {
		t.Errorf("a second apply ran %v", log)
	}
}

func TestAnExistingJumpIsNotDuplicatedAndFailuresAreReported(t *testing.T) {
	var log []recorded
	i := NewIptables("tap0", "10.0.2.100")
	i.run = fakeRun(&log, nil) // -C succeeds: the jump exists
	if err := i.Apply(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range log {
		if strings.Contains(r.args, " -I ") {
			t.Error("the jump was inserted twice")
		}
	}
	bad := NewIptables("tap0", "10.0.2.100")
	bad.run = fakeRun(&log, func(name, args string) error { return errors.New("boom") })
	if err := bad.Apply(context.Background(), nil); err == nil {
		t.Error("an iptables failure was not reported")
	}
}

func TestCloseRemovesTheJumpAndTheChain(t *testing.T) {
	var log []recorded
	i := NewIptables("tap0", "10.0.2.100")
	i.run = fakeRun(&log, nil)
	i.Close(context.Background()) // nothing was made
	if len(log) != 0 {
		t.Errorf("closing an unused manager ran %v", log)
	}
	_ = i.Apply(context.Background(), nil)
	log = nil
	i.Close(context.Background())
	var kinds []string
	for _, r := range log {
		kinds = append(kinds, r.args)
	}
	want := "-w -t nat -D PREROUTING -i tap0 -j PLAID-NODEPORT|-w -t nat -F PLAID-NODEPORT|-w -t nat -X PLAID-NODEPORT"
	if strings.Join(kinds, "|") != want {
		t.Errorf("close ran %v", kinds)
	}
}
