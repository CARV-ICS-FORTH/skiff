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

package runtime

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type JobIDType string

const (
	JobIDTypeProcess JobIDType = "pid://"
)

// FormatProcessJobID formats a PID and start time into a typed job ID string "pid://<pid>:<starttime>" or "pid://<pid>".
func FormatProcessJobID(pid int, startTime uint64) string {
	if startTime > 0 {
		return fmt.Sprintf("%s%d:%d", JobIDTypeProcess, pid, startTime)
	}
	return fmt.Sprintf("%s%d", JobIDTypeProcess, pid)
}

// ParseProcessJobID parses a process job ID string (e.g. "pid://12345:67890", "12345:67890", "pid://12345", or "12345")
// into a PID and optional start time. PIDs <= 1 are strictly rejected.
func ParseProcessJobID(raw string) (pid int, startTime uint64, err error) {
	val := strings.TrimSpace(raw)
	if strings.HasPrefix(val, string(JobIDTypeProcess)) {
		val = strings.TrimPrefix(val, string(JobIDTypeProcess))
		val = strings.TrimSpace(val)
	}
	if val == "" {
		return 0, 0, errors.New("empty process id")
	}

	parts := strings.Split(val, ":")
	if len(parts) > 2 {
		return 0, 0, fmt.Errorf("invalid job id format '%s'", raw)
	}

	p, err := strconv.Atoi(parts[0])
	if err != nil || p <= 1 {
		return 0, 0, fmt.Errorf("invalid pid '%s': PID must be > 1", parts[0])
	}

	var st uint64
	if len(parts) == 2 {
		st, err = strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid starttime '%s'", parts[1])
		}
	}

	return p, st, nil
}
