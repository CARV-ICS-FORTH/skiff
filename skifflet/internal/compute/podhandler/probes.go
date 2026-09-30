package podhandler

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Probes follow the kubelet's model. A container's probes run for one attempt and
// stop with it (the attempt's context); what they conclude reaches the pod only
// through the attempt, which drops it once the attempt has been superseded.
//
//   - The startup probe runs first. Until it succeeds the container is not Started
//     and not Ready, and neither the readiness nor the liveness probe runs. It
//     stops after its success. Failing it (failureThreshold consecutive failures)
//     stops the container, which then follows the restart policy.
//   - The readiness probe changes the container's Ready condition in both
//     directions, and never restarts anything.
//   - The liveness probe stops the container when it fails failureThreshold times
//     in a row; the restart policy decides what happens next.
//
// Every probe measures its initial delay from the start of the container, not from
// the end of the startup probe. Network probes (HTTP, TCP) are made from here
// against the pod IP; exec probes run inside the container through the attempt's
// verified target. gRPC probes are not supported and are rejected with the pod.

// Kubernetes probe defaults (the values the API server applies to a probe that
// leaves a field unset, which the kubelet also assumes).
const (
	defaultProbePeriod           = 10 * time.Second
	defaultProbeTimeout          = time.Second
	defaultProbeSuccessThreshold = 1
	defaultProbeFailureThreshold = 3

	// minKillGrace is the shortest time a container gets to stop after the
	// termination signal before it is killed: the kubelet never grants less.
	minKillGrace = 2 * time.Second

	// probeUserAgent is what the kubelet's HTTP probes call themselves.
	probeUserAgent = "kube-probe/1.35"
)

// execRunner runs a command probe's command inside the container.
type execRunner func(ctx context.Context, argv []string, timeout time.Duration) (ok bool, detail string)

// probeSecond is the length of the "second" in a probe's settings. Only tests
// change it, to run the timing rules without waiting.
var probeSecond = time.Second

// startProbes runs the probes of a container for one attempt.
func startProbes(a *Attempt, podIP string, spec *corev1.Container, runExec execRunner) {
	pod := a.pl.GetPod()
	since := time.Now() // initial delays count from the start of the container
	ctx := a.Context()

	checkOf := func(probe *corev1.Probe) probeCheck {
		return func(ctx context.Context, timeout time.Duration) (bool, string) {
			return evaluateProbe(ctx, probe, spec.Ports, podIP, timeout, runExec)
		}
	}

	// stopOn stops the container for a liveness or startup probe that has failed.
	stopOn := func(kind string, probe *corev1.Probe, detail string) {
		a.event(corev1.EventTypeWarning, "Unhealthy", "%s probe failed: %s", capitalize(kind), detail)
		if kind == "startup" {
			a.event(corev1.EventTypeNormal, "Killing", "failed startup probe, will be restarted")
		} else {
			a.event(corev1.EventTypeNormal, "Killing", "failed %s probe, will be restarted", kind)
		}
		a.Stop(probeKillGrace(pod, probe), fmt.Sprintf("%s probe failed: %s", kind, detail))
	}

	runRest := func() {
		if probe := spec.ReadinessProbe; probe != nil {
			go pollProbe(ctx, settingsOf(probe), since, probeFailure, checkOf(probe), func(r probeResult, detail string) bool {
				if r == probeFailure {
					a.event(corev1.EventTypeWarning, "Unhealthy", "Readiness probe failed: %s", detail)
				}
				a.SetReady(r == probeSuccess)
				return false
			})
		}
		if probe := spec.LivenessProbe; probe != nil {
			go pollProbe(ctx, settingsOf(probe), since, probeSuccess, checkOf(probe), func(r probeResult, detail string) bool {
				if r == probeSuccess {
					return false
				}
				stopOn("liveness", probe, detail)
				return true
			})
		}
	}

	probe := spec.StartupProbe
	if probe == nil {
		runRest()
		return
	}
	go pollProbe(ctx, settingsOf(probe), since, probeUnknown, checkOf(probe), func(r probeResult, detail string) bool {
		if r == probeSuccess {
			a.SetStarted()
			runRest()
		} else {
			stopOn("startup", probe, detail)
		}
		return true
	})
}

// probeResult is what a probe has concluded so far.
type probeResult int

const (
	probeUnknown probeResult = iota
	probeSuccess
	probeFailure
)

// probeCheck runs one probe once and says why it failed.
type probeCheck func(ctx context.Context, timeout time.Duration) (ok bool, detail string)

// probeSettings are a probe's timing rules, with the Kubernetes defaults applied.
type probeSettings struct {
	initialDelay     time.Duration
	period           time.Duration
	timeout          time.Duration
	successThreshold int
	failureThreshold int
}

func settingsOf(p *corev1.Probe) probeSettings {
	s := probeSettings{
		initialDelay:     time.Duration(p.InitialDelaySeconds) * probeSecond,
		period:           time.Duration(p.PeriodSeconds) * probeSecond,
		timeout:          time.Duration(p.TimeoutSeconds) * probeSecond,
		successThreshold: int(p.SuccessThreshold),
		failureThreshold: int(p.FailureThreshold),
	}
	if s.period <= 0 {
		s.period = defaultProbePeriod
	}
	if s.timeout <= 0 {
		s.timeout = defaultProbeTimeout
	}
	if s.successThreshold <= 0 {
		s.successThreshold = defaultProbeSuccessThreshold
	}
	if s.failureThreshold <= 0 {
		s.failureThreshold = defaultProbeFailureThreshold
	}
	return s
}

// pollProbe runs a probe until ctx ends or onResult returns true. It waits until
// the initial delay after since has passed, then checks once per period. The
// probe's result changes only after successThreshold successes or
// failureThreshold failures in a row; each change (from `initial` at first) is
// passed to onResult, with the detail of the failure that caused it. Checks are
// bound to ctx, so a hanging probe cannot outlive its attempt.
func pollProbe(ctx context.Context, s probeSettings, since time.Time, initial probeResult, check probeCheck, onResult func(r probeResult, detail string) (done bool)) {
	if wait := time.Until(since.Add(s.initialDelay)); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	ticker := time.NewTicker(s.period)
	defer ticker.Stop()

	current, last, run := initial, probeUnknown, 0
	for {
		if ctx.Err() != nil {
			return
		}
		ok, detail := check(ctx, s.timeout)
		if ctx.Err() != nil {
			return // whatever the check concluded belongs to an attempt that has ended
		}
		result, threshold := probeFailure, s.failureThreshold
		if ok {
			result, threshold = probeSuccess, s.successThreshold
		}
		if result == last {
			run++
		} else {
			last, run = result, 1
		}
		if run >= threshold && result != current {
			current = result
			if onResult(result, detail) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func capitalize(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

// probeKillGrace is how long a container that failed a liveness or startup probe
// gets to stop: the probe's own termination grace period when it has one, else the
// pod's.
func probeKillGrace(pod *corev1.Pod, probe *corev1.Probe) time.Duration {
	grace := TerminationGrace(nil, pod)
	if g := probe.TerminationGracePeriodSeconds; g != nil && *g > 0 {
		grace = time.Duration(*g) * time.Second
	}
	return max(grace, minKillGrace)
}

// execProbe runs a command probe inside the container. A container that is not
// (yet) a usable target fails the probe.
func (a *Attempt) execProbe(ctx context.Context, argv []string, timeout time.Duration) (bool, string) {
	target, err := a.Target(ctx, 0)
	if err != nil {
		return false, err.Error()
	}
	code, err := target.Run(ctx, argv, timeout)
	if err != nil {
		return false, err.Error()
	}
	if code != 0 {
		return false, fmt.Sprintf("command exited with code %d", code)
	}
	return true, ""
}

// evaluateProbe runs one probe once. Network probes are made from here against
// the pod IP (or the probe's host), bound to ctx; exec probes are handed to
// runExec. Named ports are resolved against the probed container's ports, and a
// name it does not declare fails the probe.
func evaluateProbe(ctx context.Context, probe *corev1.Probe, ports []corev1.ContainerPort, podIP string, timeout time.Duration,
	runExec execRunner) (bool, string) {
	switch {
	case probe.HTTPGet != nil:
		port, err := resolvePort(probe.HTTPGet.Port, ports)
		if err != nil {
			return false, err.Error()
		}
		host := hostOrPod(probe.HTTPGet.Host, podIP)
		if host == "" {
			return false, errNoHost
		}
		return httpProbe(ctx, probe.HTTPGet, host, port, timeout)
	case probe.TCPSocket != nil:
		port, err := resolvePort(probe.TCPSocket.Port, ports)
		if err != nil {
			return false, err.Error()
		}
		host := hostOrPod(probe.TCPSocket.Host, podIP)
		if host == "" {
			return false, errNoHost
		}
		return tcpProbe(ctx, host, port, timeout)
	case probe.Exec != nil:
		if len(probe.Exec.Command) == 0 || runExec == nil {
			return false, "no command to run"
		}
		return runExec(ctx, probe.Exec.Command, timeout)
	}
	return false, "the probe has no supported handler"
}

func hostOrPod(host, podIP string) string {
	if host != "" {
		return host
	}
	return podIP
}

// errNoHost fails a network probe that has nothing to connect to. An empty host
// must never be dialed: it would mean the node itself.
const errNoHost = "the pod has no IP address to probe"

// resolvePort turns a probe's port into a number: a name is looked up among the
// probed container's declared ports, and nothing is defaulted.
func resolvePort(port intstr.IntOrString, ports []corev1.ContainerPort) (int, error) {
	n := 0
	if port.Type == intstr.String {
		found := false
		for _, p := range ports {
			if p.Name == port.StrVal {
				n, found = int(p.ContainerPort), true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("unresolved named port %q", port.StrVal)
		}
	} else {
		n = port.IntValue()
	}
	if n <= 0 || n > 65535 {
		return 0, fmt.Errorf("invalid port number %d", n)
	}
	return n, nil
}

// maxRedirects and maxProbeBody are the kubelet's limits for an HTTP probe.
const (
	maxRedirects = 10
	maxProbeBody = 10 * 1024
)

// httpProbe makes one GET request the way the kubelet does: any status from 200 to
// 399 succeeds, HTTPS certificates are not verified, a Host header overrides the
// request's host, and a redirect to another host is not followed (and counts as a
// success), so a probe cannot be sent elsewhere.
func httpProbe(ctx context.Context, action *corev1.HTTPGetAction, host string, port int, timeout time.Duration) (bool, string) {
	u, err := url.Parse(action.Path)
	if err != nil {
		return false, fmt.Sprintf("invalid path %q: %v", action.Path, err)
	}
	u.Scheme = "http"
	if strings.EqualFold(string(action.Scheme), string(corev1.URISchemeHTTPS)) {
		u.Scheme = "https"
	}
	u.Host = net.JoinHostPort(host, strconv.Itoa(port))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err.Error()
	}
	for _, h := range action.HTTPHeaders {
		req.Header.Add(h.Name, h.Value)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", probeUserAgent)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}
	if h := req.Header.Get("Host"); h != "" {
		req.Host = h
	}

	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext:       (&net.Dialer{Timeout: timeout}).DialContext,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // a probe checks liveness, not identity
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if next.URL.Hostname() != via[0].URL.Hostname() {
				return http.ErrUseLastResponse
			}
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return false, fmt.Sprintf("HTTP probe failed with statuscode: %d", resp.StatusCode)
	}
	return true, ""
}

// tcpProbe succeeds when a connection to the port can be opened.
func tcpProbe(ctx context.Context, host string, port int, timeout time.Duration) (bool, string) {
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false, err.Error()
	}
	_ = conn.Close()
	return true, ""
}
