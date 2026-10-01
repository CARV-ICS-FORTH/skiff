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
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"errors"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/volume"
	"skifflet/internal/compute/volume/configmap"
	"skifflet/internal/compute/volume/downwardapi"
	"skifflet/internal/compute/volume/emptydir"
	"skifflet/internal/compute/volume/hostpath"
	"skifflet/internal/compute/volume/projected"
	"skifflet/internal/compute/volume/secret"
	"skifflet/internal/compute/volume/util"

	mounter "k8s.io/utils/mount"

	"skifflet/internal/compute"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// mountVolumeSource prepares the volumes into the local pod directory.
// Critical errors related to Skiff fail directly.
// Misconfigurations (like wrong hostpaths), are returned as errors
func (h *PodHandler) mountVolumeSource(ctx context.Context, vol corev1.Volume) error {
	switch {
	case vol.VolumeSource.EmptyDir != nil:
		/*---------------------------------------------------
		 * EmptyDir
		 *---------------------------------------------------*/
		emptyDir := filepath.Join(h.podDirectory.VolumeDir(), vol.Name)

		if err := os.MkdirAll(emptyDir, endpoint.PodGlobalDirectoryPermissions); err != nil {
			return fmt.Errorf("cannot create dir '%s': %w", emptyDir, err)
		}

		mounter := emptydir.VolumeMounter{
			Volume: vol,
			Pod:    *h.Pod,
			Logger: h.logger,
		}

		if err := mounter.SetUpAt(ctx, emptyDir); err != nil {
			return fmt.Errorf("mount emptyDir volume to dir '%s' has failed: %w", emptyDir, err)
		}

		h.logger.Info("  * EmptyDir Volume is mounted", "name", vol.Name)

		return nil

	case vol.VolumeSource.ConfigMap != nil:
		/*---------------------------------------------------
		 * ConfigMap
		 *---------------------------------------------------*/
		configMapDir := filepath.Join(h.podDirectory.VolumeDir(), vol.Name)

		if err := os.MkdirAll(configMapDir, endpoint.PodGlobalDirectoryPermissions); err != nil {
			return fmt.Errorf("cannot create dir '%s': %w", configMapDir, err)
		}

		mounter := configmap.VolumeMounter{
			Volume: vol,
			Pod:    *h.Pod,
			Logger: h.logger,
		}

		if err := mounter.SetUpAt(ctx, configMapDir); err != nil {
			compute.DefaultLogger.Info("mount configMap volume has failed",
				"volume", vol.Name,
				"dir", configMapDir,
			)

			return fmt.Errorf("failed to mount ConfigMap '%s': %w", vol.Name, err)
		}

		h.logger.Info("  * ConfigMap Volume is mounted", "name", vol.Name)

		return nil

	case vol.VolumeSource.Secret != nil:
		/*---------------------------------------------------
		 * Secret
		 *---------------------------------------------------*/
		secretDir := filepath.Join(h.podDirectory.VolumeDir(), vol.Name)

		if err := os.MkdirAll(secretDir, endpoint.PodGlobalDirectoryPermissions); err != nil {
			return fmt.Errorf("cannot create dir '%s': %w", secretDir, err)
		}

		mounter := secret.VolumeMounter{
			Volume: vol,
			Pod:    *h.Pod,
			Logger: h.logger,
		}

		if err := mounter.SetUpAt(ctx, secretDir); err != nil {
			compute.DefaultLogger.Info("mount secret volume has failed",
				"volume", vol.Name,
				"dir", secretDir,
			)

			return fmt.Errorf("failed to mount Secret '%s': %w", vol.Name, err)
		}

		h.logger.Info("  * Secret Volume is mounted", "name", vol.Name)

		return nil

	case vol.VolumeSource.DownwardAPI != nil:
		/*---------------------------------------------------
		 * Downward API
		 *---------------------------------------------------*/
		if err := h.DownwardAPIVolumeSource(ctx, vol); err != nil {
			return fmt.Errorf("failed to mount DownwardAPI volume '%s': %w", vol.Name, err)
		}

		h.logger.Info("  * DownwardAPI Volume is mounted", "name", vol.Name)

		return nil

	case vol.VolumeSource.HostPath != nil:
		/*---------------------------------------------------
		 * HostPath
		 *---------------------------------------------------*/
		if vol.VolumeSource.HostPath.Type == nil || *vol.VolumeSource.HostPath.Type == corev1.HostPathUnset {
			// ensure that the references host path exists
			exists, err := mounter.PathExists(vol.VolumeSource.HostPath.Path)
			if err != nil {
				return fmt.Errorf("failed to inspect HostPath at path '%s': %w", vol.VolumeSource.HostPath.Path, err)
			}

			if !exists {
				return fmt.Errorf("HostPath '%s' does not exist", vol.VolumeSource.HostPath.Path)
			}

			// Empty string (default) is for backward compatibility,
			// which means that no checks will be performed before mounting the hostPath volume.
			dstFullPath := filepath.Join(h.podDirectory.VolumeDir(), vol.Name)

			if err := os.Symlink(vol.VolumeSource.HostPath.Path, dstFullPath); err != nil {
				return fmt.Errorf("cannot link symlink at path '%s': %w", dstFullPath, err)
			}

			// nothing to do
			return nil
		}

		mounter := hostpath.VolumeMounter{
			Volume: vol,
			Pod:    *h.Pod,
			Logger: h.logger,
		}

		if err := mounter.SetUpAt(ctx); err != nil {
			return fmt.Errorf("mount hostpath volume has failed: %w", err)
		}

		dstFullPath := filepath.Join(h.podDirectory.VolumeDir(), vol.Name)
		if _, err := os.Lstat(dstFullPath); os.IsNotExist(err) {
			if err := os.Symlink(vol.VolumeSource.HostPath.Path, dstFullPath); err != nil {
				return fmt.Errorf("cannot link symlink at path '%s': %w", dstFullPath, err)
			}
		}

		h.logger.Info("  * HostPath Volume is mounted", "name", vol.Name)

		return nil

	case vol.VolumeSource.PersistentVolumeClaim != nil:
		/*---------------------------------------------------
		 * Persistent Volume Claim
		 *---------------------------------------------------*/
		if err := h.PersistentVolumeClaimSource(ctx, vol); err != nil {
			return fmt.Errorf("failed to mount PersistentVolumeClaim volume '%s': %w", vol.Name, err)
		}

		h.logger.Info("  * PersistentVolumeClaim Volume is mounted", "name", vol.Name)

		return nil

	case vol.VolumeSource.Projected != nil:
		/*---------------------------------------------------
		 * Projected
		 *---------------------------------------------------*/
		projectedDir := filepath.Join(h.podDirectory.VolumeDir(), vol.Name)

		if err := os.MkdirAll(projectedDir, endpoint.PodGlobalDirectoryPermissions); err != nil {
			return fmt.Errorf("cannot create dir '%s': %w", projectedDir, err)
		}

		mounter := projected.VolumeMounter{
			Volume:    vol,
			Pod:       *h.Pod,
			Resources: h.resourceResolver(ctx),
			Logger:    h.logger,
		}

		if err := mounter.SetUpAt(ctx, projectedDir); err != nil {
			return fmt.Errorf("failed to mount ProjectedVolume '%s': %w", vol.Name, err)
		}

		return nil

	default:
		logrus.Warn(vol)

		return fmt.Errorf("unsupported volume type for volume '%s'", vol.Name)
	}
}

func (h *PodHandler) DownwardAPIVolumeSource(ctx context.Context, vol corev1.Volume) error {
	downApiDir := filepath.Join(h.podDirectory.VolumeDir(), vol.Name)

	if err := os.MkdirAll(downApiDir, endpoint.PodGlobalDirectoryPermissions); err != nil {
		return fmt.Errorf("cannot create dir '%s': %w", downApiDir, err)
	}

	if vol.DownwardAPI == nil {
		return nil
	}

	defaultMode := int32(0644)
	if vol.DownwardAPI.DefaultMode != nil {
		defaultMode = *vol.DownwardAPI.DefaultMode
	}

	data, err := downwardapi.CollectData(vol.DownwardAPI.Items, h.Pod, &defaultMode, h.resourceResolver(ctx))
	if err != nil {
		return fmt.Errorf("downwardAPI volume '%s': %w", vol.Name, err)
	}

	for relPath, fileProj := range data {
		itemPath := filepath.Join(downApiDir, relPath)
		if err := os.MkdirAll(filepath.Dir(itemPath), endpoint.PodGlobalDirectoryPermissions); err != nil {
			return fmt.Errorf("cannot create dir '%s': %w", filepath.Dir(itemPath), err)
		}
		if err := os.WriteFile(itemPath, fileProj.Data, fs.FileMode(fileProj.Mode)); err != nil {
			return fmt.Errorf("cannot write downwardAPI file '%s': %w", itemPath, err)
		}
	}

	return nil
}

func (h *PodHandler) PersistentVolumeClaimSource(ctx context.Context, vol corev1.Volume) error {
	/*---------------------------------------------------
	 * Get the Referenced PVC from Volume
	 *---------------------------------------------------*/
	var pvc corev1.PersistentVolumeClaim
	{
		source := vol.VolumeSource.PersistentVolumeClaim

		key := types.NamespacedName{Namespace: h.Pod.GetNamespace(), Name: source.ClaimName}

		if errPVC := retry.OnError(volume.NotFoundBackoff,
			func(err error) bool { // retry condition
				return k8errors.IsNotFound(err) || errors.Is(err, compute.ErrUnboundedPVC)
			},
			func() error { // execution
				if err := compute.K8SClient.Get(ctx, key, &pvc); err != nil {
					compute.DefaultLogger.Info("Failed to get PVC", "pvcName", pvc.GetName())

					return err
				}

				// filter-out unsupported pvc
				if util.CheckPersistentVolumeClaimModeBlock(&pvc) {
					compute.DefaultLogger.Info("Unsupported PVC mode", "pvcName", pvc.GetName())

					return compute.ErrUnsupportedClaimMode
				}

				// ensure that pvc is bounded to a pv
				if pvc.Status.Phase != corev1.ClaimBound {
					compute.DefaultLogger.Info("Waiting for PVC to become bounded to a PV",
						"pvcName", pvc.GetName(),
					)

					return compute.ErrUnboundedPVC
				}

				return nil
			},
		); errPVC != nil { // error cehcking
			return fmt.Errorf("PVC (%s) has failed: %w", pvc.GetName(), errPVC)
		}
	}

	/*---------------------------------------------------
	 * Get the referenced PV from PVC
	 *---------------------------------------------------*/
	var pv corev1.PersistentVolume
	{
		// A PersistentVolume is not namespaced.
		key := types.NamespacedName{Name: pvc.Spec.VolumeName}

		if errPV := retry.OnError(volume.NotFoundBackoff,
			func(err error) bool { // retry condition
				return k8errors.IsNotFound(err)
			},
			func() error { // execution
				return compute.K8SClient.Get(ctx, key, &pv)
			},
		); errPV != nil { // error checking
			return fmt.Errorf("PV (%s) has failed: %w", pvc.Spec.VolumeName, errPV)
		}
	}

	h.logger.Info("PVC bounding info", "pvc", pvc.Spec, "pv", pv.Spec)

	/*---------------------------------------------------
	 * Link the Referenced PV to the Pod's Volumes
	 *---------------------------------------------------*/
	// Where the volume is, in the host's words, must be somewhere the bubble sees
	// (hostPath volumes are checked when the pod is created, a claim's volume only now).
	volumePath := ""
	switch {
	case pv.Spec.HostPath != nil:
		volumePath = pv.Spec.HostPath.Path
	case pv.Spec.Local != nil:
		volumePath = pv.Spec.Local.Path
	}
	if volumePath != "" {
		if err := compute.Skiff.CheckVolumePath(volumePath); err != nil {
			return fmt.Errorf("PersistentVolume %s: %w", pv.Name, err)
		}
	}

	switch {
	case pv.Spec.HostPath != nil:
		dstFullPath := filepath.Join(h.podDirectory.VolumeDir(), vol.Name)

		if err := os.MkdirAll(pv.Spec.HostPath.Path, endpoint.PodGlobalDirectoryPermissions); err != nil && !os.IsExist(err) {
			return fmt.Errorf("cannot create hostpath directory at path '%s': %w", pv.Spec.HostPath.Path, err)
		}
		if err := os.Symlink(pv.Spec.HostPath.Path, dstFullPath); err != nil && !os.IsExist(err) {
			return fmt.Errorf("cannot link symlink at path '%s': %w", dstFullPath, err)
		}

		h.logger.Info("  * HostPath PV Volume is mounted", "fullpath", dstFullPath)
		return nil
	case pv.Spec.Local != nil:
		dstFullPath := filepath.Join(h.podDirectory.VolumeDir(), vol.Name)

		if err := os.Symlink(pv.Spec.Local.Path, dstFullPath); err != nil {
			return fmt.Errorf("cannot link symlink at path '%s': %w", dstFullPath, err)
		}

		h.logger.Info("  * Local Volume is mounted", "fullpath", dstFullPath)
		return nil
	default:
		logrus.Warn(vol)

		return fmt.Errorf("unsupported PersistentVolume type for volume '%s'", vol.Name)
	}
}
