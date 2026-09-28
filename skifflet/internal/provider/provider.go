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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	PodHandler "skifflet/internal/compute/podhandler"
	"skifflet/internal/compute/runtime"
	"skifflet/pkg/container"

	"github.com/go-logr/logr"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	utilexec "k8s.io/utils/exec"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// InitConfig is the config passed to initialize a registered provider.
type InitConfig struct {
	NodeName   string
	InternalIP string
	DaemonPort int32

	BuildVersion string

	RestConfig *rest.Config

	PauseImage string
}

func isPodOwnedByNode(pod *corev1.Pod, ownNode string) bool {
	if ownNode == "" || pod == nil || pod.Spec.NodeName == "" {
		return true
	}
	return pod.Spec.NodeName == ownNode
}

// VirtualK8S implements the virtual-kubelet provider interface and stores pods in memory.
type VirtualK8S struct {
	InitConfig

	Logger logr.Logger

	updatedPod func(*corev1.Pod)
	// dispatchPreparation defaults to a goroutine; tests can join dispatched work.
	dispatchPreparation func(func())

	activeUIDs   sync.Map // map[client.ObjectKey]types.UID
	pods         sync.Map // map[client.ObjectKey]*corev1.Pod
	cancelFuncs  sync.Map // map[client.ObjectKey]context.CancelFunc
	stateMu      sync.Mutex
	transitionMu sync.Mutex
	lock         *InstanceLock
}

// NewVirtualK8S reads a kubeconfig file and sets up a client to interact
// with the execution environment.
func NewVirtualK8S(config InitConfig) (*VirtualK8S, error) {
	logger := zap.New(zap.UseDevMode(true))

	/*---------------------------------------------------
	 * Initialize Skiff Environment
	 *---------------------------------------------------*/
	if err := runtime.Initialize(config.PauseImage); err != nil {
		return nil, fmt.Errorf("Failed to initialize Skiff paths '%s': %w", compute.Skiff.String(), err)
	}

	/*---------------------------------------------------
	 * Acquire Advisory Runtime Instance Lock
	 *---------------------------------------------------*/
	lock, err := AcquireInstanceLock(compute.Skiff.String())
	if err != nil {
		return nil, fmt.Errorf("failed to acquire instance lock: %w", err)
	}

	/*---------------------------------------------------
	 * Reconcile Surviving Workloads from Journal
	 *---------------------------------------------------*/
	if err := ReconcileSurvivingWorkloads(compute.Skiff.String()); err != nil {
		logger.Error(err, "warning: error during surviving workloads reconciliation")
	}

	/*---------------------------------------------------
	 * Clean up any leftover pod directories from prior runs
	 *---------------------------------------------------*/
	_ = compute.Skiff.WalkPodDirectories(func(podpath endpoint.PodPath) error {
		logger.Info("Cleaning up leftover pod directory", "path", podpath)
		pidsPath := filepath.Join(podpath.JobDir(), "pids.json")
		if data, err := os.ReadFile(pidsPath); err == nil {
			var pids struct {
				PausePID            int            `json:"pause_pid"`
				PauseStartTime      uint64         `json:"pause_start_time"`
				ContainerPIDs       []int          `json:"container_pids"`
				ContainerStartTimes map[int]uint64 `json:"container_start_times"`
			}
			if err := json.Unmarshal(data, &pids); err == nil {
				for _, pid := range pids.ContainerPIDs {
					st := pids.ContainerStartTimes[pid]
					if pid > 1 && runtime.VerifyProcessIdentity(pid, st) && !runtime.IsProcessDead(pid) {
						_ = syscall.Kill(-pid, syscall.SIGKILL)
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
				if pids.PausePID > 1 && runtime.VerifyProcessIdentity(pids.PausePID, pids.PauseStartTime) && !runtime.IsProcessDead(pids.PausePID) {
					_ = syscall.Kill(-pids.PausePID, syscall.SIGKILL)
					_ = syscall.Kill(pids.PausePID, syscall.SIGKILL)
				}
			}
		}
		_ = os.RemoveAll(podpath.String())
		namespaceDir := filepath.Dir(podpath.String())
		if empty, _ := endpoint.IsEmpty(namespaceDir); empty {
			_ = os.Remove(namespaceDir)
		}
		return nil
	})

	return &VirtualK8S{
		InitConfig: config,
		Logger:     logger,
		lock:       lock,
	}, nil
}

// CreatePod takes a Kubernetes Pod and deploys it within the provider.
func (v *VirtualK8S) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	v.transitionMu.Lock()
	cleanup, err := v.createPodLocked(ctx, pod)
	v.transitionMu.Unlock()
	if cleanup != nil {
		cleanup()
	}
	return err
}

func (v *VirtualK8S) createPodLocked(ctx context.Context, pod *corev1.Pod) (func(), error) {
	pod = pod.DeepCopy()
	podKey := client.ObjectKeyFromObject(pod)
	logger := v.Logger.WithValues("obj", podKey, "uid", pod.GetUID())

	logger.Info("[K8s] -> CreatePod")

	defer func() {
		logger.Info("[K8s] <- CreatePod")
	}()

	pod.Status.HostIP = v.InitConfig.InternalIP

	// The generation transition owns registration; preparation cannot choose or
	// replace an owner later. Duplicate creates reuse the installed UID.
	if owner := PodHandler.GlobalRegistry.Get(pod.UID); owner != nil {
		owner.UpdateFromAPI(pod)
		if active, ok := v.activeUIDs.Load(podKey); ok && active == pod.UID {
			v.stateMu.Lock()
			v.pods.Store(podKey, owner.GetPod())
			v.stateMu.Unlock()
		}
		return nil, nil
	}
	var cleanup func()
	if val, ok := v.pods.Load(podKey); ok {
		cleanup, _ = v.deletePodLocked(ctx, val.(*corev1.Pod))
	}
	executionPod := pod.DeepCopy()
	notify := func(p *corev1.Pod) { v.saveAndNotifyPod("podhandler", p) }
	owner := PodHandler.NewPodLifecycle(executionPod,
		compute.Skiff.PodWithUID(podKey, pod.UID), notify)
	owner.BeginLaunch()
	PodHandler.GlobalRegistry.Register(owner)
	v.stateMu.Lock()
	v.activeUIDs.Store(podKey, pod.UID)
	v.pods.Store(podKey, pod.DeepCopy())
	v.cancelFuncs.Store(podKey, context.CancelFunc(owner.Revoke))
	v.stateMu.Unlock()

	prepare := func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error(fmt.Errorf("%v", r), "Recovered panic in CreatePod goroutine", "pod", podKey)
				compute.PodError(executionPod, "PanicError", "internal panic occurred during pod creation: %v", r)
				owner.Publish(executionPod)
				v.saveAndNotifyPod("panic", owner.GetPod())
			}
		}()

		PodHandler.PreparePod(owner, executionPod, notify)
	}
	if v.dispatchPreparation != nil {
		v.dispatchPreparation(prepare)
	} else {
		go prepare()
	}

	return cleanup, nil
}

// UpdatePod takes a Kubernetes Pod and updates it within the provider.
func (v *VirtualK8S) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	v.transitionMu.Lock()
	cleanup, err := v.updatePodLocked(ctx, pod)
	v.transitionMu.Unlock()
	if cleanup != nil {
		cleanup()
	}
	return err
}

func (v *VirtualK8S) updatePodLocked(ctx context.Context, pod *corev1.Pod) (func(), error) {
	podKey := client.ObjectKeyFromObject(pod)
	logger := v.Logger.WithValues("obj", podKey, "uid", pod.GetUID())

	logger.Info("[K8s] -> UpdatePod",
		"version", pod.GetResourceVersion(),
		"phase", pod.Status.Phase,
	)

	defer logger.Info("[K8s] <- UpdatePod")

	val, ok := v.pods.Load(podKey)
	if !ok {
		logger.Info("Pod not found in UpdatePod, creating as new pod", "uid", pod.UID)
		return v.createPodLocked(ctx, pod)
	}
	localPod := val.(*corev1.Pod)

	if localPod.UID != pod.UID {
		logger.Info("Pod UID changed in UpdatePod, replacing old pod instance", "oldUID", localPod.UID, "newUID", pod.UID)
		return v.createPodLocked(ctx, pod)
	}

	if pl := PodHandler.GlobalRegistry.Get(pod.GetUID()); pl != nil {
		pl.UpdateFromAPI(pod)
		v.stateMu.Lock()
		v.pods.Store(podKey, pl.GetPod())
		v.stateMu.Unlock()
		return nil, nil
	}

	localVersion, errLocal := strconv.ParseUint(localPod.ResourceVersion, 10, 64)
	newVersion, errNew := strconv.ParseUint(pod.ResourceVersion, 10, 64)

	isOlder := false
	if errLocal == nil && errNew == nil {
		isOlder = localVersion >= newVersion
	} else {
		isOlder = localPod.ResourceVersion == pod.ResourceVersion
	}

	if isOlder {
		logger.Info("Discard update since its ResourceVersion is not newer than local")
		return nil, nil
	}

	if !runtime.HasJobID(pod) {
		logger.Info("Discard update because container is still starting")
		return nil, nil
	}

	v.stateMu.Lock()
	v.pods.Store(podKey, pod.DeepCopy())
	v.stateMu.Unlock()

	return nil, nil
}

// DeletePod takes a Kubernetes Pod and deletes it from the provider.
func (v *VirtualK8S) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	v.transitionMu.Lock()
	cleanup, err := v.deletePodLocked(ctx, pod)
	v.transitionMu.Unlock()
	if cleanup != nil {
		cleanup()
	}
	return err
}

func (v *VirtualK8S) deletePodLocked(ctx context.Context, pod *corev1.Pod) (func(), error) {
	podKey := client.ObjectKeyFromObject(pod)
	logger := v.Logger.WithValues("obj", podKey, "uid", pod.GetUID())

	logger.Info("[K8s] -> DeletePod")

	if !isPodOwnedByNode(pod, v.NodeName) {
		logger.Info("[K8s] <- DeletePod (SKIPPED - pod owned by another node)", "podNode", pod.Spec.NodeName, "ownNode", v.NodeName)
		return nil, nil
	}

	// UID verification: check if this DeletePod call is targeting a superseded UID
	if activeUIDVal, ok := v.activeUIDs.Load(podKey); ok {
		if activeUID, ok := activeUIDVal.(types.UID); ok && activeUID != "" && pod.GetUID() != "" {
			if activeUID != pod.GetUID() {
				logger.Info("[K8s] <- DeletePod (SKIPPED - stale UID deletion)", "activeUID", activeUID, "targetUID", pod.GetUID())
				return nil, nil
			}
		}
	}

	var localPod *corev1.Pod
	pl := PodHandler.GlobalRegistry.Get(pod.GetUID())
	if pl != nil {
		localPod = pl.GetPod()
	} else if val, ok := v.pods.Load(podKey); ok {
		localPod = val.(*corev1.Pod)
	} else {
		localPod = pod
	}

	if !isPodOwnedByNode(localPod, v.NodeName) {
		logger.Info("[K8s] <- DeletePod (SKIPPED - local pod owned by another node)", "podNode", localPod.Spec.NodeName, "ownNode", v.NodeName)
		return nil, nil
	}

	// Double-check UID against localPod if present
	if localPod.GetUID() != "" && pod.GetUID() != "" && localPod.GetUID() != pod.GetUID() {
		logger.Info("[K8s] <- DeletePod (SKIPPED - local pod UID mismatch)", "localUID", localPod.GetUID(), "targetUID", pod.GetUID())
		return nil, nil
	}

	gracePeriod := 30 * time.Second
	if localPod.Spec.TerminationGracePeriodSeconds != nil && *localPod.Spec.TerminationGracePeriodSeconds >= 0 {
		gracePeriod = time.Duration(*localPod.Spec.TerminationGracePeriodSeconds) * time.Second
	}

	v.stateMu.Lock()
	if cancelVal, ok := v.cancelFuncs.LoadAndDelete(podKey); ok {
		cancelVal.(context.CancelFunc)()
	}

	v.activeUIDs.Delete(podKey)
	v.pods.Delete(podKey)
	v.stateMu.Unlock()

	if pl != nil {
		PodHandler.GlobalRegistry.Detach(pl)
	}
	// Waiting for startup/processes or callbacks while holding transitionMu can
	// block the generation that must run to finish that work.
	return func() {
		if pl != nil {
			pl.Terminate(gracePeriod)
			PodHandler.GlobalRegistry.DeleteOwner(pl)
		} else {
			_ = PodHandler.DeletePod(podKey, localPod)
		}
		RemoveJournalRecord(compute.Skiff.String(), localPod.GetUID())
		logger.Info("[K8s] <- DeletePod (SUCCESS)")
	}, nil
}

// GetPod retrieves a pod by name from the provider.
func (v *VirtualK8S) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	podKey := client.ObjectKey{Namespace: namespace, Name: name}
	logger := v.Logger.WithValues("obj", podKey)

	logger.Info("[K8s] -> GetPod")

	val, ok := v.pods.Load(podKey)
	if !ok {
		logger.Info("[K8s] <- GetPod (POD NOT FOUND)")
		return nil, errdefs.NotFoundf("object not found")
	}
	pod := val.(*corev1.Pod)

	logger.Info("[K8s] <- GetPod",
		"version", pod.GetResourceVersion(),
		"phase", pod.Status.Phase,
	)

	return pod.DeepCopy(), nil
}

// GetPodStatus retrieves the status of a pod by name from the provider.
func (v *VirtualK8S) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	podKey := client.ObjectKey{Namespace: namespace, Name: name}
	logger := v.Logger.WithValues("obj", podKey, "job")

	logger.Info("[K8s] -> GetPodStatus")

	val, ok := v.pods.Load(podKey)
	if !ok {
		logger.Info("[K8s] <- GetPodStatus (POD NOT FOUND)")
		return nil, errdefs.NotFoundf("object not found")
	}
	pod := val.(*corev1.Pod)

	logger.Info("[K8s] <- GetPodStatus",
		"version", pod.GetResourceVersion(),
		"phase", pod.Status.Phase,
	)

	return pod.Status.DeepCopy(), nil
}

// GetPods retrieves a list of all pods running on the provider.
func (v *VirtualK8S) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	v.Logger.Info("[K8s] -> GetPods")
	defer v.Logger.Info("[K8s] <- GetPods")

	var pods []*corev1.Pod
	v.pods.Range(func(key, val interface{}) bool {
		pod := val.(*corev1.Pod).DeepCopy()
		if !isPodOwnedByNode(pod, v.NodeName) {
			return true
		}

		PodHandler.SyncContainerStatuses(pod)
		PodHandler.UpdateStatusFromRuntime(pod)

		pods = append(pods, pod)
		return true
	})

	return pods, nil
}

// NotifyPods instructs the notifier to call the passed in function when
// the pod status changes.
func (v *VirtualK8S) NotifyPods(ctx context.Context, f func(*corev1.Pod)) {
	v.Logger.Info("[K8s] -> NotifyPods")
	defer v.Logger.Info("[K8s] <- NotifyPods")

	v.stateMu.Lock()
	v.updatedPod = f
	v.stateMu.Unlock()

	/*-- periodic reconcile of non-terminal pods --*/
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				v.reconcileNonTerminalPods()
			}
		}
	}()
}

func (v *VirtualK8S) saveAndNotifyPod(source string, pod *corev1.Pod) {
	v.saveAndNotifyPodExpected(source, pod, nil)
}

func (v *VirtualK8S) saveAndNotifyPodExpected(source string, pod, expected *corev1.Pod) {
	if pod == nil {
		v.Logger.Error(fmt.Errorf("nil pod received in saveAndNotifyPod"), "skipping notification", "source", source)
		return
	}

	podKey := client.ObjectKeyFromObject(pod)
	uid := pod.GetUID()
	v.stateMu.Lock()
	defer v.stateMu.Unlock()
	if expected != nil {
		current, ok := v.pods.Load(podKey)
		if !ok || current != expected {
			return
		}
	}

	// UID validation: discard callbacks from superseded UIDs
	if activeUIDVal, ok := v.activeUIDs.Load(podKey); ok {
		if activeUID, ok := activeUIDVal.(types.UID); ok && activeUID != "" && uid != "" {
			if activeUID != uid {
				v.Logger.Info("Discard callback from superseded pod UID", "key", podKey, "callbackUID", uid, "activeUID", activeUID)
				return
			}
		}
	} else {
		// Fallback for tests or callers populating v.pods directly
		if val, exists := v.pods.Load(podKey); exists {
			existing := val.(*corev1.Pod)
			if existing.GetUID() != "" && uid != "" && existing.GetUID() != uid {
				v.Logger.Info("Discard callback from superseded pod UID", "key", podKey, "callbackUID", uid, "activeUID", existing.GetUID())
				return
			}
		} else {
			// Pod is not in local cache (was deleted); do not resurrect into v.pods
			return
		}
	}

	v.pods.Store(podKey, pod.DeepCopy())

	if v.updatedPod != nil {
		v.updatedPod(pod.DeepCopy())
		v.Logger.Info(" * K8s status is synchronized",
			"source", source,
			"version", pod.ResourceVersion,
			"phase", pod.Status.Phase,
		)
	}
}

func (v *VirtualK8S) reconcileNonTerminalPods() {
	v.pods.Range(func(key, val interface{}) bool {
		podKey := key.(client.ObjectKey)
		pod := val.(*corev1.Pod).DeepCopy()
		if !isPodOwnedByNode(pod, v.NodeName) {
			return true
		}

		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			return true
		}

		// Verify UID is still active for this podKey
		if activeUIDVal, ok := v.activeUIDs.Load(podKey); ok {
			if activeUID, ok := activeUIDVal.(types.UID); ok && activeUID != "" && pod.GetUID() != "" {
				if activeUID != pod.GetUID() {
					return true
				}
			}
		}

		oldStatus := pod.Status.DeepCopy()
		PodHandler.UpdateStatusFromRuntime(pod)

		changed := oldStatus.Phase != pod.Status.Phase ||
			!apiequality.Semantic.DeepEqual(oldStatus.Conditions, pod.Status.Conditions) ||
			!apiequality.Semantic.DeepEqual(oldStatus.ContainerStatuses, pod.Status.ContainerStatuses) ||
			!apiequality.Semantic.DeepEqual(oldStatus.InitContainerStatuses, pod.Status.InitContainerStatuses)

		if changed {
			v.saveAndNotifyPodExpected("reconciler", pod, val.(*corev1.Pod))
		}

		if compute.K8SClientset != nil {
			if apiPod, err := compute.K8SClientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{}); err == nil {
				// Require matching UID before pushing status to API server!
				if apiPod.GetUID() == pod.GetUID() {
					if !apiequality.Semantic.DeepEqual(apiPod.Status.Conditions, pod.Status.Conditions) || apiPod.Status.Phase != pod.Status.Phase {
						apiPodCopy := apiPod.DeepCopy()
						apiPodCopy.Status = *pod.Status.DeepCopy()
						if _, err := compute.K8SClientset.CoreV1().Pods(pod.Namespace).UpdateStatus(context.Background(), apiPodCopy, metav1.UpdateOptions{}); err == nil {
							v.Logger.Info("Reconciled out-of-sync pod status on API server", "pod", key)
						}
					}
				} else {
					v.Logger.Info("Skipping API server status reconcile: pod UID mismatch", "pod", key, "localUID", pod.GetUID(), "apiUID", apiPod.GetUID())
				}
			}
		}
		return true
	})
}

func (v *VirtualK8S) PortForward(ctx context.Context, namespace, pod string, port int32, stream io.ReadWriteCloser) error {
	podKey := client.ObjectKey{Namespace: namespace, Name: pod}
	if stream == nil {
		return fmt.Errorf("port-forward stream is nil")
	}
	value, ok := v.pods.Load(podKey)
	if !ok {
		return errdefs.NotFoundf("pod %s/%s not found", namespace, pod)
	}
	podIP := value.(*corev1.Pod).Status.PodIP
	if net.ParseIP(podIP) == nil || port <= 0 || port > 65535 {
		return fmt.Errorf("pod %s/%s has no valid forwarding target", namespace, pod)
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(podIP, strconv.Itoa(int(port))))
	if err != nil {
		return err
	}
	defer conn.Close()
	defer stream.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	go func() {
		_, _ = io.Copy(conn, stream)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	_, err = io.Copy(stream, conn)
	return err
}

func (v *VirtualK8S) GetStatsSummary(context.Context) (*statsv1alpha1.Summary, error) {
	v.Logger.Info("[K8s] -> GetStatsSummary")
	defer v.Logger.Info("[K8s] <- GetStatsSummary")

	return nil, errors.New("GetStatsSummary is not supported")
}

type followLogReader struct {
	ctx      context.Context
	filePath string
	file     *os.File
	initial  *bytes.Reader
	offset   int64
	closeCh  chan struct{}
	once     sync.Once
	mu       sync.Mutex
}

func newFollowLogReader(ctx context.Context, filePath string, initialBytes []byte, startAtEnd bool) (*followLogReader, error) {
	f, err := os.Open(filePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	var initReader *bytes.Reader
	if len(initialBytes) > 0 {
		initReader = bytes.NewReader(initialBytes)
	}

	var offset int64
	if f != nil && startAtEnd {
		offset, _ = f.Seek(0, io.SeekEnd)
	}

	return &followLogReader{
		ctx:      ctx,
		filePath: filePath,
		file:     f,
		initial:  initReader,
		offset:   offset,
		closeCh:  make(chan struct{}),
	}, nil
}

func (r *followLogReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.closeCh:
		return 0, io.EOF
	default:
	}

	if r.initial != nil && r.initial.Len() > 0 {
		return r.initial.Read(p)
	}

	for {
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-r.closeCh:
			return 0, io.EOF
		default:
		}

		r.mu.Lock()
		if r.file == nil {
			if f, err := os.Open(r.filePath); err == nil {
				r.file = f
				if r.offset > 0 {
					_, _ = r.file.Seek(r.offset, io.SeekStart)
				}
			}
		}
		f := r.file
		r.mu.Unlock()

		if f != nil {
			n, err := f.Read(p)
			if n > 0 {
				r.mu.Lock()
				r.offset += int64(n)
				r.mu.Unlock()
				return n, nil
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return 0, err
			}
		}

		// At EOF or file absent: wait 100ms or until cancelled/closed without holding any mutex
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-r.closeCh:
			return 0, io.EOF
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (r *followLogReader) Close() error {
	var closeErr error
	r.once.Do(func() {
		close(r.closeCh)
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.file != nil {
			closeErr = r.file.Close()
			r.file = nil
		}
	})
	return closeErr
}

// GetContainerLogs retrieves the logs of a container by name from the provider.
func (v *VirtualK8S) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts vkapi.ContainerLogOpts) (io.ReadCloser, error) {
	podKey := client.ObjectKey{Namespace: namespace, Name: podName}
	logger := v.Logger.WithValues("obj", podKey, "container", containerName)

	logger.Info("[K8s] -> GetContainerLogs")
	defer logger.Info("[K8s] <- GetContainerLogs")

	if opts.Previous {
		return nil, errdefs.InvalidInputf("previous terminated container logs are not supported")
	}

	// Validate pod existence
	pl := PodHandler.GlobalRegistry.GetByKey(podKey)
	var pod *corev1.Pod
	if pl != nil {
		pod = pl.GetPod()
	} else if val, ok := v.pods.Load(podKey); ok {
		pod = val.(*corev1.Pod)
	} else {
		return nil, errdefs.NotFoundf("pod %s/%s not found", namespace, podName)
	}

	// Validate container existence in pod spec
	containerFound := false
	for _, c := range pod.Spec.Containers {
		if c.Name == containerName {
			containerFound = true
			break
		}
	}
	if !containerFound {
		for _, c := range pod.Spec.InitContainers {
			if c.Name == containerName {
				containerFound = true
				break
			}
		}
	}
	if !containerFound {
		return nil, errdefs.NotFoundf("container %s not found in pod %s/%s", containerName, namespace, podName)
	}

	var podDir endpoint.PodPath
	if pl != nil {
		podDir = pl.PodDir()
	} else {
		podDir = compute.Skiff.PodWithUID(podKey, pod.GetUID())
	}
	logfilePath := podDir.Container(containerName).LogsPath()

	var initialBytes []byte
	startAtEnd := false
	if opts.Tail > 0 {
		logs, err := container.GetTailLog(logfilePath, opts.Tail)
		if err == nil {
			var buf bytes.Buffer
			for _, line := range logs {
				buf.WriteString(line + "\n")
			}
			initialBytes = buf.Bytes()
		}
		startAtEnd = true
	} else if !opts.Follow {
		data, err := os.ReadFile(logfilePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return io.NopCloser(bytes.NewReader([]byte{})), nil
			}
			return nil, fmt.Errorf("unable to read logs: %w", err)
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}

	if opts.Follow {
		return newFollowLogReader(ctx, logfilePath, initialBytes, startAtEnd)
	}

	return io.NopCloser(bytes.NewReader(initialBytes)), nil
}

// RunInContainer executes a command in a live container in the pod locally without looping back through the API server.
func (v *VirtualK8S) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach vkapi.AttachIO) error {
	podKey := client.ObjectKey{Namespace: namespace, Name: podName}
	logger := v.Logger.WithValues("obj", podKey, "container", containerName)

	logger.Info("[K8s] -> RunInContainer")
	defer logger.Info("[K8s] <- RunInContainer")

	defer func() {
		if attach != nil {
			if attach.Stdout() != nil {
				attach.Stdout().Close()
			}
			if attach.Stderr() != nil {
				attach.Stderr().Close()
			}
		}
	}()

	value, ok := v.pods.Load(podKey)
	if !ok {
		return errdefs.NotFoundf("pod %s/%s not found", namespace, podName)
	}
	containerID := ""
	for _, status := range value.(*corev1.Pod).Status.ContainerStatuses {
		if status.Name == containerName && status.State.Running != nil {
			containerID = status.ContainerID
			break
		}
	}
	launcherPID, startTime, err := runtime.ParseProcessJobID(containerID)
	if err != nil || runtime.IsProcessDead(launcherPID) {
		return errdefs.NotFoundf("container %s is not running in pod %s/%s", containerName, namespace, podName)
	}
	if currentStart, err := runtime.GetProcessStartTime(launcherPID); err != nil || (startTime > 0 && currentStart != startTime) {
		return errdefs.NotFoundf("container %s process was replaced", containerName)
	}
	containerPID := PodHandler.FindContainerPID(launcherPID)
	if containerPID == launcherPID || runtime.IsProcessDead(containerPID) {
		return errdefs.NotFoundf("container %s payload is not running", containerName)
	}

	// Execute command locally into the container's namespaces using nsenter
	nsenterArgs := []string{
		"-t", strconv.Itoa(containerPID),
		"-m", "-n", "-p", "-r",
		"--",
	}
	nsenterArgs = append(nsenterArgs, cmd...)

	execCmd := exec.CommandContext(ctx, "nsenter", nsenterArgs...)
	if attach != nil {
		execCmd.Stdin = attach.Stdin()
		execCmd.Stdout = attach.Stdout()
		execCmd.Stderr = attach.Stderr()
	}

	if err := execCmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return utilexec.CodeExitError{
				Err:  exitErr,
				Code: exitErr.ExitCode(),
			}
		}
		return err
	}
	return nil
}
