package main

import "testing"

func TestProcs(t *testing.T) {
	for _, c := range []struct {
		env  string
		def  int
		want int
	}{
		{"", 64, maxProcs}, // a large node: capped
		{"", 128, maxProcs},
		{"", 16, 16},
		{"", 8, 8}, // a small node, or a cgroup limit: the runtime's own
		{"", 1, 1},
		{"32", 64, 0}, // the environment decides
		{"4", 64, 0},
	} {
		if got := procs(c.env, c.def); got != c.want {
			t.Errorf("procs(%q, %d) = %d, want %d", c.env, c.def, got, c.want)
		}
	}
}
