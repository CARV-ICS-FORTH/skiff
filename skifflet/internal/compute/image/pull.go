// Copyright © 2023 FORTH-ICS
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package image

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"skifflet/internal/compute"
	"skifflet/pkg/process"

	"golang.org/x/sys/unix"
)

// ResolveLocal resolves an image reference to a local SIF file without pulling.
func ResolveLocal(imageDir string, imageName string) (*Image, error) {
	if strings.HasPrefix(imageName, "/") {
		if _, err := os.Stat(imageName); err != nil {
			return nil, fmt.Errorf("local image '%s' not found: %w", imageName, err)
		}

		return &Image{Filepath: imageName}, nil
	}

	img := &Image{Filepath: imageDir + ParseImageName(imageName)}

	file, err := os.Stat(img.Filepath)
	if err != nil {
		return nil, fmt.Errorf("local image '%s' not found: %w", img.Filepath, err)
	}

	if !file.Mode().IsRegular() {
		return nil, fmt.Errorf("imagePath '%s' is not a regular file", img.Filepath)
	}

	return img, nil
}

// pullLocks protects concurrent pulls of the same image within a single process.
var pullLocks sync.Map

func getPullLock(targetPath string) *sync.Mutex {
	l, _ := pullLocks.LoadOrStore(targetPath, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// FormatPullReference prepares an image reference for Apptainer.
// When an image contains both a tag and a digest, Apptainer fails.
// To preserve cryptographic digest pinning, the tag is stripped and the digest is preserved.
func FormatPullReference(rawImageName string) string {
	partsAt := strings.SplitN(rawImageName, "@", 2)
	if len(partsAt) == 2 {
		digest := partsAt[1]
		namePart := partsAt[0]

		slashIdx := strings.LastIndex(namePart, "/")
		colonIdx := -1
		if slashIdx == -1 {
			colonIdx = strings.LastIndex(namePart, ":")
		} else {
			rel := namePart[slashIdx+1:]
			if idx := strings.LastIndex(rel, ":"); idx != -1 {
				colonIdx = slashIdx + 1 + idx
			}
		}

		repo := namePart
		if colonIdx != -1 {
			repo = namePart[:colonIdx]
		}
		return repo + "@" + digest
	}
	return rawImageName
}

// ParseImageName generates a collision-resistant, architecture-specific cache filename for an image.
func ParseImageName(rawImageName string) string {
	return ParseImageNameForArch(rawImageName, runtime.GOARCH)
}

// ParseImageNameForArch generates a collision-resistant cache filename for the given architecture.
func ParseImageNameForArch(rawImageName string, arch string) string {
	if rawImageName == "" {
		return "/unnamed_" + arch + ".sif"
	}

	reg := regexp.MustCompile(`[^a-zA-Z0-9.-]+`)
	cleanPrefix := reg.ReplaceAllString(rawImageName, "_")
	if len(cleanPrefix) > 64 {
		cleanPrefix = cleanPrefix[:64]
	}

	// 16-hex sha256 hash of the exact canonical raw reference to ensure collision resistance
	// across ports (registry:5000/app:v1 vs registry:5000/app:v2) and path separators (a/b vs a_b).
	h := sha256.Sum256([]byte(rawImageName))
	hashSuffix := hex.EncodeToString(h[:8])

	return fmt.Sprintf("/%s_%s_%s.sif", cleanPrefix, hashSuffix, arch)
}

// Pull downloads an image if not present using default pull policy.
func Pull(ctx context.Context, imageDir string, transport Transport, imageName string) (*Image, error) {
	return PullWithPolicy(ctx, imageDir, transport, imageName, corev1.PullIfNotPresent)
}

// PullWithPolicy downloads an image honoring ImagePullPolicy and caller context.
func PullWithPolicy(ctx context.Context, imageDir string, transport Transport, imageName string, policy corev1.PullPolicy) (*Image, error) {
	if policy == corev1.PullNever {
		return ResolveLocal(imageDir, imageName)
	}

	if policy != corev1.PullAlways {
		if img, err := ResolveLocal(imageDir, imageName); err == nil {
			return img, nil
		}
	}

	img := &Image{Filepath: imageDir + ParseImageName(imageName)}

	// 1. Process-local synchronization (cancellable)
	lock := getPullLock(img.Filepath)
	for !lock.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer lock.Unlock()

	// 2. Between the nodes, which share the cache, a lock on the shared filesystem
	if unlock := lockBetweenNodes(ctx, img.Filepath+".flock"); unlock != nil {
		defer unlock()
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Re-check after acquiring locks. Listing the directory first makes an NFS client look again
	// (it revalidates a directory it opens), so the image another node just finished is seen.
	_, _ = os.ReadDir(imageDir)
	if policy != corev1.PullAlways {
		if checkedImg, err := ResolveLocal(imageDir, imageName); err == nil {
			return checkedImg, nil
		}
	}

	tmpFile := img.Filepath + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)

	// Format pull reference: preserve digest, discard tag if both present
	pullImageName := FormatPullReference(imageName)

	compute.DefaultLogger.Info(" * Downloading image...", "image", imageName, "dir", imageDir, "pullRef", pullImageName)
	if _, err := executePullWithProgress(
		ctx,
		imageName,
		compute.Environment.ApptainerBin,
		"pull",
		"--arch", runtime.GOARCH,
		tmpFile,
		transport.Wrap(pullImageName),
	); err != nil {
		_ = os.Remove(tmpFile)
		return nil, fmt.Errorf("downloading has failed: %w", err)
	}

	// Atomic rename to published SIF
	if err := os.Rename(tmpFile, img.Filepath); err != nil {
		_ = os.Remove(tmpFile)
		return nil, fmt.Errorf("failed to rename downloaded image: %w", err)
	}

	compute.DefaultLogger.Info(" * Download completed", "image", imageName, "path", img.Filepath)

	return img, nil
}

func executePullWithProgress(ctx context.Context, imageName string, command string, arguments ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, arguments...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, process.GoEnviron...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("could not prepare stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("could not prepare stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start process: %w", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	output := &bytes.Buffer{}

	progressRe := regexp.MustCompile(`(\d{1,3})%`)
	lastBucket := -1
	lastPercent := -1

	reportProgress := func(p int) {
		if p < 0 || p > 100 {
			return
		}

		mu.Lock()
		defer mu.Unlock()

		bucket := p / 5
		if p != 100 && bucket <= lastBucket {
			return
		}

		lastBucket = bucket
		lastPercent = p

		const width = 20
		filled := p * width / 100
		bar := strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
		compute.DefaultLogger.Info(" * Pull progress", "image", imageName, "progress", fmt.Sprintf("%3d%% [%s]", p, bar))
	}

	const maxOutputBytes = 64 * 1024

	scanStream := func(r io.Reader) {
		defer wg.Done()

		scanner := bufio.NewScanner(r)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			line := scanner.Text()

			mu.Lock()
			if output.Len() < maxOutputBytes {
				output.WriteString(line)
				output.WriteByte('\n')
			}
			mu.Unlock()

			matches := progressRe.FindAllStringSubmatch(line, -1)
			for _, m := range matches {
				p, convErr := strconv.Atoi(m[1])
				if convErr != nil {
					continue
				}
				reportProgress(p)
			}
		}

		if err := scanner.Err(); err != nil {
			mu.Lock()
			if output.Len() < maxOutputBytes {
				output.WriteString(err.Error())
				output.WriteByte('\n')
			}
			mu.Unlock()
		}
	}

	wg.Add(2)
	go scanStream(stdout)
	go scanStream(stderr)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	start := time.Now()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = cmd.Cancel()
			<-done
			wg.Wait()
			return nil, ctx.Err()

		case err := <-done:
			wg.Wait()

			mu.Lock()
			out := output.Bytes()
			curPercent := lastPercent
			mu.Unlock()

			if err != nil {
				return out, fmt.Errorf("process error: %w\noutput: %s", err, string(out))
			}

			if curPercent >= 0 && curPercent < 100 {
				reportProgress(100)
			}

			return out, nil

		case <-ticker.C:
			mu.Lock()
			curPercent := lastPercent
			mu.Unlock()
			if curPercent < 0 {
				compute.DefaultLogger.Info(" * Pull still in progress", "image", imageName, "elapsed", time.Since(start).Round(time.Second).String())
			}
		}
	}
}

// CleanupTempFiles removes only abandoned .tmp-* files (older than 2 hours and unlocked).
func CleanupTempFiles(imageDir string) error {
	return CleanupAbandonedTempFiles(imageDir, 2*time.Hour)
}

// CleanupAbandonedTempFiles removes .tmp-* files older than maxAge whose lock is not held.
func CleanupAbandonedTempFiles(imageDir string, maxAge time.Duration) error {
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to read image directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() && strings.Contains(entry.Name(), ".tmp-") {
			path := filepath.Join(imageDir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				continue
			}

			if maxAge > 0 && time.Since(info.ModTime()) < maxAge {
				continue // active or recent file, do not remove
			}

			// Verify file is not actively locked directly or via base image .flock
			baseFlockPath := strings.Split(path, ".tmp-")[0] + ".flock"
			if baseFlock, err := os.OpenFile(baseFlockPath, os.O_RDWR, 0); err == nil {
				if flockErr := syscall.Flock(int(baseFlock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); flockErr != nil {
					_ = baseFlock.Close()
					continue // base image is actively being downloaded
				}
				_ = syscall.Flock(int(baseFlock.Fd()), syscall.LOCK_UN)
				_ = baseFlock.Close()
			}

			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err == nil {
				if flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); flockErr != nil {
					_ = f.Close()
					continue // actively locked
				}
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}

			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				compute.DefaultLogger.Error(err, "failed to remove abandoned temp image file", "path", path)
			} else {
				compute.DefaultLogger.Info("Cleaned up abandoned temp image file", "path", path)
			}
		}
	}
	return nil
}

// The lock between nodes. Every node's Skifflet pulls into the same cache on the shared
// home, so that an image is converted once per user and not once per node: while one node
// converts it, another that wants it waits for the lock and then finds the finished image
// (the check after the lock). The lock is not needed for correctness: an image is written to
// a file of its own and renamed into place, which is atomic, so a reader sees the old file or
// the new one, never part of one. It only saves converting an image twice.
//
// So nothing but another holder makes a pull wait: a filesystem whose locks do not work
// (Lustre without its flock option refuses them; NFS mounted nolock only locks one node), a
// lock file that cannot be made, is a pull without the lock, said once in the log. And the
// wait is bounded (lockWait): a pull on another node may hang, and a filesystem may keep the
// lock of a node that died until its lock recovery runs.

// lockFile takes an exclusive lock on the whole file without waiting, and unlockFile gives it
// back (tests replace them). The lock is a POSIX open-file-description lock (F_OFD_SETLK), not
// flock: on Linux an NFS client turns its locks into POSIX locks on the server, which do not
// conflict with a flock taken on the server itself (a node whose home is local and exported
// to the others), and conflict with a POSIX lock. A held lock is EAGAIN or EACCES.
var lockFile = func(fd int) error {
	return unix.FcntlFlock(uintptr(fd), unix.F_OFD_SETLK, &unix.Flock_t{Type: unix.F_WRLCK, Whence: 0})
}

var unlockFile = func(fd int) error {
	return unix.FcntlFlock(uintptr(fd), unix.F_OFD_SETLK, &unix.Flock_t{Type: unix.F_UNLCK, Whence: 0})
}

// heldByAnother says whether a failed lockFile means that another process holds the lock.
func heldByAnother(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EWOULDBLOCK)
}

// lockWait is the longest a pull waits for another node's.
var lockWait = 10 * time.Minute

var lockUnusable sync.Once

// lockBetweenNodes takes the lock of an image's file and returns what gives it back, or nil
// when the pull goes on without it (it could not be had, or ctx ended: the caller tells).
func lockBetweenNodes(ctx context.Context, path string) (unlock func()) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		lockUnusable.Do(func() {
			compute.DefaultLogger.Info("images are pulled without a lock between the nodes: the lock file cannot be made", "path", path, "err", err.Error())
		})
		return nil
	}
	deadline := time.Now().Add(lockWait)
	for {
		err := lockFile(int(file.Fd()))
		if err == nil {
			return func() {
				_ = unlockFile(int(file.Fd()))
				_ = file.Close()
			}
		}
		if !heldByAnother(err) {
			lockUnusable.Do(func() {
				compute.DefaultLogger.Info("images are pulled without a lock between the nodes: the shared filesystem refuses it", "path", path, "err", err.Error())
			})
			_ = file.Close()
			return nil
		}
		if time.Now().After(deadline) {
			compute.DefaultLogger.Info("another node has held the image's lock too long; pulling without it", "path", path, "waited", lockWait.String())
			_ = file.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}
