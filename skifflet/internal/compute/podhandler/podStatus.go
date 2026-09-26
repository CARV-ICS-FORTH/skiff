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
	"fmt"
	"sort"

	"skifflet/internal/compute"
	"skifflet/pkg/crdtools"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// UpdateStatusFromRuntime performs a deep investigation of the running conditions of the pod to resolve its current status.
func UpdateStatusFromRuntime(pod *corev1.Pod) {
	podKey := client.ObjectKeyFromObject(pod)
	logger := compute.DefaultLogger.WithValues("pod", podKey)

	/*---------------------------------------------------
	 * Handle Initialization and Final States
	 *---------------------------------------------------*/
	switch pod.Status.Phase {
	case "":
		/*-- If met for first time, check for unsupported fields --*/
		if err := ValidatePodCapabilities(pod); err != nil {
			compute.PodError(pod, compute.ReasonUnsupportedFeatures, "%v", err)
			return
		}
	case corev1.PodSucceeded, corev1.PodFailed:
		/*-- If on final states, there is nothing else to do --*/
		if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
			return
		}
	}

	/*-- Initialization of virtual environment  --*/
	if pod.Status.PodIP == "" {
		if ip, ok := pod.Annotations["skiff.io/pod-ip"]; ok && ip != "" {
			pod.Status.PodIP = ip
			pod.Status.PodIPs = append(pod.Status.PodIPs, corev1.PodIP{IP: ip})
		}
	}

	/*---------------------------------------------------
	 * Load Container Statuses
	 *---------------------------------------------------*/
	SyncContainerStatuses(pod)

	/*---------------------------------------------------
	 * Check status of Init Containers
	 *---------------------------------------------------*/
	// https://kubernetes.io/docs/concepts/workloads/pods/init-containers/

	/*-- A Pod that is initializing is in the Pending state --*/
	if pod.Status.Phase == corev1.PodPending || pod.Status.Phase == "" {
		for _, initContainer := range pod.Status.InitContainerStatuses {
			if initContainer.State.Terminated == nil {
				/*-- Still Initializing: at least one init container is still running --*/
				return
			} else {
				/*-- Initialization error: at least one init container has failed --*/
				if initContainer.State.Terminated.ExitCode != 0 {
					compute.PodError(pod, compute.ReasonInitializationError, "Init container '%s' has failed", initContainer.Name)

					return
				}
			}
		}

		/*-- Pod is Ready: all init containers have completed successfully --*/
		crdtools.SetPodStatusCondition(&pod.Status.Conditions, corev1.PodCondition{
			Type:               corev1.PodInitialized,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "Initialized",
			Message:            "all init containers in the pod have started successfully",
		})

		pod.Status.Message = "Waiting"
		pod.Status.Reason = "ContainerCreating"
	}

	/*---------------------------------------------------
	 * Classify container statuses
	 *---------------------------------------------------*/
	var state Classifier
	state.Reset()

	for i, containerStatus := range pod.Status.ContainerStatuses {
		state.Classify(containerStatus.Name, &pod.Status.ContainerStatuses[i])
	}

	totalJobs := len(pod.Spec.Containers)

	/*---------------------------------------------------
	 * Define Expected Lifecycle Transitions
	 *---------------------------------------------------*/
	type transition struct {
		expression bool
		change     func(status *corev1.PodStatus)
	}

	phaseTransitionSequence := []transition{
		{ /*-- FAILED: at least one job has failed and will not be restarted --*/
			expression: state.NumFailedJobs() > 0 && pod.Spec.RestartPolicy == corev1.RestartPolicyNever,
			change: func(status *corev1.PodStatus) {
				status.Phase = corev1.PodFailed
				status.Reason = "ContainerFailed"
				status.Message = fmt.Sprintf("Failed containers: %s", state.ListFailedJobs())

				setTerminationConditions(pod)
			},
		},

		{ /*-- SUCCESS: all jobs are successfully completed --*/
			expression: pod.Spec.RestartPolicy != corev1.RestartPolicyAlways && state.NumSuccessfulJobs() == totalJobs,
			change: func(status *corev1.PodStatus) {
				status.Phase = corev1.PodSucceeded
				status.Reason = "Completed"
				status.Message = fmt.Sprintf("Success containers: %s", state.ListSuccessfulJobs())

				setTerminationConditions(pod)
			},
		},

		{ /*-- RUNNING: jobs are running or restarting --*/
			expression: (state.NumRunningJobs() > 0) || (pod.Spec.RestartPolicy == corev1.RestartPolicyAlways) || (pod.Spec.RestartPolicy == corev1.RestartPolicyOnFailure && state.NumSuccessfulJobs() < totalJobs),
			change: func(status *corev1.PodStatus) {
				status.Phase = corev1.PodRunning
				status.Reason = "Running"
				status.Message = "at least one container is still running"
				allReady := true
				for _, containerStatus := range status.ContainerStatuses {
					if !containerStatus.Ready {
						allReady = false
						break
					}
				}
				conditionStatus := corev1.ConditionFalse
				reason := "ContainersUnready"
				message := "at least one container is not ready"
				if allReady && len(status.ContainerStatuses) > 0 {
					conditionStatus = corev1.ConditionTrue
					reason = "ContainersReady"
					message = "all containers are ready"
				}

				crdtools.SetPodStatusCondition(&pod.Status.Conditions, corev1.PodCondition{
					Type:               corev1.ContainersReady,
					Status:             conditionStatus,
					LastTransitionTime: metav1.Now(),
					Reason:             reason,
					Message:            message,
				})

				crdtools.SetPodStatusCondition(&pod.Status.Conditions, corev1.PodCondition{
					Type:               corev1.PodReady,
					Status:             conditionStatus,
					LastTransitionTime: metav1.Now(),
					Reason:             reason,
					Message:            message,
				})
			},
		},

		{ /*-- PENDING: some jobs are not yet created or starting --*/
			expression: state.NumPendingJobs() > 0 || totalJobs == 0 || (state.NumRunningJobs() == 0 && state.NumSuccessfulJobs() == 0 && state.NumFailedJobs() == 0),
			change: func(status *corev1.PodStatus) {
				status.Phase = corev1.PodPending
				status.Reason = "InQueue"
				status.Message = fmt.Sprintf("PendingJobs: %s", state.ListPendingJobs())
			},
		},

		{ /*-- FAILED: invalid state transition --*/
			expression: true,
			change: func(status *corev1.PodStatus) {
				status.Phase = corev1.PodFailed
				status.Reason = "PodFailure"
				status.Message = fmt.Sprintf("Invalid container state transition (pending: %d, running: %d, succeeded: %d, failed: %d)", state.NumPendingJobs(), state.NumRunningJobs(), state.NumSuccessfulJobs(), state.NumFailedJobs())
			},
		},
	}

	/*---------------------------------------------------
	 * Check for Expected Lifecycle Transitions
	 *---------------------------------------------------*/
	for _, testcase := range phaseTransitionSequence {
		if testcase.expression {
			testcase.change(&pod.Status)

			logger.Info(" * Pod Status has been updated",
				"phase", pod.Status.Phase,
				"completed", state.ListSuccessfulJobs(),
				"failed", state.ListFailedJobs(),
			)

			return
		}
	}

	compute.PodError(pod, "StatusError", "unhandled lifecycle conditions. current: '%v', totalJobs: '%d', jobs: '%s'", pod.Status.Phase, totalJobs, state.ListAll())
}

func setTerminationConditions(pod *corev1.Pod) {
	crdtools.SetPodStatusCondition(&pod.Status.Conditions, corev1.PodCondition{
		Type:               corev1.ContainersReady,
		Status:             corev1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "ContainersUnready",
		Message:            "Pod Has been Successfully Terminated.",
	})

	crdtools.SetPodStatusCondition(&pod.Status.Conditions, corev1.PodCondition{
		Type:               corev1.PodReady,
		Status:             corev1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "PodUnready",
		Message:            "Pod Has been Successfully Terminated.",
	})
}

// HumanReadableCode translates the exit code into a human-readable form.
func HumanReadableCode(code int) string {
	switch code {
	case 0:
		return "Container exited"
	case 1:
		return "Application error"
	case 125:
		return "Container failed to run error"
	case 126:
		return "Command invoke error"
	case 127:
		return "File or directory not found"
	case 128:
		return "Invalid argument used on exit"
	case 134:
		return "Abnormal termination (SIGABRT)"
	case 137:
		return "Immediate termination (SIGKILL)"
	case 139:
		return "Segmentation fault (SIGSEGV)"
	case 143:
		return "Graceful termination (SIGTERM)"
	case 255:
		return "Exit Status Out Of Range"
	default:
		return "Unknown Exit Code"
	}
}

/*************************************************************

				Pod Lifecycle Classifier

*************************************************************/

// Classifier splits jobs into Pending, Running, Successful, and Failed.
type Classifier struct {
	pendingJobs    map[string]*corev1.ContainerStatus
	runningJobs    map[string]*corev1.ContainerStatus
	successfulJobs map[string]*corev1.ContainerStatus
	failedJobs     map[string]*corev1.ContainerStatus
}

func (in *Classifier) Reset() {
	in.pendingJobs = make(map[string]*corev1.ContainerStatus)
	in.runningJobs = make(map[string]*corev1.ContainerStatus)
	in.successfulJobs = make(map[string]*corev1.ContainerStatus)
	in.failedJobs = make(map[string]*corev1.ContainerStatus)
}

func (in *Classifier) Classify(name string, status *corev1.ContainerStatus) {
	switch {
	case status.State.Terminated != nil:
		if status.State.Terminated.ExitCode == 0 {
			in.successfulJobs[name] = status
		} else {
			in.failedJobs[name] = status
		}
	case status.State.Running != nil:
		in.runningJobs[name] = status
	case status.State.Waiting != nil:
		in.pendingJobs[name] = status
	default:
		in.pendingJobs[name] = status
	}
}

func (in *Classifier) NumPendingJobs() int {
	return len(in.pendingJobs)
}

func (in *Classifier) NumRunningJobs() int {
	return len(in.runningJobs)
}

func (in *Classifier) NumSuccessfulJobs() int {
	return len(in.successfulJobs)
}

func (in *Classifier) NumFailedJobs() int {
	return len(in.failedJobs)
}

func (in *Classifier) ListPendingJobs() []string {
	list := make([]string, 0, len(in.pendingJobs))
	for jobName := range in.pendingJobs {
		list = append(list, jobName)
	}
	sort.Strings(list)
	return list
}

func (in *Classifier) ListSuccessfulJobs() []string {
	list := make([]string, 0, len(in.successfulJobs))
	for jobName := range in.successfulJobs {
		list = append(list, jobName)
	}
	sort.Strings(list)
	return list
}

func (in *Classifier) ListFailedJobs() []string {
	list := make([]string, 0, len(in.failedJobs))
	for jobName := range in.failedJobs {
		list = append(list, jobName)
	}
	sort.Strings(list)
	return list
}

func (in *Classifier) ListAll() string {
	return fmt.Sprint(
		"\n * Pending:", in.ListPendingJobs(),
		"\n * Success:", in.ListSuccessfulJobs(),
		"\n * Failed:", in.ListFailedJobs(),
		"\n",
	)
}
