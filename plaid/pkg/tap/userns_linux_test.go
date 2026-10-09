//go:build linux

package tap

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// needUserns skips the test where an unprivileged user cannot make a user and network
// namespace.
func needUserns(t *testing.T) {
	t.Helper()
	if err := exec.Command("unshare", "-Urn", "true").Run(); err != nil {
		t.Skipf("cannot create a user and network namespace here: %v", err)
	}
}

// runInUserns runs the test (by name) again in a child process in a user and network
// namespace of its own, which needs no privileges, with env (NAME=value) set in the child,
// and fails if the child fails. The test tells the child by that variable and runs its
// scenario there, with root and the network namespaces it makes in it.
func runInUserns(t *testing.T, test, env string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "unshare", "-Urn", os.Args[0], "-test.run=^"+test+"$", "-test.v", "-test.cpu="+strconv.Itoa(runtime.GOMAXPROCS(0)))
	cmd.Env = append(os.Environ(), env)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if strings.Contains(string(out), "--- SKIP") {
		t.Skip("skipped in the namespace")
	}
	if err != nil {
		t.Fatalf("the namespace child failed: %v", err)
	}
}
