package podhandler

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"skifflet/internal/compute"
)

func serveTestReadyNetwork(t *testing.T, socket string, beforeResponse func()) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var header [4]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		request := make([]byte, binary.BigEndian.Uint32(header[:]))
		if _, err := io.ReadFull(conn, request); err != nil {
			return
		}
		if beforeResponse != nil {
			beforeResponse()
		}
		response := []byte(`{"success":true,"ready":true,"gateway_ready":true,"gateway_mode":"tap","node_cidr":"10.244.1.0/24"}`)
		binary.BigEndian.PutUint32(header[:], uint32(len(response)))
		conn.Write(append(header[:], response...))
	}()
}

func TestCreateDeleteWhileNetworkInitializationBlocked(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "skiff-network-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "daemon.sock")
	requested, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	serveTestReadyNetwork(t, socket, func() { close(requested); <-release })

	scheme := kruntime.NewScheme()
	corev1.AddToScheme(scheme)
	oldClient, oldSocket := compute.K8SClient, podNetworkSocket
	compute.K8SClient = fake.NewClientBuilder().WithScheme(scheme).Build()
	podNetworkSocket = socket
	defer func() { compute.K8SClient, podNetworkSocket = oldClient, oldSocket }()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "blocked-network", Namespace: "startup-cancel", UID: "blocked-network-uid"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "unused"}}}}
	states := make(chan *corev1.Pod, 16)
	owner := installOwner(t, pod, func(p *corev1.Pod) { states <- p.DeepCopy() })
	launched := prepare(owner)
	go func() { <-launched; close(states) }()
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("production launcher did not reach readiness barrier")
	}
	pl := GlobalRegistry.Get(pod.UID)
	if pl != owner {
		t.Fatal("production launcher has no lifecycle owner")
	}
	deleted := make(chan bool, 1)
	go func() { deleted <- DeletePod(client.ObjectKeyFromObject(pod), pod.DeepCopy()) }()
	select {
	case <-pl.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("delete did not cancel blocked initialization")
	}
	close(release)
	select {
	case <-launched:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled launcher did not quiesce")
	}
	select {
	case ok := <-deleted:
		if !ok {
			t.Fatal("production deletion failed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deletion did not finish after startup quiesced")
	}
	if GlobalRegistry.Get(pod.UID) != nil {
		t.Fatal("deleted lifecycle remained registered")
	}
	pl.mu.RLock()
	allocated := pl.sandbox != nil || len(pl.live) != 0
	pl.mu.RUnlock()
	if allocated {
		t.Fatal("cancelled startup allocated processes or sandbox after deletion")
	}
	var final *corev1.Pod
	for snapshot := range states {
		final = snapshot
	}
	if final == nil || final.Status.Reason != "NetworkNotReady" {
		t.Fatalf("late ready response bypassed cancellation: %+v", final)
	}
	if _, err := os.Stat(pl.PodDir().String()); !os.IsNotExist(err) {
		t.Fatalf("cancelled startup retained its owned directory: %v", err)
	}
}

func TestCreateDeleteDuringPauseImagePull(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "skiff-pull-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "daemon.sock")
	serveTestReadyNetwork(t, socket, nil)
	launcher := filepath.Join(root, "launcher")
	script := "#!/bin/sh\nif [ \"$1\" = pull ]; then touch \"$SKIFF_TEST_PULL_MARKER\"; exec sleep 30; fi\ntouch \"$SKIFF_TEST_LAUNCH_MARKER\"\nexit 1\n"
	if err := os.WriteFile(launcher, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	pullMarker, launchMarker := filepath.Join(root, "pull"), filepath.Join(root, "launch")
	t.Setenv("SKIFF_TEST_PULL_MARKER", pullMarker)
	t.Setenv("SKIFF_TEST_LAUNCH_MARKER", launchMarker)
	scheme := kruntime.NewScheme()
	corev1.AddToScheme(scheme)
	oldClient, oldSocket, oldEnvironment := compute.K8SClient, podNetworkSocket, compute.Environment
	compute.K8SClient = fake.NewClientBuilder().WithScheme(scheme).Build()
	podNetworkSocket = socket
	compute.Environment.ApptainerBin = launcher
	compute.Environment.PauseImage = "test/blocked-pause:cancel"
	defer func() {
		compute.K8SClient, podNetworkSocket, compute.Environment = oldClient, oldSocket, oldEnvironment
	}()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "blocked-pull", Namespace: "startup-cancel", UID: "blocked-pull-uid"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "unused"}}}}
	finished := prepare(installOwner(t, pod, nil))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(pullMarker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("production launcher did not reach image pull barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !DeletePod(client.ObjectKeyFromObject(pod), pod.DeepCopy()) {
		t.Fatal("deletion during image pull failed")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled image pull did not finish")
	}
	if _, err := os.Stat(launchMarker); !os.IsNotExist(err) {
		t.Fatal("pause/application launcher ran after pull cancellation")
	}
	if GlobalRegistry.Get(pod.UID) != nil {
		t.Fatal("cancelled image pull lifecycle remained registered")
	}
}
