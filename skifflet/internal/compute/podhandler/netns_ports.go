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

package podhandler

import (
	"fmt"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"
)

// unprivilegedPortStart is the sysctl that says from which port on anyone may bind.
const unprivilegedPortStart = "net.ipv4.ip_unprivileged_port_start"

// allowUnprivilegedPorts lowers, in the network namespace of a pod, the first port that
// does not need a privilege to bind to 0, as container runtimes do (Docker sets it in every
// container). The namespace belongs to the bubble, and a container that runs as another user
// than root, in a user namespace of its own, has no authority over it: without this it cannot
// bind a port below 1024 (CoreDNS, whose image runs as 65532, binds 53; a web server binds
// 80). The namespace is the pod's alone, so nothing else is affected. pid is a process in it.
func allowUnprivilegedPorts(pid int) error {
	if goruntime.GOOS != "linux" || pid <= 1 {
		return nil
	}
	out, err := exec.Command("nsenter", "-t", strconv.Itoa(pid), "-n", "sysctl", "-w", unprivilegedPortStart+"=0").CombinedOutput()
	if err != nil {
		return fmt.Errorf("sysctl %s in the namespace of process %d: %v: %s", unprivilegedPortStart, pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}
