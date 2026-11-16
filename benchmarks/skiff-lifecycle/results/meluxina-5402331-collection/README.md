# Skiff lifecycle command timings

This directory contains the two-node lifecycle benchmark for Slurm job `5402331`. Durations are sorted from slowest to fastest.

Lifecycle validation: **FAIL** — 0/2 entrypoints completed cleanup.
The cluster reached both ready nodes, both ready entrypoints and both timestamp pods.
Startup command timings are complete; failed cleanup validation means shutdown rows must
not be presented as a successful graceful-close measurement.

Aggregate gates and waits are included because they explain elapsed setup time, but
they are not attributed to one executable. Rows marked `direct command` measure only
the named invocation. In particular, every `apptainer --version` invocation is measured
directly rather than inferred from the surrounding `skiff.sh check` duration.

## Slowest directly timed command

`apptainer --version` on `mel0129` took **3.938 s** (direct command (caller: `skiff_node_report`)).

## `apptainer --version` invocations

| Target | Caller | Duration |
| --- | --- | ---: |
| `mel0129` | `skiff_node_report` | 3.938 s |
| `mel0132` | `skiff_node_report` | 1.512 s |
| `mel0132` | `skiff_check_node` | 1.288 s |
| `mel0129` | `skiff_check_node` | 0.073 s |
| `mel0129` | `skiff_check_node` | 0.067 s |
| `mel0132` | `skiff_check_node` | 0.040 s |

The first version query on each node was much slower than later calls, consistent
with a cold first-invocation cost rather than a steady per-call cost.

## All instrumented commands and waits

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

The complete merged event order is in `timeline.tsv`; adjacent per-process intervals
are in `intervals.tsv`; raw command measurements are retained in `skiff-timings/`.
See `report.md` for Slurm, Kubernetes, pod-readiness and shutdown timings.
