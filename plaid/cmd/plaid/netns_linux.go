//go:build linux

package main

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
	"plaid/pkg/tap"
)

// createTapInNetNS creates a TAP with the given number of queues in the network namespace
// at netnsPath (the current one if it is empty). All the queues are attached in it, by the
// thread that is in it.
func createTapInNetNS(netnsPath string, cfg tap.TapConfig, queues int) ([]*os.File, error) {
	if netnsPath == "" {
		return tap.CreateAndConfigureTapQueues(cfg, queues)
	}

	runtime.LockOSThread()
	// If we fail to restore the thread's original netns, we must NOT call
	// runtime.UnlockOSThread(), as that would return an OS thread trapped in the
	// container's netns back into the Go runtime worker pool.
	restored := false
	defer func() {
		if restored {
			runtime.UnlockOSThread()
		}
	}()

	// Open the calling thread's current network namespace
	curNS, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		// Fallback to /proc/self/ns/net for older kernels (<3.17)
		curNS, err = os.Open("/proc/self/ns/net")
		if err != nil {
			restored = true
			return nil, fmt.Errorf("failed to open current netns: %w", err)
		}
	}
	defer curNS.Close()

	targetNS, err := os.Open(netnsPath)
	if err != nil {
		restored = true
		return nil, fmt.Errorf("failed to open target netns %s: %w", netnsPath, err)
	}
	defer targetNS.Close()

	if err := unix.Setns(int(targetNS.Fd()), unix.CLONE_NEWNET); err != nil {
		restored = true
		return nil, fmt.Errorf("setns to target netns failed: %w", err)
	}

	tapFiles, tapErr := tap.CreateAndConfigureTapQueues(cfg, queues)

	// Revert to original host netns
	if err := unix.Setns(int(curNS.Fd()), unix.CLONE_NEWNET); err != nil {
		for _, f := range tapFiles {
			_ = f.Close()
		}
		return nil, fmt.Errorf("failed to restore original netns (OS thread permanently locked): %w", err)
	}

	restored = true
	return tapFiles, tapErr
}
