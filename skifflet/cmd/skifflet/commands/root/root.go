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
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"skifflet/cmd/skifflet/commands"
	"skifflet/internal/compute"

	"github.com/hashicorp/go-multierror"
	"github.com/sirupsen/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	vklog "github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/log/klogv2"
	"golang.org/x/time/rate"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"skifflet/internal/provider"

	"errors"

	"github.com/dimiro1/banner"
	"github.com/spf13/cobra"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func logo() string {
	buf := bytes.NewBuffer(nil)

	banner.InitString(buf, true, true, `
{{ .AnsiColor.BrightGreen }}
{{ .Title "Skifflet" "" 4 }}
{{ .AnsiColor.Default }}
	`)

	return buf.String()
}

// NewCommand creates a new top-level command.
// This command is used to start the virtual-kubelet daemon
func NewCommand(ctx context.Context, name string, c Opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   name,
		Short: name + " run skifflet",
		Long:  name + ` run a kubelet-alike daemon that allows to schedule kubernetes workloads on HPC nodes`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(logo())

			var merr *multierror.Error

			/*---------------------------------------------------
			 * Sanitize Input Params
			 *---------------------------------------------------*/
			if c.PodSyncWorkers == 0 {
				merr = multierror.Append(merr, errors.New("pod sync workers must be greater than 0"))
			}

			if c.KubeletAddress == "" {
				merr = multierror.Append(merr, fmt.Errorf("empty kubelet address. Use flags or set %s", EnvKubeletAddress))
			}

			if c.K8sAPICertFilepath == "" {
				merr = multierror.Append(merr, fmt.Errorf("empty certificate path. Use flags or set %s", EnvAPICertLocation))
			}

			if c.K8sAPIKeyFilepath == "" {
				merr = multierror.Append(merr, fmt.Errorf("empty key path. Use flags or set %s", EnvAPIKeyLocation))
			}

			if err := validateClusterDomain(c.DefaultHostEnvironment.ClusterDomainOrDefault()); err != nil {
				merr = multierror.Append(merr, err)
			}

			if merr.ErrorOrNil() != nil {
				return merr.ErrorOrNil()
			}

			/*---------------------------------------------------
			 * Start the execution
			 *---------------------------------------------------*/
			return runRootCommand(ctx, c)
		},
	}

	installFlags(cmd.Flags(), &c)

	return cmd
}

var DefaultLogger = zap.New(zap.UseDevMode(true))

// https://medium.com/microsoftazure/virtual-kubelet-turns-1-0-deep-dive-b64056061b18
func runRootCommand(ctx context.Context, c Opts) error {
	/*--
	Don't ask .... Just believe that this is the only way to dump logs from the virtual kubelet to the terminal
	*/
	vklog.L = klogv2.New(vklog.Fields{})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			DefaultLogger.Error(fmt.Errorf("%v", r), "Recovered from panic in runRootCommand")
		}
	}()

	DefaultLogger.Info("Starting runRootCommand...")

	/*---------------------------------------------------
	 * Setup a client to Kubernetes API
	 *---------------------------------------------------*/
	restConfig, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("unable to get kubeconfig: %w", err)
	}

	{
		k8sclientset, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return fmt.Errorf("unable to start kubernetes k8sclientset: %w", err)
		}

		k8sclient, err := client.New(restConfig, client.Options{})
		if err != nil {
			return fmt.Errorf("unable to start kubernetes client: %w", err)
		}

		compute.K8SClient = k8sclient
		compute.K8SClientset = k8sclientset

		compute.Environment = c.DefaultHostEnvironment
		compute.Environment.PauseImage = c.PauseImage

		kubemaster, err := url.Parse(restConfig.Host)
		if err != nil {
			return fmt.Errorf("failed to extract hostname from url '%s': %w", restConfig.Host, err)
		}
		compute.Environment.KubeMasterHost = kubemaster.Hostname()
		compute.Environment.KubeMasterPort = kubemaster.Port()
		if compute.Environment.KubeMasterPort == "" {
			compute.Environment.KubeMasterPort = "443"
		}

		DefaultLogger.Info("KubeClient is ready",
			"Address", restConfig.Host,
			"ContainerRegistry", compute.Environment.ContainerRegistry,
		)
	}

	/*---------------------------------------------------
	 * Discover Kubernetes DNS server
	 *---------------------------------------------------*/
	{
		DefaultLogger.Info("Discovering KubeDNS Service...")

		// In a self-hosted setup (where this Kubelet runs CoreDNS), we must use the
		// Service IP (ClusterIP) rather than waiting for Endpoints.
		// Endpoints require the Pod to be running, which requires this Kubelet to be Ready.
		svc, err := compute.K8SClientset.CoreV1().Services("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{})
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("unable to discover kube-dns service: %w", err)
		}

		if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
			return fmt.Errorf("kube-dns service has no ClusterIP")
		}

		compute.Environment.KubeDNS = svc.Spec.ClusterIP

		DefaultLogger.Info("KubeDNS client is ready",
			"address", compute.Environment.KubeDNS,
		)
	}

	/*---------------------------------------------------
	 * Register the Provisioner of Virtual Nodes
	 *---------------------------------------------------*/
	virtualk8s, err := provider.NewVirtualK8S(provider.InitConfig{
		NodeName:     c.NodeName,
		InternalIP:   c.KubeletAddress,
		DaemonPort:   c.KubeletPort,
		BuildVersion: commands.BuildVersion,
		RestConfig:   restConfig,
		PauseImage:   c.PauseImage,
	})
	if err != nil {
		return err
	}

	StartAPIServer(c, virtualk8s)

	// Register before pod execution so the controller can allocate a PodCIDR.
	// The node stays NotReady until Plaid has attached the bubble gateway.
	earlyNode := virtualk8s.NewVirtualNode(ctx, c.NodeName, nil)
	setNodeNotReady(earlyNode)
	if _, err := compute.K8SClientset.CoreV1().Nodes().Create(ctx, earlyNode, metav1.CreateOptions{}); err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("register node before networking: %w", err)
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := compute.K8SClientset.CoreV1().Nodes().Get(ctx, c.NodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		setNodeNotReady(current)
		_, err = compute.K8SClientset.CoreV1().Nodes().UpdateStatus(ctx, current, metav1.UpdateOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("set initial node network condition: %w", err)
	}

	DefaultLogger.Info("Virtual Node Provisioner is ready",
		"Address", virtualk8s.InternalIP,
		"DaemonPort", virtualk8s.DaemonPort,
	)

	/*---------------------------------------------------
	 * Create Informers for CRDs and Pod Controller
	 *---------------------------------------------------*/
	{
		podInformer, secretInformer, configMapInformer, serviceInformer, _, err := AddInformers(ctx, c, compute.K8SClientset)
		if err != nil {
			return fmt.Errorf("failed to add informers: %w", err)
		}
		virtualk8s.SetAssignedPodLister(podInformer.Lister())

		DefaultLogger.Info("Informers are ready",
			"crds", []string{
				"pods", "secrets", "configMap", "service", "serviceAccount",
			})

		eb := record.NewBroadcaster()
		eb.StartLogging(logrus.Infof)
		// Events reach the API server, where `kubectl describe` shows them. The
		// runtime's own (failed hooks and probes, kills) come from "kubelet" on this
		// node, as they would from a real one.
		eb.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: compute.K8SClientset.CoreV1().Events("")})
		compute.Events = eb.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "kubelet", Host: c.NodeName})

		pc, err := node.NewPodController(node.PodControllerConfig{
			PodClient:                            compute.K8SClientset.CoreV1(),
			PodInformer:                          podInformer,
			EventRecorder:                        provider.NewUpdateEventFilter(eb.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "skifflet-controller"}), virtualk8s),
			Provider:                             virtualk8s,
			ConfigMapInformer:                    configMapInformer,
			SecretInformer:                       secretInformer,
			ServiceInformer:                      serviceInformer,
			SyncPodsFromKubernetesRateLimiter:    rateLimiter(),
			DeletePodsFromKubernetesRateLimiter:  rateLimiter(),
			SyncPodStatusFromProviderRateLimiter: rateLimiter(),
			SkipDownwardAPIResolution:            true,
		})
		if err != nil {
			return err
		}

		// Start the Pod controller.
		go func() {
			if err := pc.Run(ctx, c.PodSyncWorkers); err != nil && !errors.Is(err, context.Canceled) {
				DefaultLogger.Error(err, "Pod Controller Has failed")
				// handle error
			}
		}()

		// If there is a startup timeout, it does two things:
		// 1. It causes the VK to shut down if we haven't gotten into an operational state in a time period
		// 2. It prevents node advertisement from happening until we're in an operational state
		if c.StartupTimeout > 0 {
			pcCtx, pcCancel := context.WithTimeout(ctx, c.StartupTimeout)
			select {
			case <-pcCtx.Done():
				DefaultLogger.Info("... Aborted before initialization is complete ....")

				pcCancel()
				return pcCtx.Err()
			case <-pc.Ready():
			}
			pcCancel()
			if err := pc.Err(); err != nil {
				return err
			}
		}

		DefaultLogger.Info("Pod Controller is Ready")
	}

	/*---------------------------------------------------
	 * Create Node Controller
	 *---------------------------------------------------*/
	{
		np := node.NewNaiveNodeProvider()

		var taint *corev1.Taint
		if !c.DisableTaint {
			taint, err = getTaint(c)
			if err != nil {
				return err
			}
		}

		virtualNode := virtualk8s.NewVirtualNode(ctx, c.NodeName, taint)
		setNodeNotReady(virtualNode)
		if err := waitForPlaid(ctx, 3*time.Minute); err != nil {
			return err
		}

		nc, err := node.NewNodeController(
			np,
			virtualNode,
			compute.K8SClientset.CoreV1().Nodes(),
			node.WithNodeEnableLeaseV1(compute.K8SClientset.CoordinationV1().Leases(corev1.NamespaceNodeLease), 0),
			node.WithNodeStatusUpdateInterval(10*time.Second),
			node.WithNodeStatusUpdateErrorHandler(func(ctx context.Context, err error) error {
				if !k8serrors.IsNotFound(err) {
					return err
				}

				DefaultLogger.Info("node not found")
				newNode := virtualNode.DeepCopy()
				newNode.ResourceVersion = ""

				if _, err = compute.K8SClientset.CoreV1().Nodes().Create(ctx, newNode, metav1.CreateOptions{}); err != nil {
					return err
				}

				DefaultLogger.Info("created new node")
				return nil
			}),
		)
		if err != nil {
			return err
		}

		// Start the Node controller.
		go func() {
			if err := nc.Run(ctx); err != nil && err != context.Canceled {
				DefaultLogger.Error(err, "NodeController has failed")

				// handle error
			}
		}()

		// Wait for node controller to become ready
		<-nc.Ready()

		// If we got here, set Node condition Ready.
		setNodeReady(virtualNode)
		if err := np.UpdateStatus(ctx, virtualNode); err != nil {
			return fmt.Errorf("error marking the node as ready: %w", err)
		}

		DefaultLogger.Info("Node Controller is Ready")

		// Monitor Plaid daemon liveness and update Node condition
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			wasReady := true
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					conn, err := net.DialTimeout("unix", compute.Environment.PlaidSocketPath(), time.Second)
					isPlaidAlive := err == nil
					if conn != nil {
						_ = conn.Close()
					}

					if !isPlaidAlive && wasReady {
						wasReady = false
						setNodeNotReady(virtualNode)
						_ = np.UpdateStatus(ctx, virtualNode)
						DefaultLogger.Info("Plaid daemon unreachable, marked node NotReady")
					} else if isPlaidAlive && !wasReady {
						wasReady = true
						setNodeReady(virtualNode)
						_ = np.UpdateStatus(ctx, virtualNode)
						DefaultLogger.Info("Plaid daemon reachable, marked node Ready")
					}
				}
			}
		}()

		DefaultLogger.Info("... Skifflet is successfully initialized and waiting for jobs....")

		// wait for as long the app is running
		<-nc.Done()

		DefaultLogger.Info("... Skifflet has been gracefully terminated ....")
	}

	return nil
}

func rateLimiter() workqueue.RateLimiter {
	return workqueue.NewMaxOfRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(5*time.Millisecond, 10*time.Second),
		// 100 qps, 1000 bucket size.  This is only for retry speed, and it's only the overall factor (not per item)
		&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(100), 1000)},
	)
}

func setNodeReady(n *corev1.Node) {
	for i, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}

		c.LastHeartbeatTime = metav1.Now()
		c.LastTransitionTime = metav1.Now()
		c.Reason = "KubeletReady"
		c.Message = "Skifflet is ready"
		c.Status = corev1.ConditionTrue
		n.Status.Conditions[i] = c
		return
	}
}

func setNodeNotReady(n *corev1.Node) {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == corev1.NodeReady {
			n.Status.Conditions[i].Status = corev1.ConditionFalse
			n.Status.Conditions[i].Reason = "NetworkPending"
			n.Status.Conditions[i].Message = "Waiting for Plaid gateway"
			return
		}
	}
}

func waitForPlaid(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		conn, err := net.DialTimeout("unix", compute.Environment.PlaidSocketPath(), time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for Plaid gateway: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// getTaint creates a taint using the provided key/value.
// Taint effect is read from the environment
// The taint key/value may be overwritten by the environment.
func getTaint(o Opts) (*corev1.Taint, error) {
	var effect corev1.TaintEffect
	switch o.TaintEffect {
	case "NoSchedule":
		effect = corev1.TaintEffectNoSchedule
	case "NoExecute":
		effect = corev1.TaintEffectNoExecute
	case "PreferNoSchedule":
		effect = corev1.TaintEffectPreferNoSchedule
	default:
		return nil, errdefs.InvalidInputf("taint effect %q is not supported", o.TaintEffect)
	}

	return &corev1.Taint{
		Key:    o.TaintKey,
		Value:  o.TaintValue,
		Effect: effect,
	}, nil
}

func GetUserHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		DefaultLogger.Info("Failed to infer UserHome", "err", err)
	}

	return home
}
