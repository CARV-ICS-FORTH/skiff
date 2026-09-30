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

package compute

import (
	"skifflet/internal/compute/endpoint"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// HostEnvironment containers information about the execution environment.
type HostEnvironment struct {
	KubeMasterHost    string
	KubeMasterPort    string
	ContainerRegistry string
	ApptainerBin      string

	WorkingDirectory string
	PodsDirectory    string

	// KubeDNS points to the internal DNS of a Kubernetes cluster.
	KubeDNS string

	// PauseImage is the image used for the pause container.
	PauseImage string

	// PlaidSocket is the API socket of plaidd; empty means endpoint.DefaultPlaidSocket.
	PlaidSocket string
}

// PlaidSocketPath is the API socket of plaidd that every part of Skifflet talks to.
func (e HostEnvironment) PlaidSocketPath() string {
	if e.PlaidSocket == "" {
		return endpoint.DefaultPlaidSocket
	}
	return e.PlaidSocket
}

// Instantiated Types
var (
	DefaultLogger = zap.New(zap.UseDevMode(true))
)

var (
	Environment  HostEnvironment
	K8SClient    client.Client
	K8SClientset *kubernetes.Clientset

	// Events records the events of pods and their containers (failed hooks, failed
	// probes, kills) with the API server. It may be nil, and then nothing is recorded.
	Events record.EventRecorder

	Skiff endpoint.SkiffPath
)
