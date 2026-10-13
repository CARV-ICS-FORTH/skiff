//go:build !linux

package main

import (
	"os"

	"plaid/pkg/tap"
)

func createTapInNetNS(netnsPath string, cfg tap.TapConfig, queues int) ([]*os.File, error) {
	return tap.CreateAndConfigureTapQueues(cfg, queues)
}
