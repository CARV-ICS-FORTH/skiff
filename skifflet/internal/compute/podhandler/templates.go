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
	InstanceName  string
	Hostname      string
	RunAsUser     int64
	RunAsGroup    int64
	ImageFilePath string
	EnvFilePath   string
	Binds         []string
	Command       []string
	Args          []string
	ExecutionMode string
	LogsPath      string
}

// BuildApptainerArgs constructs the CLI arguments for executing a container via Apptainer/plaidtainer.
func (c *Container) BuildApptainerArgs(pausePID int, podDir endpoint.PodPath) []string {
	args := []string{
		"--host-networking",
		c.ExecutionMode,
		"--pid",
		"--no-init",
		"--nv",
		"--cleanenv",
		"--no-eval",
		"--writable-tmpfs",
		"--no-mount", "home,bind-paths",
		"--unsquash",
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
