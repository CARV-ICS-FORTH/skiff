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
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	PodGlobalDirectoryPermissions = os.FileMode(0o777)
	DefaultPodsDir                = "/tmp/.skiff/.pods"
)

// Container-Related Extensions
const (
	// ExtensionEnvironment describes the file where the environment variables for the container are held.
	ExtensionEnvironment = ".env"

	// ExtensionLogs describes the file where container execution will write its logs.
	ExtensionLogs = ".logs"
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

func (c ContainerPath) EnvFilePath() string {
	return filepath.Join(c.p.JobDir(), c.containerName+ExtensionEnvironment)
}
