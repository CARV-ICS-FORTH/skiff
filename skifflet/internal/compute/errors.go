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

package compute

import (
	"fmt"

	"errors"
	"skifflet/pkg/crdtools"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	ReasonUnsupportedFeatures = "UnsupportedFeatures"
	ReasonInitializationError = "InitializationError"
)

// Volume Errors
var (
	ErrUnsupportedClaimMode = errors.New("skiff does not support block volume provisioning")
)

func PodError(pod *corev1.Pod, reason string, msgFormat string, msgArgs ...any) {
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = reason
	pod.Status.Message = fmt.Sprintf(msgFormat, msgArgs...)

	crdtools.SetPodStatusCondition(&pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodReady,
		Status: corev1.ConditionFalse,
		// LastProbeTime:      metav1.Time{},
		LastTransitionTime: metav1.Now(),
		Reason:             "PodUnready",
		Message:            "Pod Has Failed.",
	})
}
