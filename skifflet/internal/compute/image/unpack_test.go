package image

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"skifflet/internal/compute"
)

// unpacker puts a launcher in Apptainer's place that stands for `build --sandbox <directory>
// <image>`: body is what it does with $directory and $image, after it has noted its run and
// the temporary directory it was given. It returns the file with one line per run.
func unpacker(t *testing.T, body string) (runs string) {
	t.Helper()
	root := t.TempDir()
	runs = filepath.Join(root, "runs")
	launcher := filepath.Join(root, "launcher")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = build ] && [ \"$2\" = --sandbox ] || { echo \"unexpected arguments: $*\" >&2; exit 64; }\n" +
		"directory=$3 image=$4\n" +
		"echo \"$directory $image $APPTAINER_TMPDIR $SINGULARITY_TMPDIR\" >> " + runs + "\n" +
		body + "\n"
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := compute.Environment.ApptainerBin
	compute.Environment.ApptainerBin = launcher
	t.Cleanup(func() { compute.Environment.ApptainerBin = old })
	return runs
}

// unpacks is what the launcher did in a working run: the directory, with a file of the image.
const unpacks = `mkdir -p "$directory/bin" && cp "$image" "$directory/bin/content"`

func imageFile(t *testing.T, content string) *Image {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry_app_1.0_0123456789abcdef_amd64.sif")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Image{Filepath: path}
}

func runsOf(t *testing.T, runs string) []string {
	t.Helper()
	data, err := os.ReadFile(runs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func entriesOf(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// The containers of a node that need the same image at the same time get one copy of it, made
// once: the others wait for the one that unpacks it. The copy is below the directory they
// name and it is the image's. It was made inside a directory of its own beside it, which was
// Apptainer's temporary directory meanwhile (Apptainer assembles a copy in a directory it
// makes next to its target: what a stopped unpacking leaves is then all in there), and which
// is gone.
func TestUnpacked_OnceForTheContainersThatStartTogether(t *testing.T) {
	runs := unpacker(t, "sleep 0.3\n"+unpacks)
	img := imageFile(t, "the image")
	dir := filepath.Join(t.TempDir(), ".unpacked")

	const containers = 8
	copies, errs := make([]string, containers), make([]error, containers)
	var started sync.WaitGroup
	for n := 0; n < containers; n++ {
		started.Add(1)
		go func() {
			defer started.Done()
			copies[n], errs[n] = img.Unpacked(context.Background(), dir)
		}()
	}
	started.Wait()

	for n := 0; n < containers; n++ {
		if errs[n] != nil {
			t.Fatalf("container %d: %v", n, errs[n])
		}
		if copies[n] != copies[0] {
			t.Fatalf("container %d starts from %s and container 0 from %s: they do not share the copy", n, copies[n], copies[0])
		}
	}
	if filepath.Dir(copies[0]) != dir || !strings.HasPrefix(filepath.Base(copies[0]), "registry_app_1.0_0123456789abcdef_amd64-") {
		t.Errorf("the copy is %s, want a directory below %s named after the image file", copies[0], dir)
	}
	if data, err := os.ReadFile(filepath.Join(copies[0], "bin", "content")); err != nil || string(data) != "the image" {
		t.Errorf("the copy does not hold the image: %q %v", data, err)
	}
	lines := runsOf(t, runs)
	if len(lines) != 1 {
		t.Fatalf("the image was unpacked %d times for %d containers, want once: %q", len(lines), containers, lines)
	}
	fields := strings.Fields(lines[0])
	own := copies[0] + unpackTmpSuffix
	if len(fields) != 4 || fields[0] != filepath.Join(own, unpackRoot) || fields[1] != img.Filepath {
		t.Fatalf("the launcher was asked for %q, want %s made inside %s", lines[0], img.Filepath, own)
	}
	if fields[2] != own || fields[3] != own {
		t.Errorf("Apptainer's temporary directory was %s and %s, want the copy's own %s", fields[2], fields[3], own)
	}
	if names := entriesOf(t, dir); len(names) != 1 {
		t.Errorf("the directory of unpacked images holds %v, want the copy alone", names)
	}

	// A container that comes later finds it.
	if again, err := img.Unpacked(context.Background(), dir); err != nil || again != copies[0] {
		t.Fatalf("a later container: %s %v, want the copy %s", again, err, copies[0])
	}
	if lines := runsOf(t, runs); len(lines) != 1 {
		t.Errorf("a later container had the image unpacked again: %d runs", len(lines))
	}
}

// An image that cannot be unpacked leaves nothing behind, and the error says what the
// launcher said. It is unpacked when it is tried again and can be.
func TestUnpacked_FailureLeavesNothingAndSaysWhy(t *testing.T) {
	runs := unpacker(t, `mkdir -p "$directory/half" "$(dirname "$directory")/build-temp-1/rootfs"; echo "FATAL: no space left on device" >&2; exit 255`)
	img := imageFile(t, "the image")
	dir := filepath.Join(t.TempDir(), ".unpacked")

	copied, err := img.Unpacked(context.Background(), dir)
	if err == nil {
		t.Fatalf("an unpacking that failed gave the copy %s", copied)
	}
	for _, want := range []string{img.Filepath, dir, "no space left on device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not say %q", err, want)
		}
	}
	if names := entriesOf(t, dir); len(names) != 0 {
		t.Errorf("the failed unpacking left %v", names)
	}

	_ = unpacker(t, unpacks)
	copied, err = img.Unpacked(context.Background(), dir)
	if err != nil {
		t.Fatalf("the image was not unpacked when it could be: %v", err)
	}
	if _, err := os.Stat(filepath.Join(copied, "half")); err == nil {
		t.Error("the copy holds what the failed unpacking wrote")
	}
	if len(runsOf(t, runs)) != 1 {
		t.Errorf("the failing launcher ran %d times, want once", len(runsOf(t, runs)))
	}
}

// A launcher that says it is done is not believed when it made no directory: nothing, or a
// file where the copy should be.
func TestUnpacked_NoDirectoryIsAFailure(t *testing.T) {
	for name, body := range map[string]string{"nothing": "exit 0", "a file": `: > "$directory"`} {
		_ = unpacker(t, body)
		dir := filepath.Join(t.TempDir(), ".unpacked")
		if copied, err := imageFile(t, "the image").Unpacked(context.Background(), dir); err == nil {
			t.Errorf("the launcher made %s, and the copy %s was returned", name, copied)
		}
		if names := entriesOf(t, dir); len(names) != 0 {
			t.Errorf("the launcher made %s; left behind: %v", name, names)
		}
	}
}

// An image file that was replaced (pulled again under the same name) is another image: it is
// unpacked anew, and the copy of the one before stays for the containers that run from it.
func TestUnpacked_ReplacedImageFileIsUnpackedAnew(t *testing.T) {
	runs := unpacker(t, unpacks)
	img := imageFile(t, "the image")
	dir := filepath.Join(t.TempDir(), ".unpacked")

	first, err := img.Unpacked(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img.Filepath, []byte("the image, pulled again"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := img.Unpacked(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("the replaced image shares the copy %s of the one before", first)
	}
	for copied, want := range map[string]string{first: "the image", second: "the image, pulled again"} {
		if data, err := os.ReadFile(filepath.Join(copied, "bin", "content")); err != nil || string(data) != want {
			t.Errorf("%s holds %q (%v), want %q", copied, data, err, want)
		}
	}
	if n := len(runsOf(t, runs)); n != 2 {
		t.Errorf("unpacked %d times, want twice", n)
	}

	// The same content written again at another time is not taken for the copy either.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(img.Filepath, later, later); err != nil {
		t.Fatal(err)
	}
	if third, err := img.Unpacked(context.Background(), dir); err != nil || third == second {
		t.Errorf("an image file of another time: %s %v, want a copy of its own", third, err)
	}
}

// A copy that this Skifflet did not make is not used: a Skifflet that was killed while it
// unpacked the image, or stopped while it removed the copy, leaves a directory of the same
// name with only some of the image's files. It is replaced.
func TestUnpacked_WhatAnotherSkiffletLeftIsReplaced(t *testing.T) {
	runs := unpacker(t, unpacks)
	img := imageFile(t, "the image")
	dir := filepath.Join(t.TempDir(), ".unpacked")
	copied, err := img.Unpacked(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	// As another Skifflet finds it: the directory is there, some of it removed, with what an
	// unpacking of its own left beside it.
	unpacked.Delete(copied)
	if err := os.Remove(filepath.Join(copied, "bin", "content")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(copied+unpackTmpSuffix, "bundle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copied, "stale"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	again, err := img.Unpacked(context.Background(), dir)
	if err != nil || again != copied {
		t.Fatalf("%s %v, want the copy %s made anew", again, err, copied)
	}
	if data, err := os.ReadFile(filepath.Join(copied, "bin", "content")); err != nil || string(data) != "the image" {
		t.Errorf("the copy was not made anew: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(copied, "stale")); err == nil {
		t.Error("the copy holds what was left by another")
	}
	if n := len(runsOf(t, runs)); n != 2 {
		t.Errorf("unpacked %d times, want twice", n)
	}
	if names := entriesOf(t, dir); len(names) != 1 {
		t.Errorf("the directory of unpacked images holds %v, want the copy alone", names)
	}
}

// The unpacking ends with the container's context (its pod is deleted while it waits): at
// once, with what it started, and with nothing left. A container that waited behind it for
// the same image is let go too.
func TestUnpacked_EndsWithItsContext(t *testing.T) {
	runs := unpacker(t, `mkdir -p "$directory/half"; sleep 30`)
	img := imageFile(t, "the image")
	dir := filepath.Join(t.TempDir(), ".unpacked")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		copied string
		err    error
	}
	first, waiting := make(chan result, 1), make(chan result, 1)
	go func() { copied, err := img.Unpacked(ctx, dir); first <- result{copied, err} }()
	deadline := time.Now().Add(5 * time.Second)
	for len(runsOf(t, runs)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the launcher did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitCtx, stopWaiting := context.WithCancel(context.Background())
	defer stopWaiting()
	go func() { copied, err := img.Unpacked(waitCtx, dir); waiting <- result{copied, err} }()

	stopWaiting()
	select {
	case r := <-waiting:
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("the container that waited: %s %v, want its context's end", r.copied, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a container that waits for the image outlived its context")
	}

	cancel()
	select {
	case r := <-first:
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("%s %v, want the context's end", r.copied, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the unpacking outlived its context")
	}
	if names := entriesOf(t, dir); len(names) != 0 {
		t.Errorf("the unpacking that was stopped left %v", names)
	}
}

// What is not an image file is not unpacked.
func TestUnpacked_OnlyAFile(t *testing.T) {
	runs := unpacker(t, unpacks)
	dir := filepath.Join(t.TempDir(), ".unpacked")
	for name, img := range map[string]*Image{
		"a directory":    {Filepath: t.TempDir()},
		"a missing file": {Filepath: filepath.Join(t.TempDir(), "none.sif")},
	} {
		if copied, err := img.Unpacked(context.Background(), dir); err == nil {
			t.Errorf("%s was unpacked into %s", name, copied)
		}
	}
	if n := len(runsOf(t, runs)); n != 0 {
		t.Errorf("the launcher ran %d times", n)
	}
}

// When no container of the node runs, its unpacked images are removed, all of them and what
// an unpacking left beside them, with the directories an image does not let its owner write
// to or read. An image that is needed afterwards is unpacked again.
func TestRemoveUnpacked(t *testing.T) {
	runs := unpacker(t, unpacks+` && mkdir -p "$directory/usr/lib" "$directory/home/nonroot" && touch "$directory/usr/lib/file" "$directory/home/nonroot/file" && chmod 555 "$directory/usr/lib" "$directory/usr" && chmod 0 "$directory/home/nonroot"`)
	dir := filepath.Join(t.TempDir(), ".unpacked")
	one, other := imageFile(t, "one image"), imageFile(t, "another image, larger")
	var copies []string
	for _, img := range []*Image{one, other} {
		copied, err := img.Unpacked(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		copies = append(copies, copied)
	}
	if copies[0] == copies[1] {
		t.Fatalf("two images share the copy %s", copies[0])
	}
	if err := os.MkdirAll(filepath.Join(dir, "left-by-another"+unpackTmpSuffix, "bundle"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := RemoveUnpacked(dir); err != nil {
		t.Fatal(err)
	}
	if names := entriesOf(t, dir); len(names) != 0 {
		t.Fatalf("still there: %v", names)
	}
	if err := RemoveUnpacked(dir); err != nil {
		t.Errorf("removing from an empty directory: %v", err)
	}
	if err := RemoveUnpacked(filepath.Join(dir, "never-made")); err != nil {
		t.Errorf("removing from a directory that is not there: %v", err)
	}

	again, err := one.Unpacked(context.Background(), dir)
	if err != nil || again != copies[0] {
		t.Fatalf("%s %v, want the copy %s again", again, err, copies[0])
	}
	if _, err := os.Stat(filepath.Join(again, "bin", "content")); err != nil {
		t.Errorf("the image was not unpacked again after its copy was removed: %v", err)
	}
	if n := len(runsOf(t, runs)); n != 3 {
		t.Errorf("unpacked %d times, want three (two images, and one again)", n)
	}
	if err := RemoveUnpacked(dir); err != nil { // (the temporary directory of the test is removed by its owner)
		t.Fatal(err)
	}
}
