# Skiff lifecycle command timings

This directory contains the two-node lifecycle benchmark for Slurm job `5402280`. Durations are sorted from slowest to fastest.

Lifecycle validation: **FAIL** — 0/2 entrypoints completed cleanup.
The cluster reached both ready nodes, both ready entrypoints and both timestamp pods.
Startup command timings are complete; failed cleanup validation means shutdown rows must
not be presented as a successful graceful-close measurement.

Aggregate gates and waits are included because they explain elapsed setup time, but
they are not attributed to one executable. Rows marked `direct command` measure only
the named invocation. In particular, every `apptainer --version` invocation is measured
directly rather than inferred from the surrounding `skiff.sh check` duration.

## Slowest directly timed command

`apptainer --version` on `mel0126` took **4.803 s** (direct command (caller: `skiff_node_report`)).

## `apptainer --version` invocations

| Target | Caller | Duration |
| --- | --- | ---: |
| `mel0126` | `skiff_node_report` | 4.803 s |
| `mel0132` | `skiff_node_report` | 1.534 s |
| `mel0126` | `skiff_check_node` | 0.080 s |
| `mel0132` | `skiff_check_node` | 0.050 s |
| `mel0126` | `skiff_check_node` | 0.048 s |
| `mel0132` | `skiff_check_node` | 0.031 s |

The first version query on each node was much slower than later calls, consistent
with a cold first-invocation cost rather than a steady per-call cost.

## All instrumented commands and waits

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

The complete merged event order is in `timeline.tsv`; adjacent per-process intervals
are in `intervals.tsv`; raw command measurements are retained in `skiff-timings/`.
See `report.md` for Slurm, Kubernetes, pod-readiness and shutdown timings.
