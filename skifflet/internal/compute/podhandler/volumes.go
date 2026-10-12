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
	"os"
	"path/filepath"
	"time"

	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/volume/configmap"
	"skifflet/internal/compute/volume/downwardapi"
	"skifflet/internal/compute/volume/emptydir"
	"skifflet/internal/compute/volume/hostpath"
	"skifflet/internal/compute/volume/projected"
	"skifflet/internal/compute/volume/secret"
	"skifflet/internal/compute/volume/util"

	"skifflet/internal/compute"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
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

		if vol.EmptyDir.Medium == corev1.StorageMediumMemory {
			if err := h.mountMemoryVolume(emptyDir, vol.EmptyDir); err != nil {
				return fmt.Errorf("cannot mount the memory emptyDir volume %q: %w", vol.Name, err)
			}
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

		mounter := &projected.VolumeMounter{
			Volume:    vol,
			Pod:       *h.Pod,
			Resources: h.resourceResolver(ctx),
			Logger:    h.logger,
		}

		if err := mounter.SetUpAt(ctx, projectedDir); err != nil {
			return fmt.Errorf("failed to mount ProjectedVolume '%s': %w", vol.Name, err)
		}
		if h.projected == nil {
			h.projected = map[string]*projected.VolumeMounter{}
		}
		h.projected[vol.Name] = mounter // (kept: syncVolumes renews its tokens)

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

	// The pod as it is now: its labels and annotations follow the API (see syncVolumes).
	data, err := downwardapi.CollectData(vol.DownwardAPI.Items, h.lifecycle.GetPod(), &defaultMode, h.resourceResolver(ctx))
	if err != nil {
		return fmt.Errorf("downwardAPI volume '%s': %w", vol.Name, err)
	}
	writer, err := util.NewAtomicWriter(downApiDir, fmt.Sprintf("pod %s volume %s", h.podKey, vol.Name))
	if err != nil {
		return fmt.Errorf("downwardAPI volume '%s': %w", vol.Name, err)
	}
	if err := writer.Write(data); err != nil {
		return fmt.Errorf("downwardAPI volume '%s': %w", vol.Name, err)
	}
	return nil
}

// volumeSyncPeriod is how often the files of the pod's ConfigMap, Secret, Downward API and
// projected volumes are written again (tests change it).
var volumeSyncPeriod = 30 * time.Second

// syncVolumes writes the pod's ConfigMap, Secret, Downward API and projected volumes again,
// every volumeSyncPeriod, for as long as the pod lives, as the kubelet does: a changed
// ConfigMap or Secret, a changed label or annotation, a service account token due to be
// renewed reach the files (through the atomic writer, so a reader sees one version or the
// other). A mount of a subPath of them does not change: it is bound to what it was when the
// container started, as in Kubernetes. What cannot be read now is tried again next time.
func (h *PodHandler) syncVolumes(ctx context.Context) {
	ticker := time.NewTicker(volumeSyncPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pod := h.lifecycle.GetPod()
		for _, vol := range pod.Spec.Volumes {
			dir := filepath.Join(h.podDirectory.VolumeDir(), vol.Name)
			var err error
			switch {
			case vol.ConfigMap != nil:
				err = (&configmap.VolumeMounter{Volume: vol, Pod: *pod, Logger: h.logger}).Sync(ctx, dir)
			case vol.Secret != nil:
				err = (&secret.VolumeMounter{Volume: vol, Pod: *pod, Logger: h.logger}).Sync(ctx, dir)
			case vol.DownwardAPI != nil:
				err = h.DownwardAPIVolumeSource(ctx, vol)
			case vol.Projected != nil:
				if m := h.projected[vol.Name]; m != nil {
					m.Pod = *pod
					err = m.Sync(ctx, dir)
				}
			}
			if err != nil && ctx.Err() == nil {
				h.logger.Info("a volume could not be brought up to date; trying again later", "volume", vol.Name, "reason", err.Error())
			}
		}
	}
}

// VolumePendingError says a volume cannot be mounted yet but may be later: its claim does
// not exist or is not bound, or the volume it is bound to is not there. The kubelet keeps
// the pod in ContainerCreating with FailedMount events and tries again, because the
// claim's provisioner, or whoever creates the volume, may be late; so does Skifflet
// (waitForVolumes). Everything else that stops a volume (a block volume, a source
// Skifflet cannot mount, a path outside the locations a volume may use, a claim that lost
// its volume) is not going to change and fails the pod.
type VolumePendingError struct{ Message string }

func (e *VolumePendingError) Error() string { return e.Message }

func volumePendingf(format string, args ...any) error {
	return &VolumePendingError{Message: fmt.Sprintf(format, args...)}
}

// PersistentVolumeClaimSource links the volume a claim is bound to into the pod. It looks
// once: a claim or volume that is not there yet is a VolumePendingError for the caller to
// wait on, and nothing here waits (it must not hold up the pod's deletion or a
// replacement, which cancel the caller).
func (h *PodHandler) PersistentVolumeClaimSource(ctx context.Context, vol corev1.Volume) error {
	/*---------------------------------------------------
	 * Get the Referenced PVC from Volume
	 *---------------------------------------------------*/
	var pvc corev1.PersistentVolumeClaim
	{
		source := vol.VolumeSource.PersistentVolumeClaim

		key := types.NamespacedName{Namespace: h.Pod.GetNamespace(), Name: source.ClaimName}

		if err := compute.K8SClient.Get(ctx, key, &pvc); err != nil {
			if k8errors.IsNotFound(err) {
				return volumePendingf("persistentvolumeclaim %q not found", source.ClaimName)
			}
			// The API server being briefly unreachable is not a reason to fail the pod.
			return volumePendingf("cannot read persistentvolumeclaim %q: %v", source.ClaimName, err)
		}

		// filter-out unsupported pvc
		if util.CheckPersistentVolumeClaimModeBlock(&pvc) {
			return fmt.Errorf("PVC (%s) has failed: %w", pvc.GetName(), compute.ErrUnsupportedClaimMode)
		}

		switch pvc.Status.Phase {
		case corev1.ClaimBound:
		case corev1.ClaimLost:
			return fmt.Errorf("PVC (%s) has lost its volume %q", pvc.GetName(), pvc.Spec.VolumeName)
		default:
			return volumePendingf("persistentvolumeclaim %q is not bound to a volume yet (phase %q)", pvc.GetName(), pvc.Status.Phase)
		}
	}

	/*---------------------------------------------------
	 * Get the referenced PV from PVC
	 *---------------------------------------------------*/
	var pv corev1.PersistentVolume
	{
		// A PersistentVolume is not namespaced.
		key := types.NamespacedName{Name: pvc.Spec.VolumeName}

		if err := compute.K8SClient.Get(ctx, key, &pv); err != nil {
			if k8errors.IsNotFound(err) {
				return volumePendingf("persistentvolume %q of claim %q not found", pvc.Spec.VolumeName, pvc.GetName())
			}
			return volumePendingf("cannot read persistentvolume %q: %v", pvc.Spec.VolumeName, err)
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

		// The volume's type, as for a pod's hostPath (with none, a missing directory is made).
		if err := hostpath.CheckType(pv.Spec.HostPath.Path, pv.Spec.HostPath.Type); err != nil {
			return fmt.Errorf("PersistentVolume %s: %w", pv.Name, err)
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
