package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// enterRun runs the program as `enter`, as root of a new user namespace with all the capabilities
// there, the way Skifflet's nsenter does in the bubble, with the environment on its descriptor.
func enterRun(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	return enterRunIn(t, "", env, args...)
}

// enterRunIn is enterRun with the container's root (and working directory) at enterRoot.
func enterRunIn(t *testing.T, enterRoot string, env []string, args ...string) (string, int) {
	t.Helper()
	if err := exec.Command("unshare", "-Ur", "true").Run(); err != nil {
		t.Skipf("no user namespaces here: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "plaidtainer")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	program, err := os.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.WriteString(strings.Join(env, "\x00")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	root := "/"
	if enterRoot != "" {
		root = enterRoot
	}
	script := `exec 5</proc/self/ns/mnt 6<"$0" 7<"$0"; exec "$@"`
	cmd := exec.Command("unshare", append([]string{"-Urm", "sh", "-c", script, root, bin, "enter"}, args...)...)
	cmd.Env = []string{"PLAID_DIRECT_WATCHDOG=1"} // what the program itself has must not count
	cmd.ExtraFiles = []*os.File{program, r}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return string(out), cmd.ProcessState.ExitCode()
}

// keepCapsRun runs a command with the capabilities of a mask and a PATH.
func keepCapsRun(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return enterRun(t, []string{"PATH=" + os.Getenv("PATH")}, args...)
}

func capLines(status string) map[string]string {
	caps := map[string]string{}
	for _, line := range strings.Split(status, "\n") {
		if name, value, ok := strings.Cut(line, ":\t"); ok && strings.HasPrefix(name, "Cap") {
			caps[name] = value
		}
	}
	return caps
}

func TestKeepCaps(t *testing.T) {
	// (the default of Kubernetes' container runtimes, and nothing of it through a further exec)
	out, code := keepCapsRun(t, "a80425fb", "sh", "-c", "exec cat /proc/self/status")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	caps := capLines(out)
	for _, name := range []string{"CapEff", "CapPrm", "CapBnd"} {
		if caps[name] != "00000000a80425fb" {
			t.Errorf("%s = %q, in\n%s", name, caps[name], out)
		}
	}
	if caps["CapAmb"] != "0000000000000000" {
		t.Errorf("CapAmb = %q", caps["CapAmb"])
	}

	out, code = keepCapsRun(t, "0", "cat", "/proc/self/status")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	caps = capLines(out)
	for _, name := range []string{"CapEff", "CapPrm", "CapBnd", "CapInh"} {
		if caps[name] != "0000000000000000" {
			t.Errorf("no capabilities kept, %s = %q", name, caps[name])
		}
	}
}

func TestKeepCapsRunsTheCommandAsNsenterWould(t *testing.T) {
	if out, code := keepCapsRun(t, "0", "sh", "-c", "exit 7"); code != 7 {
		t.Errorf("the command's own exit: %d: %s", code, out)
	}
	out, code := keepCapsRun(t, "0", "no-such-command")
	if code != 127 || !strings.Contains(out, "failed to execute no-such-command") {
		t.Errorf("a command that does not exist: %d: %s", code, out)
	}
	script := filepath.Join(t.TempDir(), "no-interpreter-line")
	if err := os.WriteFile(script, []byte("exit \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := keepCapsRun(t, "0", script, "9"); code != 9 {
		t.Errorf("a script with no #!, for a shell as in execvp: %d: %s", code, out)
	}
	notExecutable := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notExecutable, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := keepCapsRun(t, "0", notExecutable); code != 126 {
		t.Errorf("a file that cannot be executed: %d: %s", code, out)
	}
}

func TestKeepCapsRunsNothingWithABadMask(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	if out, code := keepCapsRun(t, "not-a-mask", "touch", marker); code != 126 {
		t.Errorf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran with the capabilities it had")
	}
}

// The command has the container's environment, exactly, and with no capabilities to give up
// keeps what it had.
func TestEnterGivesTheCommandTheEnvironment(t *testing.T) {
	env := []string{"PATH=" + os.Getenv("PATH"), "SECRET=one two\nthree", "EMPTY="}
	out, code := enterRun(t, env, "-", "sh", "-c", `printf '%s|%s|%s|%s' "$SECRET" "$EMPTY" "${EMPTY+set}" "$PLAID_DIRECT_WATCHDOG"`)
	if code != 0 || out != "one two\nthree||set|" {
		t.Errorf("exit %d: %q", code, out)
	}
	out, code = enterRun(t, env, "-", "cat", "/proc/self/status")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if caps := capLines(out); caps["CapBnd"] == "0000000000000000" {
		t.Errorf("\"-\" took the capabilities: %v", caps)
	}
	// With no PATH of the container's, the places a shell would look in.
	if out, code := enterRun(t, nil, "-", "sh", "-c", "exit 7"); code != 7 {
		t.Errorf("no environment: exit %d: %s", code, out)
	}
}

// The command is looked for in the root that Skifflet gave a descriptor of, wherever the program
// itself came from, and starts in the working directory; it does not get the descriptors.
func TestEnterTakesTheCommandIntoTheContainersRoot(t *testing.T) {
	jail := t.TempDir()
	if out, err := exec.Command("env", "CGO_ENABLED=0", "go", "build", "-o", filepath.Join(jail, "only-in-the-root"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if out, code := enterRunIn(t, jail, nil, "-", "/only-in-the-root", "version"); code == 127 || strings.Contains(out, "failed to execute") {
		t.Errorf("a command only the root has: exit %d: %s", code, out)
	}
	if _, code := enterRunIn(t, jail, nil, "-", "sh", "-c", "exit 0"); code != 127 {
		t.Errorf("a command only the node has, found from the container's root: exit %d", code)
	}
}
