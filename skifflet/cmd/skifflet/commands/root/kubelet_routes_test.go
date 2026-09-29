package root

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func podList(names ...string) api.PodListerFunc {
	return func(context.Context) ([]*corev1.Pod, error) {
		var pods []*corev1.Pod
		for _, n := range names {
			pods = append(pods, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "default"}})
		}
		return pods, nil
	}
}

type recordedLogs struct {
	namespace, pod, container string
	opts                      api.ContainerLogOpts
}

func testRoutes(logs *recordedLogs) kubeletRoutes {
	return kubeletRoutes{
		GetRunningPods:  podList("running"),
		GetAssignedPods: podList("assigned-a", "assigned-b"),
		GetContainerLogs: func(_ context.Context, ns, pod, container string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
			if logs != nil {
				*logs = recordedLogs{ns, pod, container, opts}
			}
			if pod == "missing" {
				return nil, errdefs.NotFoundf("pod %s/%s not found", ns, pod)
			}
			return io.NopCloser(strings.NewReader("log-body\n")), nil
		},
		RunInContainer: func(context.Context, string, string, string, []string, api.AttachIO) error { return nil },
		PortForward:    func(context.Context, string, string, int32, io.ReadWriteCloser) error { return nil },
	}
}

func do(t *testing.T, h http.Handler, method, target string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec.Code, rec.Body.String()
}

func names(t *testing.T, body string) []string {
	t.Helper()
	var list corev1.PodList
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("not a pod list: %v: %s", err, body)
	}
	var out []string
	for _, p := range list.Items {
		out = append(out, p.Name)
	}
	return out
}

// T04: the two pod views have different meanings.
func TestKubeletRoutes_PodViews(t *testing.T) {
	h := newKubeletMux(testRoutes(nil))

	code, body := do(t, h, "GET", "/pods")
	if got := names(t, body); code != 200 || strings.Join(got, ",") != "assigned-a,assigned-b" {
		t.Fatalf("/pods must list the pods assigned by Kubernetes: %d %v", code, got)
	}
	code, body = do(t, h, "GET", "/runningpods/")
	if got := names(t, body); code != 200 || strings.Join(got, ",") != "running" {
		t.Fatalf("/runningpods/ must list the provider's pods: %d %v", code, got)
	}
}

func TestKubeletRoutes_AssignedPodsErrorIsNotEmptySuccess(t *testing.T) {
	r := testRoutes(nil)
	r.GetAssignedPods = func(context.Context) ([]*corev1.Pod, error) { return nil, errors.New("informer not started") }
	if code, _ := do(t, newKubeletMux(r), "GET", "/pods"); code != http.StatusInternalServerError {
		t.Fatalf("expected 500 while assigned pods are unavailable, got %d", code)
	}
}

// T04: deferred methods answer 501 with an explanation, on every spelling.
func TestKubeletRoutes_DeferredMethodsAreExplicit501(t *testing.T) {
	h := newKubeletMux(testRoutes(nil))
	for _, tc := range []struct{ method, target, want string }{
		{"GET", "/attach/default/pod/app", "attach"},
		{"POST", "/attach/default/pod/app?input=1&output=1", "attach"},
		{"GET", "/attach/default/nonexistent/app", "attach"},
		{"GET", "/stats/summary", "statistics"},
		{"GET", "/stats/summary/", "statistics"},
		{"GET", "/metrics/resource", "metrics"},
		{"GET", "/metrics/resource/", "metrics"},
	} {
		code, body := do(t, h, tc.method, tc.target)
		if code != http.StatusNotImplemented || !strings.Contains(body, tc.want) {
			t.Errorf("%s %s = %d %q, want 501 mentioning %q", tc.method, tc.target, code, body, tc.want)
		}
	}
}

// T04: genuine missing resources and malformed requests keep their own errors.
func TestKubeletRoutes_RealErrorsStayDistinct(t *testing.T) {
	h := newKubeletMux(testRoutes(nil))
	if code, _ := do(t, h, "GET", "/containerLogs/default/missing/app"); code != http.StatusNotFound {
		t.Errorf("logs of an unknown pod = %d, want 404", code)
	}
	if code, _ := do(t, h, "GET", "/containerLogs/default/pod/app?tailLines=notanumber"); code != http.StatusBadRequest {
		t.Errorf("malformed log option = %d, want 400", code)
	}
	if code, _ := do(t, h, "GET", "/exec/default/pod/app?command=ls"); code != http.StatusBadRequest {
		t.Errorf("exec without any stream = %d, want 400", code)
	}
	if code, _ := do(t, h, "GET", "/no/such/route"); code != http.StatusNotFound {
		t.Errorf("unknown route = %d, want 404", code)
	}
}

func TestKubeletRoutes_LogsReachTheProvider(t *testing.T) {
	var got recordedLogs
	h := newKubeletMux(testRoutes(&got))
	code, body := do(t, h, "GET", "/containerLogs/ns1/pod1/app?tailLines=5&timestamps=true&previous=true")
	if code != 200 || body != "log-body\n" {
		t.Fatalf("logs = %d %q", code, body)
	}
	if got.namespace != "ns1" || got.pod != "pod1" || got.container != "app" ||
		got.opts.Tail != 5 || !got.opts.Timestamps || !got.opts.Previous {
		t.Fatalf("provider received %+v", got)
	}
}

func TestKubeletRoutes_ExecAndPortForwardAreRouted(t *testing.T) {
	h := newKubeletMux(testRoutes(nil))
	for _, target := range []string{"/exec/default/pod/app?command=ls&output=1", "/portForward/default/pod"} {
		code, _ := do(t, h, "GET", target)
		if code == http.StatusNotFound || code == http.StatusNotImplemented {
			t.Errorf("%s is not routed: %d", target, code)
		}
	}
}

// T04: a streaming client must see the explanation. A bare 501 to an upgrade
// request surfaces in kubectl only as "error stream protocol error". (The
// websocket transport is covered by the e2e case: it needs a browser Origin.)
func TestKubeletRoutes_StreamingAttachExplainsItself(t *testing.T) {
	server := httptest.NewServer(newKubeletMux(testRoutes(nil)))
	defer server.Close()
	target, err := url.Parse(server.URL + "/attach/default/pod/app?container=app&output=1&error=1")
	if err != nil {
		t.Fatal(err)
	}

	executor, err := remotecommand.NewSPDYExecutor(&rest.Config{}, "POST", target)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	if err == nil || !strings.Contains(err.Error(), "attach is not implemented") {
		t.Fatalf("streaming attach error = %v, want the explanation", err)
	}
	// Refusing the upgrade is what kubectl reports as "stream protocol error".
	if strings.Contains(err.Error(), "unable to upgrade") {
		t.Fatalf("the upgrade was refused; the explanation must arrive on the error stream: %v", err)
	}
}
