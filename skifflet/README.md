# Skifflet

`skifflet` is a Kubernetes [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) implementation tailored for High-Performance Computing (HPC) environments running [Apptainer](https://apptainer.org/) (formerly Singularity) and [Plaid](https://github.com/forth-ics/plaid) user-space networking.

## Overview

Rather than running standard rootful container runtimes (like `containerd` or `dockerd`), `skifflet`:
- Registers as a standard Kubernetes Node with custom capacity and attributes.
- Schedules Pods directly as unprivileged Apptainer containers on HPC nodes.
- Uses the standard Kubernetes pause container (`registry.k8s.io/pause:3.10`) to hold the shared pod network namespace, while Skifflet supervises application processes and lifecycle.
- Interacts with Plaid's execution wrapper (`plaidtainer`) to give each Pod its own IP on the cluster overlay network without requiring root or host network reconfiguration.
- Supports volume mounts (ConfigMaps, Secrets, EmptyDir, HostPath, DownwardAPI, Projected volumes) directly inside unprivileged Apptainer user namespaces.
- Registers its Node before accepting pods, then waits for Plaid's ready gateway socket before marking the Node Ready. This gives K3s time to allocate a PodCIDR without starting CoreDNS or workloads before their network exists.
- Gives `ClusterFirst` pods the `kube-dns` Service IP in `/etc/resolv.conf`; `Default` pods inherit the bubble's upstream resolver. The same file is mounted into pause and application containers.
- Runs startup, readiness and liveness probes (HTTP, TCP and exec) with the Kubernetes timing and threshold rules, after the PodIP is available; `PodReady` follows the containers' readiness.
- Serves logs, exec, and port forwarding through its advertised kubelet address. Exec enters the live Apptainer payload's mount, network, PID, and root filesystem; port forwarding connects to the PodIP from the bubble.

## Building

```bash
make build
```

This compiles the `skifflet` binary for this host into `bin/skifflet`. The Linux binary that runs in the cluster is built inside the builder container (`make images` from the repository root).

## Running Tests

```bash
make test
```

## Kubernetes Support

Skifflet is a Virtual Kubelet provider: it implements the pod lifecycle and the kubelet's HTTP operations for a node, not the full kubelet. Behaviour outside the table is not qualified. Each row names the test that establishes it (see [test/README.md](../test/README.md)); the table grows as the sprint's features land.

**Kubelet HTTP operations** (served on the node's kubelet port; `kubectl get pods` talks to the API server, not to these):

| Operation | Status | Result | Alternative | Test |
|---|---|---|---|---|
| `/containerLogs` (`kubectl logs`) | implemented | Current container output | | T04 |
| `/exec` (`kubectl exec`) | implemented | Runs a command inside the running container: its namespaces, root filesystem, working directory and environment; no new container is created. Refused if the container's main process or namespaces do not verify | | T04, T17 |
| `/portForward` (`kubectl port-forward`) | implemented | Connects to the pod IP from the node | | T04 |
| `/pods` | implemented | The pods Kubernetes has assigned to this node (desired state) | | T04 |
| `/runningpods/` | implemented | The pods Skifflet is running (runtime view) | | T04 |
| `/attach` (`kubectl attach`) | not implemented | Streaming clients get the explanation on the error stream (`kubectl` prints `error attaching to container: attach is not implemented ...`); other requests get HTTP 501 | `kubectl logs -f` to follow output; `kubectl exec` to run a command in the container | T04 |
| `/stats/summary`, `/metrics/resource` | not implemented | HTTP 501; no usage values are invented, so usage-based `kubectl top` and autoscaling do not work | | T04 |

Unknown pods or containers, and malformed requests, keep their normal 404 and 400 responses.

**Container probes** (`startupProbe`, `readinessProbe`, `livenessProbe`):

| Field | Status | Result | Alternative | Test |
|---|---|---|---|---|
| `httpGet`, `tcpSocket`, `exec` probes | implemented | Kubelet behaviour: HTTP 200-399 succeeds (HTTPS is not verified; a `Host` header sets the request host; a redirect to another host is not followed), TCP connects, `exec` runs in the container's namespaces, root filesystem and environment and succeeds on exit code 0. Network probes use the pod IP unless the probe names a host. Defaults and thresholds are Kubernetes' | | T07, T08 |
| startup gating | implemented | Until the startup probe succeeds the container is not `Started` or `Ready` and its readiness and liveness probes do not run; initial delays count from the container's start | | T07 |
| readiness failure | implemented | The container's `Ready` and the pod's `Ready` follow the probe in both directions; nothing is restarted, and other containers are untouched | | T07, T08 |
| liveness or startup failure | implemented | The container gets SIGTERM, then SIGKILL after the probe's `terminationGracePeriodSeconds` (else the pod's, never less than 2 seconds); the restart policy decides what follows. A container that ignores SIGTERM is killed at the deadline, as with any container whose main process handles no signals | | T07 |
| named ports (`port: http`) | implemented | Resolved against the probed container's `ports`; a name it does not declare fails the probe (never defaulted to another port) | | T07 |
| probe termination and deletion | implemented | A hanging probe command is bounded by its timeout and the container's lifetime: deleting or restarting cancels it and leaves no process behind | | T09 |
| `grpc` probes | not supported | The pod is rejected at creation (`UnsupportedFeatures`) naming the field | `httpGet`, `tcpSocket` or `exec` | unit (capabilities) |

## Command Line Flags

| Flag | Default | Description |
|---|---|---|
| `--nodename` | `skifflet` | Kubernetes node name advertised to the API server |
| `--kubelet-addr` | `$VKUBELET_ADDRESS` | Address to advertise for Kubelet API |
| `--apptainer` | `plaidtainer` | Path or binary name for Apptainer execution wrapper |
| `--pause-image` | `registry.k8s.io/pause:3.10` | OCI image for the Pod pause container |
| `--working-dir` | `$HOME` | Base working directory for Skiff runtime files (`~/.skiff`) |
| `--pods-dir` | `/tmp/.skiff/.pods` | Ephemeral directory for pod job state and volume mounts |
| `--plaid-socket` | `$PLAID_SOCKET_PATH`, else `/run/plaid/plaidd.sock` | API socket of `plaidd` |
| `--disable-taint` | `false` | Whether to disable the default `virtual-kubelet.io/provider=skiff` taint |
| `--certificate` | `$APISERVER_CERT_LOCATION` | Server TLS certificate for Kubelet HTTPS serving endpoint (logs/exec) |
| `--key` | `$APISERVER_KEY_LOCATION` | Server TLS private key for Kubelet HTTPS serving endpoint (logs/exec) |
