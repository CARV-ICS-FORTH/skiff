package podhandler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"skifflet/internal/compute/endpoint"
)

func TestEscapeSingleQuote_ShellRoundTrip(t *testing.T) {
	testCases := []struct {
		name  string
		input string
	}{
		{"simple", "simple"},
		{"spaces", "hello world"},
		{"single_quote", "foo'bar"},
		{"double_quote", `foo"bar`},
		{"dollar_sign", "foo$bar"},
		{"env_expansion_attempt", "$PATH ${VAR}"},
		{"backticks", "foo`whoami`bar"},
		{"backslashes", `foo\bar\baz`},
		{"newlines", "line1\nline2\nline3"},
		{"tabs", "col1\tcol2"},
		{"semicolon_and_pipes", "foo; bar | baz && qux || exit 1"},
		{"glob_chars", "* ? [a-z] {1..5}"},
		{"mixed_all", `echo "hello '$USER'" && \` + "\n" + `\` + "`whoami`" + ` $PATH`},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			escaped := EscapeSingleQuote(tc.input)

			// Execute a real shell command that outputs the escaped argument via printf '%s'
			cmd := exec.Command("sh", "-c", "printf '%s' "+escaped)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("shell execution failed for input %q (escaped %q): %v", tc.input, escaped, err)
			}

			if string(out) != tc.input {
				t.Errorf("Round-trip mismatch for %s:\ninput:   %q\nescaped: %q\ngot:     %q", tc.name, tc.input, escaped, string(out))
			}
		})
	}
}

func TestBuildApptainerArgs(t *testing.T) {
	tmpDir := t.TempDir()
	podDir := endpoint.PodPath(filepath.Join(tmpDir, "pod"))
	_ = os.MkdirAll(podDir.JobDir(), 0755)
	_ = os.WriteFile(filepath.Join(podDir.JobDir(), "resolv.conf"), []byte("nameserver 1.1.1.1\n"), 0644)
	_ = os.WriteFile(filepath.Join(podDir.JobDir(), "hosts"), []byte("127.0.0.1 localhost\n"), 0644)

	c := &Container{
		Name:          "app",
		ExecutionMode: "exec",
		ImageFilePath: "/images/alpine.sif",
		Command:       []string{"/bin/sh"},
		Args:          []string{"-c", "echo hi"},
		RunAsUser:     1000,
		RunAsGroup:    1000,
		Binds:         []string{"/host/data:/data:rw"},
	}

	args := c.BuildApptainerArgs(12345, podDir, 3)

	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "--host-networking exec") {
		t.Errorf("expected --host-networking exec prefix, got: %s", joined)
	}
	// The container runs as 1000: it is started inside the pod's network namespace
	// (LaunchCommand), not given it as an argument that Apptainer cannot honor for it.
	if strings.Contains(joined, "--netns-path") {
		t.Errorf("a container in a user namespace was given --netns-path: %s", joined)
	}
	if !strings.Contains(joined, "resolv.conf:/etc/resolv.conf") {
		t.Errorf("expected resolv.conf bind in args, got: %s", joined)
	}
	if !strings.Contains(joined, "--security uid:1000,gid:1000") {
		t.Errorf("expected security uid/gid in args, got: %s", joined)
	}
}

// A container does not get the node's /tmp and /var/tmp, which Apptainer would mount by
// default: it gets its own for the attempt, on disk, unless the pod mounts a volume
// there. (The binds are what the container sees; the rest of the arguments are tested above.)
func TestBuildApptainerArgs_ContainerOwnTmp(t *testing.T) {
	podDir := endpoint.PodPath(filepath.Join(t.TempDir(), "pod"))
	own := podDir.Container("app")
	bindsOf := func(c *Container) string {
		args := c.BuildApptainerArgs(1, podDir, 2)
		for i, arg := range args {
			if arg == "--no-mount" && i+1 < len(args) {
				if args[i+1] != NoMounts {
					t.Errorf("--no-mount %s, want %s", args[i+1], NoMounts)
				}
			}
			if arg == "--bind" {
				return args[i+1]
			}
		}
		return ""
	}

	plain := bindsOf(&Container{Name: "app", ExecutionMode: "exec"})
	for _, want := range []string{own.AttemptTmp(2) + ":/tmp:rw", own.AttemptVarTmp(2) + ":/var/tmp:rw"} {
		if !strings.Contains(plain, want) {
			t.Errorf("binds %q lack %q", plain, want)
		}
	}
	// The directories are the container's own and the attempt's: no other container or
	// attempt shares them, and the runtime's own scratch is not among them.
	for _, dir := range []string{own.AttemptTmp(2), own.AttemptVarTmp(2)} {
		if strings.HasPrefix(dir, podDir.TmpDir()+"/") || strings.HasPrefix(dir, podDir.Container("other").ScratchDir()+"/") || strings.HasPrefix(dir, podDir.JobDir()) {
			t.Errorf("%s is shared with something else", dir)
		}
	}
	if own.AttemptTmp(2) == own.AttemptTmp(3) || own.AttemptTmp(2) == podDir.Container("other").AttemptTmp(2) {
		t.Error("attempts or containers share a /tmp")
	}

	// A volume the pod mounts at /tmp or /var/tmp is what the container sees there.
	mounted := bindsOf(&Container{Name: "app", ExecutionMode: "exec", Binds: []string{"/vol/scratch:/tmp/:rw", "/vol/log:/var/log:ro"}})
	if strings.Contains(mounted, ":/tmp:rw") && strings.Contains(mounted, own.AttemptTmp(2)) {
		t.Errorf("the container's own /tmp is bound over the pod's volume at /tmp: %s", mounted)
	}
	if !strings.Contains(mounted, own.AttemptVarTmp(2)+":/var/tmp:rw") {
		t.Errorf("the /var/tmp is not the container's own: %s", mounted)
	}
}

// Every attempt starts with empty temporary directories of its own, and what the
// attempts before left is gone; another container's directories are untouched.
func TestPrepareAttemptScratch(t *testing.T) {
	podDir := endpoint.PodPath(filepath.Join(t.TempDir(), "pod"))
	app, sidecar := podDir.Container("app"), podDir.Container("sidecar")
	for _, c := range []endpoint.ContainerPath{app, sidecar} {
		if err := prepareAttemptScratch(c, 1); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{app.AttemptTmp(1), app.AttemptVarTmp(1)} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("%s: %v", dir, err)
		}
		if info.Mode()&os.ModeSticky == 0 || info.Mode().Perm() != 0o777 {
			t.Errorf("%s has mode %v, want a world-writable sticky directory like /tmp", dir, info.Mode())
		}
	}
	// Attempt 1 leaves files, including a directory nobody can read (an unpacked tree).
	if err := os.WriteFile(filepath.Join(app.AttemptTmp(1), "marker"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidecar.AttemptTmp(1), "marker"), []byte("sidecar"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(app.AttemptVarTmp(1), "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}

	if err := prepareAttemptScratch(app, 2); err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	if attempts, _ := app.ScratchAttempts(); len(attempts) != 1 || attempts[0] != 2 {
		t.Errorf("attempts with a /tmp after attempt 2 started: %v, want [2]", attempts)
	}
	if entries, _ := os.ReadDir(app.AttemptTmp(2)); len(entries) != 0 {
		t.Errorf("attempt 2 starts with %d entries in /tmp", len(entries))
	}
	if _, err := os.Stat(filepath.Join(app.AttemptTmp(1), "marker")); err == nil {
		t.Error("the file of attempt 1 is still there")
	}
	if data, err := os.ReadFile(filepath.Join(sidecar.AttemptTmp(1), "marker")); err != nil || string(data) != "sidecar" {
		t.Errorf("another container's /tmp was touched: %q, %v", data, err)
	}
	// The same attempt number again (a leftover) is emptied too.
	if err := os.WriteFile(filepath.Join(app.AttemptTmp(2), "stale"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareAttemptScratch(app, 2); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(app.AttemptTmp(2)); len(entries) != 0 {
		t.Errorf("a leftover of the same attempt survived: %d entries", len(entries))
	}
}

// A container's writable layer is an overlay directory of its attempt over the image
// unpacked for it, on the node's disk: --overlay, not --writable-tmpfs, whose layer is 64 MiB
// of memory (a package install or a build would fail with no space, where Kubernetes would
// not), and not --writable on the unpacked copy, which leaves no way to create the mount
// points of the pod's volumes.
func TestBuildApptainerArgs_WritableLayerIsOnDisk(t *testing.T) {
	podDir := endpoint.PodPath(filepath.Join(t.TempDir(), "pod"))
	args := (&Container{Name: "app", ExecutionMode: "exec", ImageFilePath: "/images/app.sif"}).BuildApptainerArgs(1, podDir, 2)
	has := func(flag string) bool {
		for _, arg := range args {
			if arg == flag {
				return true
			}
		}
		return false
	}
	overlay := ""
	for i, arg := range args {
		if arg == "--overlay" && i+1 < len(args) {
			overlay = args[i+1]
		}
	}
	if want := podDir.Container("app").AttemptOverlayDir(2); overlay != want {
		t.Errorf("the writable layer is %q, want the attempt's overlay directory %q: %v", overlay, want, args)
	}
	if !has("--unsquash") {
		t.Errorf("the image is not unpacked for the attempt: %v", args)
	}
	for _, flag := range []string{"--writable-tmpfs", "--writable"} {
		if has(flag) {
			t.Errorf("the container has %s: %v", flag, args)
		}
	}
}

// Every attempt has a writable layer and an unpack directory of its own, empty when it
// starts, and what the attempts before it wrote there is gone with them; another
// container's is untouched.
func TestPrepareAttemptScratch_WritableLayerIsPerAttempt(t *testing.T) {
	podDir := endpoint.PodPath(filepath.Join(t.TempDir(), "pod"))
	app, other := podDir.Container("app"), podDir.Container("other")
	for _, c := range []endpoint.ContainerPath{app, other} {
		if err := prepareAttemptScratch(c, 1); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []endpoint.ContainerPath{app, other} {
		for _, dir := range []string{c.AttemptOverlayDir(1), c.AttemptUnpackDir(1)} {
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				t.Fatalf("attempt 1 lacks %s: %v", dir, err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(app.AttemptOverlayDir(1), "written"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other.AttemptOverlayDir(1), "written"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareAttemptScratch(app, 2); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(app.AttemptOverlayDir(2)); err != nil || len(entries) != 0 {
		t.Errorf("attempt 2 starts with %v (%v), want an empty directory", entries, err)
	}
	if _, err := os.Stat(app.AttemptOverlayDir(1)); err == nil {
		t.Error("what attempt 1 wrote survived the next attempt")
	}
	if data, err := os.ReadFile(filepath.Join(other.AttemptOverlayDir(1), "written")); err != nil || string(data) != "other" {
		t.Errorf("another container's layer was touched: %q %v", data, err)
	}
	if app.AttemptOverlayDir(2) == other.AttemptOverlayDir(2) || app.AttemptOverlayDir(1) == app.AttemptOverlayDir(2) {
		t.Error("attempts or containers share a writable layer")
	}
	for _, dir := range []string{app.AttemptOverlayDir(2), app.AttemptUnpackDir(2), app.AttemptTmp(2), app.AttemptVarTmp(2)} {
		if !strings.HasPrefix(dir, app.AttemptScratchDir(2)+"/") {
			t.Errorf("%s is not inside the attempt's scratch directory, which goes with the pod", dir)
		}
	}
}

// Where a container is put in the pod's network namespace depends on whether it has a user
// namespace of its own: root's is handed to Apptainer, one that runs as another user is
// started inside the namespace (Apptainer silently leaves it in the bubble's network).
func TestLaunchCommand_PodNetworkNamespace(t *testing.T) {
	podDir := endpoint.PodPath(filepath.Join(t.TempDir(), "pod"))
	root := &Container{Name: "app", ExecutionMode: "exec", ImageFilePath: "/i.sif"}
	bin, args := root.LaunchCommand("/usr/bin/apptainer", 777, podDir, 1)
	if bin != "/usr/bin/apptainer" || !strings.Contains(strings.Join(args, " "), "--netns-path /proc/777/ns/net") {
		t.Errorf("root: %s %v, want Apptainer with --netns-path", bin, args)
	}
	for _, c := range []*Container{
		{Name: "app", ExecutionMode: "exec", ImageFilePath: "/i.sif", RunAsUser: 1000},
		{Name: "app", ExecutionMode: "exec", ImageFilePath: "/i.sif", RunAsGroup: 2000},
	} {
		bin, args = c.LaunchCommand("/usr/bin/apptainer", 777, podDir, 1)
		if bin != "nsenter" || len(args) < 4 || args[0] != "--net=/proc/777/ns/net" || args[1] != "--" || args[2] != "/usr/bin/apptainer" {
			t.Errorf("non-root: %s %v, want nsenter --net=/proc/777/ns/net -- /usr/bin/apptainer ...", bin, args)
		}
		if strings.Contains(strings.Join(args, " "), "--netns-path") || !strings.Contains(strings.Join(args, " "), "--userns") {
			t.Errorf("non-root arguments %v: want --userns and no --netns-path", args)
		}
	}
	// No sandbox (a bare launch): nothing to join.
	if bin, args = (&Container{Name: "app", ExecutionMode: "exec", RunAsUser: 1000}).LaunchCommand("/usr/bin/apptainer", 0, podDir, 1); bin != "/usr/bin/apptainer" {
		t.Errorf("without a sandbox: %s %v", bin, args)
	}
}
