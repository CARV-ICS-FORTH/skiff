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

// Package configmap contains the internal representation of configMap volumes.
package configmap

import (
	"context"
	"fmt"

	"skifflet/internal/compute"
	"skifflet/internal/compute/volume"
	"skifflet/internal/compute/volume/util"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// VolumeMounter handles retrieving secrets from the API server
// and placing them into the volume on the host.
type VolumeMounter struct {
	Volume corev1.Volume
	Pod    corev1.Pod

	Logger logr.Logger
}

func (b *VolumeMounter) SetUpAt(ctx context.Context, dir string) error {
	var configMap corev1.ConfigMap

	source := b.Volume.ConfigMap
	optional := source.Optional != nil && *source.Optional

	/*---------------------------------------------------
	 * Get the Resource from Kubernetes
	 *---------------------------------------------------*/

	key := types.NamespacedName{Namespace: b.Pod.GetNamespace(), Name: source.Name}

	if optional {
		if err := compute.K8SClient.Get(ctx, key, &configMap); err != nil {
			if k8errors.IsNotFound(err) {
				configMap = corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: b.Pod.GetNamespace(),
						Name:      source.Name,
					},
				}
			} else {
				return fmt.Errorf("Couldn't get configMap '%s': %w", key, err)
			}
		}
	} else {
		if err := retry.OnError(volume.NotFoundBackoff,
			k8errors.IsNotFound, // retry condition
			func() error { // execution
				return compute.K8SClient.Get(ctx, key, &configMap)
			}); err != nil {
			return fmt.Errorf("Couldn't get configMap '%s': %w", key, err)
		}
	}

	// totalBytes := totalBytes(&configMap)

	/*---------------------------------------------------
	 * Mount Resource to the host
	 *---------------------------------------------------*/
	payload, err := MakePayload(source.Items, &configMap, source.DefaultMode, optional)
	if err != nil {
		return err
	}

	if err := util.MakeNestedMountpoints(b.Volume.Name, dir, b.Pod); err != nil {
		return err
	}

	// todo: Clean up directories if setup fails

	writerContext := fmt.Sprintf("Pod %v/%v volume %v", b.Pod.Namespace, b.Pod.Name, b.Volume.Name)

	writer, err := util.NewAtomicWriter(dir, writerContext)
	if err != nil {
		return fmt.Errorf("Error creating atomic writer: %w", err)
	}

	if err := writer.Write(payload); err != nil {
		return fmt.Errorf("Error writing payload to dir: %w", err)
	}

	return nil
}

// Sync writes the volume again from the ConfigMap as it is now, which the kubelet does for as long
// as the pod runs. One read, without waiting: a ConfigMap that cannot be read now, or that is gone,
// leaves the files as they are; an optional one that is gone empties the volume.
func (b *VolumeMounter) Sync(ctx context.Context, dir string) error {
	var object corev1.ConfigMap
	source := b.Volume.ConfigMap
	optional := source.Optional != nil && *source.Optional
	key := types.NamespacedName{Namespace: b.Pod.GetNamespace(), Name: source.Name}
	if err := compute.K8SClient.Get(ctx, key, &object); err != nil {
		if !optional || !k8errors.IsNotFound(err) {
			return err
		}
		object = corev1.ConfigMap{}
	}
	payload, err := MakePayload(source.Items, &object, source.DefaultMode, optional)
	if err != nil {
		return err
	}
	writer, err := util.NewAtomicWriter(dir, fmt.Sprintf("Pod %v/%v volume %v", b.Pod.Namespace, b.Pod.Name, b.Volume.Name))
	if err != nil {
		return err
	}
	return writer.Write(payload)
}

// MakePayload function is exported so that it can be called from the projection volume driver
func MakePayload(mappings []corev1.KeyToPath, configMap *corev1.ConfigMap, defaultMode *int32, optional bool) (map[string]util.FileProjection, error) {
	if defaultMode == nil {
		return nil, fmt.Errorf("no defaultMode used, not even the default value for it")
	}

	payload := make(map[string]util.FileProjection, (len(configMap.Data) + len(configMap.BinaryData)))
	var fileProjection util.FileProjection

	if len(mappings) == 0 {
		for name, data := range configMap.Data {
			fileProjection.Data = []byte(data)
			fileProjection.Mode = *defaultMode
			payload[name] = fileProjection
		}
		for name, data := range configMap.BinaryData {
			fileProjection.Data = data
			fileProjection.Mode = *defaultMode
			payload[name] = fileProjection
		}
	} else {
		for _, ktp := range mappings {
			if stringData, ok := configMap.Data[ktp.Key]; ok {
				fileProjection.Data = []byte(stringData)
			} else if binaryData, ok := configMap.BinaryData[ktp.Key]; ok {
				fileProjection.Data = binaryData
			} else {
				if optional {
					continue
				}
				return nil, fmt.Errorf("configmap references non-existent config key: %s", ktp.Key)
			}

			if ktp.Mode != nil {
				fileProjection.Mode = *ktp.Mode
			} else {
				fileProjection.Mode = *defaultMode
			}
			payload[ktp.Path] = fileProjection
		}
	}

	return payload, nil
}
