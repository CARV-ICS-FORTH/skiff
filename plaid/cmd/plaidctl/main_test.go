package main

import (
	"strings"
	"testing"

	"plaid/pkg/api"
)

func TestSuperSummaryShowsTheFailedSends(t *testing.T) {
	got := superSummary(&api.OverlaySuperStatus{Enabled: true, Tx: 3, TxChunks: 9, Rx: 4, Partial: 1, Invalid: 2, Evicted: 5, Peers: 6, TxFailed: 7})
	want := "on tx 3/9 chunks of the segment size rx 4 lost 1 invalid 2 evicted 5 peers 6 send failures 7"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got = superSummary(&api.OverlaySuperStatus{Enabled: true, Tx: 3, TxChunks: 9, LinkMTU: 9000})
	want = "on tx 3/9 chunks of link MTU 9000 rx 0 lost 0 invalid 0 evicted 0 peers 0 send failures 0"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if superSummary(nil) != "none" {
		t.Error("no status is not none")
	}
}

func TestPrintDirectShowsTheNodeAndTheContainers(t *testing.T) {
	d := &api.DirectStatus{
		Running: 2, Ended: 1,
		Total: api.DirectCounters{Connects: 10, Binds: 2, Accepts: 7, Proxied: 3, Errors: 1,
			Fallbacks: map[string]uint64{"refused": 2, "no-ack": 1}, Rejected: map[string]uint64{"bad-mac": 4}},
		Containers: []api.DirectContainer{
			{Name: "default/a/c", PID: 11, Counters: api.DirectCounters{Connects: 6, Fallbacks: map[string]uint64{"refused": 2}}},
			{Name: "default/b/c", PID: 12, Counters: api.DirectCounters{Accepts: 7}},
		},
	}
	var node strings.Builder
	printDirect(&node, d, false)
	for _, want := range []string{"2 supervisors running, 1 ended", "connects=10 binds=2 accepts=7, proxied=3 (failed 0), errors=1",
		"no-ack=1 refused=2", "bad-mac=4"} {
		if !strings.Contains(node.String(), want) {
			t.Errorf("%q is not in:\n%s", want, node.String())
		}
	}
	if strings.Contains(node.String(), "default/a/c") {
		t.Errorf("the containers are listed without the flag:\n%s", node.String())
	}
	var each strings.Builder
	printDirect(&each, d, true)
	if !strings.Contains(each.String(), "default/a/c (pid 11): connects=6") || !strings.Contains(each.String(), "default/b/c (pid 12)") ||
		!strings.Contains(each.String(), "turned away: none") {
		t.Errorf("per container:\n%s", each.String())
	}
}
