package root

import (
	"context"
	"errors"
	"net/http"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"k8s.io/apimachinery/pkg/util/httpstream"
)

// kubeletRoutes are the operations behind the kubelet HTTP surface. The
// production server and the route tests both go through newKubeletMux.
type kubeletRoutes struct {
	RunInContainer   api.ContainerExecHandlerFunc
	GetContainerLogs api.ContainerLogsHandlerFunc
	PortForward      api.PortForwardHandlerFunc

	// GetRunningPods serves /runningpods/: what the provider is running.
	GetRunningPods api.PodListerFunc
	// GetAssignedPods serves /pods: the pods Kubernetes has assigned to this
	// node. Neither endpoint is what `kubectl get pods` reads, which asks the
	// API server.
	GetAssignedPods api.PodListerFunc
}

const attachUnsupported = "attach is not implemented by skifflet: use kubectl logs to read the container's output, or kubectl exec to run a command in it"

// Deliberately unimplemented kubelet operations answer 501 with an
// explanation, instead of the 404 an unregistered route would produce.
var unimplementedRoutes = map[string]string{
	"/stats/summary":     "resource statistics are not implemented by skifflet, so usage-based kubectl top and autoscaling are unavailable",
	"/stats/summary/":    "resource statistics are not implemented by skifflet, so usage-based kubectl top and autoscaling are unavailable",
	"/metrics/resource":  "resource metrics are not implemented by skifflet, so usage-based kubectl top and autoscaling are unavailable",
	"/metrics/resource/": "resource metrics are not implemented by skifflet, so usage-based kubectl top and autoscaling are unavailable",
}

func newKubeletMux(r kubeletRoutes) *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle("/hello", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("Hi there! I 'm Skifflet. My job is to run your Kubernetes stuff on HPC.\n"))
	}))

	// Registered ahead of Virtual Kubelet's catch-all so they take precedence.
	for path, message := range unimplementedRoutes {
		mux.Handle(path, notImplemented(message))
	}

	mux.Handle("/attach/", attachUnsupportedHandler())

	api.AttachPodRoutes(api.PodHandlerConfig{
		RunInContainer:        r.RunInContainer,
		GetContainerLogs:      r.GetContainerLogs,
		GetPods:               r.GetRunningPods,
		GetPodsFromKubernetes: r.GetAssignedPods,
		PortForward:           r.PortForward,
	}, mux, true)

	return mux
}

func notImplemented(message string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "501 not implemented: "+message, http.StatusNotImplemented)
	})
}

// attachUnsupportedHandler answers attach in the protocol the caller speaks. A
// streaming client (kubectl attach) shows only "stream protocol error" when the
// upgrade is refused with a bare 501, so an upgrade request is accepted and the
// explanation is delivered on the error stream. Any other request gets the 501.
func attachUnsupportedHandler() http.Handler {
	streaming := api.HandleContainerAttach(func(context.Context, string, string, string, api.AttachIO) error {
		return errors.New(attachUnsupported)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if httpstream.IsUpgradeRequest(r) {
			streaming(w, r)
			return
		}
		http.Error(w, "501 not implemented: "+attachUnsupported, http.StatusNotImplemented)
	})
}
