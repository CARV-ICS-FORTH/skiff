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
	if !strings.Contains(joined, "--netns-path /proc/12345/ns/net") {
		t.Errorf("expected netns-path in args, got: %s", joined)
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
