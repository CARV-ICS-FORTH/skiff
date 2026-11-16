# Skiff lifecycle command timings

This directory contains the two-node lifecycle benchmark for Slurm job `5402316`. Durations are sorted from slowest to fastest.

Lifecycle validation: **FAIL** — 0/2 entrypoints completed cleanup.
The cluster reached both ready nodes, both ready entrypoints and both timestamp pods.
Startup command timings are complete; failed cleanup validation means shutdown rows must
not be presented as a successful graceful-close measurement.

Aggregate gates and waits are included because they explain elapsed setup time, but
they are not attributed to one executable. Rows marked `direct command` measure only
the named invocation. In particular, every `apptainer --version` invocation is measured
directly rather than inferred from the surrounding `skiff.sh check` duration.

## Slowest directly timed command

`apptainer --version` on `mel0216` took **4.482 s** (direct command (caller: `skiff_node_report`)).

## `apptainer --version` invocations

| Target | Caller | Duration |
| --- | --- | ---: |
| `mel0216` | `skiff_node_report` | 4.482 s |
| `mel0220` | `skiff_check_node` | 1.609 s |
| `mel0220` | `skiff_node_report` | 1.400 s |
| `mel0220` | `skiff_check_node` | 0.076 s |
| `mel0216` | `skiff_check_node` | 0.044 s |
| `mel0216` | `skiff_check_node` | 0.027 s |

The first version query on each node was much slower than later calls, consistent
with a cold first-invocation cost rather than a steady per-call cost.

## All instrumented commands and waits

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

The complete merged event order is in `timeline.tsv`; adjacent per-process intervals
are in `intervals.tsv`; raw command measurements are retained in `skiff-timings/`.
See `report.md` for Slurm, Kubernetes, pod-readiness and shutdown timings.
