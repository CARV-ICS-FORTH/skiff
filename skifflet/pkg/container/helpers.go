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

package container

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"skifflet/pkg/expansion"
)

// envVarsToMap constructs a map of environment name to value from a slice
// of env vars.
func envVarsToMap(envs []corev1.EnvVar) map[string]string {

	result := map[string]string{}
	for _, env := range envs {
		result[env.Name] = env.Value
	}
	return result
}

// ExpandContainerCommand substitutes $(VAR) references in a container's command or arguments
// with the values of its environment, which the caller gives fully resolved (valueFrom too).
func ExpandContainerCommand(containerCommand []string, envs []corev1.EnvVar) (command []string) {
	mapping := expansion.MappingFuncFor(envVarsToMap(envs))
	if len(containerCommand) != 0 {
		for _, cmd := range containerCommand {
			command = append(command, expansion.Expand(cmd, mapping))
		}
	}
	return command
}

// ExpandContainerVolumeMounts expands the subpath of the given VolumeMount by replacing variable references with the values of given EnvVar.
func ExpandContainerVolumeMounts(mount corev1.VolumeMount, envs []corev1.EnvVar) (string, error) {

	envmap := envVarsToMap(envs)
	missingKeys := sets.NewString()
	expanded := expansion.Expand(mount.SubPathExpr, func(key string) string {
		value, ok := envmap[key]
		if !ok || len(value) == 0 {
			missingKeys.Insert(key)
		}
		return value
	})

	if len(missingKeys) > 0 {
		return "", fmt.Errorf("missing value for %s", strings.Join(missingKeys.List(), ", "))
	}
	return expanded, nil
}
