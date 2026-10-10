package netcfg

import (
	"net"
	"testing"
)

func TestSocketPath(t *testing.T) {
	t.Setenv(EnvSocket, "")
	if got := SocketPath(""); got != DefaultSocketPath {
		t.Errorf("default %q", got)
	}
	t.Setenv(EnvSocket, "/run/lab/plaidd.sock")
	if got := SocketPath(""); got != "/run/lab/plaidd.sock" {
		t.Errorf("environment %q", got)
	}
	if got := SocketPath("/explicit.sock"); got != "/explicit.sock" {
		t.Errorf("explicit over environment %q", got)
	}
}

func TestEffectiveMTU(t *testing.T) {
	t.Setenv(EnvMTU, "")
	if got := EffectiveMTU(0); got != DefaultMTU {
		t.Errorf("default %d", got)
	}
	t.Setenv(EnvMTU, "1400")
	if got := EffectiveMTU(0); got != 1400 {
		t.Errorf("environment %d", got)
	}
	if got := EffectiveMTU(1300); got != 1300 {
		t.Errorf("explicit over environment %d", got)
	}
	for _, bad := range []string{"big", "-5", "0", "1e3"} {
		t.Setenv(EnvMTU, bad)
		if got := EffectiveMTU(0); got != DefaultMTU {
			t.Errorf("environment %q gave %d, want the default", bad, got)
		}
	}
}

func TestValidateMTU(t *testing.T) {
	for _, ok := range []int{MinMTU, 1400, DefaultMTU, 9000, MaxMTU} {
		if err := ValidateMTU(ok); err != nil {
			t.Errorf("%d refused: %v", ok, err)
		}
	}
	for _, bad := range []int{-1, 0, 100, MinMTU - 1, MaxMTU + 1} {
		if err := ValidateMTU(bad); err == nil {
			t.Errorf("%d accepted", bad)
		}
	}
}

func TestValidateResolver(t *testing.T) {
	_, service, _ := net.ParseCIDR("10.201.0.0/16")
	if err := ValidateResolver("10.201.0.10", service); err != nil {
		t.Errorf("a resolver inside the Service network refused: %v", err)
	}
	if err := ValidateResolver("10.43.0.10", service); err == nil {
		t.Error("a resolver outside the Service network accepted")
	}
	for _, bad := range []string{"", "coredns", "fd00::10", "10.201.0"} {
		if err := ValidateResolver(bad, service); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := ValidateResolver("1.1.1.1", nil); err != nil {
		t.Errorf("without a Service network any IPv4 resolver stands: %v", err)
	}
}

// The standalone slirp resolver is the third address of the gateway's network.
func TestStandaloneResolver(t *testing.T) {
	for gateway, want := range map[string]string{"10.0.2.2": "10.0.2.3", "10.244.1.1": "10.244.1.3", "": "", "nope": "", "fd00::1": ""} {
		if got := StandaloneResolver(gateway); got != want {
			t.Errorf("gateway %q: %q, want %q", gateway, got, want)
		}
	}
}

func TestUplinkMTU(t *testing.T) {
	for _, tc := range []struct {
		explicit, pod, want int
		ok                  bool
	}{
		{0, 1450, 1450, true}, // none: the pod MTU
		{65520, 1450, 65520, true},
		{1450, 1450, 1450, true},
		{9000, 8950, 9000, true},
		{1449, 1450, 0, false},  // below the pod MTU
		{65521, 1450, 0, false}, // above what slirp4netns takes
		{70000, 1450, 0, false},
	} {
		got, err := UplinkMTU(tc.explicit, tc.pod)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("UplinkMTU(%d, %d) = %d, %v", tc.explicit, tc.pod, got, err)
		}
	}
}
