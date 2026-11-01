package podhandler_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/podhandler"
	"skifflet/pkg/tmpfs"
)

func TestMain(m *testing.M) {
	enterLikePlaidtainer()
	tmpDir, err := os.MkdirTemp("", "skiff-podhandler-test-*")
	if err != nil {
		panic(err)
	}

	setup(tmpDir)
	code := m.Run()
	shutdown(tmpDir)
	os.Exit(code)
}

// enterLikePlaidtainer is `plaidtainer enter` for the tests, which have this program in its place
// and run their commands on the host: it gives the command the environment and leaves the
// capabilities alone.
func enterLikePlaidtainer() {
	if len(os.Args) < 4 || os.Args[1] != "enter" {
		return
	}
	data, err := io.ReadAll(os.NewFile(4, "environment"))
	if err != nil {
		panic(err)
	}
	var env []string
	if len(data) > 0 {
		env = strings.Split(string(data), "\x00")
	}
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			os.Setenv("PATH", value)
		}
	}
	path, err := exec.LookPath(os.Args[3])
	if err == nil {
		syscall.CloseOnExec(3)
		syscall.CloseOnExec(4)
		err = syscall.Exec(path, os.Args[3:], env)
	}
	fmt.Fprintf(os.Stderr, "failed to execute %s: %v\n", os.Args[3], err)
	os.Exit(127)
}

func setup(tmpDir string) {
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	// (the tests run without the authority to mount a tmpfs; the e2e suite does it for real)
	podhandler.MountTmpfs = func(string, tmpfs.Options) error { return nil }
	podhandler.UnmountTmpfs = func(string) error { return nil }

	compute.Environment = compute.HostEnvironment{
		ApptainerBin:     self,
		WorkingDirectory: tmpDir,
		TmpDirectory:     filepath.Join(tmpDir, ".skiff"),
		KubeDNS:          "",
	}
	compute.Skiff = endpoint.SkiffWithTmp(tmpDir, compute.Environment.TmpDirectory)
	_ = os.MkdirAll(compute.Skiff.PodsDir(), 0755)
	_ = os.MkdirAll(compute.Skiff.ImageDir(), 0755)
}

func shutdown(tmpDir string) {
	_ = os.RemoveAll(tmpDir)
}
