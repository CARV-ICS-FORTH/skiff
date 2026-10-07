package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"time"
)

// EnvPprofSocket names the variable that turns profiling on, like the flag --pprof-socket.
const EnvPprofSocket = "PLAID_PPROF_SOCKET"

// startPprof serves Go's profiling endpoints (CPU, heap and allocations, goroutines, mutex and block
// contention, execution trace) on a UNIX socket at path, for finding where plaidd's time goes where
// perf cannot be used (no root, perf_event_paranoid). It is off unless a path is given. The socket
// is the owner's only (mode 0600) and nothing listens on the network. The handlers are served from a
// mux of their own; importing net/http/pprof also registers them on http.DefaultServeMux, which plaidd
// never serves. The returned function stops the server and removes the socket.
//
// Turning it on also samples mutex and blocking events (1 in 100 mutex contentions, blocking events
// longer than 100 µs on average), which cost next to nothing; both are reset when it is stopped.
// The README shows how to use it.
func startPprof(path string) (stop func(), err error) {
	// A socket a crashed plaidd left behind is replaced; anything else at the path is not touched.
	if fi, statErr := os.Lstat(path); statErr == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("pprof socket %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("pprof socket %s: %w", path, err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("pprof socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("pprof socket %s: %w", path, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index) // also serves allocs, heap, goroutine, mutex, block by name
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// ReadHeaderTimeout only: a profile or trace streams for as long as the caller asked.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	runtime.SetMutexProfileFraction(100)
	runtime.SetBlockProfileRate(100_000) // nanoseconds of blocking per sample
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "[plaidd] pprof server: %v\n", err)
		}
	}()
	fmt.Printf("[plaidd] profiling on %s\n", path)

	return func() {
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close() // a profile still streaming does not hold plaidd's exit
		}
		_ = os.Remove(path)
	}, nil
}
