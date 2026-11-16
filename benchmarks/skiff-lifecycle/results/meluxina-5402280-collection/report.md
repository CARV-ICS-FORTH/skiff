# Skiff lifecycle benchmark report

- Slurm job: `5402280`
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
| Allocation to Skiff supervisor | 3.439 s |
| Network discovery | 1.132 s |
| Node preflight | 8.479 s |
| Skiff image preparation | 0.010 s |
| Controller initialization gate | 8.024 s |
| Worker launch calls | 0.022 s |
| Total allocated lifetime | 45.000 s |
| Allocation to all Kubernetes Nodes Ready | 30.888 s |
| Allocation to all Skiff entrypoints Ready | 36.786 s |
| Manifest apply | 0.239 s |
| Skiff entrypoints Ready to timestamp Pods Ready | 2.495 s |
| Manifest apply to timestamp Pods Ready | 2.485 s |
| Shutdown request delivery | 0.057 s |
| Skiff supervisor cleanup | 4.940 s |
| Requested shutdown to allocation end | 4.814 s |

## Instrumented commands and waits

These are the command or polling spans that have matching start/end markers,
sorted by duration. Aggregate rows contain multiple commands or a launch plus
a readiness wait; they must not be attributed to one executable.

| Phase | Target | Command or wait | Measurement | Duration |
| --- | --- | --- | --- | ---: |
| startup | `mel0126` | `launch controller bubble; poll for kubeconfig and server CA` | aggregate gate | 8.024 s |
| startup | `mel0126` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 6.060 s |
| startup | `mel0132` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 5.537 s |
| startup | `mel0126` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 5.165 s |
| startup | `mel0126` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 4.803 s |
| startup | `mel0126` | `start k3s server; poll for /etc/rancher/k3s/k3s.yaml` | command + wait | 4.301 s |
| startup | `mel0126` | `poll plaidctl status` | polling wait | 3.315 s |
| shutdown | `mel0126` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.661 s |
| startup | `mel0126` | `apptainer instance run ...` | command | 2.564 s |
| startup | `mel0126` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.403 s |
| startup | `mel0132` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.397 s |
| startup | `mel0132` | `poll plaidctl status` | polling wait | 2.299 s |
| pod setup | `cluster` | `poll DaemonSet status until one timestamp pod per node is Ready` | polling wait | 2.246 s |
| shutdown | `mel0132` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.039 s |
| startup | `mel0132` | `poll for the controller-signed kubelet certificate` | polling wait | 2.035 s |
| startup | `mel0132` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 1.831 s |
| startup | `mel0132` | `apptainer instance run ...` | command | 1.583 s |
| startup | `mel0132` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.534 s |
| startup | `mel0126` | `poll for the controller-signed kubelet certificate` | polling wait | 1.016 s |
| startup | `mel0132` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.627 s |
| startup | `mel0132` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.516 s |
| pod setup | `login03.meluxina.lxp.lu` | `kubectl get pods; kubectl logs for every timestamp pod` | aggregate commands | 0.475 s |
| startup | `mel0126` | `verify Service addresses; start skifflet` | aggregate commands | 0.455 s |
| pod setup | `login03.meluxina.lxp.lu` | `kubectl get nodes/daemonset/pods/events/version` | aggregate commands | 0.420 s |
| startup | `mel0132` | `verify Service addresses; start skifflet` | aggregate commands | 0.419 s |
| startup | `mel0126` | `skiff_launch_run NODE 0 bash skiff.sh network-probe` | command | 0.371 s |
| startup | `mel0126` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.369 s |
| startup | `mel0132` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.360 s |
| startup | `mel0126` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.332 s |
| startup | `mel0126` | `start plaidd` | command dispatch | 0.313 s |
| startup | `mel0126` | `poll ip link show tap0` | polling wait | 0.278 s |
| startup | `mel0126` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 0.254 s |
| pod setup | `login03.meluxina.lxp.lu` | `kubectl apply -f timestamp-per-node.yaml` | command | 0.239 s |
| startup | `mel0132` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 0.220 s |
| startup | `mel0132` | `start plaidd` | command dispatch | 0.178 s |
| startup | `mel0126` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.080 s |
| shutdown | `mel0126` | `skiff_launch_stop; wait for bubble processes` | aggregate wait | 0.063 s |
| startup | `mel0126` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.062 s |
| startup | `mel0126` | `poll apptainer instance list for the instance PID` | polling wait | 0.052 s |
| startup | `mel0132` | `poll apptainer instance list for the instance PID` | polling wait | 0.051 s |
| startup | `mel0132` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.050 s |
| startup | `mel0126` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.048 s |
| startup | `mel0132` | `poll ip link show tap0` | polling wait | 0.042 s |
| shutdown | `login03.meluxina.lxp.lu` | `scancel --batch --signal=TERM JOB_ID` | command | 0.035 s |
| startup | `mel0132` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.031 s |
| startup | `mel0126` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.030 s |
| startup | `mel0132` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.023 s |
| startup | `mel0126` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.022 s |
| startup | `mel0126` | `skiff_image_files; build a newer/missing SIF with apptainer build` | command/branch | 0.010 s |
| startup | `mel0132` | `start background srun for worker bubble` | command dispatch | 0.008 s |
| startup | `mel0132` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.004 s |
| startup | `mel0126` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0132` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |

## Per-node bubble entrypoint

| Host | Bubble | Role | Bootstrap | In-bubble cleanup |
| --- | ---: | --- | ---: | ---: |
| `mel0126` | 1 | controller | 17.823 s | — |
| `mel0132` | 2 | node | 13.190 s | — |

## Timestamp pods

| Pod | Node | Printed marker |
| --- | --- | --- |
| `timestamp-per-node-ltmxs` | `mel0126` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793357605 utc=2026-10-30T10:53:25Z node=mel0126 pod=timestamp-per-node-ltmxs` |
| `timestamp-per-node-wf66c` | `mel0132` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793357606 utc=2026-10-30T10:53:26Z node=mel0132 pod=timestamp-per-node-wf66c` |

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
