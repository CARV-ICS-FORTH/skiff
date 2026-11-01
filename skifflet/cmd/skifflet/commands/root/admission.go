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
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"skifflet/internal/provider"

	"errors"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/rest"
)

func StartAPIServer(c Opts, virtualk8s *provider.VirtualK8S, restConfig *rest.Config) error {
	auth, err := newKubeletAuth(restConfig, c.NodeName)
	if err != nil {
		return fmt.Errorf("kubelet API authentication: %w", err)
	}
	tlsConfig, err := kubeletTLSConfig(c.K8sClientCAFilepath)
	if err != nil {
		return fmt.Errorf("kubelet API authentication: %w", err)
	}

	mux := newKubeletMux(kubeletRoutes{
		RunInContainer:     virtualk8s.RunInContainer,
		GetContainerLogs:   virtualk8s.GetContainerLogs,
		PortForward:        virtualk8s.PortForward,
		GetRunningPods:     virtualk8s.GetPods,
		GetAssignedPods:    virtualk8s.GetAssignedPods,
		GetStatsSummary:    virtualk8s.GetStatsSummary,
		GetMetricsResource: virtualk8s.GetMetricsResource,
	})

	/*---------------------------------------------------
	 * Start the kubelet API on the background
	 *---------------------------------------------------*/
	allAddr := fmt.Sprintf(":%d", c.KubeletPort)
	advertisedAddr := fmt.Sprintf("%s:%d", c.KubeletAddress, c.KubeletPort)

	server := kubeletServer(allAddr, auth.wrap(mux), tlsConfig)

	go func() {
		if err := server.ListenAndServeTLS(
			c.K8sAPICertFilepath,
			c.K8sAPIKeyFilepath,
		); err != nil && !errors.Is(err, context.Canceled) {
			logrus.Fatal("API Server has failed. Err:", err)
			// handle error
		}
	}()

	DefaultLogger.Info("HTTPS server is ready",
		"Address", advertisedAddr,
		"cert", c.K8sAPICertFilepath,
		"key", c.K8sAPIKeyFilepath,
		"client-ca", c.K8sClientCAFilepath,
	)
	return nil
}

// How long a connection to the kubelet API may take over what comes before its authentication, on
// the network: the handshake and the headers of a request, and the wait for the next one.
// There is no limit on a request as a whole (ReadTimeout, WriteTimeout): logs that are followed
// run for as long as they run, and a read deadline would end them.
const (
	kubeletReadHeaderTimeout = 10 * time.Second
	kubeletIdleTimeout       = 2 * time.Minute
)

func kubeletServer(addr string, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: kubeletReadHeaderTimeout,
		IdleTimeout:       kubeletIdleTimeout,
	}
}
