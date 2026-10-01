package root

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"k8s.io/apimachinery/pkg/util/httpstream"

	"skifflet/internal/provider"
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

	// GetStatsSummary and GetMetricsResource serve /stats/summary and
	// /metrics/resource, what metrics-server reads for kubectl top and autoscaling.
	GetStatsSummary    api.PodStatsSummaryHandlerFunc
	GetMetricsResource api.PodMetricsResourceHandlerFunc
}

const attachUnsupported = "attach is not implemented by skifflet: use kubectl logs to read the container's output, or kubectl exec to run a command in it"

func newKubeletMux(r kubeletRoutes) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/hello", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("Hi there! I 'm Skifflet. My job is to run your Kubernetes stuff on HPC.\n"))
	}))

	mux.Handle("/attach/", attachUnsupportedHandler())

	api.AttachPodRoutes(api.PodHandlerConfig{
		RunInContainer:        r.RunInContainer,
		GetContainerLogs:      r.GetContainerLogs,
		GetPods:               r.GetRunningPods,
		GetPodsFromKubernetes: r.GetAssignedPods,
		PortForward:           r.PortForward,
		GetStatsSummary:       r.GetStatsSummary,
		GetMetricsResource:    r.GetMetricsResource,
	}, mux, true)

	return withLogQuery(mux)
}

// withLogQuery hands the query of a log request to the provider (see
// provider.WithLogQuery): Virtual Kubelet's own parsing cannot tell an absent
// tailLines from tailLines=0.
func withLogQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/containerLogs/") {
			r = r.WithContext(provider.WithLogQuery(r.Context(), r.URL.Query()))
		}
		next.ServeHTTP(w, r)
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
