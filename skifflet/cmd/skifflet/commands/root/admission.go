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
	"fmt"
	"net/http"

	"skifflet/internal/provider"

	"errors"

	"github.com/sirupsen/logrus"
)

func StartAPIServer(c Opts, virtualk8s *provider.VirtualK8S) {
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
	 * Start the Webhook on the background
	 *---------------------------------------------------*/
	allAddr := fmt.Sprintf(":%d", c.KubeletPort)
	advertisedAddr := fmt.Sprintf("%s:%d", c.KubeletAddress, c.KubeletPort)

	go func() {
		if err := http.ListenAndServeTLS(
			allAddr,
			c.K8sAPICertFilepath,
			c.K8sAPIKeyFilepath,
			mux,
		); err != nil && !errors.Is(err, context.Canceled) {
			logrus.Fatal("API Server has failed. Err:", err)
			// handle error
		}
	}()

	DefaultLogger.Info("HTTPS server is ready",
		"Address", advertisedAddr,
		"cert", c.K8sAPICertFilepath,
		"key", c.K8sAPIKeyFilepath,
	)
}
