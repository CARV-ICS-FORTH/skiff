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

package provider

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// statusRepairPeriod is how often the status of the pods in the API server is compared
// with Skifflet's, as the kubelet's status manager does.
var statusRepairPeriod = 10 * time.Second

// RunStatusRepair puts back the status of this node's pods when someone else changed it
// in the API server, until ctx ends.
//
// Virtual Kubelet writes every status Skifflet notifies, and writes it again after a
// change by another writer only when it had just skipped a notification; a pod whose
// status is stable sends none. The node lifecycle controller marks every pod of a node
// not Ready when the node's heartbeat is late, and nothing made those pods Ready again
// once the node was back: their Services lost their endpoints for good.
//
// A difference is acted on only when the API server still has the same version of the
// pod at the next look, so a write that Virtual Kubelet has queued is not raced. The
// write carries that version, so it loses to any write in between.
func (v *VirtualK8S) RunStatusRepair(ctx context.Context, pods corev1client.PodsGetter) {
	ticker := time.NewTicker(statusRepairPeriod)
	defer ticker.Stop()
	var differing map[types.UID]string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			differing = v.repairStatus(ctx, pods, differing)
		}
	}
}

// repairStatus compares every pod Skifflet runs with the API server's copy, writes back
// the status of those that differed in the same version of the pod at the previous look
// (before), and returns the ones that differ now, by UID, with the version seen.
func (v *VirtualK8S) repairStatus(ctx context.Context, pods corev1client.PodsGetter, before map[types.UID]string) map[types.UID]string {
	lister := v.assigned.Load()
	if lister == nil || pods == nil {
		return nil
	}
	now := map[types.UID]string{}
	v.pods.Range(func(key, val any) bool {
		own := val.(*corev1.Pod)
		if !isPodOwnedByNode(own, v.NodeName) || isTerminal(own.Status.Phase) {
			return true
		}
		api, err := (*lister).Pods(own.Namespace).Get(own.Name)
		if err != nil || api.UID != own.UID || isTerminal(api.Status.Phase) {
			return true
		}
		what := statusDifference(api.Status, own.Status)
		if what == "" {
			return true
		}
		if before[api.UID] != api.ResourceVersion {
			now[api.UID] = api.ResourceVersion
			return true
		}
		update := api.DeepCopy()
		update.Status = *own.Status.DeepCopy()
		writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err = pods.Pods(api.Namespace).UpdateStatus(writeCtx, update, metav1.UpdateOptions{})
		switch {
		case err == nil:
			v.Logger.Info("the pod's status was changed by another writer; put back", "obj", key.(client.ObjectKey), "difference", what)
		case k8serrors.IsConflict(err) || k8serrors.IsNotFound(err):
			// Written or removed in between: the next look decides again.
		default:
			now[api.UID] = api.ResourceVersion
			v.Logger.Info("the pod's status, changed by another writer, could not be put back", "obj", key.(client.ObjectKey), "difference", what, "reason", err.Error())
		}
		return true
	})
	return now
}

func isTerminal(phase corev1.PodPhase) bool {
	return phase == corev1.PodSucceeded || phase == corev1.PodFailed
}

// statusDifference says how the API server's status differs from Skifflet's in what
// Skifflet decides: the phase, and the status and reason of the conditions it sets.
// Conditions of other types (another controller's) and times (which the API server
// keeps to the second) are not compared. Nothing is returned when they agree.
func statusDifference(api, own corev1.PodStatus) string {
	if api.Phase != own.Phase {
		return fmt.Sprintf("phase %s, not %s", api.Phase, own.Phase)
	}
	for _, want := range own.Conditions {
		var got *corev1.PodCondition
		for i := range api.Conditions {
			if api.Conditions[i].Type == want.Type {
				got = &api.Conditions[i]
				break
			}
		}
		switch {
		case got == nil:
			return fmt.Sprintf("condition %s missing", want.Type)
		case got.Status != want.Status || got.Reason != want.Reason:
			return fmt.Sprintf("condition %s=%s (%s), not %s (%s)", want.Type, got.Status, got.Reason, want.Status, want.Reason)
		}
	}
	return ""
}
