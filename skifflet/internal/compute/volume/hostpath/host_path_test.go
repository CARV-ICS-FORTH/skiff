package hostpath

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// A hostPath's type is checked, and what it says to make is made; with no type a missing
// directory is made, as the container runtime does.
func TestCheckType(t *testing.T) {
	root := t.TempDir()
	typ := func(v corev1.HostPathType) *corev1.HostPathType { return &v }
	if err := CheckType(filepath.Join(root, "unset", "made"), nil); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "unset", "made")); err != nil || !info.IsDir() {
		t.Fatalf("no type: a missing directory was not made: %v", err)
	}
	if err := CheckType(filepath.Join(root, "missing"), typ(corev1.HostPathDirectory)); err == nil {
		t.Fatal("Directory: a missing directory passed")
	}
	if err := CheckType(filepath.Join(root, "made-dir"), typ(corev1.HostPathDirectoryOrCreate)); err != nil {
		t.Fatal(err)
	}
	if err := CheckType(filepath.Join(root, "made-dir"), typ(corev1.HostPathFile)); err == nil {
		t.Fatal("File: a directory passed")
	}
	if err := CheckType(filepath.Join(root, "made-file"), typ(corev1.HostPathFileOrCreate)); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "made-file")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("FileOrCreate: no file was made: %v", err)
	}
}
