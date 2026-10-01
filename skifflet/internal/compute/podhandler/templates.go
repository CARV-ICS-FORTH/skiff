// Copyright © 2022 FORTH-ICS
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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"skifflet/internal/compute/endpoint"
)

func EscapeSingleQuote(str ...interface{}) string {
	out := make([]string, 0, len(str))
	for _, s := range str {
		if s != nil {
			escaped := shellescape.Quote(strval(s))
			out = append(out, fmt.Sprintf("%v", escaped))
		}
	}
	return strings.Join(out, " ")
}

func strval(v interface{}) string {
	switch v := v.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case error:
		return v.Error()
	case fmt.Stringer:
		return v.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// Container holds definition and execution metadata for a container within a Pod.
type Container struct {
	InstanceName string
	// Name is the container's name in the pod.
	Name          string
	Hostname      string
	RunAsUser     int64
	RunAsGroup    int64
	ImageFilePath string
	EnvFilePath   string
	Binds         []string
	Command       []string
	Args          []string
	ExecutionMode string
}

// NoMounts are what Apptainer would mount into a container by default and Skiff does not:
// the user's home, the paths the site configured, the node's /tmp and /var/tmp (a
// container has its own, see prepareAttemptScratch) and the working directory of the
// launcher, which is the bubble's /root with the state directory (credentials included)
// below it. A container sees its image, the volumes of its pod and nothing else.
const NoMounts = "home,bind-paths,tmp,cwd"

// BuildApptainerArgs constructs the CLI arguments for executing attempt N of a
// container via Apptainer/plaidtainer.
//
// The container's root filesystem is the image unpacked for this attempt (--unsquash, into
// the attempt's own directory, see ContainerPath.AttemptUnpackDir) with a writable layer
// over it that is an overlay directory of the attempt (--overlay, see
// ContainerPath.AttemptOverlayDir): private to the container, on the node's disk (not the
// 64 MiB of memory --writable-tmpfs would give it), new with every attempt and gone with
// it, as in Kubernetes. (Writing to the unpacked copy itself, --writable, would also be on
// disk but leaves Apptainer unable to create the mount points of the pod's volumes.)
//
// The node's /tmp and /var/tmp are not mounted
// (Apptainer would do it by default, giving every container, and the pod directories
// below it, the whole node's /tmp): the container's own directories for the attempt
// (prepareAttemptScratch creates them) take their place, unless the pod mounts a
// volume there itself.
func (c *Container) BuildApptainerArgs(pausePID int, podDir endpoint.PodPath, attempt int) []string {
	args := []string{
		"--host-networking",
		c.ExecutionMode,
		"--pid",
		"--no-init",
		"--nv",
		"--cleanenv",
		"--no-eval",
		"--no-mount", NoMounts,
		"--unsquash",
		"--overlay", podDir.Container(c.Name).AttemptOverlayDir(attempt),
	}

	if c.Hostname != "" {
		args = append(args, "--hostname", c.Hostname)
	}

	if pausePID > 0 {
		args = append(args, "--netns-path", fmt.Sprintf("/proc/%d/ns/net", pausePID))
	}

	var binds []string
	resolvPath := podDir.ResolvConf()
	hostsPath := podDir.HostsFile()
	if _, err := os.Stat(resolvPath); err == nil {
		binds = append(binds, resolvPath+":/etc/resolv.conf")
	}
	if _, err := os.Stat(hostsPath); err == nil {
		binds = append(binds, hostsPath+":/etc/hosts")
	}
	own := podDir.Container(c.Name)
	for _, tmp := range []struct{ host, mountPath string }{
		{own.AttemptTmp(attempt), "/tmp"},
		{own.AttemptVarTmp(attempt), "/var/tmp"},
	} {
		if !c.mounts(tmp.mountPath) {
			binds = append(binds, tmp.host+":"+tmp.mountPath+":rw")
		}
	}
	binds = append(binds, c.Binds...)
	if len(binds) > 0 {
		args = append(args, "--bind", strings.Join(binds, ","))
	}

	if c.RunAsUser != 0 || c.RunAsGroup != 0 {
		args = append(args, "--security", fmt.Sprintf("uid:%d,gid:%d", c.RunAsUser, c.RunAsGroup), "--userns")
	}

	if c.EnvFilePath != "" {
		if _, err := os.Stat(c.EnvFilePath); err == nil {
			args = append(args, "--env-file", c.EnvFilePath)
		}
	}

	args = append(args, c.ImageFilePath)
	args = append(args, c.Command...)
	args = append(args, c.Args...)

	return args
}

// mounts says whether one of the container's volume mounts is at path.
func (c *Container) mounts(path string) bool {
	for _, bind := range c.Binds {
		if parts := strings.Split(bind, ":"); len(parts) >= 2 && filepath.Clean(parts[1]) == path {
			return true
		}
	}
	return false
}
