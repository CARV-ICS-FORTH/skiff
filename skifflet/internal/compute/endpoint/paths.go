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

package endpoint

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Layout of the runtime files of a pod, all under one directory per pod
// generation, <podsDir>/<namespace>/<name>_<pod UID>/ (the pod UID keeps a
// replacement of the same name apart from the pod it replaces):
//
//	job/pids.json        the on-disk record of the pod's processes (crash cleanup)
//	job/readiness.json   the sandbox handshake written by plaidtainer
//	job/resolv.conf      the pod's /etc/resolv.conf, mounted into every container
//	job/hosts            the pod's /etc/hosts
//	job/<container>.env  the container's environment, given to Apptainer
//	volumes/<volume>     the pod's volumes
//	logs/                container logs (see ContainerPath)
//	tmp/                 the pod's Apptainer temporary directory (see TmpDir)
//
// Everything that launches, reads, execs into or cleans up a pod builds these
// paths here, from the runtime directory it was configured with (SkiffPath); no
// other code spells out a file name or a path. The pods directory is node-local
// and disposable. User volume backing data does not live here.
const (
	PodGlobalDirectoryPermissions = os.FileMode(0o777)
	DefaultPodsDir                = "/tmp/.skiff/.pods"

	// DefaultPlaidSocket is the API socket of plaidd, the daemon that gives pods
	// their network and removes their endpoints.
	DefaultPlaidSocket = "/run/plaid/plaidd.sock"
)

// Container-Related Extensions
const (
	// ExtensionEnvironment describes the file where the environment variables for the container are held.
	ExtensionEnvironment = ".env"

	// ExtensionLogs describes the file where container execution will write its logs.
	ExtensionLogs = ".logs"

	// ExtensionAttemptLog ends the name of the log file of one attempt.
	ExtensionAttemptLog = ".log"
)

type SkiffPath struct {
	rootPath string
	podsPath string
}

func Skiff(rootPath string) SkiffPath {
	return SkiffWithPods(rootPath, DefaultPodsDir)
}

func SkiffWithPods(rootPath string, podsPath string) SkiffPath {
	if podsPath == "" {
		podsPath = DefaultPodsDir
	}
	return SkiffPath{
		rootPath: filepath.Clean(filepath.Join(rootPath, ".skiff")),
		podsPath: filepath.Clean(podsPath),
	}
}

func (p SkiffPath) String() string {
	if p.rootPath == "" {
		panic("Skiff path has not been initialized")
	}

	return p.rootPath
}

func (p SkiffPath) ImageDir() string {
	return filepath.Join(p.rootPath, ".images")
}

func (p SkiffPath) PodsDir() string {
	if p.podsPath == "" {
		return DefaultPodsDir
	}
	return p.podsPath
}

type WalkPodFunc func(path PodPath) error

func (p SkiffPath) WalkPodDirectories(f WalkPodFunc) error {
	if _, err := os.Stat(p.PodsDir()); os.IsNotExist(err) {
		return nil
	}

	maxDepth := strings.Count(p.PodsDir(), string(os.PathSeparator)) + 2 // expect path <podsDir>/namespace/pod

	return filepath.WalkDir(p.PodsDir(), func(path string, info os.DirEntry, err error) error {
		// check for traversing errors
		if err != nil {
			return fmt.Errorf("Pod traversal error: %w", err)
		}

		// skip files
		if !info.IsDir() {
			return nil
		}

		// skip hidden system paths starting with .
		if path != p.PodsDir() && strings.HasPrefix(info.Name(), ".") {
			return filepath.SkipDir
		}

		// pod directory is found
		depth := strings.Count(path, string(os.PathSeparator))
		switch {
		case depth < maxDepth: // pods root or pod's namespace
			return nil
		case depth == maxDepth: // pod directory
			if err := f(PodPath(path)); err != nil {
				return err
			}
			return filepath.SkipDir
		default: // pod contents
			return filepath.SkipDir
		}
	})
}

func (p SkiffPath) Pod(podRef client.ObjectKey) PodPath {
	path := filepath.Join(p.PodsDir(), podRef.Namespace, podRef.Name)

	return PodPath(path)
}

// PodWithUID generates an isolated, UID-qualified path for a pod generation.
func (p SkiffPath) PodWithUID(podRef client.ObjectKey, uid types.UID) PodPath {
	if uid == "" {
		return p.Pod(podRef)
	}
	path := filepath.Join(p.PodsDir(), podRef.Namespace, fmt.Sprintf("%s_%s", podRef.Name, uid))
	return PodPath(path)
}

type PodPath string

func (p PodPath) String() string {
	return string(p)
}

func (p PodPath) JobDir() string {
	return filepath.Join(string(p), "job")
}

// PidsFile is the on-disk record of the pod's processes.
func (p PodPath) PidsFile() string { return filepath.Join(p.JobDir(), "pids.json") }

// ReadinessFile is where the sandbox handshake of plaidtainer is written.
func (p PodPath) ReadinessFile() string { return filepath.Join(p.JobDir(), "readiness.json") }

// ResolvConf is the pod's /etc/resolv.conf.
func (p PodPath) ResolvConf() string { return filepath.Join(p.JobDir(), "resolv.conf") }

// HostsFile is the pod's /etc/hosts.
func (p PodPath) HostsFile() string { return filepath.Join(p.JobDir(), "hosts") }

func (p PodPath) VolumeDir() string {
	return filepath.Join(string(p), "volumes")
}

// TmpDir is the pod's own Apptainer temporary directory (APPTAINER_TMPDIR).
// Apptainer unpacks each container's root filesystem here and removes it when the
// container exits normally; a container that is killed leaves it behind, so it
// must live inside the pod directory, where pod cleanup removes it.
func (p PodPath) TmpDir() string {
	return filepath.Join(string(p), "tmp")
}

func (p PodPath) LogDir() string {
	return filepath.Join(string(p), "logs")
}

func (p PodPath) Container(containerName string) ContainerPath {
	return ContainerPath{
		p:             p,
		containerName: containerName,
	}
}

type ContainerPath struct {
	p             PodPath
	containerName string
}

func (c ContainerPath) LogsPath() string {
	return filepath.Join(c.p.LogDir(), c.containerName+ExtensionLogs)
}

// LogDir is the directory of the container's per-attempt logs.
func (c ContainerPath) LogDir() string {
	return filepath.Join(c.p.LogDir(), c.containerName)
}

// AttemptLogPath is the log of one attempt (launch) of the container. Attempts
// are numbered from 1; the number is part of the file name, so a reader can pick
// the attempt it was asked for and can never mix the output of two.
func (c ContainerPath) AttemptLogPath(attempt int) string {
	return filepath.Join(c.LogDir(), fmt.Sprintf("%d%s", attempt, ExtensionAttemptLog))
}

// AttemptLogs lists the attempts that have a log, oldest first.
func (c ContainerPath) AttemptLogs() ([]int, error) {
	entries, err := os.ReadDir(c.LogDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var attempts []int
	for _, entry := range entries {
		if attempt, ok := ParseAttemptLogName(entry.Name()); ok && !entry.IsDir() {
			attempts = append(attempts, attempt)
		}
	}
	sort.Ints(attempts)
	return attempts, nil
}

// ParseAttemptLogName returns the attempt a log file name belongs to.
func ParseAttemptLogName(name string) (attempt int, ok bool) {
	number, found := strings.CutSuffix(name, ExtensionAttemptLog)
	if !found || number == "" || (len(number) > 1 && number[0] == '0') {
		return 0, false
	}
	attempt, err := strconv.Atoi(number)
	if err != nil || attempt <= 0 {
		return 0, false
	}
	return attempt, true
}

func (c ContainerPath) EnvFilePath() string {
	return filepath.Join(c.p.JobDir(), c.containerName+ExtensionEnvironment)
}
