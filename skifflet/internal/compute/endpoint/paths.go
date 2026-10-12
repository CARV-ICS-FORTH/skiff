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

// Skiff's two roots. The state root is the user's ~/.skiff (the shared home: credentials,
// images, volumes); inside a bubble, which runs as root, it is /root/.skiff. The
// scratch root, DefaultTmpDir unless the deployment names another (SKIFF_TMP_DIR),
// is node-local and disposable: the pods directory, Apptainer's unpack and conversion
// scratch and the sockets between the host and the bubble live in it.
//
// Layout of the runtime files of a pod, all under one directory per pod
// generation, <scratch root>/.pods/<namespace>/<name>_<pod UID>/ (the pod UID keeps a
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
//	scratch/<container>/<attempt>/{tmp,var-tmp,overlay,rootfs}
//	                     what a container sees as /tmp and /var/tmp, one pair per
//	                     attempt (see ContainerPath.AttemptTmp), its writable layer
//	                     (overlay, see ContainerPath.AttemptOverlayDir) and where
//	                     Apptainer unpacks its image (rootfs)
//
// Everything that launches, reads, execs into or cleans up a pod builds these
// paths here, from the scratch root it was configured with (SkiffPath); no
// other code spells out a file name or a path. User volume backing data does not
// live here.
const (
	PodGlobalDirectoryPermissions = os.FileMode(0o777)
	DefaultTmpDir                 = "/tmp/.skiff"
	DefaultPodsDir                = DefaultTmpDir + "/.pods"

	// DefaultPlaidSocket is the API socket of plaidd, the daemon that gives pods
	// their network and removes their endpoints.
	DefaultPlaidSocket = "/run/plaid/plaidd.sock"
)

// Container-Related Extensions
const (
	// ExtensionEnvironment describes the file where the environment variables for the container are held.
	ExtensionEnvironment = ".env"

	// ExtensionLogs describes the file where container execution will write its logs.

	// ExtensionAttemptLog ends the name of the log file of one attempt.
	ExtensionAttemptLog = ".log"
)

type SkiffPath struct {
	rootPath string // the state root
	tmpPath  string // the scratch root
}

func Skiff(rootPath string) SkiffPath {
	return SkiffWithTmp(rootPath, DefaultTmpDir)
}

// SkiffWithTmp is the layout of a deployment: the state root is <rootPath>/.skiff
// and the scratch root is tmpPath (DefaultTmpDir when empty).
func SkiffWithTmp(rootPath string, tmpPath string) SkiffPath {
	if tmpPath == "" {
		tmpPath = DefaultTmpDir
	}
	return SkiffPath{
		rootPath: filepath.Clean(filepath.Join(rootPath, ".skiff")),
		tmpPath:  filepath.Clean(tmpPath),
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

// TmpDir is the scratch root.
func (p SkiffPath) TmpDir() string {
	if p.tmpPath == "" {
		return DefaultTmpDir
	}
	return p.tmpPath
}

func (p SkiffPath) PodsDir() string {
	return filepath.Join(p.TmpDir(), ".pods")
}

// CheckVolumePath says whether a volume (a hostPath, or the path of a volume that a
// claim is bound to) may use path. Skifflet runs inside a bubble that sees the host
// through the state root (the user's ~/.skiff, /root/.skiff in the bubble) and the
// scratch root only, so a path anywhere else does not exist for it: a directory
// created there would be lost with the bubble, and one that must exist could not be
// found. It is refused with that explanation rather than failing or losing data later.
func (p SkiffPath) CheckVolumePath(path string) error {
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) {
		for _, root := range []string{p.String(), p.TmpDir()} {
			if rel, err := filepath.Rel(root, clean); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				return nil
			}
		}
	}
	return fmt.Errorf("volume path %q is outside the locations a volume may use: %s (the user's ~/.skiff on the host) and %s (the node's scratch directory)",
		path, p.String(), p.TmpDir())
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

// ShmDir is where the pod's /dev/shm is mounted: a tmpfs of its own, shared by its containers.
func (p PodPath) ShmDir() string { return filepath.Join(string(p), "shm") }

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

// ScratchDir holds the containers' own /tmp and /var/tmp (see ContainerPath.AttemptTmp).
// It is separate from TmpDir, which is Apptainer's: what an application writes and
// what the runtime unpacks never share a directory, and a container sees only its own.
func (p PodPath) ScratchDir() string {
	return filepath.Join(string(p), "scratch")
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

// ScratchDir is the directory of the container's per-attempt /tmp and /var/tmp.
func (c ContainerPath) ScratchDir() string {
	return filepath.Join(c.p.ScratchDir(), c.containerName)
}

// AttemptTmp is what attempt N of the container sees as /tmp, and AttemptVarTmp as
// /var/tmp. As in Kubernetes, where they are part of the container's own writable
// layer, they belong to one container (init containers included), start empty with
// every attempt, and go with the pod. They are on the node's disk, not in memory, and
// not shared with another container (an emptyDir volume is how containers share files).
func (c ContainerPath) AttemptTmp(attempt int) string {
	return filepath.Join(c.ScratchDir(), strconv.Itoa(attempt), "tmp")
}

func (c ContainerPath) AttemptVarTmp(attempt int) string {
	return filepath.Join(c.ScratchDir(), strconv.Itoa(attempt), "var-tmp")
}

// AttemptOverlayDir is the writable layer of attempt N of the container: what the container
// writes to its root filesystem (anywhere outside its volumes) goes here, over the image
// as unpacked for it. As in Kubernetes the layer is private to the container, new with every
// attempt (a restart finds none of what the attempt before wrote), on the node's disk and
// not in memory, as large as the disk allows, and removed with the attempt's scratch
// directory.
func (c ContainerPath) AttemptOverlayDir(attempt int) string {
	return filepath.Join(c.AttemptScratchDir(attempt), "overlay")
}

// AttemptUnpackDir is attempt N's Apptainer temporary directory (APPTAINER_TMPDIR): where
// Apptainer unpacks the image when the container starts (--unsquash), the read-only layer
// the writable one lies over. Apptainer removes it when the container exits; one that
// is killed leaves it, and it goes with the attempt's scratch directory.
func (c ContainerPath) AttemptUnpackDir(attempt int) string {
	return filepath.Join(c.AttemptScratchDir(attempt), "rootfs")
}

// AttemptTerminationLog is the file a container's termination message is written to in
// attempt N, bound at its terminationMessagePath (see Attempt.terminationMessage).
func (c ContainerPath) AttemptTerminationLog(attempt int) string {
	return filepath.Join(c.AttemptScratchDir(attempt), "termination-log")
}

// AttemptScratchDir is the directory that holds all of attempt N's directories.
func (c ContainerPath) AttemptScratchDir(attempt int) string {
	return filepath.Join(c.ScratchDir(), strconv.Itoa(attempt))
}

// ScratchAttempts lists the attempts that have a scratch directory.
func (c ContainerPath) ScratchAttempts() ([]int, error) {
	entries, err := os.ReadDir(c.ScratchDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var attempts []int
	for _, entry := range entries {
		if attempt, err := strconv.Atoi(entry.Name()); err == nil && attempt > 0 && entry.IsDir() {
			attempts = append(attempts, attempt)
		}
	}
	sort.Ints(attempts)
	return attempts, nil
}

func (c ContainerPath) EnvFilePath() string {
	return filepath.Join(c.p.JobDir(), c.containerName+ExtensionEnvironment)
}
