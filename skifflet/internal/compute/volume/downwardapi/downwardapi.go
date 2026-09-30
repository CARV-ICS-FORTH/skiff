// Copyright © 2022 FORTH-ICS
// Copyright 2015 The Kubernetes Authors.
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

package downwardapi

import (
	"fmt"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	volumeutil "skifflet/internal/compute/volume/util"
	"skifflet/pkg/fieldpath"
)

// CollectData collects requested downwardAPI in data map.
// Map's key is the requested name of file to dump
// Map's value is the (sorted) content of the field to be dumped in the file.
//
// Note: this function is exported so that it can be called from the projection volume driver
func CollectData(items []corev1.DownwardAPIVolumeFile, pod *corev1.Pod, defaultMode *int32, resources fieldpath.ResourceResolver) (map[string]volumeutil.FileProjection, error) {
	if defaultMode == nil {
		return nil, fmt.Errorf("no defaultMode used, not even the default value for it")
	}

	errlist := []error{}
	data := make(map[string]volumeutil.FileProjection)
	for _, fileInfo := range items {
		var fileProjection volumeutil.FileProjection
		fPath := filepath.Clean(fileInfo.Path)
		if fileInfo.Mode != nil {
			fileProjection.Mode = *fileInfo.Mode
		} else {
			fileProjection.Mode = *defaultMode
		}

		if fileInfo.FieldRef != nil {
			if values, err := fieldpath.PodFieldValue(pod, fileInfo.FieldRef.FieldPath); err != nil {
				klog.Errorf("Unable to extract field %s: %s", fileInfo.FieldRef.FieldPath, err.Error())
				errlist = append(errlist, err)
			} else {
				fileProjection.Data = []byte(values)
			}
		} else if fileInfo.ResourceFieldRef != nil {
			if resources == nil {
				err := fmt.Errorf("resourceFieldRef for container '%s', resource '%s' at path '%s' cannot be resolved here", fileInfo.ResourceFieldRef.ContainerName, fileInfo.ResourceFieldRef.Resource, fileInfo.Path)
				klog.Errorf("%v", err)
				errlist = append(errlist, err)
			} else if value, err := resources(fileInfo.ResourceFieldRef); err != nil {
				err = fmt.Errorf("path '%s': %w", fileInfo.Path, err)
				klog.Errorf("%v", err)
				errlist = append(errlist, err)
			} else {
				fileProjection.Data = []byte(value)
			}
		} else {
			err := fmt.Errorf("fieldRef and resourceFieldRef are both missing for path '%s'", fileInfo.Path)
			klog.Errorf("%v", err)
			errlist = append(errlist, err)
		}

		data[fPath] = fileProjection
	}
	return data, utilerrors.NewAggregate(errlist)
}
