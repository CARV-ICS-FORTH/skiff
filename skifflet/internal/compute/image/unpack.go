package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"skifflet/internal/compute"
	"skifflet/pkg/process"
)

// unpackTmpSuffix names the directory a copy is made in, beside where the copy will be and on
// its filesystem: Apptainer's temporary directory while it makes the copy, with the copy in it
// (unpackRoot) and the directory Apptainer assembles it in, which it makes next to its target.
// What an unpacking that was stopped leaves is all in there. It is gone when the copy is made.
const (
	unpackTmpSuffix = ".tmp"
	unpackRoot      = "root"
)

// unpackLocks holds one lock per unpacked copy, so that the containers of the node which
// need an image that is not unpacked yet wait for the one that unpacks it. The node's
// scratch directory has one Skifflet (see provider.AcquireInstanceLock), so a lock of this
// process is enough.
var unpackLocks sync.Map

// unpacked are the copies this Skifflet made, by their directory: only these are whole for
// certain. A directory of the same name that it finds (left by a Skifflet that was killed
// while it unpacked the image, or stopped while it removed the copy) is replaced.
var unpacked sync.Map

func getUnpackLock(target string) *sync.Mutex {
	l, _ := unpackLocks.LoadOrStore(target, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// runUnpack runs the launcher that unpacks an image, with tmp as Apptainer's temporary
// directory, and returns what it printed. It ends with the context, and so does everything
// it started.
func runUnpack(ctx context.Context, tmp string, command string, arguments ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, arguments...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second // (a child that survived must not keep the output open)
	cmd.Env = append(os.Environ(), process.GoEnviron...)
	cmd.Env = append(cmd.Env, "APPTAINER_TMPDIR="+tmp, "SINGULARITY_TMPDIR="+tmp)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

// Unpacked returns the directory that holds the image unpacked for this node, below dir
// (endpoint.SkiffPath.UnpackedImagesDir), and unpacks the image first when no container of
// the node has needed it yet.
//
// Every container of the image starts from this one copy, which it only reads, with a
// writable layer of its own over it (see podhandler.Container.BuildApptainerArgs). Were the
// image unpacked for every start of every container instead (Apptainer's --unsquash), the
// node would hold one copy per running container on its scratch directory, and every start
// would wait for the unpacking, where one copy and one unpacking are enough.
//
// The copy is made by Apptainer's own build --sandbox: the extraction of --unsquash, by the
// same user. What a container finds differs from an image unpacked for it in Apptainer's own
// records of the build (/.singularity.d/Singularity, labels.json and bootstrap_history), and in
// that a file the image lets its group write to stays so (--unsquash unpacked with the umask
// of its caller, build --sandbox with 0002).
//
// The copy is named after the image file with its size and modification time, so a file that
// was replaced (imagePullPolicy Always) is unpacked anew, beside the copy the running
// containers have. A copy is used only once this Skifflet has made it whole: what an earlier
// one left under the same name is replaced, and so is what an unpacking that failed left.
func (i *Image) Unpacked(ctx context.Context, dir string) (string, error) {
	info, err := os.Stat(i.Filepath)
	if err != nil {
		return "", fmt.Errorf("cannot unpack image %s: %w", i.Filepath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("cannot unpack image %s: it is not a regular file", i.Filepath)
	}
	name := fmt.Sprintf("%s-%d-%d", strings.TrimSuffix(filepath.Base(i.Filepath), ".sif"), info.Size(), info.ModTime().UnixNano())
	target := filepath.Join(dir, name)

	lock := getUnpackLock(target)
	for !lock.TryLock() {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer lock.Unlock()

	if _, made := unpacked.Load(target); made {
		if copied, err := os.Stat(target); err == nil && copied.IsDir() {
			return target, nil
		}
		unpacked.Delete(target)
	}

	tmp := target + unpackTmpSuffix
	making := filepath.Join(tmp, unpackRoot)
	discard := func() error {
		if err := removeTree(target); err != nil {
			return err
		}
		return removeTree(tmp)
	}
	if err := discard(); err != nil { // (what another Skifflet left under this name)
		return "", fmt.Errorf("cannot unpack image %s: %w", i.Filepath, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cannot unpack image %s: %w", i.Filepath, err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return "", fmt.Errorf("cannot unpack image %s: %w", i.Filepath, err)
	}

	compute.DefaultLogger.Info(" * Unpacking image for this node...", "image", i.Filepath, "to", target)
	started := time.Now()
	out, err := runUnpack(ctx, tmp, compute.Environment.ApptainerBin, "build", "--sandbox", making, i.Filepath)
	if err != nil {
		_ = discard()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("cannot unpack image %s into %s: %w: %s", i.Filepath, dir, err, lastLines(out, 5))
	}
	if copied, err := os.Stat(making); err != nil || !copied.IsDir() {
		_ = discard()
		return "", fmt.Errorf("cannot unpack image %s into %s: the launcher left no directory: %s", i.Filepath, dir, lastLines(out, 5))
	}
	if err := os.Rename(making, target); err != nil {
		_ = discard()
		return "", fmt.Errorf("cannot unpack image %s: %w", i.Filepath, err)
	}
	_ = removeTree(tmp)
	unpacked.Store(target, struct{}{})
	compute.DefaultLogger.Info(" * Image unpacked", "image", i.Filepath, "to", target, "elapsed", time.Since(started).Round(time.Millisecond).String())
	return target, nil
}

// RemoveUnpacked removes every image unpacked below dir (see Unpacked), and what an
// unpacking that did not finish left there. It is for when no container of the node runs:
// before the first one starts, where an earlier Skifflet that was killed may have left its
// copies, and after the last one has stopped.
func RemoveUnpacked(dir string) error {
	unpacked.Range(func(target, _ any) bool {
		if filepath.Dir(target.(string)) == dir {
			unpacked.Delete(target)
		}
		return true
	})
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var failed error
	for _, entry := range entries {
		if err := removeTree(filepath.Join(dir, entry.Name())); err != nil && failed == nil {
			failed = err
		}
	}
	return failed
}

// removeTree removes path and everything below it. An unpacked image has directories that
// their owner may not write to, which only root removes as they are: their modes are fixed
// first where that is what stands in the way.
func removeTree(path string) error {
	err := os.RemoveAll(path)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// lastLines is the end of what a program printed, on one line, for an error message.
func lastLines(out []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.Join(lines, "; ")
}
