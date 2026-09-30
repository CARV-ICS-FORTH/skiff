package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cri-client/pkg/logs"
	"sigs.k8s.io/controller-runtime/pkg/client"

	PodHandler "skifflet/internal/compute/podhandler"
)

// The kubelet's log operation takes its options from the query of the request:
// tailLines, limitBytes, sinceSeconds, sinceTime, timestamps, follow, previous.
// Virtual Kubelet folds them into ContainerLogOpts, where an absent tailLines and
// tailLines=0 are the same zero, though the first means every line and the second
// none. So the route puts the request's query in the context (WithLogQuery), and
// the options are read from it, exactly as the kubelet reads them.

type logQueryKey struct{}

// WithLogQuery returns a context that carries the query of a log request.
func WithLogQuery(ctx context.Context, query url.Values) context.Context {
	return context.WithValue(ctx, logQueryKey{}, query)
}

// LogQueryFromContext returns the query WithLogQuery stored, if any.
func LogQueryFromContext(ctx context.Context) (url.Values, bool) {
	query, ok := ctx.Value(logQueryKey{}).(url.Values)
	return query, ok
}

// logOptions reads the options of a request: from the query when the route
// provided it, otherwise from what Virtual Kubelet parsed (where tail 0 can only
// mean "all").
func logOptions(ctx context.Context, vk vkapi.ContainerLogOpts) (*corev1.PodLogOptions, error) {
	if query, ok := LogQueryFromContext(ctx); ok {
		opts, err := parseLogQuery(query)
		if err != nil {
			return nil, errdefs.AsInvalidInput(err)
		}
		return opts, validateLogOptions(opts)
	}
	opts := &corev1.PodLogOptions{Follow: vk.Follow, Previous: vk.Previous, Timestamps: vk.Timestamps}
	if vk.Tail > 0 {
		tail := int64(vk.Tail)
		opts.TailLines = &tail
	}
	if vk.LimitBytes > 0 {
		limit := int64(vk.LimitBytes)
		opts.LimitBytes = &limit
	}
	if vk.SinceSeconds > 0 {
		seconds := int64(vk.SinceSeconds)
		opts.SinceSeconds = &seconds
	}
	if !vk.SinceTime.IsZero() {
		since := metav1.NewTime(vk.SinceTime)
		opts.SinceTime = &since
	}
	return opts, validateLogOptions(opts)
}

// parseLogQuery reads the parameters of a log request, as the API does.
func parseLogQuery(query url.Values) (*corev1.PodLogOptions, error) {
	opts := &corev1.PodLogOptions{}
	number := func(name string) (*int64, error) {
		raw := query.Get(name)
		if raw == "" {
			return nil, nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", name, raw, err)
		}
		return &n, nil
	}
	flag := func(name string) (bool, error) {
		raw := query.Get(name)
		if raw == "" {
			return false, nil
		}
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return false, fmt.Errorf("invalid %s %q: %w", name, raw, err)
		}
		return b, nil
	}
	var err error
	if opts.TailLines, err = number("tailLines"); err != nil {
		return nil, err
	}
	if opts.LimitBytes, err = number("limitBytes"); err != nil {
		return nil, err
	}
	if opts.SinceSeconds, err = number("sinceSeconds"); err != nil {
		return nil, err
	}
	if raw := query.Get("sinceTime"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, fmt.Errorf("invalid sinceTime %q: %w", raw, err)
		}
		since := metav1.NewTime(t)
		opts.SinceTime = &since
	}
	if opts.Timestamps, err = flag("timestamps"); err != nil {
		return nil, err
	}
	if opts.Follow, err = flag("follow"); err != nil {
		return nil, err
	}
	if opts.Previous, err = flag("previous"); err != nil {
		return nil, err
	}
	return opts, nil
}

// validateLogOptions applies the API server's rules for log options: a request
// that breaks them is refused, not interpreted.
func validateLogOptions(opts *corev1.PodLogOptions) error {
	switch {
	case opts.TailLines != nil && *opts.TailLines < 0:
		return errdefs.InvalidInputf("tailLines must be at least 0 (%d)", *opts.TailLines)
	case opts.LimitBytes != nil && *opts.LimitBytes < 1:
		return errdefs.InvalidInputf("limitBytes must be greater than 0 (%d)", *opts.LimitBytes)
	case opts.SinceSeconds != nil && opts.SinceTime != nil:
		return errdefs.InvalidInputf("at most one of sinceTime or sinceSeconds may be specified")
	case opts.SinceSeconds != nil && *opts.SinceSeconds < 1:
		return errdefs.InvalidInputf("sinceSeconds must be greater than 0 (%d)", *opts.SinceSeconds)
	}
	return nil
}

// GetContainerLogs serves a container's log: the current attempt's, or with
// previous the one before it, as the options ask. A request reads one attempt
// only; following it ends when that attempt has ended and what it wrote has been
// read, and never continues into the next attempt. The stream stops when the
// client goes away.
func (v *VirtualK8S) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, vkOpts vkapi.ContainerLogOpts) (io.ReadCloser, error) {
	podKey := client.ObjectKey{Namespace: namespace, Name: podName}
	logger := v.Logger.WithValues("obj", podKey, "container", containerName)
	logger.Info("[K8s] -> GetContainerLogs")
	defer logger.Info("[K8s] <- GetContainerLogs")

	now := time.Now() // relative times (sinceSeconds) are measured from the request's arrival
	apiOpts, err := logOptions(ctx, vkOpts)
	if err != nil {
		return nil, err
	}

	owner := PodHandler.GlobalRegistry.GetByKey(podKey)
	if owner == nil {
		return nil, errdefs.NotFoundf("pod %s/%s not found", namespace, podName)
	}
	sel, err := owner.SelectLog(containerName, apiOpts.Previous)
	switch {
	case errors.Is(err, PodHandler.ErrNoSuchContainer):
		return nil, errdefs.NotFoundf("container %s not found in pod %s/%s", containerName, namespace, podName)
	case errors.Is(err, PodHandler.ErrLogNotStarted):
		return nil, errdefs.InvalidInputf("container %q in pod %q is waiting to start: ContainerCreating", containerName, podName)
	case errors.Is(err, PodHandler.ErrNoPreviousLog):
		return nil, errdefs.InvalidInputf("previous terminated container %q in pod %q not found", containerName, podName)
	case err != nil:
		return nil, err
	}
	logger.Info("reading a container log", "action", "logs", "attempt", sel.Attempt, "live", sel.Live(), "follow", apiOpts.Follow)

	return newLogStream(ctx, sel, criLogOptions(apiOpts, now)), nil
}

// criLogOptions turns the API's options into the reader's. The time a relative
// request is measured from is the moment it arrived.
func criLogOptions(o *corev1.PodLogOptions, now time.Time) *logs.LogOptions {
	opts := &logs.LogOptions{TailLines: o.TailLines, LimitBytes: o.LimitBytes, Follow: o.Follow, Timestamp: o.Timestamps}
	switch {
	case o.SinceSeconds != nil:
		opts.Since = now.Add(-time.Duration(*o.SinceSeconds) * time.Second)
	case o.SinceTime != nil:
		opts.Since = o.SinceTime.Time
	}
	return opts
}

// logStream is the reader of a log request. The log is copied by a goroutine into
// a pipe, which ends with the request: closing the stream (or the client's
// context ending) stops the copying and releases the file and the goroutine.
type logStream struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func newLogStream(ctx context.Context, sel PodHandler.LogSelection, opts *logs.LogOptions) *logStream {
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	s := &logStream{PipeReader: pr, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		err := PodHandler.ReadLog(ctx, sel, opts, pw)
		if ctx.Err() != nil {
			err = nil // the reader went away: nothing to report to
		}
		_ = pw.CloseWithError(err) // a nil error is end of file
	}()
	return s
}

// Close ends the request's reading and waits, for a bounded time, for the copier.
func (s *logStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.PipeReader.Close()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
	})
	return nil
}
