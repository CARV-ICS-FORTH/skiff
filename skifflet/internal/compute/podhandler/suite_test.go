package podhandler_test

import (
	"os"
	"path/filepath"
	"testing"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/podhandler"
	"skifflet/pkg/tmpfs"
)

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "skiff-podhandler-test-*")
	if err != nil {
		panic(err)
	}

	setup(tmpDir)
	code := m.Run()
	shutdown(tmpDir)
	os.Exit(code)
}

func setup(tmpDir string) {
	// (the tests run without the authority to mount a tmpfs; the e2e suite does it for real)
	podhandler.MountTmpfs = func(string, tmpfs.Options) error { return nil }
	podhandler.UnmountTmpfs = func(string) error { return nil }

	compute.Environment = compute.HostEnvironment{
		ApptainerBin:     "apptainer",
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
