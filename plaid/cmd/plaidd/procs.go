package main

import (
	"fmt"
	"os"
	"runtime"
)

// maxProcs caps the processors (Go's Ps) that plaidd runs Go code on. With one P per CPU of
// a large node, the Go scheduler keeps up to half of them spinning to look for work after
// every wake-up of a goroutine, which plaidd's pipeline makes for every frame. On tie1 and
// tie2 (64 CPUs, InfiniBand FDR) a cap of 16 gave the same or more throughput over RDMA with
// 8 to 16% less of plaidd's CPU per Gbit/s and a much smaller spread between runs, and
// over UDP no clear change in throughput with 5 to 7% less CPU; the CPU saved is the pods'.
// Not measured: other caps, and nodes with faster networks, which may need more.
const maxProcs = 16

// procs is the GOMAXPROCS that plaidd sets: the runtime's own (the CPUs, or fewer where a
// cgroup limits the process) capped at maxProcs, or 0 (leave it) when the environment sets
// GOMAXPROCS, which then decides, as it does for any Go program.
func procs(env string, runtimeDefault int) int {
	if env != "" {
		return 0
	}
	return min(runtimeDefault, maxProcs)
}

// capProcs applies procs and says what it did.
func capProcs() {
	n := procs(os.Getenv("GOMAXPROCS"), runtime.GOMAXPROCS(0))
	if n == 0 {
		fmt.Printf("[plaidd] GOMAXPROCS %d, from the environment\n", runtime.GOMAXPROCS(0))
		return
	}
	runtime.GOMAXPROCS(n)
	fmt.Printf("[plaidd] GOMAXPROCS %d (%d CPUs; at most %d)\n", n, runtime.NumCPU(), maxProcs)
}
