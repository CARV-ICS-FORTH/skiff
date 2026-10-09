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

This compiles the `skifflet` binary for this host into `bin/skifflet`. The Linux binary that runs in the cluster is built inside the builder container (`make image` from the repository root).

## Running Tests

```bash
make test
```

## Kubernetes Support

Skifflet is a Virtual Kubelet provider: it implements the pod lifecycle and the kubelet's HTTP operations for a node, not the full kubelet. Behaviour outside the table is not qualified. Each row names the test that establishes it (see [test/README.md](../test/README.md)); the table grows as the sprint's features land.

**Kubelet HTTP operations** (served on the node's kubelet port; `kubectl get pods` talks to the API server, not to these):

| Operation | Status | Result | Alternative | Test |
|---|---|---|---|---|
| `/containerLogs` (`kubectl logs`) | implemented | The output of one attempt of the container, standard output and error in the order they were written, from records Skifflet writes with a timestamp as the container produces them. Supports `--tail` (an absent tail is every line, `--tail=0` none), `--follow`, `--timestamps`, `--since`, `--since-time`, `--limit-bytes` and `--previous`, alone and combined. A line still open when the container ends stays open (no newline is invented), blank lines are kept. The current attempt is the latest one, running or ended; `--previous` is the attempt it replaced (or, while the container waits to be restarted, the one that just ended), and a container with none says so. A follow reads one attempt: it ends after that attempt's remaining output, and never continues into the next. Only the current and previous attempts are kept, and they go with the pod. Options the API rejects (negative tail, zero byte limit, both time filters) are refused with 400 | Output written while Skifflet itself is down is lost (the container's pipes break) | T04, T12, T13 |
| `/exec` (`kubectl exec`) | implemented | Runs a command inside the running container: its namespaces, root filesystem, working directory and environment; no new container is created. Without `-t`, output and error are separate streams, input ends with end of file and the exit status is the command's (128 plus the signal if it was killed). With `-t` the command gets a real pseudo-terminal (`TERM=xterm` unless the container sets one) that follows the client's terminal size. A running init container is a valid target; a container that is not running (finished, not started, restarting) is refused with "container is not running". The session, and everything it started, ends when the client disconnects, the container's process ends or is replaced, or the pod is deleted, and never affects the application. Refused if the container's main process or namespaces do not verify | | T04, T14, T17 |
| `/portForward` (`kubectl port-forward`) | implemented | Connects to the pod IP from the node | | T04 |
| `/pods` | implemented | The pods Kubernetes has assigned to this node (desired state) | | T04 |
| `/runningpods/` | implemented | The pods Skifflet is running (runtime view) | | T04 |
| `/attach` (`kubectl attach`) | not implemented | Streaming clients get the explanation on the error stream (`kubectl` prints `error attaching to container: attach is not implemented ...`); other requests get HTTP 501 | `kubectl logs -f` to follow output; `kubectl exec` to run a command in the container | T04 |
| `/stats/summary`, `/metrics/resource` (`kubectl top`, metrics-server, autoscaling) | implemented | From the processes of each container (the tree below its launcher) and of the node, read from `/proc`: CPU time is exact for those processes, memory is their proportional share of the pages they hold, so it is an approximation of what a cgroup would say. K3s's metrics-server runs in the cluster and `kubectl top node` and `kubectl top pod` work. | | T04, T37 |

Unknown pods or containers, and malformed requests, keep their normal 404 and 400 responses.

**Lifecycle hooks** (`lifecycle.postStart`, `lifecycle.preStop` of application containers):

| Field | Status | Result | Alternative | Test |
|---|---|---|---|---|
| `exec`, `httpGet` and `sleep` handlers | implemented | Kubelet semantics. `postStart` runs once the container's application process is in place, alongside its entrypoint (no ordering between them; an HTTP hook may reach the application before it listens, as with any kubelet); the container is not reported Running, and its probes do not start, until the hook succeeded. A failing hook stops the container (`FailedPostStartHook` event) and the restart policy applies. `preStop` runs before the termination signal, whatever ends the container (deletion, a failing liveness or startup probe); it is bounded by the grace period, which it shares with the application's own shutdown (the application always gets at least 2 seconds after the signal); with a zero grace period it does not run; a failing hook is an event (`FailedPreStopHook`) and does not prevent the termination; it does not run for a container that already exited. Containers of a pod stop side by side | | T10, T11 |
| `tcpSocket` handler | not supported | The pod is rejected at creation (`UnsupportedFeatures`) naming the field (the API deprecates it) | `exec`, `httpGet` or `sleep` | unit (capabilities) |
| hooks of init containers | not supported | The pod is rejected at creation (`UnsupportedFeatures`) naming the field | Use the entrypoint or a separate init container | unit (capabilities) |

**Pod and container fields** (a pod that needs an unsupported form is refused at creation: it fails with the reason `UnsupportedFeatures` and a message naming each field, and nothing of it is launched):

| Field | Status | Result | Alternative | Test |
|---|---|---|---|---|
| `emptyDir`, `configMap`, `secret`, `downwardAPI`, `projected` (ConfigMap, Secret, Downward API, service account token) volumes | implemented | Mounted into the containers | | T15 |
| `hostPath` volumes | implemented for the two locations a volume may use | Below `/root/.skiff` (the user's `~/.skiff` on the host) and below the scratch directory (node-local): mounted. Anywhere else: the pod is refused at creation, naming the field and the two locations; nothing is created there | A path below `/root/.skiff` or the scratch directory | T32 |
| What a container sees of the node | its image, the volumes of its pod and a minimal `/dev` | Not the node's `/tmp`, the user's home, the launcher's working directory (the bubble's `/root`, where `~/.skiff` and the cluster's credentials are) or any path of the site's configuration. A container starts in `/` (`workingDir` is not supported yet) | Mount what a container needs as a volume | T31 |
| `/tmp` and `/var/tmp` of a container (init containers too) | implemented as in Kubernetes | Each container has its own, on the node's disk (in the pod's directory below the scratch directory, not in memory), empty at every start of the container and removed with the pod; no other container or pod sees them, and the node's own `/tmp` is not visible. A volume mounted there replaces them. Everything else a container writes outside its volumes goes to Apptainer's writable layer, which is in memory and small (the default is 64 MiB): put data that is large on a volume | An `emptyDir` to share files between containers; a volume for large files | T31 |
| The root filesystem of a container (its writable layer) | implemented as in Kubernetes | A layer of the container's own on the node's disk (below the pod's directory in the scratch directory, as large as the disk allows, not the 64 MiB of memory it once was), over the image unpacked for the attempt: new at every start of the container (a restart finds nothing of what the attempt before wrote), the same in every `exec`, and removed with the pod. Ephemeral-storage requests and limits are accepted and **not enforced** | Mount a volume for data that must outlive the container | T34 |
| `securityContext`: `runAsUser`, `runAsGroup`, `runAsNonRoot` | implemented | The container runs as that user and group in a user namespace of its own, in the pod's network (a container that runs as another user than root was once left in the bubble's network). Without them it runs as the user its **image** says (`USER`, a name or a number, with the group of its passwd entry), else as root, as the kubelet's runtime does. `runAsNonRoot` is the kubelet's: a container that would run as root (`runAsUser: 0`, or no user and an image that runs as root) is refused, `CreateContainerConfigError` with `container has runAsNonRoot and image will run as root`, and a `USER` that the image does not have fails the container. A container that is not root may bind any port: the pod's network namespace has `net.ipv4.ip_unprivileged_port_start` at 0, as Docker sets it | | T38, T44 |
| `securityContext`: `fsGroup`, `supplementalGroups`, `readOnlyRootFilesystem`, `capabilities`, `privileged`, `allowPrivilegeEscalation`, `seccompProfile`, `seLinuxOptions`, `appArmorProfile`, `procMount`, `sysctls` | accepted, not applied | The pod runs, so that charts that set them run unchanged, and gets a Warning event `SecurityContextNotApplied` naming them: a container runs without privileges in a user namespace of the job's user, with a writable root filesystem of its own, and its volumes are writable by any user, so `fsGroup` is not needed to write | | T38 |
| `resources`: requests and limits of cpu, memory, ephemeral-storage | requests used, limits not enforced | The scheduler places pods by their requests against what the job grants the node (all of the node's CPUs and memory, and the disk of the scratch directory); a request that fits nowhere leaves the pod Pending. A limit is accepted and not enforced (no cgroup per container): a container that uses more memory than the node has is killed by the kernel, which can be the wrong process | | T38 |
| `emptyDir` with `medium: Memory` | implemented as in Kubernetes | A tmpfs mounted by Skifflet, as large as the `sizeLimit` says, else as the memory the node offers pods (its allocatable memory); what is written counts as the memory of the job; a write past the size fails; it is removed with the pod | | T55, T56 |
| `/dev/shm` | implemented as in Kubernetes | A tmpfs of 64 MiB for the pod, shared by its containers (as they share the IPC namespace of a pod) and removed with it; a volume mounted at `/dev/shm` takes its place, the usual way to give a program (a PyTorch data loader, an MPI job) more | An `emptyDir` with `medium: Memory` at `/dev/shm` | T54, T55 |
| `resources` limit `nvidia.com/gpu` (NVIDIA GPUs) | implemented as the device plugin does | The node advertises as `nvidia.com/gpu` the GPUs Slurm gave the step the bubble runs in (an `--exclusive` job on a node with GPUs has them; the job's own request, `--gpus-per-node` or `--gres`, says how many), labeled `nvidia.com/gpu.present`, `.count` and `.product`. A pod has the GPUs of its containers' **limits** and no other: their device files and the node's driver libraries and `nvidia-smi`, `NVIDIA_VISIBLE_DEVICES` set to their UUIDs; containers of one pod have different GPUs, an init container's are the ones after it has, a restarted container has the same. A pod that asks for no GPU has none, whatever its image sets in `NVIDIA_VISIBLE_DEVICES`. The scheduler counts them; a pod sent to the node by name when there are none free is refused, `UnexpectedAdmissionError`, as by the kubelet. Whole GPUs only (no MIG, no sharing); the driver's libraries come from the node, so the CUDA of an image must be one the node's driver supports | | T46, T47, T48, T49, T50, T51, T52 |
| `runtimeClassName` | the NVIDIA classes | A pod of the RuntimeClass `nvidia` or `nvidia-experimental` (K3s creates them) runs as any pod does: the class grants no GPU, the limits do. A class with another handler is refused at creation, naming the field | | T53 |
| `hostPort` | accepted, not applied | A `hostPort` is not published on the host | A NodePort Service or `kubectl port-forward` | |
| Dynamic provisioning | implemented by the cluster's default StorageClass (not by Skifflet) | A claim that names no class gets a directory of its own on the shared home (the default class `local-path`, reclaim policy Delete); `ReadWriteOnce`, `ReadOnlyMany` and `ReadWriteMany` are accepted; the requested size is not enforced | Your own PersistentVolume for existing data | T27, T28, T29, T30 |
| `persistentVolumeClaim` volumes | partly | Claims bound to a `hostPath` or `local` volume in filesystem mode, with a path in the two locations, work, read-only when the claim says so. A claim that does not exist or is not bound yet, or whose volume is not there yet, is waited for as the kubelet does, for as long as it takes: the pod stays Pending, its containers in `ContainerCreating` with the reason, a `FailedMount` event (one per distinct reason), and it starts when the volume appears; deleting the pod ends the wait at once. A volume elsewhere, other volume types, block mode and a claim that lost its volume fail the pod at once with the reason. Node affinity of a `local` volume is the scheduler's business, as with the kubelet | Use `hostPath` or `local` volumes below `/root/.skiff` | T27, T32, unit |
| Other volume sources (`nfs`, `csi`, `ephemeral`, cloud volumes, ...), projected `clusterTrustBundle` and `podCertificate`, `volumeDevices` (block devices) | not supported | Refused at creation, naming the field | `emptyDir`, `hostPath` or a `local` claim | T05 |
| Restartable init containers (`restartPolicy: Always` on an init container), pod `activeDeadlineSeconds`, hooks of init containers, host namespaces, ephemeral containers | not supported | Refused at creation, naming the field | | T05 |
| Environment: `env`, `envFrom`, `valueFrom` of a ConfigMap or Secret key (optional references that are absent are left out) | implemented | Resolved when the container starts | | T15 |
| Downward API `fieldRef` (`metadata.name/namespace/uid/labels/annotations`, by key or whole, `spec.nodeName`, `spec.serviceAccountName`, `status.hostIP(s)`, `status.podIP(s)`) and `resourceFieldRef` (`limits.` and `requests.` of cpu, memory, ephemeral-storage, hugepages; divisors; a limit that is not set is the node's allocatable amount), in the environment and in `downwardAPI` and `projected` volumes | implemented | The same evaluation everywhere; a reference that cannot be honoured is an error, never an empty value. A reference to something Skifflet cannot resolve at all (an unknown field, `fileKeyRef`) is refused at creation | | T15 |
| A ConfigMap or Secret a container needs is missing, or a reference cannot be resolved | implemented | Only that container waits, with the reason `CreateContainerConfigError` and the message, and a `Failed` event; it is tried again with a growing pause and starts when the configuration exists. Its siblings and the pod are not failed | | T15 |
| Update of a running pod: labels and annotations | implemented | Applied without restarting anything or losing status | | T06 |
| Update of a running pod: image, resources (including the resize subresource), `activeDeadlineSeconds`, ephemeral containers | not applied | The pod keeps running exactly as it is (its container statuses show the image and attempt that really run); a Warning event `UpdateNotApplied` says what was asked, what runs and what to do, once per difference; the pod is not marked failed and no "updated successfully" event is recorded for it | Delete the pod, or roll out a new one (for example with a Deployment): a replacement pod runs the new spec | T06, T16 |

**Container probes** (`startupProbe`, `readinessProbe`, `livenessProbe`):

| Field | Status | Result | Alternative | Test |
|---|---|---|---|---|
| `httpGet`, `tcpSocket`, `exec` probes | implemented | Kubelet behaviour: HTTP 200-399 succeeds (HTTPS is not verified; a `Host` header sets the request host; a redirect to another host is not followed), TCP connects, `exec` runs in the container's namespaces, root filesystem and environment and succeeds on exit code 0. Network probes use the pod IP unless the probe names a host. Defaults and thresholds are Kubernetes' | | T07, T08 |
| startup gating | implemented | Until the startup probe succeeds the container is not `Started` or `Ready` and its readiness and liveness probes do not run; initial delays count from the container's start | | T07 |
| readiness failure | implemented | The container's `Ready` and the pod's `Ready` follow the probe in both directions; nothing is restarted, and other containers are untouched | | T07, T08 |
| liveness or startup failure | implemented | The container gets SIGTERM, then SIGKILL after the probe's `terminationGracePeriodSeconds` (else the pod's, never less than 2 seconds); the restart policy decides what follows. A container that ignores SIGTERM is killed at the deadline, as with any container whose main process handles no signals | | T07 |
| named ports (`port: http`) | implemented | Resolved against the probed container's `ports`; a name it does not declare fails the probe (never defaulted to another port) | | T07 |
| probe termination and deletion | implemented | A hanging probe command is bounded by its timeout and the container's lifetime: deleting or restarting cancels it and leaves no process behind | | T09 |
| probe events | implemented | `Unhealthy` (with the failure) for a failing readiness, liveness or startup probe and `Killing` when a container is stopped for one, recorded with the API server as from `kubelet` on the node | | T11 |
| `grpc` probes | not supported | The pod is rejected at creation (`UnsupportedFeatures`) naming the field | `httpGet`, `tcpSocket` or `exec` | unit (capabilities) |

## Examples

A slow-starting application: the startup probe gives it up to five minutes; only then do the aggressive readiness and liveness probes begin, and a PostStart hook has to succeed before the container is reported Running. The PreStop hook runs before the termination signal and shares the pod's grace period.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: slow-start
spec:
  terminationGracePeriodSeconds: 30
  containers:
  - name: app
    image: registry.example/app:1.0
    ports:
    - {name: http, containerPort: 8080}
    startupProbe:
      httpGet: {path: /startup, port: http}
      periodSeconds: 5
      failureThreshold: 60
    readinessProbe:
      httpGet: {path: /ready, port: http}
      periodSeconds: 2
    livenessProbe:
      httpGet: {path: /live, port: http}
      periodSeconds: 2
      failureThreshold: 2
    lifecycle:
      postStart:
        exec: {command: ["/bin/sh", "-c", "echo started >> /tmp/hooks"]}
      preStop:
        httpGet: {path: /drain, port: http}
```

Logs and exec work as with any kubelet:

```bash
kubectl logs slow-start --tail=20 --timestamps      # current attempt
kubectl logs slow-start --previous                  # the attempt that ended before it
kubectl logs -f slow-start --since=1m
kubectl exec slow-start -- ls /tmp                  # separate output and error streams, exit status kept
kubectl exec -it slow-start -- sh                   # a pseudo-terminal that follows the window size
```

What to use where a request is not supported: `kubectl attach` is replaced by `kubectl logs -f` and `kubectl exec`; a running pod's image or resources cannot be changed, so delete the pod or roll out a Deployment; a `grpc` probe is replaced by an `httpGet`, `tcpSocket` or `exec` probe.

## Where things live

For those changing Skifflet. Each item owns its state, and everything else asks it.

| What | Where | Owns |
|---|---|---|
| Pod admission | `podhandler/capabilities.go` | The list of fields Skifflet refuses, with the field named. A form leaves this list only when its behaviour and test exist |
| Execution of one pod | `podhandler/lifecycle.go` (`PodLifecycle`) | The one owner per pod UID: launched spec (only labels and annotations follow later updates), observed status (one writer, published in order), process facts. `podhandler/registry.go` maps UID and name to it, so a pod with the same name and a new UID replaces the old one |
| One launch of a container | `podhandler/lifecycle.go` (`Attempt`) | The attempt identity. Probes, hooks, exec sessions and log readers hold the attempt they were started for; once a newer attempt exists or it has ended, what they do is a no-op. The launcher is the only waiter for the process |
| Running commands in a container | `podhandler/target.go` (`Target`) | Finding and verifying the live container process and its namespaces. Exec, exec probes and hooks all run through it |
| Probes, hooks | `podhandler/probes.go`, `hooks.go` | Timing and thresholds, and the gates a container waits behind (`Started`, PostStart, readiness). They report to the attempt, never to the pod status directly |
| Exec sessions | `podhandler/exec.go` | Streams, pseudo-terminal and cleanup of the session and its children |
| Logs | `podhandler/logs.go`, `logread.go`, `internal/provider/logs.go` | One CRI-format file per attempt; the reader selects the attempt and applies the options; the provider layer keeps the presence of `tailLines` |
| Downward API, environment | `pkg/fieldpath`, `podhandler/environment.go` | The single evaluation of `fieldRef` and `resourceFieldRef`, used by the environment and by the volumes |
| Missing configuration | `podhandler/podhandler.go` (`prepareContainer`) | Per-container waiting and retry for configuration that is not there yet |
| Update handling | `podhandler/updates.go`, `internal/provider/update.go` | Which differences are applied and which are reported (`UpdateNotApplied`), and the filter that drops Virtual Kubelet's misleading success event |
| Network settings | `scripts/skiff.sh`, `plaid/pkg/netcfg` | The one definition of the pod and Service ranges, DNS address, MTU and cluster domain; the bubble entrypoint and Plaid read what it derives |
| Temporary files of a container | `podhandler/podhandler.go` (`prepareAttemptScratch`), `podhandler/templates.go`, `endpoint/paths.go` | One `/tmp` and `/var/tmp` per container attempt in the pod's `scratch/` directory, created empty when the attempt starts and removed when the next one starts or with the pod; separate from Apptainer's own `tmp/` of the pod, which holds unpacked root filesystems |
| Directories | `scripts/skiff.sh`, `endpoint/paths.go` | The two roots (the state directory `~/.skiff`, fixed, and the scratch directory `SKIFF_TMP_DIR`), what lives below each, and `CheckVolumePath`, the one rule for where a volume may be. The scripts and Skifflet spell them nowhere else |
| Cleanup | `podhandler/ledger.go` | A stack of cleanup actions for what the pod acquired, run once when the pod ends |

Rules that the code comments hold and the tests enforce: nothing publishes status or signals a process except through the pod's owner and the current attempt; an operation that cannot be honoured says so, as the kubelet does, and does not warn and continue; no operation started for a pod outlives the pod or its attempt.

## Command Line Flags

| Flag | Default | Description |
|---|---|---|
| `--nodename` | `skifflet` | Kubernetes node name advertised to the API server |
| `--kubelet-addr` | `$VKUBELET_ADDRESS` | Address to advertise for Kubelet API |
| `--apptainer` | `plaidtainer` | Path or binary name for Apptainer execution wrapper |
| `--pause-image` | `registry.k8s.io/pause:3.10` | OCI image for the Pod pause container |
| `--working-dir` | `$HOME` | Base working directory for Skiff runtime files (`~/.skiff`) |
| `--tmp-dir` | `/tmp/.skiff` | The node's scratch directory (the deployment's `SKIFF_TMP_DIR`, which `scripts/skiff.sh` always passes, by default `/tmp/.skiff.<uid>`): pod runtime state is below it (`.pods`) and so is Apptainer's scratch. A volume may use it and `~/.skiff` (`/root/.skiff` in a bubble), and nothing else |
| `--cluster-domain` | `$SKIFF_CLUSTER_DOMAIN`, else `cluster.local` | DNS domain of the cluster, for the search path of pods that use the cluster DNS |
| `--pod-sync-workers` | `8` | Pods Virtual Kubelet's controller works on at once. A pod is deleted inside the worker that took it, for as long as its PreStop hook and grace period take, so this is also how many pods can be terminating before one that arrives has to wait (T26) |
| `--plaid-socket` | `$PLAID_SOCKET_PATH`, else `/run/plaid/plaidd.sock` | API socket of `plaidd` |
| `--disable-taint` | `false` | Whether to disable the default `virtual-kubelet.io/provider=skiff` taint |
| `--certificate` | `$APISERVER_CERT_LOCATION` | Server TLS certificate for Kubelet HTTPS serving endpoint (logs/exec) |
| `--key` | `$APISERVER_KEY_LOCATION` | Server TLS private key for Kubelet HTTPS serving endpoint (logs/exec) |
