// Copyright © 2026 FORTH-ICS
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

package podhandler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/cri-client/pkg/logs"
)

// stopSignalOf is the signal that asks a container to stop, as the kubelet's runtime chooses
// it: the container's lifecycle.stopSignal, else its image's STOPSIGNAL (a name or a number),
// else SIGTERM. A signal that is not one is a configuration error.
func stopSignalOf(container *corev1.Container, imageSignal string) (syscall.Signal, error) {
	name := imageSignal
	if container.Lifecycle != nil && container.Lifecycle.StopSignal != nil {
		name = string(*container.Lifecycle.StopSignal)
	}
	if name == "" {
		return syscall.SIGTERM, nil
	}
	if n, err := strconv.Atoi(name); err == nil && n > 0 && n < 65 {
		return syscall.Signal(n), nil
	}
	upper := strings.ToUpper(name)
	if !strings.HasPrefix(upper, "SIG") {
		upper = "SIG" + upper
	}
	if sig := unix.SignalNum(upper); sig != 0 {
		return sig, nil
	}
	return 0, &ConfigError{Message: fmt.Sprintf("the stop signal %q is not a signal", name), Reason: "CreateContainerConfigError"}
}

// The termination message of a container, as the kubelet reads it: what the container wrote
// to the file at its terminationMessagePath (at most maxTerminationMessage bytes) becomes the
// message of its terminated state; with the policy FallbackToLogsOnError, a container that
// failed and wrote nothing there gets the end of its log instead (at most fallbackLogLines
// lines and fallbackLogBytes bytes).
//
// The file is one of the attempt's, bound at the path. Apptainer cannot bind a file below
// /dev of a container that has a /dev of its own (--contain): "destination doesn't exist in
// container". So a path below /dev, the default /dev/termination-log among them, gets no file:
// what a container writes there is not kept, and only the fallback to the log applies.
const (
	maxTerminationMessage = 4096
	fallbackLogLines      = 80
	fallbackLogBytes      = 2048
)

// terminationMessageBound says whether Skifflet can keep what a container writes at path.
func terminationMessageBound(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	return clean != "/dev" && !strings.HasPrefix(clean, "/dev/")
}

// prepareTerminationLog creates the attempt's empty termination file, writable by whatever
// user the container runs as.
func prepareTerminationLog(c *Container, path string) error {
	if c.TerminationMessagePath == "" {
		return nil
	}
	if err := os.WriteFile(path, nil, 0o666); err != nil {
		return err
	}
	return os.Chmod(path, 0o666) // the umask would have trimmed the mode
}

// terminationMessage is the message of the attempt's terminated state ("" when there is none).
func (a *Attempt) terminationMessage(exitCode int) string {
	if a.spec == nil {
		return ""
	}
	container := a.pl.podDir.Container(a.Container)
	message := ""
	if terminationMessageBound(a.spec.TerminationMessagePath) {
		if f, err := os.Open(container.AttemptTerminationLog(a.ID)); err == nil {
			data, _ := io.ReadAll(io.LimitReader(f, maxTerminationMessage))
			_ = f.Close()
			message = string(data)
		}
	}
	if message == "" && exitCode != 0 && a.spec.TerminationMessagePolicy == corev1.TerminationMessageFallbackToLogsOnError {
		message = logTail(container.AttemptLogPath(a.ID))
	}
	return message
}

// logTail is the end of an attempt's log, its content without the records' framing.
func logTail(path string) string {
	lines, limit := int64(fallbackLogLines), int64(fallbackLogBytes)
	opts := &logs.LogOptions{TailLines: &lines, LimitBytes: &limit}
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = logs.ReadLogs(ctx, path, "", opts, attemptRuntime{}, &out, &out)
	return out.String()
}
