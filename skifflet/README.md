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
- Evaluates HTTP, TCP, and exec readiness probes after the PodIP is available; `PodReady` follows the container readiness state.
- Serves logs, exec, and port forwarding through its advertised kubelet address. Exec enters the live Apptainer payload's mount, network, PID, and root filesystem; port forwarding connects to the PodIP from the bubble.

## Building

```bash
make build
```

This compiles the `skifflet` binary into `bin/skifflet`.

To cross-compile for Linux:

```bash
make build-linux
```

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
| `/exec` (`kubectl exec`) | implemented | Runs a new process in the running container | | T04 |
| `/portForward` (`kubectl port-forward`) | implemented | Connects to the pod IP from the node | | T04 |
| `/pods` | implemented | The pods Kubernetes has assigned to this node (desired state) | | T04 |
| `/runningpods/` | implemented | The pods Skifflet is running (runtime view) | | T04 |
| `/attach` (`kubectl attach`) | not implemented | Streaming clients get the explanation on the error stream (`kubectl` prints `error attaching to container: attach is not implemented ...`); other requests get HTTP 501 | `kubectl exec` starts a new process instead | T04 |
| `/stats/summary`, `/metrics/resource` | not implemented | HTTP 501; no usage values are invented, so usage-based `kubectl top` and autoscaling do not work | | T04 |

Unknown pods or containers, and malformed requests, keep their normal 404 and 400 responses.

## Command Line Flags

| Flag | Default | Description |
|---|---|---|
| `--nodename` | `skifflet` | Kubernetes node name advertised to the API server |
| `--kubelet-addr` | `$VKUBELET_ADDRESS` | Address to advertise for Kubelet API |
| `--apptainer` | `plaidtainer` | Path or binary name for Apptainer execution wrapper |
| `--pause-image` | `registry.k8s.io/pause:3.10` | OCI image for the Pod pause container |
| `--working-dir` | `$HOME` | Base working directory for Skiff runtime files (`~/.skiff`) |
| `--pods-dir` | `/tmp/.skiff/.pods` | Ephemeral directory for pod job state and volume mounts |
| `--disable-taint` | `false` | Whether to disable the default `virtual-kubelet.io/provider=skiff` taint |
| `--certificate` | `$APISERVER_CERT_LOCATION` | Server TLS certificate for Kubelet HTTPS serving endpoint (logs/exec) |
| `--key` | `$APISERVER_KEY_LOCATION` | Server TLS private key for Kubelet HTTPS serving endpoint (logs/exec) |
