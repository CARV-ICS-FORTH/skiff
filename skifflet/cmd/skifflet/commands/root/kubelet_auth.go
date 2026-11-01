package root

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/cache"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// kubeletAuth decides who may use the kubelet API, with the rules of a kubelet that has
// webhook authentication and authorization. A caller holds a certificate from the cluster's
// client CA (the API server does) or a token the API server confirms (metrics-server does),
// and RBAC says what that user may do on this node. Nobody is anonymous.
type kubeletAuth struct {
	client   kubernetes.Interface
	nodeName string
	// What the API server has confirmed, for a while, as a kubelet keeps it.
	callers *cache.LRUExpireCache
	allowed *cache.LRUExpireCache // of what a user may do: true or false
	// These failures must not fill the log.
	failures *rate.Sometimes
	// Tokens the API server has confirmed before, and the rate of asking about any others (see
	// kubeletUnknownTokenQPS).
	knownTokens   *cache.LRUExpireCache
	unknownTokens *rate.Limiter
}

const (
	// kubeletReviewTimeout bounds what one request waits for the API server's answers.
	kubeletReviewTimeout = 10 * time.Second
	// As a kubelet does, the group the API server's certificate is in: Kubernetes allows it
	// everything, whatever RBAC says, so asking would change nothing.
	kubeletSuperuserGroup = "system:masters"
	// What the API server adds to every user a certificate of its client CA names.
	kubeletAuthenticatedGroup = "system:authenticated"
	// The reviews have a client of their own, and this rate, apart from the one pods need.
	kubeletReviewQPS   = 5
	kubeletReviewBurst = 10
	// Tokens not seen before get a part of the rate above and do not wait for it: a token that was
	// confirmed and the reviews of who it is are not counted here.
	kubeletUnknownTokenQPS   = 2
	kubeletUnknownTokenBurst = 5
	kubeletKnownTokenKeep    = time.Hour
	// kubeletReviewKeep is how long an answer of the API server holds: what is revoked ends this
	// much later. A token is kept only for a yes, so that it is not refused for longer than it was
	// invalid. A no about what a user may do holds for kubeletDenialKeep, as a kubelet keeps it.
	kubeletReviewKeep  = 2 * time.Minute
	kubeletDenialKeep  = 30 * time.Second
	kubeletReviewsKept = 1024
)

// errNotAuthenticated is that the request has no credentials the API server confirms, which is
// not the same as the API server giving no answer.
var errNotAuthenticated = errors.New("not authenticated")

// errTooManyUnknownTokens is that the token was not asked about: too many are being sent.
var errTooManyUnknownTokens = errors.New("too many tokens to review")

func newKubeletAuth(restConfig *rest.Config, nodeName string) (kubeletAuth, error) {
	config := rest.CopyConfig(restConfig)
	config.QPS, config.Burst = kubeletReviewQPS, kubeletReviewBurst
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return kubeletAuth{}, err
	}
	return newKubeletAuthWith(client, nodeName), nil
}

func newKubeletAuthWith(client kubernetes.Interface, nodeName string) kubeletAuth {
	return kubeletAuth{
		client:   client,
		nodeName: nodeName,
		callers:  cache.NewLRUExpireCache(kubeletReviewsKept),
		allowed:  cache.NewLRUExpireCache(kubeletReviewsKept),
		failures: &rate.Sometimes{Interval: time.Minute},

		knownTokens:   cache.NewLRUExpireCache(kubeletReviewsKept),
		unknownTokens: rate.NewLimiter(kubeletUnknownTokenQPS, kubeletUnknownTokenBurst),
	}
}

// kubeletTLSConfig has TLS verify a client's certificate, if it offers one, against the client
// CA. A certificate is not required: a token is the other way in.
func kubeletTLSConfig(clientCAFile string) (*tls.Config, error) {
	bundle, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read the client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, fmt.Errorf("no certificates in the client CA %s", clientCAFile)
	}
	return &tls.Config{ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool}, nil
}

func (a kubeletAuth) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := a.check(r); status != http.StatusOK {
			http.Error(w, http.StatusText(status), status)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// check answers the status of a request: OK if it may go on.
func (a kubeletAuth) check(r *http.Request) int {
	ctx, cancel := context.WithTimeout(r.Context(), kubeletReviewTimeout)
	defer cancel()

	caller, err := a.authenticate(ctx, r)
	if errors.Is(err, errNotAuthenticated) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, errTooManyUnknownTokens) {
		return http.StatusTooManyRequests
	}
	if err != nil {
		a.failures.Do(func() { logrus.WithError(err).Error("kubelet API: authentication failed") })
		return http.StatusInternalServerError
	}
	if caller.inGroup(kubeletSuperuserGroup) {
		return http.StatusOK
	}
	allowed, err := a.authorize(ctx, caller, r)
	if err != nil {
		a.failures.Do(func() {
			logrus.WithError(err).WithField("user", caller.Username).Error("kubelet API: authorization failed")
		})
		return http.StatusInternalServerError
	}
	if !allowed {
		return http.StatusForbidden
	}
	return http.StatusOK
}

// kubeletCaller is who a request comes from, in the terms of a SubjectAccessReview.
type kubeletCaller authenticationv1.UserInfo

func (c kubeletCaller) inGroup(group string) bool {
	for _, g := range c.Groups {
		if g == group {
			return true
		}
	}
	return false
}

func (a kubeletAuth) authenticate(ctx context.Context, r *http.Request) (kubeletCaller, error) {
	// TLS has verified the chains it reports: what is here came from the client CA.
	if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
		subject := r.TLS.VerifiedChains[0][0].Subject
		groups := append(slices.Clone(subject.Organization), kubeletAuthenticatedGroup)
		return kubeletCaller{Username: subject.CommonName, Groups: groups}, nil
	}

	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") || token == "" {
		return kubeletCaller{}, fmt.Errorf("%w: no credentials", errNotAuthenticated)
	}
	key := sha256.Sum256([]byte(token))
	if caller, ok := a.callers.Get(key); ok {
		return caller.(kubeletCaller), nil
	}
	if _, known := a.knownTokens.Get(key); !known && !a.unknownTokens.Allow() {
		return kubeletCaller{}, errTooManyUnknownTokens
	}
	review, err := a.client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		return kubeletCaller{}, err
	}
	if !review.Status.Authenticated {
		return kubeletCaller{}, fmt.Errorf("%w: the API server does not confirm the token", errNotAuthenticated)
	}
	caller := kubeletCaller(review.Status.User)
	a.callers.Add(key, caller, kubeletReviewKeep)
	a.knownTokens.Add(key, struct{}{}, kubeletKnownTokenKeep)
	return caller, nil
}

func (a kubeletAuth) authorize(ctx context.Context, caller kubeletCaller, r *http.Request) (bool, error) {
	extra := make(map[string]authorizationv1.ExtraValue, len(caller.Extra))
	for k, v := range caller.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	verb, subresource := kubeletAccess(r)
	key := fmt.Sprintf("%q %q", []string{verb, subresource, caller.Username, caller.UID}, caller.Groups)
	if len(extra) > 0 {
		key += fmt.Sprintf(" %v", extra)
	}
	if allowed, ok := a.allowed.Get(key); ok {
		return allowed.(bool), nil
	}
	review, err := a.client.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   caller.Username,
			UID:    caller.UID,
			Groups: caller.Groups,
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:        verb,
				Resource:    "nodes",
				Subresource: subresource,
				Name:        a.nodeName,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	keep := kubeletDenialKeep
	if review.Status.Allowed {
		keep = kubeletReviewKeep
	}
	a.allowed.Add(key, review.Status.Allowed, keep)
	return review.Status.Allowed, nil
}

// kubeletAccess is what a request needs on this node, as a kubelet maps it, but for one thing:
// exec, attach and port forwarding need create whatever the method. They also answer GET (a
// WebSocket upgrade starts with one), and its verb would let read-only access to nodes/proxy
// run commands in containers.
func kubeletAccess(r *http.Request) (verb, subresource string) {
	verb, subresource = "create", "proxy"
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		verb = "get"
	}
	switch path := r.URL.Path; {
	case strings.HasPrefix(path, "/exec/"), strings.HasPrefix(path, "/attach/"), strings.HasPrefix(path, "/portForward/"):
		verb = "create"
	case path == "/stats" || strings.HasPrefix(path, "/stats/"):
		subresource = "stats"
	case path == "/metrics" || strings.HasPrefix(path, "/metrics/"):
		subresource = "metrics"
	}
	return verb, subresource
}
