package root

import (
	"testing"

	"github.com/spf13/pflag"

	"skifflet/internal/compute"
)

func TestValidateClusterDomain(t *testing.T) {
	for _, ok := range []string{"cluster.local", "lab.test", "a", "k8s.example.org", "x-y.z"} {
		if err := validateClusterDomain(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Cluster.Local", "my_cluster", "-lead.ing", "trailing.", "sp ace", "a..b"} {
		if err := validateClusterDomain(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The flag defaults to the standard domain, follows the deployment's variable, and
// is overridden by the flag itself.
func TestClusterDomainFlag(t *testing.T) {
	parse := func(env string, args ...string) string {
		t.Setenv(EnvClusterDomain, env)
		var c Opts
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		installFlags(flags, &c)
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		return c.DefaultHostEnvironment.ClusterDomainOrDefault()
	}
	if got := parse(""); got != compute.DefaultClusterDomain {
		t.Errorf("default %q", got)
	}
	if got := parse("lab.test"); got != "lab.test" {
		t.Errorf("from the deployment's variable: %q", got)
	}
	if got := parse("lab.test", "--cluster-domain=other.example"); got != "other.example" {
		t.Errorf("flag over variable: %q", got)
	}
}
