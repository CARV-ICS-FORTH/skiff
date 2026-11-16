# Skiff lifecycle benchmark report

- Slurm job: `5402316`
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
| Allocation to Skiff supervisor | 4.613 s |
| Network discovery | 1.082 s |
| Node preflight | 7.922 s |
| Skiff image preparation | 0.008 s |
| Controller initialization gate | 10.028 s |
| Worker launch calls | 0.021 s |
| Total allocated lifetime | 47.000 s |
| Allocation to all Kubernetes Nodes Ready | 33.614 s |
| Allocation to all Skiff entrypoints Ready | 36.835 s |
| Manifest apply | 0.630 s |
| Skiff entrypoints Ready to timestamp Pods Ready | 2.894 s |
| Manifest apply to timestamp Pods Ready | 2.882 s |
| Shutdown request delivery | 1.576 s |
| Skiff supervisor cleanup | 4.956 s |
| Requested shutdown to allocation end | 6.421 s |

## Instrumented commands and waits

These are the command or polling spans that have matching start/end markers,
sorted by duration. Aggregate rows contain multiple commands or a launch plus
a readiness wait; they must not be attributed to one executable.

| Phase | Target | Command or wait | Measurement | Duration |
| --- | --- | --- | --- | ---: |
| startup | `mel0216` | `launch controller bubble; poll for kubeconfig and server CA` | aggregate gate | 10.028 s |
| startup | `mel0216` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 5.410 s |
| startup | `mel0216` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 5.123 s |
| startup | `mel0216` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 4.482 s |
| startup | `mel0216` | `apptainer instance run ...` | command | 4.246 s |
| startup | `mel0216` | `start k3s server; poll for /etc/rancher/k3s/k3s.yaml` | command + wait | 4.240 s |
| startup | `mel0216` | `poll plaidctl status` | polling wait | 3.266 s |
| shutdown | `mel0216` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.683 s |
| startup | `mel0220` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.493 s |
| startup | `mel0216` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.400 s |
| pod setup | `cluster` | `poll DaemonSet status until one timestamp pod per node is Ready` | polling wait | 2.252 s |
| startup | `mel0220` | `poll plaidctl status` | polling wait | 2.219 s |
| startup | `mel0220` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.088 s |
| startup | `mel0220` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 1.819 s |
| startup | `mel0220` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 1.609 s |
| shutdown | `login03.meluxina.lxp.lu` | `scancel --batch --signal=TERM JOB_ID` | command | 1.555 s |
| startup | `mel0220` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.400 s |
| startup | `mel0220` | `apptainer instance run ...` | command | 1.214 s |
| shutdown | `mel0216` | `skiff_launch_stop; wait for bubble processes` | aggregate wait | 1.062 s |
| shutdown | `mel0220` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 1.049 s |
| startup | `mel0216` | `poll for the controller-signed kubelet certificate` | polling wait | 1.024 s |
| startup | `mel0220` | `poll for the controller-signed kubelet certificate` | polling wait | 1.017 s |
| startup | `mel0220` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.771 s |
| pod setup | `login03.meluxina.lxp.lu` | `kubectl apply -f timestamp-per-node.yaml` | command | 0.630 s |
| startup | `mel0220` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.513 s |
| pod setup | `login03.meluxina.lxp.lu` | `kubectl get nodes/daemonset/pods/events/version` | aggregate commands | 0.437 s |
| startup | `mel0216` | `verify Service addresses; start skifflet` | aggregate commands | 0.419 s |
| pod setup | `login03.meluxina.lxp.lu` | `kubectl get pods; kubectl logs for every timestamp pod` | aggregate commands | 0.405 s |
| startup | `mel0216` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.394 s |
| startup | `mel0220` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.374 s |
| startup | `mel0216` | `skiff_launch_run NODE 0 bash skiff.sh network-probe` | command | 0.353 s |
| startup | `mel0220` | `verify Service addresses; start skifflet` | aggregate commands | 0.336 s |
| startup | `mel0216` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.325 s |
| startup | `mel0216` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 0.323 s |
| startup | `mel0216` | `poll ip link show tap0` | polling wait | 0.268 s |
| startup | `mel0220` | `poll ip link show tap0` | polling wait | 0.234 s |
| startup | `mel0216` | `start plaidd` | command dispatch | 0.199 s |
| startup | `mel0220` | `start plaidd` | command dispatch | 0.100 s |
| startup | `mel0216` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.079 s |
| startup | `mel0220` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.076 s |
| startup | `mel0220` | `poll apptainer instance list for the instance PID` | polling wait | 0.056 s |
| startup | `mel0216` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.044 s |
| startup | `mel0216` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.044 s |
| startup | `mel0216` | `poll apptainer instance list for the instance PID` | polling wait | 0.043 s |
| startup | `mel0216` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.030 s |
| startup | `mel0216` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.027 s |
| startup | `mel0220` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.025 s |
| startup | `mel0220` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.020 s |
| startup | `mel0220` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.014 s |
| startup | `mel0216` | `skiff_image_files; build a newer/missing SIF with apptainer build` | command/branch | 0.008 s |
| startup | `mel0220` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0220` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0216` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |

## Per-node bubble entrypoint

| Host | Bubble | Role | Bootstrap | In-bubble cleanup |
| --- | ---: | --- | ---: | ---: |
| `mel0216` | 1 | controller | 17.648 s | — |
| `mel0220` | 2 | node | 7.386 s | — |

## Timestamp pods

| Pod | Node | Printed marker |
| --- | --- | --- |
| `timestamp-per-node-65ttt` | `mel0216` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793358109 utc=2026-10-30T11:01:49Z node=mel0216 pod=timestamp-per-node-65ttt` |
| `timestamp-per-node-krxf7` | `mel0220` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793358109 utc=2026-10-30T11:01:49Z node=mel0220 pod=timestamp-per-node-krxf7` |

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
