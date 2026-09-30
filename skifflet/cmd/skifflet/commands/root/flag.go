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

package root

import (
	"fmt"
	"os"
	"strings"
	"time"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"

	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Opts stores all the options for configuring the root virtual-kubelet command.
// It is used for setting flag values.
//
// You can set the default options by creating a new `Opts` struct and passing
// it into `SetDefaultOpts`
type Opts struct {
	DefaultHostEnvironment compute.HostEnvironment

	// KubeletAddress determines which address to tell API Server to use.
	// Node listens on all of its IP addresses on port 10250 and advertises the value specified in KubeletAddress to other nodes.
	KubeletAddress string

	// KubeletPorts determines the port to listen for requests from the Kubernetes API server.
	KubeletPort int32

	// MetricsPort int32

	K8sAPICertFilepath string
	K8sAPIKeyFilepath  string

	// Node name to use when creating a node in Kubernetes
	NodeName string

	// Number of workers to use to handle pod notifications
	PodSyncWorkers       int
	InformerResyncPeriod time.Duration

	// Startup Timeout is how long to wait for the kubelet to start
	StartupTimeout time.Duration

	DisableTaint bool
	TaintKey     string
	TaintValue   string
	TaintEffect  string

	// PauseImage is the image used for the pause container
	PauseImage string
}

const (
	EnvKubeletAddress  = "VKUBELET_ADDRESS"
	EnvAPICertLocation = "APISERVER_CERT_LOCATION"
	EnvAPIKeyLocation  = "APISERVER_KEY_LOCATION"
	// EnvPlaidSocket is the variable the Plaid tools read the same setting from.
	EnvPlaidSocket = "PLAID_SOCKET_PATH"
	// EnvClusterDomain is where a deployment states the cluster's DNS domain.
	EnvClusterDomain = "SKIFF_CLUSTER_DOMAIN"
)

// validateClusterDomain refuses a cluster domain that is not a DNS name, before
// it is written into every pod's search path.
func validateClusterDomain(domain string) error {
	if errs := validation.IsDNS1123Subdomain(domain); len(errs) > 0 {
		return fmt.Errorf("invalid cluster domain %q: %s", domain, strings.Join(errs, "; "))
	}
	return nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func installFlags(flags *pflag.FlagSet, c *Opts) {
	flags.StringVar(&c.KubeletAddress, "kubelet-addr", os.Getenv(EnvKubeletAddress), "which address to tell API server to use")
	flags.Int32Var(&c.KubeletPort, "kubelet-port", 10250, "port to listen for incoming requests from API server")

	// flags.Int32Var(&c.MetricsPort, "metrics-port", 10255, "address to listen for metrics/stats requests")

	flags.StringVar(&c.K8sAPICertFilepath, "certificate", os.Getenv(EnvAPICertLocation), "location for certificate to the API server")
	flags.StringVar(&c.K8sAPIKeyFilepath, "key", os.Getenv(EnvAPIKeyLocation), "location for key for the API server")

	flags.StringVar(&c.NodeName, "nodename", "skifflet", "kubernetes node name")

	flags.StringVar(&c.DefaultHostEnvironment.ApptainerBin, "apptainer", "plaidtainer", "path to Apptainer bin")
	flags.StringVar(&c.DefaultHostEnvironment.ContainerRegistry, "registry", "docker://", "container registry")
	flags.StringVar(&c.DefaultHostEnvironment.WorkingDirectory, "working-dir", GetUserHomeDir(), "sets up the Skiff working directory")
	flags.StringVar(&c.DefaultHostEnvironment.PodsDirectory, "pods-dir", endpoint.DefaultPodsDir, "sets up the Skiff pods directory")
	flags.StringVar(&c.DefaultHostEnvironment.PlaidSocket, "plaid-socket", envOrDefault(EnvPlaidSocket, endpoint.DefaultPlaidSocket), "API socket of plaidd")

	flags.StringVar(&c.DefaultHostEnvironment.ClusterDomain, "cluster-domain", envOrDefault(EnvClusterDomain, compute.DefaultClusterDomain), "DNS domain of the cluster, for the search path of pods")

	flags.IntVar(&c.PodSyncWorkers, "pod-sync-workers", 1, `set the number of pod synchronization workers`)
	flags.DurationVar(&c.InformerResyncPeriod, "full-resync-period", 0, "how often to perform a full resync of pods between kubernetes and the provider")

	flags.DurationVar(&c.StartupTimeout, "startup-timeout", 30*time.Second, "How long to wait for the virtual-kubelet to start")

	flags.BoolVar(&c.DisableTaint, "disable-taint", false, "disable the virtual-kubelet node taint")
	flags.StringVar(&c.TaintKey, "taint-key", "virtual-kubelet.io/provider", "Set node taint key")
	flags.StringVar(&c.TaintValue, "taint-value", "skiff", "Set node taint value")
	flags.StringVar(&c.TaintEffect, "taint-effect", string(corev1.TaintEffectNoSchedule), "Set node taint effect")

	flags.StringVar(&c.PauseImage, "pause-image", "registry.k8s.io/pause:3.10", "image for the pause container")
}
