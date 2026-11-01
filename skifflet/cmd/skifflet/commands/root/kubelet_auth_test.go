package root

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crypto/sha256"
	"fmt"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"golang.org/x/time/rate"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sync/atomic"
)

// testCA makes a CA and the certificates of clients it signs.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-client-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca testCA) client(t *testing.T, user string, groups []string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: user, Organization: groups},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// kubeletUnderTest is the kubelet API with a fake API server behind its reviews: the token
// "good" is the user metrics, and RBAC allows what is in allowed ("verb subresource").
type kubeletUnderTest struct {
	url     string
	ca      testCA
	tls     *tls.Config
	reviews *fake.Clientset
	execs   *int
}

func startKubelet(t *testing.T, allowed ...string) kubeletUnderTest {
	t.Helper()
	ca := newTestCA(t)
	caFile := filepath.Join(t.TempDir(), "client-ca.crt")
	if err := os.WriteFile(caFile, ca.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := kubeletTLSConfig(caFile)
	if err != nil {
		t.Fatal(err)
	}

	reviews := fake.NewClientset()
	reviews.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if review.Spec.Token == "good" {
			review.Status.Authenticated = true
			review.Status.User = authenticationv1.UserInfo{Username: "metrics", Groups: []string{"system:serviceaccounts"}}
		}
		return true, review, nil
	})
	reviews.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		attrs := review.Spec.ResourceAttributes
		if attrs.Resource != "nodes" || attrs.Name != "test-node" {
			t.Errorf("review of %+v, want node test-node", attrs)
		}
		for _, a := range allowed {
			review.Status.Allowed = review.Status.Allowed || a == attrs.Verb+" "+attrs.Subresource
		}
		return true, review, nil
	})

	execs := new(int)
	routes := testRoutes(nil)
	routes.RunInContainer = func(context.Context, string, string, string, []string, api.AttachIO) error {
		*execs++
		return nil
	}
	auth := newKubeletAuthWith(reviews, "test-node")
	auth.unknownTokens.SetLimit(rate.Inf) // these tests are not about how many
	server := httptest.NewUnstartedServer(auth.wrap(newKubeletMux(routes)))
	server.TLS = tlsConfig
	server.StartTLS()
	t.Cleanup(server.Close)
	return kubeletUnderTest{server.URL, ca, &tls.Config{InsecureSkipVerify: true}, reviews, execs} //nolint:gosec // the test's own server
}

func (k kubeletUnderTest) status(t *testing.T, method, path, token string, certs ...tls.Certificate) int {
	t.Helper()
	req, err := http.NewRequest(method, k.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	config := k.tls.Clone()
	config.Certificates = certs
	client := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: config},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

var kubeletPaths = []string{
	"/hello", "/pods", "/runningpods/", "/containerLogs/default/p/c", "/stats/summary", "/metrics/resource",
	"/exec/default/p/c?command=id&output=1", "/portForward/default/p", "/attach/default/p/c",
}

func TestKubeletRefusesWhoeverHasNoCredentials(t *testing.T) {
	k := startKubelet(t, "get proxy", "create proxy", "get stats", "get metrics")
	for _, path := range kubeletPaths {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if got := k.status(t, method, path, ""); got != http.StatusUnauthorized {
				t.Errorf("%s %s without credentials: %d, want 401", method, path, got)
			}
			if got := k.status(t, method, path, "made-up"); got != http.StatusUnauthorized {
				t.Errorf("%s %s with an unknown token: %d, want 401", method, path, got)
			}
		}
	}
	if *k.execs != 0 {
		t.Errorf("%d commands ran", *k.execs)
	}
}

func TestKubeletRefusesACertificateFromAnotherCA(t *testing.T) {
	k := startKubelet(t)
	stranger := newTestCA(t).client(t, "admin", []string{kubeletSuperuserGroup}, x509.ExtKeyUsageClientAuth)
	config := k.tls.Clone()
	config.Certificates = []tls.Certificate{stranger}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
	if resp, err := client.Get(k.url + "/pods"); err == nil {
		resp.Body.Close()
		t.Fatalf("a certificate from another CA got %d", resp.StatusCode)
	}
}

func TestKubeletRefusesACertificateNotForClients(t *testing.T) {
	k := startKubelet(t)
	server := k.ca.client(t, "admin", []string{kubeletSuperuserGroup}, x509.ExtKeyUsageServerAuth)
	config := k.tls.Clone()
	config.Certificates = []tls.Certificate{server}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
	if resp, err := client.Get(k.url + "/pods"); err == nil {
		resp.Body.Close()
		t.Fatalf("a server certificate got %d", resp.StatusCode)
	}
}

// The API server's certificate is what kubectl logs, exec and port-forward come through. It
// needs no call back to the API server.
func TestKubeletLetsTheAPIServerIn(t *testing.T) {
	k := startKubelet(t)
	apiserver := k.ca.client(t, "system:apiserver", []string{kubeletSuperuserGroup}, x509.ExtKeyUsageClientAuth)
	for _, path := range []string{"/pods", "/containerLogs/default/p/c", "/stats/summary"} {
		if got := k.status(t, http.MethodGet, path, "", apiserver); got != http.StatusOK {
			t.Errorf("%s: %d, want 200", path, got)
		}
	}
	if actions := k.reviews.Actions(); len(actions) != 0 {
		t.Errorf("asked the API server: %v", actions)
	}
}

// metrics-server: a token, and RBAC that allows the metrics and nothing else.
func TestKubeletAsksRBACAboutAToken(t *testing.T) {
	k := startKubelet(t, "get metrics", "get stats")
	for path, want := range map[string]int{
		"/metrics/resource":          http.StatusOK,
		"/stats/summary":             http.StatusOK,
		"/pods":                      http.StatusForbidden,
		"/containerLogs/default/p/c": http.StatusForbidden,
	} {
		if got := k.status(t, http.MethodGet, path, "good"); got != want {
			t.Errorf("%s: %d, want %d", path, got, want)
		}
	}
}

func TestKubeletAsksRBACAboutACertificateOfAnotherUser(t *testing.T) {
	k := startKubelet(t, "get stats")
	alice := k.ca.client(t, "alice", []string{"team"}, x509.ExtKeyUsageClientAuth)
	if got := k.status(t, http.MethodGet, "/stats/summary", "", alice); got != http.StatusOK {
		t.Errorf("what RBAC allows: %d, want 200", got)
	}
	if got := k.status(t, http.MethodGet, "/pods", "", alice); got != http.StatusForbidden {
		t.Errorf("what RBAC does not allow: %d, want 403", got)
	}
}

// Read-only access to nodes/proxy is not a way to run commands, whatever the method.
func TestKubeletExecNeedsCreate(t *testing.T) {
	k := startKubelet(t, "get proxy")
	for _, path := range []string{"/exec/default/p/c?command=id&output=1", "/attach/default/p/c", "/portForward/default/p"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if got := k.status(t, method, path, "good"); got != http.StatusForbidden {
				t.Errorf("%s %s with get on nodes/proxy: %d, want 403", method, path, got)
			}
		}
	}
	if *k.execs != 0 {
		t.Errorf("%d commands ran", *k.execs)
	}
}

// Access to the stats is not access to what a path spelled to look like them leads to.
func TestKubeletPathTricksDoNotReachExec(t *testing.T) {
	k := startKubelet(t, "get stats", "create stats")
	for _, path := range []string{"/stats/../exec/default/p/c?command=id&output=1", "/stats/..%2fexec/default/p/c?command=id&output=1"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			got := k.status(t, method, path, "good")
			if got == http.StatusOK || got == http.StatusSwitchingProtocols || got == http.StatusBadRequest {
				t.Errorf("%s %s: %d", method, path, got)
			}
		}
	}
	if *k.execs != 0 {
		t.Errorf("%d commands ran", *k.execs)
	}
}

func TestKubeletFailsClosedWithoutAnAnswer(t *testing.T) {
	k := startKubelet(t, "get stats")
	k.reviews.PrependReactor("create", "subjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, os.ErrDeadlineExceeded
	})
	if got := k.status(t, http.MethodGet, "/stats/summary", "good"); got != http.StatusInternalServerError {
		t.Errorf("the authorization review failing: %d, want 500", got)
	}
	k.reviews.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, os.ErrDeadlineExceeded
	})
	if got := k.status(t, http.MethodGet, "/stats/summary", "another"); got != http.StatusInternalServerError {
		t.Errorf("the token review failing: %d, want 500", got)
	}
}

func TestKubeletTLSConfigNeedsAClientCA(t *testing.T) {
	if _, err := kubeletTLSConfig(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("no error for a client CA that is not there")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := kubeletTLSConfig(empty); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Errorf("a file with no certificates: %v", err)
	}
}

// An API server that gives no answer is not a caller without credentials, and what it has said
// yes to is not asked again.
func TestKubeletReviewFailureIsNotARefusedCaller(t *testing.T) {
	reviews := fake.NewSimpleClientset()
	failing := true
	asked := 0
	reviews.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		asked++
		if failing {
			return true, nil, errors.New("the API server is restarting")
		}
		return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
			Authenticated: true, User: authenticationv1.UserInfo{Username: "metrics"},
		}}, nil
	})
	reviewed := 0
	reviews.PrependReactor("create", "subjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		reviewed++
		return true, &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	a := newKubeletAuthWith(reviews, "test-node")
	request := func() int {
		r := httptest.NewRequest(http.MethodGet, "/stats/summary", nil)
		r.Header.Set("Authorization", "Bearer a-token")
		return a.check(r)
	}
	if got := request(); got != http.StatusInternalServerError {
		t.Errorf("no answer from the API server: %d", got)
	}
	failing = false
	if got := request(); got != http.StatusOK {
		t.Errorf("the API server is back, and the failure was not kept: %d", got)
	}
	if got := request(); got != http.StatusOK || asked != 2 || reviewed != 1 {
		t.Errorf("a confirmed caller: %d, %d token reviews, %d access reviews", got, asked, reviewed)
	}
}

// A certificate's user is authenticated as a token's is, for what RBAC gives to that group.
func TestKubeletCertificateCallerIsAuthenticated(t *testing.T) {
	ca := newTestCA(t)
	cert, err := x509.ParseCertificate(ca.client(t, "someone", []string{"a-team"}, x509.ExtKeyUsageClientAuth).Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/stats/summary", nil)
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	caller, err := newKubeletAuthWith(fake.NewSimpleClientset(), "test-node").authenticate(context.Background(), r)
	if err != nil || !caller.inGroup("a-team") || !caller.inGroup("system:authenticated") {
		t.Errorf("%+v, %v", caller, err)
	}
}

// Tokens not seen before are limited and do not wait, and a token that was confirmed is not limited.
func TestKubeletUnknownTokensDoNotStarveKnownOnes(t *testing.T) {
	var reviewed atomic.Int32
	reviews := fake.NewSimpleClientset()
	reviews.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		reviewed.Add(1)
		review := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		review.Status.Authenticated = review.Spec.Token == "good"
		review.Status.User.Username = "metrics"
		return true, review, nil
	})
	a := newKubeletAuthWith(reviews, "test-node")
	ask := func(token string) error {
		r := httptest.NewRequest(http.MethodGet, "/stats", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		_, err := a.authenticate(context.Background(), r)
		return err
	}
	if err := ask("good"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	refused := 0
	for i := range 200 {
		if err := ask(fmt.Sprintf("made-up-%d", i)); errors.Is(err, errTooManyUnknownTokens) {
			refused++
		}
	}
	if refused < 150 || reviewed.Load() > 60 || time.Since(start) > 5*time.Second {
		t.Errorf("of 200 made-up tokens %d refused, %d reviews, in %v", refused, reviewed.Load(), time.Since(start))
	}
	a.callers.Remove(sha256.Sum256([]byte("good"))) // its answer runs out
	if err := ask("good"); err != nil {
		t.Errorf("a token that was confirmed, after the limit: %v", err)
	}
}
