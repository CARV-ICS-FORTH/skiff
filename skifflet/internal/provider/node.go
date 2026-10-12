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

package provider

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"skifflet/internal/compute/gpu"
	computeruntime "skifflet/internal/compute/runtime"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewVirtualNode builds a kubernetes node object from a provider
// This is a temporary solution until node stuff actually split off from the provider interface itself.
func (v *VirtualK8S) NewVirtualNode(ctx context.Context, nodename string, taint *corev1.Taint) *corev1.Node {
	taints := make([]corev1.Taint, 0)

	if taint != nil {
		taints = append(taints, *taint)
	}

	labels := map[string]string{
		"kubernetes.io/hostname": nodename,
		"kubernetes.io/role":     "agent",
		"kubernetes.io/os":       runtime.GOOS,
		"kubernetes.io/arch":     runtime.GOARCH,
	}
	// The labels a node with GPUs has (gpu.present, gpu.count, gpu.product ...), for pods that select it.
	for k, v := range gpu.Node().Labels() {
		labels[k] = v
	}

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodename,
			Labels: labels,
		},
		Spec: corev1.NodeSpec{
			Taints: taints,
		},
		Status: corev1.NodeStatus{
			NodeInfo:        v.NodeSystemInfo(ctx),
			Addresses:       v.NodeAddresses(ctx),
			DaemonEndpoints: v.NodeDaemonEndpoints(ctx),
			Conditions:      NodeConditions(ctx),
			Phase: func() corev1.NodePhase {
				if computeruntime.ConnectionOK() {
					return corev1.NodeRunning
				}
				return corev1.NodePending
			}(),
			Capacity:    computeruntime.TotalResources(),
			Allocatable: computeruntime.AllocatableResources(ctx),
		},
	}
}

// NodeConditions creates a slice of node conditions representing a
// kubelet in perfect health. These four conditions are the ones which virtual-kubelet
// sets as Unknown when a Ping fails.
func NodeConditions(_ context.Context) []corev1.NodeCondition {
	// NodeConditions returns a list of conditions (Ready, OutOfDisk, etc), for updates to the node status
	// within Kubernetes.
	// TODO: Make this configurable
	return []corev1.NodeCondition{
		{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletPending",
			Message:            "kubelet is pending.",
		},
		{
			Type:               corev1.NodeMemoryPressure,
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientMemory",
			Message:            "kubelet has sufficient memory available",
		},
		{
			Type:               corev1.NodeDiskPressure,
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoDiskPressure",
			Message:            "kubelet has no disk pressure",
		},
		{
			Type:               corev1.NodePIDPressure,
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoPIDPressure",
			Message:            "kubelet has no PID pressure",
		},
		{
			Type:               corev1.NodeNetworkUnavailable,
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "RouteCreated",
			Message:            "RouteController created a route",
		},
	}
}

func (v *VirtualK8S) NodeAddresses(_ context.Context) []corev1.NodeAddress {
	return []corev1.NodeAddress{
		{
			// Used to server webhook traffic for logs / exec
			Type:    corev1.NodeExternalIP,
			Address: v.InitConfig.InternalIP,
		},
		{
			Type:    corev1.NodeInternalIP,
			Address: v.InitConfig.InternalIP,
		},
	}
}

func (v *VirtualK8S) NodeDaemonEndpoints(_ context.Context) corev1.NodeDaemonEndpoints {
	v.Logger.Info("[K8s] -> NodeDaemonEndpoints")
	defer v.Logger.Info("[K8s] <- NodeDaemonEndpoints")

	return corev1.NodeDaemonEndpoints{
		KubeletEndpoint: corev1.DaemonEndpoint{
			Port: v.InitConfig.DaemonPort,
		},
	}
}

// NodeSystemInfo is what the node says about itself, as the kubelet says it: the machine and
// boot ids, the kernel, and the container runtime, which is the Apptainer Skifflet runs (read
// once). There is no kube-proxy (Plaid translates Services), and the kubelet leaves the
// deprecated KubeProxyVersion empty anyway.
func (v *VirtualK8S) NodeSystemInfo(_ context.Context) corev1.NodeSystemInfo {
	kernelVersion := "unknown"
	if out, err := exec.Command("uname", "-r").Output(); err == nil {
		kernelVersion = strings.TrimSpace(string(out))
	}

	return corev1.NodeSystemInfo{
		MachineID:               readTrimmed("/etc/machine-id"),
		BootID:                  readTrimmed("/proc/sys/kernel/random/boot_id"),
		KernelVersion:           kernelVersion,
		OSImage:                 "skiff",
		KubeletVersion:          v.InitConfig.BuildVersion,
		ContainerRuntimeVersion: containerRuntimeVersion(),
		OperatingSystem:         runtime.GOOS,
		Architecture:            runtime.GOARCH,
	}
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// apptainerVersion runs `apptainer version`; tests replace it.
var apptainerVersion = func() (string, error) {
	out, err := exec.Command("apptainer", "version").Output()
	return strings.TrimSpace(string(out)), err
}

var runtimeVersionOnce = sync.OnceValue(func() string {
	if v, err := apptainerVersion(); err == nil && v != "" {
		return "apptainer://" + v
	}
	return "apptainer://unknown"
})

// containerRuntimeVersion is the container runtime as the node reports it ("apptainer://1.5.4").
func containerRuntimeVersion() string { return runtimeVersionOnce() }
