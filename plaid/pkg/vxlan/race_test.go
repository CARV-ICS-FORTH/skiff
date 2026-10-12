//go:build race

package vxlan

// raceEnabled: under the race detector sync.Pool drops buffers at random, which makes
// allocation counts noisy.
const raceEnabled = true
