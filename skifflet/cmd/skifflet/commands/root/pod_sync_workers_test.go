package root

import (
	"testing"

	"github.com/spf13/pflag"

	"skifflet/internal/provider"
)

// The deployment starts Skifflet without the flag, so its default is the number of pod
// sync workers that runs: a terminating pod occupies one, and the provider's
// deletion-progress test is written against the same constant.
func TestPodSyncWorkersDefault(t *testing.T) {
	var c Opts
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	installFlags(flags, &c)
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if c.PodSyncWorkers != provider.DefaultPodSyncWorkers {
		t.Errorf("--pod-sync-workers defaults to %d, not %d", c.PodSyncWorkers, provider.DefaultPodSyncWorkers)
	}
	if err := flags.Parse([]string{"--pod-sync-workers=2"}); err != nil || c.PodSyncWorkers != 2 {
		t.Errorf("the flag does not override the default: %d, %v", c.PodSyncWorkers, err)
	}
}
