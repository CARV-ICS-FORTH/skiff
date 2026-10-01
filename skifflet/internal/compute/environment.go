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
	// TmpDirectory is the node's scratch root (see endpoint.SkiffPath): the pods
	// directory and Apptainer's scratch are below it.
	TmpDirectory string

	// KubeDNS points to the internal DNS of a Kubernetes cluster.
	KubeDNS string

	// PauseImage is the image used for the pause container.
	PauseImage string

	// PlaidSocket is the API socket of plaidd; empty means endpoint.DefaultPlaidSocket.
	PlaidSocket string

	// ClusterDomain is the cluster's DNS domain, the tail of the search path of a
	// pod that uses the cluster DNS; empty means DefaultClusterDomain.
	ClusterDomain string
}

// DefaultClusterDomain is the domain Kubernetes gives a cluster unless told otherwise.
const DefaultClusterDomain = "cluster.local"

// ClusterDomainOrDefault is the cluster domain in effect.
func (e HostEnvironment) ClusterDomainOrDefault() string {
	if e.ClusterDomain == "" {
		return DefaultClusterDomain
	}
	return e.ClusterDomain
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
