//go:build race

package tap

// raceEnabled: under the race detector sync.Pool drops buffers at random, which makes
// allocation counts noisy.
const raceEnabled = true
