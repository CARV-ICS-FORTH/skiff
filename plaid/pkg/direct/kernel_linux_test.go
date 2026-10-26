//go:build linux && (amd64 || arm64)

package direct

import (
	"errors"
	"strings"
	"testing"
)

// The kernel is refused for what it lacks, each named with the release that brought it;
// a kernel that has everything is not.
func TestKernelFeaturesNamedWhenMissing(t *testing.T) {
	has := func() error { return nil }
	lacks := func() error { return errors.New("function not implemented") }
	feature := func(name string, probe func() error) kernelFeature { return kernelFeature{name, "5.x", probe} }

	if err := checkKernelFeatures([]kernelFeature{feature("a", has), feature("b", has)}); err != nil {
		t.Errorf("a kernel with everything: %v", err)
	}
	err := checkKernelFeatures([]kernelFeature{feature("a", has), feature("b", lacks), feature("c", lacks)})
	if err == nil {
		t.Fatal("a kernel without two interfaces was accepted")
	}
	for _, want := range []string{"b (Linux 5.x: function not implemented)", "c (Linux 5.x", "Linux " + MinKernel + " or newer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "a (") {
		t.Errorf("%q names an interface the kernel has", err)
	}
}

// The probes agree with the kernel the tests run on, which has everything the supervisor
// needs (CI's runners and the nodes: 5.9 or newer).
func TestKernelProbesOnThisKernel(t *testing.T) {
	for _, f := range kernelFeatures {
		if err := f.probe(); err != nil {
			t.Errorf("%s: %v", f.name, err)
		}
	}
}
