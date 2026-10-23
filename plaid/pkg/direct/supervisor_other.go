//go:build !(linux && (amd64 || arm64))

package direct

import (
	"errors"
	"os/exec"
)

// Supervisor is not available here: the filter's system call numbers are those of amd64
// and arm64 Linux.
type Supervisor struct{}

// Start fails: the direct path needs Linux on amd64 or arm64.
func Start(Config, *exec.Cmd) (*Supervisor, error) {
	return nil, errors.New("the direct path needs Linux on amd64 or arm64")
}

// Wait is not reached: there is no supervisor.
func (*Supervisor) Wait() error { return errors.New("no supervisor") }

// RunWatchdogIfRequested does nothing here: there is no watchdog.
func RunWatchdogIfRequested() {}

// PlaceRuntimeDescriptors does nothing where there is no supervisor.
func PlaceRuntimeDescriptors() {}
