package podhandler

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/image"
)

// A pod that sets securityContext fields Skifflet does not apply is told which; the ones it
// applies (the user, the group, runAsNonRoot) and ones that ask for nothing are not listed.
func TestUnappliedSecurityContext(t *testing.T) {
	if got := UnappliedSecurityContext(&corev1.Pod{}); got != "" {
		t.Errorf("a pod without a security context: %q", got)
	}
	applied := &corev1.Pod{Spec: corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr(int64(1000)), RunAsGroup: ptr(int64(2000)), RunAsNonRoot: ptr(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}},
		Containers: []corev1.Container{{Name: "app", SecurityContext: &corev1.SecurityContext{
			RunAsUser: ptr(int64(1000)), AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(false), Privileged: ptr(false)}}},
	}}
	if got := UnappliedSecurityContext(applied); got != "" {
		t.Errorf("only applied or neutral fields, got %q", got)
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{FSGroup: ptr(int64(3000)), SupplementalGroups: []int64{4000},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		InitContainers: []corev1.Container{{Name: "init", SecurityContext: &corev1.SecurityContext{Privileged: ptr(true)}}},
		Containers: []corev1.Container{
			{Name: "app", SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr(true),
				Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}},
			{Name: "plain"},
		},
	}}
	got := UnappliedSecurityContext(pod)
	for _, want := range []string{"pod: fsGroup, supplementalGroups, seccompProfile", "container app: readOnlyRootFilesystem"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not say %q", got, want)
		}
	}
	if strings.Contains(got, "plain") {
		t.Errorf("a container without a security context is listed: %q", got)
	}
	for _, applied := range []string{"privileged", "capabilities", "container init"} {
		if strings.Contains(got, applied) {
			t.Errorf("%q is applied, and listed: %q", applied, got)
		}
	}
}

func TestCapabilityArgs(t *testing.T) {
	defaults := strings.Join(defaultCapabilities, ",")
	withoutChown := strings.TrimPrefix(strings.Replace(defaults, ",CAP_CHOWN", "", 1), ",")
	caps := func(add, drop []corev1.Capability) *corev1.Capabilities {
		return &corev1.Capabilities{Add: add, Drop: drop}
	}
	for name, test := range map[string]struct {
		privileged bool
		caps       *corev1.Capabilities
		want       []string
	}{
		"the default":                      {false, nil, []string{"--no-privs", "--add-caps", defaults}},
		"nothing added or dropped":         {false, caps(nil, nil), []string{"--no-privs", "--add-caps", defaults}},
		"privileged":                       {true, caps(nil, []corev1.Capability{"ALL"}), nil},
		"added, as Kubernetes writes":      {false, caps([]corev1.Capability{"NET_ADMIN"}, nil), []string{"--no-privs", "--add-caps", defaults + ",CAP_NET_ADMIN"}},
		"added, with the prefix":           {false, caps([]corev1.Capability{"cap_net_admin"}, nil), []string{"--no-privs", "--add-caps", defaults + ",CAP_NET_ADMIN"}},
		"added and already there":          {false, caps([]corev1.Capability{"CHOWN"}, nil), []string{"--no-privs", "--add-caps", defaults}},
		"dropped":                          {false, caps(nil, []corev1.Capability{"CHOWN"}), []string{"--no-privs", "--add-caps", withoutChown}},
		"all dropped":                      {false, caps(nil, []corev1.Capability{"ALL"}), []string{"--no-privs"}},
		"all dropped, one added":           {false, caps([]corev1.Capability{"NET_BIND_SERVICE"}, []corev1.Capability{"ALL"}), []string{"--no-privs", "--add-caps", "CAP_NET_BIND_SERVICE"}},
		"all added":                        {false, caps([]corev1.Capability{"ALL"}, nil), nil},
		"all added and dropped":            {false, caps([]corev1.Capability{"ALL"}, []corev1.Capability{"ALL"}), []string{"--no-privs"}},
		"all added and dropped, one added": {false, caps([]corev1.Capability{"ALL", "NET_ADMIN"}, []corev1.Capability{"ALL"}), []string{"--no-privs", "--add-caps", "CAP_NET_ADMIN"}},
		"all added, all and one dropped":   {false, caps([]corev1.Capability{"ALL"}, []corev1.Capability{"ALL", "CHOWN"}), []string{"--no-privs"}},
		"all added, one dropped":           {false, caps([]corev1.Capability{"ALL"}, []corev1.Capability{"SYS_ADMIN"}), []string{"--drop-caps", "CAP_SYS_ADMIN"}},
		"dropped wins over added":          {false, caps([]corev1.Capability{"NET_ADMIN"}, []corev1.Capability{"NET_ADMIN"}), []string{"--no-privs", "--add-caps", defaults}},
	} {
		if got := capabilityArgs(test.privileged, test.caps); !slices.Equal(got, test.want) {
			t.Errorf("%s: got %q, want %q", name, got, test.want)
		}
	}
	if joined := strings.Join(defaultCapabilities, ","); strings.Contains(joined, "SYS_ADMIN") || strings.Contains(joined, "NET_ADMIN") || strings.Contains(joined, "SYS_PTRACE") {
		t.Errorf("the default gives what it is there to take away: %s", joined)
	}
}

func TestBuildApptainerArgs_CapabilitiesOnlyOfRoot(t *testing.T) {
	podDir := endpoint.PodPath(filepath.Join(t.TempDir(), "pod"))
	root := &Container{Name: "app", ImageFilePath: "/i.sif"}
	if args := root.BuildApptainerArgs(0, podDir, 1); !slices.Contains(args, "--no-privs") || !slices.Contains(args, "--add-caps") {
		t.Errorf("a container that runs as root keeps every capability: %q", args)
	}
	other := &Container{Name: "app", ImageFilePath: "/i.sif", RunAsUser: 1000}
	if args := other.BuildApptainerArgs(0, podDir, 1); slices.Contains(args, "--no-privs") || slices.Contains(args, "--add-caps") {
		t.Errorf("a container in a user namespace of its own has none to take: %q", args)
	}
}

// The user a container runs as: the pod's, else the image's, else root; runAsNonRoot refuses
// root as the kubelet does.
func TestDetermineEffectiveRunAsUser(t *testing.T) {
	imgUser := image.User{UID: 101, GID: 102, Set: true}
	for _, c := range []struct {
		name     string
		sc       *corev1.SecurityContext
		img      image.User
		uid, gid int64
		err      string
	}{
		{"nothing: root", &corev1.SecurityContext{}, image.User{}, 0, 0, ""},
		{"the image's user and its group", &corev1.SecurityContext{}, imgUser, 101, 102, ""},
		{"the pod's user over the image's (group 0 as before)", &corev1.SecurityContext{RunAsUser: ptr(int64(7))}, imgUser, 7, 0, ""},
		{"the pod's user and group", &corev1.SecurityContext{RunAsUser: ptr(int64(7)), RunAsGroup: ptr(int64(8))}, imgUser, 7, 8, ""},
		{"the pod's group over the image's, the image's user kept", &corev1.SecurityContext{RunAsGroup: ptr(int64(8))}, imgUser, 101, 8, ""},
		{"runAsNonRoot with a non-root image", &corev1.SecurityContext{RunAsNonRoot: ptr(true)}, imgUser, 101, 102, ""},
		{"runAsNonRoot with a user", &corev1.SecurityContext{RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(5))}, image.User{}, 5, 0, ""},
		{"runAsNonRoot false is nothing", &corev1.SecurityContext{RunAsNonRoot: ptr(false)}, image.User{}, 0, 0, ""},
		{"runAsNonRoot and an image that runs as root", &corev1.SecurityContext{RunAsNonRoot: ptr(true)}, image.User{}, 0, 0, "container has runAsNonRoot and image will run as root"},
		{"runAsNonRoot and an image whose USER is root", &corev1.SecurityContext{RunAsNonRoot: ptr(true)}, image.User{Set: true}, 0, 0, "container has runAsNonRoot and image will run as root"},
		{"runAsNonRoot and runAsUser 0", &corev1.SecurityContext{RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(0))}, imgUser, 0, 0, "container's runAsUser breaks non-root policy"},
		{"runAsUser 0 over a non-root image", &corev1.SecurityContext{RunAsUser: ptr(int64(0))}, imgUser, 0, 0, ""},
	} {
		uid, gid, err := DetermineEffectiveRunAsUser(c.sc, c.img)
		if c.err != "" {
			if !IsConfigError(err) || err.Error() != c.err {
				t.Errorf("%s: error = %v; want a ConfigError %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil || uid != c.uid || gid != c.gid {
			t.Errorf("%s: %d:%d, %v; want %d:%d", c.name, uid, gid, err, c.uid, c.gid)
		}
	}
}

// A ConfigError waits with CreateContainerConfigError unless it says another reason.
func TestConfigReason(t *testing.T) {
	if got := configReason(configErrorf("x")); got != "CreateContainerConfigError" {
		t.Errorf("default reason: %q", got)
	}
	if got := configReason(&ConfigError{Message: "x", Reason: "CreateContainerError"}); got != "CreateContainerError" {
		t.Errorf("its own reason: %q", got)
	}
}
