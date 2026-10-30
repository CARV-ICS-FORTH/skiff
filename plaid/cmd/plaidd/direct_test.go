package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"plaid/pkg/api"
)

func counters(connects uint64, fallbacks map[string]uint64) api.DirectCounters {
	return api.DirectCounters{Connects: connects, Accepts: connects * 2, Fallbacks: fallbacks}
}

// A supervisor's report replaces its last (they are totals), one that ended is added to those
// that did, and a supervisor that is gone without a last report has ended as well.
func TestDirectBookSumsTheSupervisors(t *testing.T) {
	gone := map[int]bool{}
	b := &directBook{alive: func(pid int) bool { return !gone[pid] }}
	if b.status() != nil {
		t.Fatal("a status before any report")
	}
	report := func(name string, pid int, ended bool, c api.DirectCounters) {
		t.Helper()
		if err := b.report(&api.DirectReport{Supervisor: name, PID: pid, Ended: ended, Counters: c}); err != nil {
			t.Fatal(err)
		}
	}
	report("default/web/a", 10, false, counters(1, nil))
	report("default/web/a", 10, false, counters(5, map[string]uint64{"refused": 1}))
	report("default/web/b", 11, false, counters(2, map[string]uint64{"refused": 2, "no-ack": 1}))
	st := b.status()
	if st.Running != 2 || st.Ended != 0 || st.Total.Connects != 7 || st.Total.Accepts != 14 || st.Total.Fallbacks["refused"] != 3 || st.Total.Fallbacks["no-ack"] != 1 {
		t.Errorf("two running: %+v", st)
	}
	if len(st.Containers) != 2 || st.Containers[0].Name != "default/web/a" || st.Containers[0].Counters.Connects != 5 {
		t.Errorf("containers %+v", st.Containers)
	}

	report("default/web/a", 10, true, counters(6, map[string]uint64{"refused": 1}))
	st = b.status()
	if st.Running != 1 || st.Ended != 1 || st.Total.Connects != 8 || st.Total.Fallbacks["refused"] != 3 {
		t.Errorf("one ended: %+v", st)
	}
	gone[11] = true // killed: no last report
	st = b.status()
	if st.Running != 0 || st.Ended != 2 || st.Total.Connects != 8 || len(st.Containers) != 0 {
		t.Errorf("one killed: %+v", st)
	}
	// A new run of a container (another process) is another supervisor.
	report("default/web/a", 12, false, counters(1, nil))
	if st = b.status(); st.Running != 1 || st.Total.Connects != 9 {
		t.Errorf("a new run: %+v", st)
	}
}

func TestDirectBookRefusesWhatCouldFillIt(t *testing.T) {
	b := &directBook{alive: func(int) bool { return true }}
	many := map[string]uint64{}
	for i := 0; i <= maxDirectReasons; i++ {
		many[strings.Repeat("x", i+1)] = 1
	}
	for name, r := range map[string]api.DirectReport{
		"no name":       {PID: 1},
		"no process":    {Supervisor: "a"},
		"a long name":   {Supervisor: strings.Repeat("a", maxDirectName+1), PID: 1},
		"many reasons":  {Supervisor: "a", PID: 1, Counters: api.DirectCounters{Fallbacks: many}},
		"a long reason": {Supervisor: "a", PID: 1, Counters: api.DirectCounters{Rejected: map[string]uint64{strings.Repeat("r", maxDirectName+1): 1}}},
	} {
		if err := b.report(&r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for i := 1; i <= maxDirectSupervisors; i++ {
		if err := b.report(&api.DirectReport{Supervisor: "s", PID: i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.report(&api.DirectReport{Supervisor: "s", PID: maxDirectSupervisors + 1}); err == nil {
		t.Error("more supervisors than the limit")
	}
	if err := b.report(&api.DirectReport{Supervisor: "s", PID: 1, Counters: api.DirectCounters{Connects: 1}}); err != nil {
		t.Errorf("a supervisor that is known could not report again: %v", err)
	}
	if err := b.report(&api.DirectReport{Supervisor: "s", PID: maxDirectSupervisors + 1, Ended: true}); err != nil {
		t.Errorf("a supervisor that ended could not say so: %v", err)
	}
}

// The reports go through plaidd's socket and come back in its status.
func TestDirectReportsGoThroughTheSocket(t *testing.T) {
	d := tapDaemon(t, nil)
	dir, err := os.MkdirTemp("/tmp", "pd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "p.sock")
	d.apiServer = api.NewServer(path, d, d, d)
	d.apiServer.SetDirectHandler(d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.apiServer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer d.apiServer.Close()
	d.apiServer.MarkReady()

	client := api.NewClient(path)
	if st, err := client.GetStatus(); err != nil || st.Direct != nil {
		t.Fatalf("status before a report: %+v, %v", st.Direct, err)
	}
	pid := os.Getpid() // a process that is there
	if err := client.ReportDirect(api.DirectReport{Supervisor: "default/p/c", PID: pid, Counters: api.DirectCounters{Connects: 3, Rejected: map[string]uint64{"bad-mac": 2}}}); err != nil {
		t.Fatal(err)
	}
	st, err := client.GetStatus()
	if err != nil || st.Direct == nil || st.Direct.Running != 1 || st.Direct.Total.Connects != 3 || st.Direct.Total.Rejected["bad-mac"] != 2 ||
		len(st.Direct.Containers) != 1 || st.Direct.Containers[0].Name != "default/p/c" {
		t.Errorf("status %+v, %v", st.Direct, err)
	}
	if err := client.ReportDirect(api.DirectReport{}); err == nil {
		t.Error("a report without a supervisor was accepted through the socket")
	}
}
