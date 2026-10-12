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

package projected

import (
	"context"
	"fmt"
	"time"

	"skifflet/internal/compute"
	"skifflet/internal/compute/volume"
	"skifflet/internal/compute/volume/configmap"
	"skifflet/internal/compute/volume/downwardapi"
	"skifflet/internal/compute/volume/secret"
	"skifflet/internal/compute/volume/util"
	"skifflet/pkg/fieldpath"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/util/retry"
)

// VolumeMounter handles retrieving secrets from the API server
// and placing them into the volume on the host.
type VolumeMounter struct {
	Volume corev1.Volume

	Pod corev1.Pod

	// Resources evaluates resourceFieldRef items of a downwardAPI source.
	Resources fieldpath.ResourceResolver

	Logger logr.Logger

	// tokens are the service account tokens of the volume, by path, kept until 80% of their
	// life has passed (as the kubelet renews them), so that a sync does not ask for new ones.
	tokens map[string]projectedToken
}

type projectedToken struct {
	data       []byte
	renewAfter time.Time
}

func (b *VolumeMounter) SetUpAt(ctx context.Context, dir string) error {
	podKey := types.NamespacedName{Namespace: b.Pod.GetNamespace(), Name: b.Pod.GetName()}

	if b.Volume.Projected.DefaultMode == nil {
		return fmt.Errorf("no defaultMode used, not even the default value for it")
	}

	var data map[string]util.FileProjection
	var earliestExpiry time.Time
	var errCollect error

	if err := retry.OnError(volume.NotFoundBackoff,
		k8errors.IsNotFound, // retry condition
		func() error { // execution
			data, earliestExpiry, errCollect = b.collectDataWithExpiry(ctx)
			return errCollect
		},
	); err != nil { // error checking
		return fmt.Errorf("error preparing data for project volume. volume:'%s' pod:'%s': %w", b.Volume.Name, podKey, err)
	}

	/*---------------------------------------------------
	 * Mount Resource to the host
	 *---------------------------------------------------*/
	if err := util.MakeNestedMountpoints(b.Volume.Name, dir, b.Pod); err != nil {
		return err
	}

	writerContext := fmt.Sprintf("pod %s volume %v", podKey, b.Volume.Name)
	writer, err := util.NewAtomicWriter(dir, writerContext)
	if err != nil {
		return fmt.Errorf("Error creating atomic writer: %w", err)
	}

	err = writer.Write(data)
	if err != nil {
		return fmt.Errorf("Error writing payload to dir: %w", err)
	}
	_ = earliestExpiry

	return nil
}

// Sync writes the volume again from what its sources say now: a changed ConfigMap, Secret or
// label, and a token that is due to be renewed. What cannot be read now leaves the volume as
// it is. The files change at once, through the atomic writer.
func (b *VolumeMounter) Sync(ctx context.Context, dir string) error {
	data, _, err := b.collectDataWithExpiry(ctx)
	if err != nil {
		return err
	}
	writer, err := util.NewAtomicWriter(dir, fmt.Sprintf("pod %s/%s volume %v", b.Pod.GetNamespace(), b.Pod.GetName(), b.Volume.Name))
	if err != nil {
		return err
	}
	return writer.Write(data)
}

func (b *VolumeMounter) collectDataWithExpiry(ctx context.Context) (map[string]util.FileProjection, time.Time, error) {
	var errlist []error
	var earliestExpiry time.Time
	payload := make(map[string]util.FileProjection)

	for _, source := range b.Volume.Projected.Sources {
		switch {
		case source.Secret != nil:
			/*---------------------------------------------------
			 * Projected Secret
			 *---------------------------------------------------*/
			var secretAPI corev1.Secret
			secretAPI.Namespace = b.Pod.GetNamespace()
			secretAPI.Name = source.Secret.Name

			optional := source.Secret.Optional != nil && *source.Secret.Optional
			key := types.NamespacedName{Namespace: b.Pod.GetNamespace(), Name: source.Secret.Name}

			{ // get the resource
				if optional {
					if err := compute.K8SClient.Get(ctx, key, &secretAPI); err != nil && !k8errors.IsNotFound(err) {
						b.Logger.Info("Couldn't get projected.secret", "key", key)
						errlist = append(errlist, err)
						continue
					}
				} else {
					if err := retry.OnError(volume.NotFoundBackoff,
						k8errors.IsNotFound, // retry condition
						func() error { // execution
							return compute.K8SClient.Get(ctx, key, &secretAPI)
						},
					); err != nil {
						b.Logger.Info("Couldn't get projected.secret", "key", key)
						errlist = append(errlist, err)
						continue
					}
				}
			}

			secretPayload, err := secret.MakePayload(source.Secret.Items, &secretAPI, b.Volume.Projected.DefaultMode, optional)
			if err != nil {
				b.Logger.Error(err, "Couldn't get secret payload")
				errlist = append(errlist, err)
				continue
			}

			for k, v := range secretPayload {
				payload[k] = v
			}

		case source.ConfigMap != nil:
			/*---------------------------------------------------
			 * Projected ConfigMap
			 *---------------------------------------------------*/
			var configMapAPI corev1.ConfigMap
			configMapAPI.Namespace = b.Pod.GetNamespace()
			configMapAPI.Name = source.ConfigMap.Name

			optional := source.ConfigMap.Optional != nil && *source.ConfigMap.Optional
			key := types.NamespacedName{Namespace: b.Pod.GetNamespace(), Name: source.ConfigMap.Name}

			{ // get the resource
				if optional {
					if err := compute.K8SClient.Get(ctx, key, &configMapAPI); err != nil && !k8errors.IsNotFound(err) {
						b.Logger.Info("Couldn't get projected.configmap", "key", key)
						errlist = append(errlist, err)
						continue
					}
				} else {
					if err := retry.OnError(volume.NotFoundBackoff,
						k8errors.IsNotFound, // retry condition
						func() error { // execution
							return compute.K8SClient.Get(ctx, key, &configMapAPI)
						},
					); err != nil {
						b.Logger.Info("Couldn't get projected.configmap", "key", key)
						errlist = append(errlist, err)
						continue
					}
				}
			}

			configMapPayload, err := configmap.MakePayload(source.ConfigMap.Items, &configMapAPI, b.Volume.Projected.DefaultMode, optional)
			if err != nil {
				b.Logger.Error(err, "Couldn't get configMap payload")

				errlist = append(errlist, err)
				continue
			}

			for k, v := range configMapPayload {
				payload[k] = v
			}
		case source.DownwardAPI != nil:
			/*---------------------------------------------------
			 * Projected DownwardAPI
			 *---------------------------------------------------*/
			downwardAPIPayload, err := downwardapi.CollectData(source.DownwardAPI.Items, &b.Pod, b.Volume.Projected.DefaultMode, b.Resources)
			if err != nil {
				b.Logger.Error(err, "Couldn't get downwardAPI payload")

				errlist = append(errlist, err)
				continue
			}

			for k, v := range downwardAPIPayload {
				payload[k] = v
			}

		case source.ServiceAccountToken != nil:
			/*---------------------------------------------------
			 * Projected ServiceAccountToken
			 *---------------------------------------------------*/
			if cached, ok := b.tokens[source.ServiceAccountToken.Path]; ok && time.Now().Before(cached.renewAfter) {
				payload[source.ServiceAccountToken.Path] = util.FileProjection{Data: cached.data, Mode: *b.Volume.Projected.DefaultMode}
				continue
			}
			tokenRequest, err := compute.K8SClientset.CoreV1().
				ServiceAccounts(b.Pod.GetNamespace()).
				CreateToken(ctx, b.Pod.Spec.ServiceAccountName, &authenticationv1.TokenRequest{
					Spec: authenticationv1.TokenRequestSpec{
						Audiences: func() []string {
							if len(source.ServiceAccountToken.Audience) != 0 {
								return []string{source.ServiceAccountToken.Audience}
							}
							return nil
						}(),
						ExpirationSeconds: source.ServiceAccountToken.ExpirationSeconds,
						BoundObjectRef: &authenticationv1.BoundObjectReference{
							Kind:       "Pod",
							APIVersion: "v1",
							Name:       b.Pod.GetName(),
							UID:        b.Pod.GetUID(),
						},
					},
				}, metav1.CreateOptions{})
			if err != nil {
				errlist = append(errlist, err)
				continue
			}

			if !tokenRequest.Status.ExpirationTimestamp.IsZero() {
				if earliestExpiry.IsZero() || tokenRequest.Status.ExpirationTimestamp.Time.Before(earliestExpiry) {
					earliestExpiry = tokenRequest.Status.ExpirationTimestamp.Time
				}
			}

			payload[source.ServiceAccountToken.Path] = util.FileProjection{
				Data: []byte(tokenRequest.Status.Token),
				Mode: *b.Volume.Projected.DefaultMode,
			}
			if b.tokens == nil {
				b.tokens = map[string]projectedToken{}
			}
			now := time.Now()
			renewAfter := now.Add(10 * time.Minute)
			if expiry := tokenRequest.Status.ExpirationTimestamp.Time; expiry.After(now) {
				renewAfter = now.Add(time.Duration(float64(expiry.Sub(now)) * 0.8))
			}
			b.tokens[source.ServiceAccountToken.Path] = projectedToken{data: []byte(tokenRequest.Status.Token), renewAfter: renewAfter}
		}
	}
	return payload, earliestExpiry, utilerrors.NewAggregate(errlist)
}
