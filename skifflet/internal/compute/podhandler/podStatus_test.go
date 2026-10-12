package podhandler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodStatusTransitions_RestartPolicyNever(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{Name: "c1"},
				{Name: "c2"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "c1",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
				},
				{
					Name:  "c2",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
				},
			},
		},
	}

	// 1. Both running -> Running
	UpdateStatusFromRuntime(pod)
	if pod.Status.Phase != corev1.PodRunning {
		t.Fatalf("expected PodRunning, got %s", pod.Status.Phase)
	}

	// 2. c1 terminates with exit code 1 (failure), but c2 is still running -> MUST remain Running!
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
	}
	UpdateStatusFromRuntime(pod)
	if pod.Status.Phase != corev1.PodRunning {
		t.Fatalf("expected PodRunning while c2 is still running, got %s", pod.Status.Phase)
	}

	// 3. c2 terminates with exit code 0 -> Now both completed, c1 failed -> PodFailed
	pod.Status.ContainerStatuses[1].State = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
	}
	UpdateStatusFromRuntime(pod)
	if pod.Status.Phase != corev1.PodFailed {
		t.Fatalf("expected PodFailed after all containers finished, got %s", pod.Status.Phase)
	}
}

func TestPodStatusTransitions_RestartPolicyAlways_PendingInitially(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
					},
					RestartCount: 0,
				},
			},
		},
	}

	// Initial waiting state with RestartPolicy Always must be Pending, NOT Running!
	UpdateStatusFromRuntime(pod)
	if pod.Status.Phase != corev1.PodPending {
		t.Fatalf("expected initial state to be PodPending, got %s", pod.Status.Phase)
	}

	// Once container starts running -> Running
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()},
	}
	UpdateStatusFromRuntime(pod)
	if pod.Status.Phase != corev1.PodRunning {
		t.Fatalf("expected running state to be PodRunning, got %s", pod.Status.Phase)
	}
}

func TestPodStatusTransitions_SuccessAllContainers(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{Name: "c1"},
				{Name: "c2"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "c1",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
				},
				{
					Name:  "c2",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
				},
			},
		},
	}

	UpdateStatusFromRuntime(pod)
	if pod.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("expected PodSucceeded when all containers exit 0, got %s", pod.Status.Phase)
	}
}

// A failed init container fails the pod only under restartPolicy Never; otherwise it is
// retried and the pod stays Pending, not initialized, as with the kubelet.
func TestPodStatus_InitFailureDependsOnRestartPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy corev1.RestartPolicy
		phase  corev1.PodPhase
	}{
		{corev1.RestartPolicyNever, corev1.PodFailed},
		{corev1.RestartPolicyOnFailure, corev1.PodPending},
		{corev1.RestartPolicyAlways, corev1.PodPending},
	} {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				RestartPolicy:  tc.policy,
				InitContainers: []corev1.Container{{Name: "init"}},
				Containers:     []corev1.Container{{Name: "app"}},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				InitContainerStatuses: []corev1.ContainerStatus{{Name: "init",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}}}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "app",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}},
			},
		}
		UpdateStatusFromRuntime(pod)
		if pod.Status.Phase != tc.phase {
			t.Fatalf("%s: phase %s, want %s", tc.policy, pod.Status.Phase, tc.phase)
		}
		if tc.phase == corev1.PodPending {
			var initialized *corev1.PodCondition
			for i := range pod.Status.Conditions {
				if pod.Status.Conditions[i].Type == corev1.PodInitialized {
					initialized = &pod.Status.Conditions[i]
				}
			}
			if initialized == nil || initialized.Status != corev1.ConditionFalse {
				t.Fatalf("%s: the pod is not reported uninitialized: %+v", tc.policy, pod.Status.Conditions)
			}
		}
	}
}

// A terminal phase is final whatever the restart policy: a pod that failed as a whole
// does not come back to Running because one of its containers still runs.
func TestPodStatus_TerminalPhaseIsFinal(t *testing.T) {
	for _, policy := range []corev1.RestartPolicy{corev1.RestartPolicyAlways, corev1.RestartPolicyOnFailure, corev1.RestartPolicyNever} {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{RestartPolicy: policy, Containers: []corev1.Container{{Name: "a"}, {Name: "b"}}},
			Status: corev1.PodStatus{
				Phase:  corev1.PodFailed,
				Reason: "LogFileError",
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "a", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128}}},
					{Name: "b", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}},
				},
			},
		}
		UpdateStatusFromRuntime(pod)
		if pod.Status.Phase != corev1.PodFailed || pod.Status.Reason != "LogFileError" {
			t.Fatalf("%s: phase %s reason %q, want Failed/LogFileError", policy, pod.Status.Phase, pod.Status.Reason)
		}
	}
}
