package main

import (
	"testing"

	"plaid/pkg/api"
)

func TestSuperSummaryShowsTheFailedSends(t *testing.T) {
	got := superSummary(&api.OverlaySuperStatus{Enabled: true, Tx: 3, TxChunks: 9, Rx: 4, Partial: 1, Invalid: 2, Evicted: 5, Peers: 6, TxFailed: 7})
	want := "on tx 3/9 chunks rx 4 lost 1 invalid 2 evicted 5 peers 6 send failures 7"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if superSummary(nil) != "none" {
		t.Error("no status is not none")
	}
}
