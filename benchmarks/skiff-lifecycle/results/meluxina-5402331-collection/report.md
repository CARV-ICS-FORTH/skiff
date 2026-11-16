# Skiff lifecycle benchmark report

- Slurm job: `5402331`
- Requested nodes: 2
- Timestamp pods observed: 2
- Skiff version: `0.1.0-a9da5aa-lifecycle`
- Source commit: `a9da5aa1c7ba2c1c460320a9ec7002bea577ed57+command-timing`
- Skiff image SHA-256: `413a80d7c4796f4285d956a76d7f0e78896528a2da2b8fad8ce5ff3f80085ad6`
- Lifecycle validation: **FAIL** — 0/2 entrypoints completed cleanup
- Workload: one long-lived BusyBox timestamp pod per Kubernetes node

## Main timings

| Boundary | Duration |
| --- | ---: |
| Slurm queue (submit to allocation) | 1.000 s |
| Allocation to Skiff supervisor | 3.687 s |
| Network discovery | 1.118 s |
| Node preflight | 7.269 s |
| Skiff image preparation | 0.008 s |
| Controller initialization gate | 11.046 s |
| Worker launch calls | 0.022 s |
| Total allocated lifetime | 46.000 s |
| Allocation to all Kubernetes Nodes Ready | 33.691 s |
| Allocation to all Skiff entrypoints Ready | 38.960 s |
| Manifest apply | 0.245 s |
| Skiff entrypoints Ready to timestamp Pods Ready | 2.487 s |
| Manifest apply to timestamp Pods Ready | 2.477 s |
| Shutdown request delivery | 0.095 s |
| Skiff supervisor cleanup | 3.918 s |
| Requested shutdown to allocation end | 3.708 s |

## Instrumented commands and waits

These are the command or polling spans that have matching start/end markers,
sorted by duration. Aggregate rows contain multiple commands or a launch plus
a readiness wait; they must not be attributed to one executable.

| Phase | Target | Command or wait | Measurement | Duration |
| --- | --- | --- | --- | ---: |
| startup | `mel0129` | `launch controller bubble; poll for kubeconfig and server CA` | aggregate gate | 11.046 s |
| startup | `mel0132` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 5.622 s |
| startup | `mel0129` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 5.175 s |
| startup | `mel0129` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 4.864 s |
| startup | `mel0129` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 4.647 s |
| startup | `mel0129` | `apptainer instance run ...` | command | 4.174 s |
| startup | `mel0129` | `start k3s server; poll for /etc/rancher/k3s/k3s.yaml` | command + wait | 4.050 s |
| startup | `mel0129` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 3.938 s |
| startup | `mel0129` | `poll plaidctl status` | polling wait | 2.743 s |
| shutdown | `mel0129` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.697 s |
| startup | `mel0132` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.383 s |
| pod setup | `cluster` | `poll DaemonSet status until one timestamp pod per node is Ready` | polling wait | 2.233 s |
| startup | `mel0132` | `poll plaidctl status` | polling wait | 2.163 s |
| startup | `mel0132` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.106 s |
| startup | `mel0132` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.512 s |
| startup | `mel0132` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 1.471 s |
| startup | `mel0132` | `apptainer instance run ...` | command | 1.363 s |
| startup | `mel0129` | `poll ip link show tap0` | polling wait | 1.322 s |
| startup | `mel0132` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 1.288 s |
| startup | `mel0129` | `poll apptainer instance list for the instance PID` | polling wait | 1.236 s |
| startup | `mel0132` | `poll for the controller-signed kubelet certificate` | polling wait | 1.013 s |
| startup | `mel0129` | `poll for the controller-signed kubelet certificate` | polling wait | 1.012 s |
| shutdown | `mel0132` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 1.011 s |
| startup | `mel0132` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.639 s |
| startup | `mel0129` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.527 s |
| pod setup | `login02.meluxina.lxp.lu` | `kubectl get pods; kubectl logs for every timestamp pod` | aggregate commands | 0.440 s |
| pod setup | `login02.meluxina.lxp.lu` | `kubectl get nodes/daemonset/pods/events/version` | aggregate commands | 0.397 s |
| startup | `mel0129` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.377 s |
| startup | `mel0129` | `skiff_launch_run NODE 0 bash skiff.sh network-probe` | command | 0.375 s |
| startup | `mel0132` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.369 s |
| startup | `mel0132` | `poll ip link show tap0` | polling wait | 0.348 s |
| startup | `mel0129` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.341 s |
| startup | `mel0132` | `verify Service addresses; start skifflet` | aggregate commands | 0.302 s |
| startup | `mel0129` | `verify Service addresses; start skifflet` | aggregate commands | 0.259 s |
| startup | `mel0129` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 0.250 s |
| pod setup | `login02.meluxina.lxp.lu` | `kubectl apply -f timestamp-per-node.yaml` | command | 0.245 s |
| startup | `mel0129` | `start plaidd` | command dispatch | 0.143 s |
| startup | `mel0132` | `poll apptainer instance list for the instance PID` | polling wait | 0.080 s |
| startup | `mel0132` | `start plaidd` | command dispatch | 0.074 s |
| startup | `mel0129` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.073 s |
| startup | `mel0129` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.067 s |
| shutdown | `mel0129` | `skiff_launch_stop; wait for bubble processes` | aggregate wait | 0.042 s |
| startup | `mel0132` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.040 s |
| shutdown | `login02.meluxina.lxp.lu` | `scancel --batch --signal=TERM JOB_ID` | command | 0.034 s |
| startup | `mel0129` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.032 s |
| startup | `mel0132` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.030 s |
| startup | `mel0132` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.019 s |
| startup | `mel0132` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.012 s |
| startup | `mel0129` | `skiff_image_files; build a newer/missing SIF with apptainer build` | command/branch | 0.008 s |
| startup | `mel0132` | `start background srun for worker bubble` | command dispatch | 0.008 s |
| startup | `mel0129` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.004 s |
| startup | `mel0132` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.003 s |
| startup | `mel0129` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |

## Per-node bubble entrypoint

| Host | Bubble | Role | Bootstrap | In-bubble cleanup |
| --- | ---: | --- | ---: | ---: |
| `mel0129` | 1 | controller | 19.929 s | — |
| `mel0132` | 2 | node | 12.396 s | — |

## Timestamp pods

| Pod | Node | Printed marker |
| --- | --- | --- |
| `timestamp-per-node-rdzd9` | `mel0129` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793358221 utc=2026-10-30T11:03:41Z node=mel0129 pod=timestamp-per-node-rdzd9` |
| `timestamp-per-node-xvf89` | `mel0132` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793358221 utc=2026-10-30T11:03:41Z node=mel0132 pod=timestamp-per-node-xvf89` |

## Interpretation

The complete, time-sorted event stream is in `timeline.tsv`. It retains every
Skiff supervisor, bubble and entrypoint event plus Slurm allocation and
Kubernetes Pod transitions. Per-process raw files remain in `skiff-timings/`.

`intervals.tsv` subtracts consecutive events within each process or Pod so
individual setup and cleanup steps can be compared without using the global
interleaving of concurrent nodes.

Cross-host intervals use UTC epoch timestamps and therefore require synchronized
node clocks. Slurm and Kubernetes timestamps have one-second precision; Skiff and
observer shell events record nanoseconds. The run enables `SKIFF_TIMING=1`, so the
reported values include the small cost of clock reads and shared-storage appends.
This benchmark measures a warm system-image path unless the cache is deliberately
cleared and that cache state is recorded separately.
