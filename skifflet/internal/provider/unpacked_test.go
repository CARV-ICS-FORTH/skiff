package provider

import (
	"os"
	"path/filepath"
	"testing"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
)

// leaveUnpacked puts what a Skifflet leaves in the node's directory of unpacked images: a copy,
// with a directory its owner may not write to, and the temporary directory of an unpacking
// beside it. It returns the directory.
func leaveUnpacked(t *testing.T) string {
	t.Helper()
	dir := compute.Skiff.UnpackedImagesDir()
	locked := filepath.Join(dir, "app-7-1", "usr", "lib")
	for _, made := range []string{locked, filepath.Join(dir, "app-7-1.tmp", "bundle")} {
		if err := os.MkdirAll(made, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(locked, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) // (for the test's own directory, should the removal fail)
	return dir
}

func entries(t *testing.T, dir string) int {
	t.Helper()
	found, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(found)
}

// The images unpacked in a node's scratch directory are those of the Skifflet that holds the
// node's lock. One that does not hold it leaves them alone, also when it is told to stop: they
// are what the containers of the other start from. The one that gets the lock finds the
// directory with what an earlier one left, and empties it; and when its own pods have
// stopped, it removes what it unpacked.
func TestUnpackedImages_BelongToTheSkiffletThatHoldsTheNodesLock(t *testing.T) {
	saved, owned := compute.Skiff, unpackedImagesOwned.Load()
	compute.Skiff = endpoint.SkiffWithTmp(t.TempDir(), filepath.Join(t.TempDir(), "scratch"))
	unpackedImagesOwned.Store(false)
	defer func() { compute.Skiff = saved; unpackedImagesOwned.Store(owned) }()

	dir := leaveUnpacked(t)
	if err := RemoveUnpackedImages(); err != nil {
		t.Fatal(err)
	}
	if n := entries(t, dir); n != 2 {
		t.Fatalf("a Skifflet without the node's lock removed unpacked images: %d of 2 entries left", n)
	}

	if err := claimUnpackedImages(); err != nil {
		t.Fatal(err)
	}
	if n := entries(t, dir); n != 0 {
		t.Fatalf("the Skifflet that got the node's lock starts with %d entries an earlier one left", n)
	}

	_ = leaveUnpacked(t)
	if err := RemoveUnpackedImages(); err != nil {
		t.Fatal(err)
	}
	if n := entries(t, dir); n != 0 {
		t.Fatalf("%d entries left after the node's pods have stopped", n)
	}
}

// A node that never unpacked an image has no such directory, which is nothing to complain of.
func TestUnpackedImages_NoneYet(t *testing.T) {
	saved, owned := compute.Skiff, unpackedImagesOwned.Load()
	compute.Skiff = endpoint.SkiffWithTmp(t.TempDir(), filepath.Join(t.TempDir(), "scratch"))
	defer func() { compute.Skiff = saved; unpackedImagesOwned.Store(owned) }()

	if err := claimUnpackedImages(); err != nil {
		t.Errorf("claiming a directory that is not there: %v", err)
	}
	if err := RemoveUnpackedImages(); err != nil {
		t.Errorf("removing from a directory that is not there: %v", err)
	}
	if _, err := os.Stat(compute.Skiff.UnpackedImagesDir()); !os.IsNotExist(err) {
		t.Errorf("the directory was made: %v", err)
	}
}
