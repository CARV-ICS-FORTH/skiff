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
